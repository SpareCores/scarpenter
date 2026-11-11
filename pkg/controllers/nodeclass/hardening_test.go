/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package nodeclass_test

import (
	"strings"
	"testing"

	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/controllers/nodeclass"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func TestHardeningAndTheDriversFragments(t *testing.T) {
	const fragment = "variant: flatcar\nversion: 1.1.0\nsystemd:\n  units:\n    - name: scarpenter-x.service\n      enabled: true\n"
	harden := func(nc *v1alpha1.SCNodeClass) {
		nc.Spec.Bootstrap = &v1alpha1.BootstrapSpec{Hardening: []v1alpha1.Hardening{v1alpha1.HardeningMetadataFromPods}}
	}
	// not offered: the driver is told in an Event and the NodeClass stays ready.
	t.Run("not offered", func(t *testing.T) {
		e := newEnv(t)
		e.create(e.newNodeClass(harden))
		wantCond(t, e.reconcile(e.name), status.ConditionReady, metav1.ConditionTrue, "")
		if !contains(e.rec.reasons(), nodeclass.EventHardeningNotOffered) {
			t.Errorf("events %v, want %s", e.rec.reasons(), nodeclass.EventHardeningNotOffered)
		}
	})
	t.Run("offered", func(t *testing.T) {
		e := newEnv(t)
		e.drv.hardenings = []string{vendors.HardeningMetadataFromPods}
		e.drv.fragments = []vendors.BootFragment{{Name: "x", Butane: fragment}}
		e.create(e.newNodeClass(harden))
		wantCond(t, e.reconcile(e.name), status.ConditionReady, metav1.ConditionTrue, "")
		if contains(e.rec.reasons(), nodeclass.EventHardeningNotOffered) {
			t.Errorf("events %v: the hardening is offered", e.rec.reasons())
		}
	})
	t.Run("bad fragment", func(t *testing.T) {
		e := newEnv(t)
		e.drv.hardenings = []string{vendors.HardeningMetadataFromPods}
		e.drv.fragments = []vendors.BootFragment{{Name: "x", Butane: "variant: flatcar\nversion: 9.9.9\n"}}
		e.create(e.newNodeClass(harden))
		nc := e.reconcile(e.name)
		wantCond(t, nc, v1alpha1.ConditionTypeValidationSucceeded, metav1.ConditionFalse, nodeclass.ReasonTemplateInvalid)
		if c := cond(nc, v1alpha1.ConditionTypeValidationSucceeded); c == nil || !strings.Contains(c.Message, "vendor fragment x") {
			t.Errorf("ValidationSucceeded = %+v, want the fragment named", c)
		}
	})
	t.Run("hash", func(t *testing.T) {
		nc := &v1alpha1.SCNodeClass{Spec: v1alpha1.SCNodeClassSpec{Vendors: v1alpha1.VendorBlocks{AWS: &aws.Settings{}}}}
		hash := func(applied ...string) string {
			h, err := nodeclass.Hash(nc, "aws", nodeclass.Inputs{Hardening: applied})
			if err != nil {
				t.Fatal(err)
			}
			return h
		}
		plain := hash()
		// A hardening the vendor does not offer (AWS) is not applied: no drift.
		harden(nc)
		if hash() != plain {
			t.Error("a hardening the vendor does not apply changes the hash")
		}
		if hash("metadata-from-pods") == plain {
			t.Error("applying a hardening does not change the hash")
		}
		if hash("a", "b") != hash("b", "a") {
			t.Error("the order of the hardenings changes the hash")
		}
	})
}
