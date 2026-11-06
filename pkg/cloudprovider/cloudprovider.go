/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package cloudprovider implements Karpenter core's CloudProvider interface
// for SCarpenter. Plan 02 ships a stub: it supports no instance types, so core
// never provisions, and every method returns an empty result or a "not
// implemented" error. Later plans replace the stub methods with the catalog and
// the driver registry (overview section 4).
package cloudprovider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/status"
	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
)

// Name is the name of the cloud provider.
const Name = "scarpenter"

// ErrNotImplemented is returned by the operations the stub does not support.
var ErrNotImplemented = errors.New("not implemented")

// repairToleration is how long a node may be NotReady before core repairs it
// (overview section 4); users who do not want repair disable core's NodeRepair
// feature gate.
const repairToleration = 10 * time.Minute

// CloudProvider is the stub implementation.
type CloudProvider struct{}

var _ cloudprovider.CloudProvider = (*CloudProvider)(nil)

// New returns the stub cloud provider.
func New() *CloudProvider {
	return &CloudProvider{}
}

// Create is not implemented: no instance type is ever offered, so core never
// calls it for a real NodeClaim.
func (c *CloudProvider) Create(_ context.Context, nodeClaim *karpv1.NodeClaim) (*karpv1.NodeClaim, error) {
	return nil, cloudprovider.NewCreateError(
		fmt.Errorf("creating nodeclaim %q: %w", nodeClaim.Name, ErrNotImplemented),
		"NotImplemented", "SCarpenter has no vendor drivers yet")
}

// Delete reports the NodeClaim as gone: the stub never creates instances, so
// there is nothing to delete and core may finalise the NodeClaim.
func (c *CloudProvider) Delete(_ context.Context, nodeClaim *karpv1.NodeClaim) error {
	return cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("nodeclaim %q has no instance", nodeClaim.Name))
}

// Get returns NodeClaimNotFound for every provider ID.
func (c *CloudProvider) Get(_ context.Context, providerID string) (*karpv1.NodeClaim, error) {
	return nil, cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("no instance with provider ID %q", providerID))
}

// List returns no NodeClaims.
func (c *CloudProvider) List(context.Context) ([]*karpv1.NodeClaim, error) {
	return nil, nil
}

// GetInstanceTypes returns no instance types.
func (c *CloudProvider) GetInstanceTypes(context.Context, *karpv1.NodePool) ([]*cloudprovider.InstanceType, error) {
	return nil, nil
}

// IsDrifted reports no drift.
func (c *CloudProvider) IsDrifted(context.Context, *karpv1.NodeClaim) (cloudprovider.DriftReason, error) {
	return "", nil
}

// Reboot is not supported.
func (c *CloudProvider) Reboot(context.Context, *karpv1.NodeClaim, string) error {
	return cloudprovider.NewNodeRebootNotImplementedError()
}

// RepairPolicies repairs nodes that are Ready=False or Ready=Unknown for ten
// minutes.
func (c *CloudProvider) RepairPolicies() []cloudprovider.RepairPolicy {
	return []cloudprovider.RepairPolicy{
		{ConditionType: corev1.NodeReady, ConditionStatus: corev1.ConditionFalse, TolerationDuration: repairToleration},
		{ConditionType: corev1.NodeReady, ConditionStatus: corev1.ConditionUnknown, TolerationDuration: repairToleration},
	}
}

// Name returns scarpenter.
func (c *CloudProvider) Name() string { return Name }

// GetSupportedNodeClasses returns SCNodeClass.
func (c *CloudProvider) GetSupportedNodeClasses() []status.Object {
	return []status.Object{&v1alpha1.SCNodeClass{}}
}
