/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider_test

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/cloudprovider"
)

func nodeClaim() *karpv1.NodeClaim {
	return &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "nc-1"}}
}

func TestStubSupportsNoInstanceTypes(t *testing.T) {
	cp := cloudprovider.New()
	ctx := context.Background()

	its, err := cp.GetInstanceTypes(ctx, &karpv1.NodePool{})
	if err != nil || len(its) != 0 {
		t.Errorf("GetInstanceTypes = %v, %v; want nothing", its, err)
	}
	nodeClaims, err := cp.List(ctx)
	if err != nil || len(nodeClaims) != 0 {
		t.Errorf("List = %v, %v; want nothing", nodeClaims, err)
	}
	if reason, err := cp.IsDrifted(ctx, nodeClaim()); reason != "" || err != nil {
		t.Errorf("IsDrifted = %q, %v; want no drift", reason, err)
	}
}

func TestStubErrors(t *testing.T) {
	cp := cloudprovider.New()
	ctx := context.Background()

	if _, err := cp.Create(ctx, nodeClaim()); !errors.Is(err, cloudprovider.ErrNotImplemented) {
		t.Errorf("Create error = %v, want ErrNotImplemented", err)
	}
	// Delete and Get say "not found", which core treats as done.
	if err := cp.Delete(ctx, nodeClaim()); !karpcloudprovider.IsNodeClaimNotFoundError(err) {
		t.Errorf("Delete error = %v, want NodeClaimNotFound", err)
	}
	if _, err := cp.Get(ctx, "scarpenter://aws/us-east-1/nc-1"); !karpcloudprovider.IsNodeClaimNotFoundError(err) {
		t.Errorf("Get error = %v, want NodeClaimNotFound", err)
	}
	if err := cp.Reboot(ctx, nodeClaim(), "op"); !errors.As(err, new(*karpcloudprovider.NodeRebootNotImplementedError)) {
		t.Errorf("Reboot error = %v, want NodeRebootNotImplemented", err)
	}
}

func TestIdentity(t *testing.T) {
	cp := cloudprovider.New()
	if cp.Name() != "scarpenter" {
		t.Errorf("Name = %q", cp.Name())
	}
	classes := cp.GetSupportedNodeClasses()
	if len(classes) != 1 {
		t.Fatalf("GetSupportedNodeClasses returned %d classes", len(classes))
	}
	if _, ok := classes[0].(*v1alpha1.SCNodeClass); !ok {
		t.Errorf("supported node class is %T, want *SCNodeClass", classes[0])
	}
}

func TestRepairPolicies(t *testing.T) {
	policies := cloudprovider.New().RepairPolicies()
	got := map[corev1.ConditionStatus]time.Duration{}
	for _, p := range policies {
		if p.ConditionType != corev1.NodeReady {
			t.Errorf("unexpected condition type %s", p.ConditionType)
		}
		got[p.ConditionStatus] = p.TolerationDuration
	}
	if len(policies) != 2 || got[corev1.ConditionFalse] != 10*time.Minute || got[corev1.ConditionUnknown] != 10*time.Minute {
		t.Errorf("repair policies = %+v, want Ready=False and Ready=Unknown for 10m", policies)
	}
}
