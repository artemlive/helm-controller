/*
Copyright 2023 The Flux authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	apierrutil "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
	kuberecorder "k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/ratelimiter"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	aclv1 "github.com/fluxcd/pkg/apis/acl"
	eventv1 "github.com/fluxcd/pkg/apis/event/v1beta1"
	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/acl"
	runtimeClient "github.com/fluxcd/pkg/runtime/client"
	"github.com/fluxcd/pkg/runtime/conditions"
	helper "github.com/fluxcd/pkg/runtime/controller"
	"github.com/fluxcd/pkg/runtime/jitter"
	"github.com/fluxcd/pkg/runtime/logger"
	"github.com/fluxcd/pkg/runtime/patch"
	"github.com/fluxcd/pkg/runtime/predicates"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta2"

	v2 "github.com/fluxcd/helm-controller/api/v2beta2"
	intacl "github.com/fluxcd/helm-controller/internal/acl"
	"github.com/fluxcd/helm-controller/internal/action"
	"github.com/fluxcd/helm-controller/internal/chartutil"
	"github.com/fluxcd/helm-controller/internal/digest"
	interrors "github.com/fluxcd/helm-controller/internal/errors"
	"github.com/fluxcd/helm-controller/internal/features"
	"github.com/fluxcd/helm-controller/internal/kube"
	"github.com/fluxcd/helm-controller/internal/loader"
	intpredicates "github.com/fluxcd/helm-controller/internal/predicates"
	intreconcile "github.com/fluxcd/helm-controller/internal/reconcile"
	"github.com/fluxcd/helm-controller/internal/release"
)

// +kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,resources=helmreleases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,resources=helmreleases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,resources=helmreleases/finalizers,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=helmcharts,verbs=get;list;watch
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=helmcharts/status,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// HelmReleaseReconciler reconciles a HelmRelease object.
type HelmReleaseReconciler struct {
	client.Client
	kuberecorder.EventRecorder
	helper.Metrics

	GetClusterConfig func() (*rest.Config, error)
	ClientOpts       runtimeClient.Options
	KubeConfigOpts   runtimeClient.KubeConfigOptions

	FieldManager          string
	DefaultServiceAccount string

	requeueDependency    time.Duration
	artifactFetchRetries int
}

type HelmReleaseReconcilerOptions struct {
	HTTPRetry                 int
	DependencyRequeueInterval time.Duration
	RateLimiter               ratelimiter.RateLimiter
}

var (
	errWaitForDependency = errors.New("must wait for dependency")
	errWaitForChart      = errors.New("must wait for chart")
)

func (r *HelmReleaseReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, opts HelmReleaseReconcilerOptions) error {
	// Index the HelmRelease by the HelmChart references they point at
	if err := mgr.GetFieldIndexer().IndexField(ctx, &v2.HelmRelease{}, v2.SourceIndexKey,
		func(o client.Object) []string {
			obj := o.(*v2.HelmRelease)
			return []string{
				types.NamespacedName{
					Namespace: obj.Spec.Chart.GetNamespace(obj.GetNamespace()),
					Name:      obj.GetHelmChartName(),
				}.String(),
			}
		},
	); err != nil {
		return err
	}

	r.requeueDependency = opts.DependencyRequeueInterval
	r.artifactFetchRetries = opts.HTTPRetry

	return ctrl.NewControllerManagedBy(mgr).
		For(&v2.HelmRelease{}, builder.WithPredicates(
			predicate.Or(predicate.GenerationChangedPredicate{}, predicates.ReconcileRequestedPredicate{}),
		)).
		Watches(
			&sourcev1.HelmChart{},
			handler.EnqueueRequestsFromMapFunc(r.requestsForHelmChartChange),
			builder.WithPredicates(intpredicates.SourceRevisionChangePredicate{}),
		).
		WithOptions(controller.Options{
			RateLimiter: opts.RateLimiter,
		}).
		Complete(r)
}

func (r *HelmReleaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	start := time.Now()
	log := ctrl.LoggerFrom(ctx)

	// Fetch the HelmRelease
	obj := &v2.HelmRelease{}
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Initialize the patch helper with the current version of the object.
	patchHelper := patch.NewSerialPatcher(obj, r.Client)

	// Always attempt to patch the object after each reconciliation.
	defer func() {
		if v, ok := meta.ReconcileAnnotationValue(obj.GetAnnotations()); ok {
			obj.Status.SetLastHandledReconcileRequest(v)
		}

		patchOpts := []patch.Option{
			patch.WithFieldOwner(r.FieldManager),
			patch.WithOwnedConditions{Conditions: intreconcile.OwnedConditions},
		}

		if errors.Is(retErr, reconcile.TerminalError(nil)) || (retErr == nil && (result.IsZero() || !result.Requeue)) {
			patchOpts = append(patchOpts, patch.WithStatusObservedGeneration{})
		}

		// We do not want to return these errors, but rather wait for the
		// designated RequeueAfter to expire and try again.
		// However, not returning an error will cause the patch helper to
		// patch the observed generation, which we do not want. So we ignore
		// these errors here after patching.
		retErr = interrors.Ignore(retErr, errWaitForDependency, errWaitForChart)

		if err := patchHelper.Patch(ctx, obj, patchOpts...); err != nil {
			if !obj.DeletionTimestamp.IsZero() {
				err = apierrutil.FilterOut(err, func(e error) bool { return apierrors.IsNotFound(e) })
			}
			retErr = apierrutil.Reduce(apierrutil.NewAggregate([]error{retErr, err}))
		}

		// Wait for the object to have synced in-cache after patching.
		// This is required to ensure that the next reconciliation will
		// operate on the patched object when an immediate reconcile is
		// requested.
		if err := wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 10*time.Second, true, r.waitForHistoryCacheSync(obj)); err != nil {
			log.Error(err, "failed to wait for object to sync in-cache after patching")
		}

		// Record the duration of the reconciliation.
		r.Metrics.RecordDuration(ctx, obj, start)
	}()

	// Examine if the object is under deletion.
	if !obj.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, obj)
	}

	// Add finalizer first if not exist to avoid the race condition
	// between init and delete.
	// Note: Finalizers in general can only be added when the deletionTimestamp
	// is not set.
	if !controllerutil.ContainsFinalizer(obj, v2.HelmReleaseFinalizer) {
		controllerutil.AddFinalizer(obj, v2.HelmReleaseFinalizer)
		return ctrl.Result{Requeue: true}, nil
	}

	// Return early if the object is suspended.
	if obj.Spec.Suspend {
		log.Info("reconciliation is suspended for this object")
		return ctrl.Result{}, nil
	}

	// Reconcile the HelmChart template.
	if err := r.reconcileChartTemplate(ctx, obj); err != nil {
		return ctrl.Result{}, err
	}

	return r.reconcileRelease(ctx, patchHelper, obj)
}

func (r *HelmReleaseReconciler) reconcileRelease(ctx context.Context, patchHelper *patch.SerialPatcher, obj *v2.HelmRelease) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// Mark the resource as under reconciliation.
	conditions.MarkReconciling(obj, meta.ProgressingReason, "Fulfilling prerequisites")
	if err := patchHelper.Patch(ctx, obj, patch.WithOwnedConditions{Conditions: intreconcile.OwnedConditions}, patch.WithFieldOwner(r.FieldManager)); err != nil {
		return ctrl.Result{}, err
	}

	// Confirm dependencies are Ready before proceeding.
	if c := len(obj.Spec.DependsOn); c > 0 {
		log.Info(fmt.Sprintf("checking %d dependencies", c))

		if err := r.checkDependencies(ctx, obj); err != nil {
			msg := fmt.Sprintf("dependencies do not meet ready condition (%s): retrying in %s",
				err.Error(), r.requeueDependency.String())
			conditions.MarkFalse(obj, meta.ReadyCondition, v2.DependencyNotReadyReason, err.Error())
			r.Eventf(obj, eventv1.EventSeverityInfo, v2.DependencyNotReadyReason, err.Error())
			log.Info(msg)

			// Exponential backoff would cause execution to be prolonged too much,
			// instead we requeue on a fixed interval.
			return ctrl.Result{RequeueAfter: r.requeueDependency}, errWaitForDependency
		}

		log.Info("all dependencies are ready")
	}
	// Remove any stale corresponding Ready=False condition with Unknown.
	if conditions.HasAnyReason(obj, meta.ReadyCondition, v2.DependencyNotReadyReason) {
		conditions.MarkUnknown(obj, meta.ReadyCondition, meta.ProgressingReason, "reconciliation in progress")
	}

	// Get the HelmChart object for the release.
	hc, err := r.getHelmChart(ctx, obj)
	if err != nil {
		if acl.IsAccessDenied(err) {
			conditions.MarkStalled(obj, aclv1.AccessDeniedReason, err.Error())
			conditions.MarkFalse(obj, meta.ReadyCondition, aclv1.AccessDeniedReason, err.Error())
			conditions.Delete(obj, meta.ReconcilingCondition)

			// Recovering from this is not possible without a restart of the
			// controller or a change of spec, both triggering a new
			// reconciliation.
			return ctrl.Result{}, reconcile.TerminalError(err)
		}

		msg := fmt.Sprintf("could not get HelmChart object: %s", err.Error())
		conditions.MarkFalse(obj, meta.ReadyCondition, v2.ArtifactFailedReason, msg)
		return ctrl.Result{}, err
	}
	// Remove any stale corresponding Ready=False condition with Unknown.
	if conditions.HasAnyReason(obj, meta.ReadyCondition, aclv1.AccessDeniedReason, v2.ArtifactFailedReason) {
		conditions.MarkUnknown(obj, meta.ReadyCondition, meta.ProgressingReason, "reconciliation in progress")
	}

	// Check if the HelmChart is ready.
	if ready, reason := isHelmChartReady(hc); !ready {
		msg := fmt.Sprintf("HelmChart '%s/%s' is not ready: %s", hc.GetNamespace(), hc.GetName(), reason)
		log.Info(msg)
		conditions.MarkFalse(obj, meta.ReadyCondition, "HelmChartNotReady", msg)
		// Do not requeue immediately, when the artifact is created
		// the watcher should trigger a reconciliation.
		return jitter.JitteredRequeueInterval(ctrl.Result{RequeueAfter: obj.GetRequeueAfter()}), errWaitForChart
	}
	// Remove any stale corresponding Ready=False condition with Unknown.
	if conditions.HasAnyReason(obj, meta.ReadyCondition, "HelmChartNotReady") {
		conditions.MarkUnknown(obj, meta.ReadyCondition, meta.ProgressingReason, "reconciliation in progress")
	}

	// Compose values based from the spec and references.
	values, err := chartutil.ChartValuesFromReferences(ctx, r.Client, obj.Namespace, obj.GetValues(), obj.Spec.ValuesFrom...)
	if err != nil {
		conditions.MarkFalse(obj, meta.ReadyCondition, "ValuesError", err.Error())
		return ctrl.Result{}, err
	}
	// Remove any stale corresponding Ready=False condition with Unknown.
	if conditions.HasAnyReason(obj, meta.ReadyCondition, "ValuesError") {
		conditions.MarkUnknown(obj, meta.ReadyCondition, meta.ProgressingReason, "reconciliation in progress")
	}

	// Load chart from artifact.
	loadedChart, err := loader.SecureLoadChartFromURL(loader.NewRetryableHTTPClient(ctx, r.artifactFetchRetries), hc.GetArtifact().URL, hc.GetArtifact().Digest)
	if err != nil {
		if errors.Is(err, loader.ErrFileNotFound) {
			msg := fmt.Sprintf("Chart not ready: artifact not found. Retrying in %s", r.requeueDependency.String())
			conditions.MarkFalse(obj, meta.ReadyCondition, v2.ArtifactFailedReason, msg)
			log.Info(msg)
			return ctrl.Result{RequeueAfter: r.requeueDependency}, errWaitForDependency
		}

		conditions.MarkFalse(obj, meta.ReadyCondition, v2.ArtifactFailedReason, fmt.Sprintf("Could not load chart: %s", err.Error()))
		return ctrl.Result{}, err
	}
	// Remove any stale corresponding Ready=False condition with Unknown.
	if conditions.HasAnyReason(obj, meta.ReadyCondition, v2.ArtifactFailedReason) {
		conditions.MarkUnknown(obj, meta.ReadyCondition, meta.ProgressingReason, "reconciliation in progress")
	}

	// Build the REST client getter.
	getter, err := r.buildRESTClientGetter(ctx, obj)
	if err != nil {
		conditions.MarkFalse(obj, meta.ReadyCondition, "RESTClientError", err.Error())
		return ctrl.Result{}, err
	}
	// Remove any stale corresponding Ready=False condition with Unknown.
	if conditions.HasAnyReason(obj, meta.ReadyCondition, "RESTClientError") {
		conditions.MarkUnknown(obj, meta.ReadyCondition, meta.ProgressingReason, "reconciliation in progress")
	}

	// Attempt to adopt "legacy" v2beta1 release state on a best-effort basis.
	// If this fails, the controller will fall back to performing an upgrade
	// to settle on the desired state.
	// TODO(hidde): remove this in a future release.
	if ok, _ := features.Enabled(features.AdoptLegacyReleases); ok {
		if err := r.adoptLegacyRelease(ctx, getter, obj); err != nil {
			log.Error(err, "failed to adopt v2beta1 release state")
		}
	}

	// If the release target configuration has changed, we need to uninstall the
	// previous release target first. If we did not do this, the installation would
	// fail due to resources already existing.
	if reason, changed := action.ReleaseTargetChanged(obj, loadedChart.Name()); changed {
		log.Info(fmt.Sprintf("release target configuration changed (%s): running uninstall for current release", reason))
		if err = r.reconcileUninstall(ctx, getter, obj); err != nil && !errors.Is(err, intreconcile.ErrNoLatest) {
			return ctrl.Result{}, err
		}
		obj.Status.ClearHistory()
		obj.Status.ClearFailures()
		obj.Status.StorageNamespace = ""
		return ctrl.Result{Requeue: true}, nil
	}

	// Set current storage namespace.
	obj.Status.StorageNamespace = obj.GetStorageNamespace()

	// Reset the failure count if the chart or values have changed.
	if reason, ok := action.MustResetFailures(obj, loadedChart.Metadata, values); ok {
		log.V(logger.DebugLevel).Info(fmt.Sprintf("resetting failure count (%s)", reason))
		obj.Status.ClearFailures()
	}

	// Set last attempt values.
	obj.Status.LastAttemptedGeneration = obj.Generation
	obj.Status.LastAttemptedRevision = loadedChart.Metadata.Version
	obj.Status.LastAttemptedConfigDigest = chartutil.DigestValues(digest.Canonical, values).String()
	obj.Status.LastAttemptedValuesChecksum = ""
	obj.Status.LastReleaseRevision = 0

	// Construct config factory for any further Helm actions.
	cfg, err := action.NewConfigFactory(getter,
		action.WithStorage(action.DefaultStorageDriver, obj.Status.StorageNamespace),
		action.WithStorageLog(action.NewDebugLog(ctrl.LoggerFrom(ctx).V(logger.TraceLevel))),
	)
	if err != nil {
		conditions.MarkFalse(obj, meta.ReadyCondition, "FactoryError", err.Error())
		return ctrl.Result{}, err
	}
	// Remove any stale corresponding Ready=False condition with Unknown.
	if conditions.HasAnyReason(obj, meta.ReadyCondition, "FactoryError") {
		conditions.MarkUnknown(obj, meta.ReadyCondition, meta.ProgressingReason, "reconciliation in progress")
	}

	// Off we go!
	if err = intreconcile.NewAtomicRelease(patchHelper, cfg, r.EventRecorder, r.FieldManager).Reconcile(ctx, &intreconcile.Request{
		Object: obj,
		Chart:  loadedChart,
		Values: values,
	}); err != nil {
		if errors.Is(err, intreconcile.ErrMustRequeue) {
			return ctrl.Result{Requeue: true}, nil
		}
		if interrors.IsOneOf(err, intreconcile.ErrExceededMaxRetries, intreconcile.ErrMissingRollbackTarget) {
			err = reconcile.TerminalError(err)
		}
		return ctrl.Result{}, err
	}
	return jitter.JitteredRequeueInterval(ctrl.Result{RequeueAfter: obj.GetRequeueAfter()}), nil
}

// reconcileDelete deletes the v1beta2.HelmChart of the v2beta2.HelmRelease,
// and uninstalls the Helm release if the resource has not been suspended.
func (r *HelmReleaseReconciler) reconcileDelete(ctx context.Context, obj *v2.HelmRelease) (ctrl.Result, error) {
	// Only uninstall the release and delete the HelmChart resource if the
	// resource is not suspended.
	if !obj.Spec.Suspend {
		if err := r.reconcileReleaseDeletion(ctx, obj); err != nil {
			return ctrl.Result{}, err
		}

		if err := r.reconcileChartTemplate(ctx, obj); err != nil {
			return ctrl.Result{}, err
		}
	}

	if !obj.DeletionTimestamp.IsZero() {
		// Remove our finalizer from the list.
		controllerutil.RemoveFinalizer(obj, v2.HelmReleaseFinalizer)

		// Stop reconciliation as the object is being deleted.
		return ctrl.Result{}, nil
	}

	return ctrl.Result{Requeue: true}, nil
}

// handleReleaseDeletion handles the deletion of a HelmRelease resource.
//
// Before uninstalling the release, it will check if the current configuration
// allows for uninstallation. If this is not the case, for example because a
// Secret reference is missing, it will skip the uninstallation gracefully.
//
// If the release is uninstalled successfully, the HelmRelease resource will
// be marked as ready and the current status will be cleared. If the release
// cannot be uninstalled, the HelmRelease resource will be marked as not ready
// and the error will be recorded in the status.
//
// Any returned error signals that the release could not be uninstalled, and
// the reconciliation should be retried.
func (r *HelmReleaseReconciler) reconcileReleaseDeletion(ctx context.Context, obj *v2.HelmRelease) error {
	// If the release is not marked for deletion, we should not attempt to
	// uninstall it.
	if obj.DeletionTimestamp.IsZero() {
		return fmt.Errorf("refusing to uninstall Helm release: deletion timestamp is not set")
	}

	// If the release has not been installed yet, we can skip the uninstallation.
	if obj.Status.StorageNamespace == "" {
		ctrl.LoggerFrom(ctx).Info("skipping Helm release uninstallation: no storage namespace configured")
		return nil
	}

	// Build client getter.
	getter, err := r.buildRESTClientGetter(ctx, obj)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Without a Secret reference, we cannot get a REST client
			// to uninstall the release.
			ctrl.LoggerFrom(ctx).Error(err, "skipping Helm release uninstallation")
			return nil
		}

		conditions.MarkFalse(obj, meta.ReadyCondition, v2.UninstallFailedReason,
			"failed to build REST client getter to uninstall release: %s", err.Error())
		return err
	}

	// Confirm any ServiceAccount used for impersonation exists before
	// attempting to uninstall.
	// If the ServiceAccount does not exist, for example, because the
	// namespace is being terminated, we should not attempt to uninstall the
	// release.
	if obj.Spec.KubeConfig == nil {
		cfg, err := getter.ToRESTConfig()
		if err != nil {
			// This should never happen.
			return err
		}

		if serviceAccount := cfg.Impersonate.UserName; serviceAccount != "" {
			i := strings.LastIndex(serviceAccount, ":")
			if i != -1 {
				serviceAccount = serviceAccount[i+1:]
			}

			if err = r.Client.Get(ctx, types.NamespacedName{
				Namespace: obj.GetNamespace(),
				Name:      serviceAccount,
			}, &corev1.ServiceAccount{}); err != nil {
				if client.IgnoreNotFound(err) == nil {
					// Without a ServiceAccount reference, we cannot confirm
					// the ServiceAccount exists.
					ctrl.LoggerFrom(ctx).Error(err, "skipping Helm release uninstallation")
					return nil
				}

				conditions.MarkFalse(obj, meta.ReadyCondition, v2.UninstallFailedReason,
					"failed to confirm ServiceAccount '%s' can be used to uninstall release: %s", serviceAccount, err.Error())
				return err
			}
		}
	}

	// Attempt to uninstall the release.
	if err = r.reconcileUninstall(ctx, getter, obj); err != nil && !errors.Is(err, intreconcile.ErrNoLatest) {
		return err
	}
	if err == nil {
		ctrl.LoggerFrom(ctx).Info("uninstalled Helm release for deleted resource")
	}

	// Truncate the current release details in the status.
	obj.Status.ClearHistory()
	obj.Status.StorageNamespace = ""

	return nil
}

// reconcileChartTemplate reconciles the HelmChart template from the HelmRelease.
// Effectively, this means that the HelmChart resource is created, updated or
// deleted based on the state of the HelmRelease.
func (r *HelmReleaseReconciler) reconcileChartTemplate(ctx context.Context, obj *v2.HelmRelease) error {
	return intreconcile.NewHelmChartTemplate(r.Client, r.EventRecorder, r.FieldManager).Reconcile(ctx, &intreconcile.Request{
		Object: obj,
	})
}

func (r *HelmReleaseReconciler) reconcileUninstall(ctx context.Context, getter genericclioptions.RESTClientGetter, obj *v2.HelmRelease) error {
	// Construct config factory for current release.
	cfg, err := action.NewConfigFactory(getter,
		action.WithStorage(action.DefaultStorageDriver, obj.Status.StorageNamespace),
		action.WithStorageLog(action.NewDebugLog(ctrl.LoggerFrom(ctx).V(logger.TraceLevel))),
	)
	if err != nil {
		conditions.MarkFalse(obj, meta.ReadyCondition, "ConfigFactoryErr", err.Error())
		return err
	}

	// Run uninstall.
	return intreconcile.NewUninstall(cfg, r.EventRecorder).Reconcile(ctx, &intreconcile.Request{Object: obj})
}

// checkDependencies checks if the dependencies of the given v2beta2.HelmRelease
// are Ready.
// It returns an error if a dependency can not be retrieved or is not Ready,
// otherwise nil.
func (r *HelmReleaseReconciler) checkDependencies(ctx context.Context, obj *v2.HelmRelease) error {
	for _, d := range obj.Spec.DependsOn {
		ref := types.NamespacedName{
			Namespace: d.Namespace,
			Name:      d.Name,
		}
		if ref.Namespace == "" {
			ref.Namespace = obj.GetNamespace()
		}

		dHr := &v2.HelmRelease{}
		if err := r.Get(ctx, ref, dHr); err != nil {
			return fmt.Errorf("unable to get '%s' dependency: %w", ref, err)
		}

		if dHr.Generation != dHr.Status.ObservedGeneration || !conditions.IsTrue(dHr, meta.ReadyCondition) {
			return fmt.Errorf("dependency '%s' is not ready", ref)
		}
	}
	return nil
}

// adoptLegacyRelease attempts to adopt a v2beta1 release into a v2beta2
// release.
// This is done by retrieving the last successful release from the Helm storage
// and converting it to a v2beta2 release snapshot.
// If the v2beta1 release has already been adopted, this function is a no-op.
func (r *HelmReleaseReconciler) adoptLegacyRelease(ctx context.Context, getter genericclioptions.RESTClientGetter, obj *v2.HelmRelease) error {
	if obj.Status.LastReleaseRevision < 1 || len(obj.Status.History) > 0 {
		return nil
	}

	var (
		log              = ctrl.LoggerFrom(ctx).V(logger.DebugLevel)
		storageNamespace = obj.GetStorageNamespace()
		releaseNamespace = obj.GetReleaseNamespace()
		releaseName      = obj.GetReleaseName()
		version          = obj.Status.LastReleaseRevision
	)

	log.Info("adopting %s/%s.v%d release from v2beta1 state", releaseNamespace, releaseName, version)

	// Construct config factory for current release.
	cfg, err := action.NewConfigFactory(getter,
		action.WithStorage(action.DefaultStorageDriver, storageNamespace),
		action.WithStorageLog(action.NewDebugLog(ctrl.LoggerFrom(ctx).V(logger.TraceLevel))),
	)
	if err != nil {
		return err
	}

	// Get the last successful release based on the observation for the v2beta1
	// object.
	rls, err := cfg.NewStorage().Get(releaseName, version)
	if err != nil {
		return err
	}

	// Convert it to a v2beta2 release snapshot.
	snap := release.ObservedToSnapshot(release.ObserveRelease(rls))

	// If tests are enabled, include them as well.
	if obj.GetTest().Enable {
		snap.SetTestHooks(release.TestHooksFromRelease(rls))
	}

	// Adopt it as the current release in the history.
	obj.Status.History = append(obj.Status.History, snap)
	obj.Status.StorageNamespace = storageNamespace

	// Erase the last release revision from the status.
	obj.Status.LastReleaseRevision = 0

	return nil
}

func (r *HelmReleaseReconciler) buildRESTClientGetter(ctx context.Context, obj *v2.HelmRelease) (genericclioptions.RESTClientGetter, error) {
	opts := []kube.Option{
		kube.WithNamespace(obj.GetReleaseNamespace()),
		kube.WithClientOptions(r.ClientOpts),
		// When ServiceAccountName is empty, it will fall back to the configured
		// default. If this is not configured either, this option will result in
		// a no-op.
		kube.WithImpersonate(obj.Spec.ServiceAccountName, obj.GetNamespace()),
		kube.WithPersistent(obj.UsePersistentClient()),
	}
	if obj.Spec.KubeConfig != nil {
		secretName := types.NamespacedName{
			Namespace: obj.GetNamespace(),
			Name:      obj.Spec.KubeConfig.SecretRef.Name,
		}
		var secret corev1.Secret
		if err := r.Get(ctx, secretName, &secret); err != nil {
			return nil, fmt.Errorf("could not get KubeConfig secret '%s': %w", secretName, err)
		}
		kubeConfig, err := kube.ConfigFromSecret(&secret, obj.Spec.KubeConfig.SecretRef.Key, r.KubeConfigOpts)
		if err != nil {
			return nil, err
		}
		return kube.NewMemoryRESTClientGetter(kubeConfig, opts...), nil
	}

	cfg, err := r.GetClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("could not get in-cluster REST config: %w", err)
	}
	return kube.NewMemoryRESTClientGetter(cfg, opts...), nil
}

// getHelmChart retrieves the v1beta2.HelmChart for the given v2beta2.HelmRelease
// using the name that is advertised in the status object.
// It returns the v1beta2.HelmChart, or an error.
func (r *HelmReleaseReconciler) getHelmChart(ctx context.Context, obj *v2.HelmRelease) (*sourcev1.HelmChart, error) {
	namespace, name := obj.Status.GetHelmChart()
	chartRef := types.NamespacedName{Namespace: namespace, Name: name}

	if err := intacl.AllowsAccessTo(obj, sourcev1.HelmChartKind, chartRef); err != nil {
		return nil, err
	}

	hc := sourcev1.HelmChart{}
	if err := r.Client.Get(ctx, chartRef, &hc); err != nil {
		return nil, err
	}
	return &hc, nil
}

// waitForHistoryCacheSync returns a function that can be used to wait for the
// cache backing the Kubernetes client to be in sync with the current state of
// the v2beta2.HelmRelease.
// This is a trade-off between not caching at all, and introducing a slight
// delay to ensure we always have the latest history state.
func (r *HelmReleaseReconciler) waitForHistoryCacheSync(obj *v2.HelmRelease) wait.ConditionWithContextFunc {
	newObj := &v2.HelmRelease{}
	return func(ctx context.Context) (bool, error) {
		if err := r.Get(ctx, client.ObjectKeyFromObject(obj), newObj); err != nil {
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}
		return apiequality.Semantic.DeepEqual(obj.Status.History, newObj.Status.History), nil
	}
}

func (r *HelmReleaseReconciler) requestsForHelmChartChange(ctx context.Context, o client.Object) []reconcile.Request {
	hc, ok := o.(*sourcev1.HelmChart)
	if !ok {
		err := fmt.Errorf("expected a HelmChart, got %T", o)
		ctrl.LoggerFrom(ctx).Error(err, "failed to get requests for HelmChart change")
		return nil
	}
	// If we do not have an artifact, we have no requests to make
	if hc.GetArtifact() == nil {
		return nil
	}

	var list v2.HelmReleaseList
	if err := r.List(ctx, &list, client.MatchingFields{
		v2.SourceIndexKey: client.ObjectKeyFromObject(hc).String(),
	}); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "failed to list HelmReleases for HelmChart change")
		return nil
	}

	var reqs []reconcile.Request
	for i, hr := range list.Items {
		// If the HelmRelease is ready and the revision of the artifact equals to the
		// last attempted revision, we should not make a request for this HelmRelease
		if conditions.IsReady(&list.Items[i]) && hc.GetArtifact().HasRevision(hr.Status.LastAttemptedRevision) {
			continue
		}
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return reqs
}

// isHelmChartReady returns true if the given HelmChart is ready, and a reason
// why it is not ready otherwise.
func isHelmChartReady(obj *sourcev1.HelmChart) (bool, string) {
	switch {
	case obj.Generation != obj.Status.ObservedGeneration:
		msg := "latest generation of object has not been reconciled"

		// If the chart is not ready, we can likely provide a more
		// concise reason why.
		// We do not do this while the Generation matches the Observed
		// Generation, as we could then potentially stall on e.g.
		// temporary errors which do not have an impact as long as
		// there is an Artifact for the current Generation.
		if conditions.IsFalse(obj, meta.ReadyCondition) {
			msg = conditions.GetMessage(obj, meta.ReadyCondition)
		}
		return false, msg
	case conditions.IsStalled(obj):
		return false, conditions.GetMessage(obj, meta.StalledCondition)
	case obj.Status.Artifact == nil:
		return false, "does not have an artifact"
	default:
		return true, ""
	}
}
