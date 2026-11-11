/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import (
	"context"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
)

// Per-call deadlines (as the AWS driver's): every Compute call runs under its
// own deadline, so a hung connection or the client library's retries cannot
// hold a launch, a garbage collection sweep or a quota refresh. The generated
// REST clients retry reads (get, list, aggregatedList) on HTTP 503 and 504 with
// backoff until the context ends, and give a call 600 s when the context has no
// deadline; mutating calls are not retried. A call that runs into its deadline
// returns context.DeadlineExceeded, which classify maps to Retryable
// (CallTimeout).
const (
	// DefaultCallTimeout bounds one API call (one page of a list).
	DefaultCallTimeout = 45 * time.Second
	// DefaultWaitTimeout bounds one operation wait; the server long-polls up to
	// two minutes and may answer with an operation that is still RUNNING.
	DefaultWaitTimeout = 120 * time.Second
)

// WithTimeouts wraps the API so that every call has a deadline: call for the
// requests, wait for the Wait*Operation long polls; zero selects the default.
// A deadline already on the caller's context that is shorter wins.
// TODO(plan 14): the driver wraps NewSDK's client with it.
func WithTimeouts(api ComputeAPI, call, wait time.Duration) ComputeAPI {
	if call <= 0 {
		call = DefaultCallTimeout
	}
	if wait <= 0 {
		wait = DefaultWaitTimeout
	}
	return &timedCompute{api: api, call: call, wait: wait}
}

type timedCompute struct {
	api        ComputeAPI
	call, wait time.Duration
}

var _ ComputeAPI = (*timedCompute)(nil)

// within runs one call under a deadline.
func within[Q, R any](ctx context.Context, d time.Duration, f func(context.Context, Q) (R, error), req Q) (R, error) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return f(ctx, req)
}

func (t *timedCompute) InsertInstance(
	ctx context.Context, req *computepb.InsertInstanceRequest,
) (*computepb.Operation, error) {
	return within(ctx, t.call, t.api.InsertInstance, req)
}

func (t *timedCompute) DeleteInstance(
	ctx context.Context, req *computepb.DeleteInstanceRequest,
) (*computepb.Operation, error) {
	return within(ctx, t.call, t.api.DeleteInstance, req)
}

func (t *timedCompute) GetInstance(
	ctx context.Context, req *computepb.GetInstanceRequest,
) (*computepb.Instance, error) {
	return within(ctx, t.call, t.api.GetInstance, req)
}

func (t *timedCompute) ListInstances(
	ctx context.Context, req *computepb.ListInstancesRequest,
) (*computepb.InstanceList, error) {
	return within(ctx, t.call, t.api.ListInstances, req)
}

func (t *timedCompute) AggregatedListInstances(
	ctx context.Context, req *computepb.AggregatedListInstancesRequest,
) (*computepb.InstanceAggregatedList, error) {
	return within(ctx, t.call, t.api.AggregatedListInstances, req)
}

func (t *timedCompute) ListZones(
	ctx context.Context, req *computepb.ListZonesRequest,
) (*computepb.ZoneList, error) {
	return within(ctx, t.call, t.api.ListZones, req)
}

func (t *timedCompute) GetRegion(
	ctx context.Context, req *computepb.GetRegionRequest,
) (*computepb.Region, error) {
	return within(ctx, t.call, t.api.GetRegion, req)
}

func (t *timedCompute) ListRegions(
	ctx context.Context, req *computepb.ListRegionsRequest,
) (*computepb.RegionList, error) {
	return within(ctx, t.call, t.api.ListRegions, req)
}

func (t *timedCompute) ListMachineTypes(
	ctx context.Context, req *computepb.ListMachineTypesRequest,
) (*computepb.MachineTypeList, error) {
	return within(ctx, t.call, t.api.ListMachineTypes, req)
}

func (t *timedCompute) GetProject(
	ctx context.Context, req *computepb.GetProjectRequest,
) (*computepb.Project, error) {
	return within(ctx, t.call, t.api.GetProject, req)
}

func (t *timedCompute) GetImage(
	ctx context.Context, req *computepb.GetImageRequest,
) (*computepb.Image, error) {
	return within(ctx, t.call, t.api.GetImage, req)
}

func (t *timedCompute) GetImageFromFamily(
	ctx context.Context, req *computepb.GetFromFamilyImageRequest,
) (*computepb.Image, error) {
	return within(ctx, t.call, t.api.GetImageFromFamily, req)
}

func (t *timedCompute) GetSubnetwork(
	ctx context.Context, req *computepb.GetSubnetworkRequest,
) (*computepb.Subnetwork, error) {
	return within(ctx, t.call, t.api.GetSubnetwork, req)
}

