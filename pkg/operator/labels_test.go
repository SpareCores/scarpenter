/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package operator_test

import (
	"testing"

	"github.com/samber/lo"

	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	coreoperator "sigs.k8s.io/karpenter/pkg/operator"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog/labels"
	"github.com/SpareCores/scarpenter/pkg/operator"
)

// TestRegisterLabelKeysFromTheNodeClassesThatExist checks the start-up hook
// (00a section 7.2): the cpu-flag keys of the curated set and the metric keys of
// the SCNodeClasses that exist at start-up are registered with core, and a
// metric added afterwards is reported as missing (RestartRequired).
func TestRegisterLabelKeysFromTheNodeClassesThatExist(t *testing.T) {
	cfg, stop, err := testutil.StartEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Error(err)
		}
	})
	lo.Must0(v1alpha1.AddToScheme(scheme.Scheme))
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	nodeClass := object(t, `
apiVersion: scarpenter.sparecores.com/v1alpha1
kind: SCNodeClass
metadata: {name: bench}
spec:
  flatcar: {version: "4757.2.3"}
  kubernetes: {version: "v1.37.1"}
  network: {template: direct}
  vendors: {fake: {}}
  metrics:
    start-metric: {benchmark: {id: "geekbench:score", config: {cores: single}}}
    other-start-metric: {workloadProfile: web}
`)
	if err := c.Create(ctx, nodeClass); err != nil {
		t.Fatal(err)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme.Scheme, Metrics: metricsserver.Options{BindAddress: "0"}})
	if err != nil {
		t.Fatal(err)
	}
	op := &operator.Operator{Operator: &coreoperator.Operator{Manager: mgr}}

	if karpv1.WellKnownLabels.Has(v1alpha1.MetricLabel("start-metric")) {
		t.Fatal("the metric key is registered before the hook ran")
	}
	if err := op.RegisterLabelKeys(ctx); err != nil {
		t.Fatal(err)
	}
	for _, k := range append(labels.FlagKeys(),
		v1alpha1.MetricLabel("start-metric"), v1alpha1.MetricPercentileLabel("start-metric"), v1alpha1.MetricPerUSDLabel("start-metric"),
		v1alpha1.MetricLabel("other-start-metric"), v1alpha1.MetricPercentileLabel("other-start-metric"), v1alpha1.MetricPerUSDLabel("other-start-metric")) {
		if !karpv1.WellKnownLabels.Has(k) {
			t.Errorf("%s is not registered with core", k)
		}
		if err := karpv1.IsRestrictedLabel(k); err != nil {
			// Registered keys of the restricted domain are accepted by core's
			// validation of NodePool requirements.
			t.Errorf("core rejects the registered key %s: %v", k, err)
		}
	}
	if missing := labels.MissingKeys([]string{"start-metric", "other-start-metric"}); len(missing) != 0 {
		t.Errorf("missing keys after the hook: %v", missing)
	}
	// A metric added later is not registered: RestartRequired names its keys.
	if missing := labels.MissingKeys([]string{"added-later"}); len(missing) != 3 {
		t.Errorf("missing keys of a later metric = %v", missing)
	}
}
