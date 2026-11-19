/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package nodeclass_test

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/controllers/nodeclass"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func instanceMetadata(access v1alpha1.InstanceMetadataAccess) func(*v1alpha1.SCNodeClass) {
	return func(nc *v1alpha1.SCNodeClass) {
		nc.Spec.InstanceMetadata = &v1alpha1.InstanceMetadataSpec{Access: access}
	}
}

// The trial rendering includes the metadata fragment of the driver, unless the
// NodeClass says Open, so that a driver declaration the core cannot render is found
// before the first launch.
func TestInstanceMetadataFragmentInTheTrialRendering(t *testing.T) {
	bad := vendors.MetadataEndpoints{
		Addresses: []netip.Addr{netip.MustParseAddr("169.254.169.254")},
		Units:     []string{"not a unit"},
	}
	for name, tc := range map[string]struct {
		mutate  func(*v1alpha1.SCNodeClass)
		wantErr bool
	}{
		"restricted by default": {nil, true},
		"restricted":            {instanceMetadata(v1alpha1.InstanceMetadataRestricted), true},
		"open":                  {instanceMetadata(v1alpha1.InstanceMetadataOpen), false},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.drv.metadata = bad
			e.create(e.newNodeClass(tc.mutate))
			nc := e.reconcile(e.name)
			if !tc.wantErr {
				wantCond(t, nc, status.ConditionReady, metav1.ConditionTrue, "")
				return
			}
			wantCond(t, nc, v1alpha1.ConditionTypeValidationSucceeded, metav1.ConditionFalse, nodeclass.ReasonTemplateInvalid)
			if c := cond(nc, v1alpha1.ConditionTypeValidationSucceeded); c == nil || !strings.Contains(c.Message, "not a unit") {
				t.Errorf("ValidationSucceeded = %+v, want the unit named", c)
			}
		})
	}
	t.Run("a good declaration", func(t *testing.T) {
		e := newEnv(t)
		e.drv.metadata = vendors.MetadataEndpoints{Addresses: []netip.Addr{netip.MustParseAddr("fd00:ec2::254")}, Units: []string{"agent.service"}}
		e.create(e.newNodeClass(nil))
		wantCond(t, e.reconcile(e.name), status.ConditionReady, metav1.ConditionTrue, "")
	})
}

// A NodeClass that does not set instanceMetadata keeps the hash it had before the
// field existed: the default reaches new nodes only and drifts no running one.
func TestHashOfInstanceMetadata(t *testing.T) {
	const aws, fake = "73bc67b8b0c44d3368cf2b63d1ee6a37d90d1daae83a45a69e3f8c5c4a19ba84", "3e25aee0c7f1d0c94840eca43e39ce38a52372cecb18f3c7fda316618129a939"
	nc := base()
	if got := hash(t, nc, "aws"); got != aws {
		t.Errorf("the hash of a NodeClass without instanceMetadata changed: %s", got)
	}
	if got := hash(t, nc, "fake"); got != fake {
		t.Errorf("the hash of a NodeClass without instanceMetadata changed: %s", got)
	}
	instanceMetadata(v1alpha1.InstanceMetadataRestricted)(nc)
	restricted := hash(t, nc, "aws")
	instanceMetadata(v1alpha1.InstanceMetadataOpen)(nc)
	if open := hash(t, nc, "aws"); open == aws || open == restricted || restricted == aws {
		t.Errorf("setting instanceMetadata must change the hash, once per value: unset %s, Restricted %s, Open %s", aws, restricted, open)
	}
}

// A NodeClass that still sets the deprecated spec.bootstrap.hardening stays ready
// and is told in an Event that the field has no effect.
func TestDeprecatedHardeningIsWarnedAbout(t *testing.T) {
	e := newEnv(t)
	e.create(e.newNodeClass(func(nc *v1alpha1.SCNodeClass) {
		nc.Spec.Bootstrap = &v1alpha1.BootstrapSpec{Hardening: []v1alpha1.Hardening{v1alpha1.HardeningMetadataFromPods}}
	}))
	wantCond(t, e.reconcile(e.name), status.ConditionReady, metav1.ConditionTrue, "")
	if !contains(e.rec.reasons(), nodeclass.EventDeprecatedHardening) {
		t.Errorf("events %v, want %s", e.rec.reasons(), nodeclass.EventDeprecatedHardening)
	}
}