func (t *timedCompute) ListSubnetworks(
	ctx context.Context, req *computepb.ListSubnetworksRequest,
) (*computepb.SubnetworkList, error) {
	return within(ctx, t.call, t.api.ListSubnetworks, req)
}

func (t *timedCompute) InsertSubnetwork(
	ctx context.Context, req *computepb.InsertSubnetworkRequest,
) (*computepb.Operation, error) {
	return within(ctx, t.call, t.api.InsertSubnetwork, req)
}

func (t *timedCompute) DeleteSubnetwork(
	ctx context.Context, req *computepb.DeleteSubnetworkRequest,
) (*computepb.Operation, error) {
	return within(ctx, t.call, t.api.DeleteSubnetwork, req)
}

func (t *timedCompute) GetNetwork(
	ctx context.Context, req *computepb.GetNetworkRequest,
) (*computepb.Network, error) {
	return within(ctx, t.call, t.api.GetNetwork, req)
}

func (t *timedCompute) ListNetworks(
	ctx context.Context, req *computepb.ListNetworksRequest,
) (*computepb.NetworkList, error) {
	return within(ctx, t.call, t.api.ListNetworks, req)
}

func (t *timedCompute) InsertNetwork(
	ctx context.Context, req *computepb.InsertNetworkRequest,
) (*computepb.Operation, error) {
	return within(ctx, t.call, t.api.InsertNetwork, req)
}

func (t *timedCompute) DeleteNetwork(
	ctx context.Context, req *computepb.DeleteNetworkRequest,
) (*computepb.Operation, error) {
	return within(ctx, t.call, t.api.DeleteNetwork, req)
}

func (t *timedCompute) ListFirewalls(
	ctx context.Context, req *computepb.ListFirewallsRequest,
) (*computepb.FirewallList, error) {
	return within(ctx, t.call, t.api.ListFirewalls, req)
}

func (t *timedCompute) InsertFirewall(
	ctx context.Context, req *computepb.InsertFirewallRequest,
) (*computepb.Operation, error) {
	return within(ctx, t.call, t.api.InsertFirewall, req)
}

func (t *timedCompute) DeleteFirewall(
	ctx context.Context, req *computepb.DeleteFirewallRequest,
) (*computepb.Operation, error) {
	return within(ctx, t.call, t.api.DeleteFirewall, req)
}

func (t *timedCompute) GetResourcePolicy(
	ctx context.Context, req *computepb.GetResourcePolicyRequest,
) (*computepb.ResourcePolicy, error) {
	return within(ctx, t.call, t.api.GetResourcePolicy, req)
}

func (t *timedCompute) ListResourcePolicies(
	ctx context.Context, req *computepb.ListResourcePoliciesRequest,
) (*computepb.ResourcePolicyList, error) {
	return within(ctx, t.call, t.api.ListResourcePolicies, req)
}

func (t *timedCompute) InsertResourcePolicy(
	ctx context.Context, req *computepb.InsertResourcePolicyRequest,
) (*computepb.Operation, error) {
	return within(ctx, t.call, t.api.InsertResourcePolicy, req)
}

func (t *timedCompute) DeleteResourcePolicy(
	ctx context.Context, req *computepb.DeleteResourcePolicyRequest,
) (*computepb.Operation, error) {
	return within(ctx, t.call, t.api.DeleteResourcePolicy, req)
}

func (t *timedCompute) WaitZoneOperation(
	ctx context.Context, req *computepb.WaitZoneOperationRequest,
) (*computepb.Operation, error) {
	return within(ctx, t.wait, t.api.WaitZoneOperation, req)
}

func (t *timedCompute) GetZoneOperation(
	ctx context.Context, req *computepb.GetZoneOperationRequest,
) (*computepb.Operation, error) {
	return within(ctx, t.call, t.api.GetZoneOperation, req)
}

func (t *timedCompute) WaitRegionOperation(
	ctx context.Context, req *computepb.WaitRegionOperationRequest,
) (*computepb.Operation, error) {
	return within(ctx, t.wait, t.api.WaitRegionOperation, req)
}

func (t *timedCompute) GetRegionOperation(
	ctx context.Context, req *computepb.GetRegionOperationRequest,
) (*computepb.Operation, error) {
	return within(ctx, t.call, t.api.GetRegionOperation, req)
}

func (t *timedCompute) WaitGlobalOperation(
	ctx context.Context, req *computepb.WaitGlobalOperationRequest,
) (*computepb.Operation, error) {
	return within(ctx, t.wait, t.api.WaitGlobalOperation, req)
}

func (t *timedCompute) GetGlobalOperation(
	ctx context.Context, req *computepb.GetGlobalOperationRequest,
) (*computepb.Operation, error) {
	return within(ctx, t.call, t.api.GetGlobalOperation, req)
}
