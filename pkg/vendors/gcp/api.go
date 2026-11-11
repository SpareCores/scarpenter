/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package gcp is the Google Compute Engine driver of SCarpenter (plan 16).
//
// Status: the lower layers only. This package holds the Compute API wrapped
// behind ComputeAPI, the error classification, the quota reader, and the pure
// mapping code (names, labels, provider IDs, states, the launch request). The
// vendors.Driver implementation, its registration, the NodeClass API types and
// the conformance wiring wait for plan 14 (TODO(plan 14) marks the seams).
package gcp

import (
	"context"

	"cloud.google.com/go/compute/apiv1/computepb"
)

// ComputeAPI is the part of the Compute Engine API the driver calls, with the
// computepb request and response messages of the generated client. It is narrow
// on purpose: the in-memory fake (pkg/vendors/gcp/fake) implements exactly this,
// and so does the SDK adapter (NewSDK). The IAM role of the driver grants the
// permissions of these methods and no others (plan 16, section 8).
//
// Differences from the generated clients, all made to keep the fake faithful:
//   - mutating calls return the computepb.Operation message, which the driver
//     waits on with the Wait* methods (the SDK's *Operation type has private
//     state and cannot be faked);
//   - list calls return one page (the response message with NextPageToken), the
//     caller pages; the adapter does the iterator-to-page conversion.
type ComputeAPI interface {
	InsertInstance(ctx context.Context, req *computepb.InsertInstanceRequest) (*computepb.Operation, error)
	DeleteInstance(ctx context.Context, req *computepb.DeleteInstanceRequest) (*computepb.Operation, error)
	GetInstance(ctx context.Context, req *computepb.GetInstanceRequest) (*computepb.Instance, error)
	ListInstances(ctx context.Context, req *computepb.ListInstancesRequest) (*computepb.InstanceList, error)
	AggregatedListInstances(ctx context.Context, req *computepb.AggregatedListInstancesRequest) (*computepb.InstanceAggregatedList, error)

	ListZones(ctx context.Context, req *computepb.ListZonesRequest) (*computepb.ZoneList, error)
	GetRegion(ctx context.Context, req *computepb.GetRegionRequest) (*computepb.Region, error)
	ListRegions(ctx context.Context, req *computepb.ListRegionsRequest) (*computepb.RegionList, error)
	ListMachineTypes(ctx context.Context, req *computepb.ListMachineTypesRequest) (*computepb.MachineTypeList, error)
	GetProject(ctx context.Context, req *computepb.GetProjectRequest) (*computepb.Project, error)

	GetImage(ctx context.Context, req *computepb.GetImageRequest) (*computepb.Image, error)
	GetImageFromFamily(ctx context.Context, req *computepb.GetFromFamilyImageRequest) (*computepb.Image, error)

	GetSubnetwork(ctx context.Context, req *computepb.GetSubnetworkRequest) (*computepb.Subnetwork, error)
	ListSubnetworks(ctx context.Context, req *computepb.ListSubnetworksRequest) (*computepb.SubnetworkList, error)
	InsertSubnetwork(ctx context.Context, req *computepb.InsertSubnetworkRequest) (*computepb.Operation, error)
	DeleteSubnetwork(ctx context.Context, req *computepb.DeleteSubnetworkRequest) (*computepb.Operation, error)

	GetNetwork(ctx context.Context, req *computepb.GetNetworkRequest) (*computepb.Network, error)
	ListNetworks(ctx context.Context, req *computepb.ListNetworksRequest) (*computepb.NetworkList, error)
	InsertNetwork(ctx context.Context, req *computepb.InsertNetworkRequest) (*computepb.Operation, error)
	DeleteNetwork(ctx context.Context, req *computepb.DeleteNetworkRequest) (*computepb.Operation, error)

	ListFirewalls(ctx context.Context, req *computepb.ListFirewallsRequest) (*computepb.FirewallList, error)
	InsertFirewall(ctx context.Context, req *computepb.InsertFirewallRequest) (*computepb.Operation, error)
	DeleteFirewall(ctx context.Context, req *computepb.DeleteFirewallRequest) (*computepb.Operation, error)

	GetResourcePolicy(ctx context.Context, req *computepb.GetResourcePolicyRequest) (*computepb.ResourcePolicy, error)
	ListResourcePolicies(ctx context.Context, req *computepb.ListResourcePoliciesRequest) (*computepb.ResourcePolicyList, error)
	InsertResourcePolicy(ctx context.Context, req *computepb.InsertResourcePolicyRequest) (*computepb.Operation, error)
	DeleteResourcePolicy(ctx context.Context, req *computepb.DeleteResourcePolicyRequest) (*computepb.Operation, error)

	// Wait long-polls (up to two minutes server side) until the operation is
	// DONE or the server gives up; the returned operation may still be
	// RUNNING. Get reads the operation without waiting.
	WaitZoneOperation(ctx context.Context, req *computepb.WaitZoneOperationRequest) (*computepb.Operation, error)
	GetZoneOperation(ctx context.Context, req *computepb.GetZoneOperationRequest) (*computepb.Operation, error)
	WaitRegionOperation(ctx context.Context, req *computepb.WaitRegionOperationRequest) (*computepb.Operation, error)
	GetRegionOperation(ctx context.Context, req *computepb.GetRegionOperationRequest) (*computepb.Operation, error)
	WaitGlobalOperation(ctx context.Context, req *computepb.WaitGlobalOperationRequest) (*computepb.Operation, error)
	GetGlobalOperation(ctx context.Context, req *computepb.GetGlobalOperationRequest) (*computepb.Operation, error)
}
