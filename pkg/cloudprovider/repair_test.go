/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider_test

import (
	"testing"

	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
	coreoptions "sigs.k8s.io/karpenter/pkg/operator/options"

	"github.com/SpareCores/scarpenter/pkg/cloudprovider"
)

// With core's NodeRepair gate on, core refuses a policy set it cannot use (no
// action, several fallbacks) and panics without a matcher, so the policies must
// pass core's own validation.
func TestRepairPoliciesPassCoreValidation(t *testing.T) {
	opts := &coreoptions.Options{FeatureGates: coreoptions.FeatureGates{NodeRepair: true}}
	matcher, err := health.NewRepairPolicyMatcher(opts.ToContext(t.Context()), cloudprovider.New(cloudprovider.Options{}))
	if err != nil {
		t.Fatalf("RepairPolicies are not valid for core's node repair: %v", err)
	}
	if matcher == nil {
		t.Fatal("no matcher with the NodeRepair gate on")
	}
}
