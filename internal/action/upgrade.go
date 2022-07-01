/*
Copyright 2022 The Flux authors

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

package action

import (
	"context"
	"fmt"

	helmaction "helm.sh/helm/v3/pkg/action"
	helmchart "helm.sh/helm/v3/pkg/chart"
	helmchartutil "helm.sh/helm/v3/pkg/chartutil"
	helmrelease "helm.sh/helm/v3/pkg/release"

	v2 "github.com/fluxcd/helm-controller/api/v2beta2"
	"github.com/fluxcd/helm-controller/internal/features"
	"github.com/fluxcd/helm-controller/internal/postrender"
)

// Upgrade runs the Helm upgrade action with the provided config, using the
// v2beta2.HelmReleaseSpec of the given object to determine the target release
// and upgrade configuration.
//
// It performs the upgrade according to the spec, which includes upgrading the
// CRDs according to the defined policy.
//
// It does not determine if there is a desire to perform the action, this is
// expected to be done by the caller. In addition, it does not take note of the
// action result. The caller is expected to listen to this using a
// storage.ObserveFunc, which provides superior access to Helm storage writes.
func Upgrade(ctx context.Context, config *helmaction.Configuration, obj *v2.HelmRelease, chrt *helmchart.Chart,
	vals helmchartutil.Values) (*helmrelease.Release, error) {
	upgrade, err := newUpgrade(config, obj)
	if err != nil {
		return nil, err
	}

	policy, err := crdPolicyOrDefault(obj.Spec.GetInstall().CRDs)
	if err != nil {
		return nil, err
	}
	if err := applyCRDs(config, policy, chrt, setOriginVisitor(v2.GroupVersion.Group, obj.Namespace, obj.Name)); err != nil {
		return nil, fmt.Errorf("failed to apply CustomResourceDefinitions: %w", err)
	}

	return upgrade.RunWithContext(ctx, obj.GetReleaseName(), chrt, vals.AsMap())
}

func newUpgrade(config *helmaction.Configuration, rel *v2.HelmRelease) (*helmaction.Upgrade, error) {
	upgrade := helmaction.NewUpgrade(config)

	upgrade.Namespace = rel.GetReleaseNamespace()
	upgrade.ResetValues = !rel.Spec.GetUpgrade().PreserveValues
	upgrade.ReuseValues = rel.Spec.GetUpgrade().PreserveValues
	upgrade.MaxHistory = rel.GetMaxHistory()
	upgrade.Timeout = rel.Spec.GetUpgrade().GetTimeout(rel.GetTimeout()).Duration
	upgrade.Wait = !rel.Spec.GetUpgrade().DisableWait
	upgrade.WaitForJobs = !rel.Spec.GetUpgrade().DisableWaitForJobs
	upgrade.DisableHooks = rel.Spec.GetUpgrade().DisableHooks
	upgrade.DisableOpenAPIValidation = rel.Spec.GetUpgrade().DisableOpenAPIValidation
	upgrade.Force = rel.Spec.GetUpgrade().Force
	upgrade.CleanupOnFail = rel.Spec.GetUpgrade().CleanupOnFail
	upgrade.Devel = true

	// If the user opted-in to allow DNS lookups, enable it.
	if allowDNS, _ := features.Enabled(features.AllowDNSLookups); allowDNS {
		upgrade.EnableDNS = allowDNS
	}

	renderer, err := postrender.BuildPostRenderers(rel)
	if err != nil {
		return nil, err
	}
	upgrade.PostRenderer = renderer

	return upgrade, err
}