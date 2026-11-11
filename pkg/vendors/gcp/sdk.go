/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import (
	"context"
	"errors"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// SDK adapts the generated REST clients of cloud.google.com/go/compute/apiv1 to
// ComputeAPI. It holds no logic: it converts operations to their message and
// iterators to one page. Build it with NewSDK; the driver and the tests never
// touch the generated clients.
type SDK struct {
	instances    *compute.InstancesClient
	zones        *compute.ZonesClient
	regions      *compute.RegionsClient
	machineTypes *compute.MachineTypesClient
	projects     *compute.ProjectsClient
	images       *compute.ImagesClient
	subnetworks  *compute.SubnetworksClient
	networks     *compute.NetworksClient
	firewalls    *compute.FirewallsClient
	policies     *compute.ResourcePoliciesClient
	zoneOps      *compute.ZoneOperationsClient
	regionOps    *compute.RegionOperationsClient
	globalOps    *compute.GlobalOperationsClient
}

var _ ComputeAPI = (*SDK)(nil)

// NewSDK creates the REST clients with the options (credentials, endpoint); the
// user agent is set to ua when it is not empty (scarpenter/<version>). Without
// options the clients use Application Default Credentials.
func NewSDK(ctx context.Context, ua string, opts ...option.ClientOption) (*SDK, error) {
	if ua != "" {
		opts = append(opts, option.WithUserAgent(ua))
	}
	s := &SDK{}
	var err error
	if s.instances, err = compute.NewInstancesRESTClient(ctx, opts...); err != nil {
		_ = s.Close()
		return nil, err
	}
	if s.zones, err = compute.NewZonesRESTClient(ctx, opts...); err != nil {
		_ = s.Close()
		return nil, err
	}
	if s.regions, err = compute.NewRegionsRESTClient(ctx, opts...); err != nil {
		_ = s.Close()
		return nil, err
	}
	if s.machineTypes, err = compute.NewMachineTypesRESTClient(ctx, opts...); err != nil {
		_ = s.Close()
		return nil, err
	}
	if s.projects, err = compute.NewProjectsRESTClient(ctx, opts...); err != nil {
		_ = s.Close()
		return nil, err
	}
	if s.images, err = compute.NewImagesRESTClient(ctx, opts...); err != nil {
		_ = s.Close()
		return nil, err
	}
	if s.subnetworks, err = compute.NewSubnetworksRESTClient(ctx, opts...); err != nil {
		_ = s.Close()
		return nil, err
	}
	if s.networks, err = compute.NewNetworksRESTClient(ctx, opts...); err != nil {
		_ = s.Close()
		return nil, err
	}
	if s.firewalls, err = compute.NewFirewallsRESTClient(ctx, opts...); err != nil {
		_ = s.Close()
		return nil, err
	}
	if s.policies, err = compute.NewResourcePoliciesRESTClient(ctx, opts...); err != nil {
		_ = s.Close()
		return nil, err
	}
	if s.zoneOps, err = compute.NewZoneOperationsRESTClient(ctx, opts...); err != nil {
		_ = s.Close()
		return nil, err
	}
	if s.regionOps, err = compute.NewRegionOperationsRESTClient(ctx, opts...); err != nil {
		_ = s.Close()
		return nil, err
	}
	if s.globalOps, err = compute.NewGlobalOperationsRESTClient(ctx, opts...); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// Close closes every client that was created.
func (s *SDK) Close() error {
	var errs []error
	for _, c := range []interface{ Close() error }{
		closerOf(s.instances),
		closerOf(s.zones),
		closerOf(s.regions),
		closerOf(s.machineTypes),
		closerOf(s.projects),
		closerOf(s.images),
		closerOf(s.subnetworks),
		closerOf(s.networks),
		closerOf(s.firewalls),
		closerOf(s.policies),
		closerOf(s.zoneOps),
		closerOf(s.regionOps),
		closerOf(s.globalOps),
	} {
		if c != nil {
			errs = append(errs, c.Close())
		}
	}
	return errors.Join(errs...)
}

// closerOf returns nil for a client that was never created.
func closerOf[T interface{ Close() error }](c T) interface{ Close() error } {
	var zero T
	if any(c) == any(zero) {
		return nil
	}
	return c
}

// defaultPageSize is the API's own default (maxResults 500); the pager needs a
// positive size.
const defaultPageSize = 500

// page reads one page of an SDK iterator.
func page[T any](it interface{ PageInfo() *iterator.PageInfo }, size *uint32, token *string) ([]T, string, error) {
	n, tok := defaultPageSize, ""
	if size != nil && *size > 0 {
		n = int(*size)
	}
	if token != nil {
		tok = *token
	}
	var items []T
	next, err := iterator.NewPager(it, n, tok).NextPage(&items)
	return items, next, err
}

func nextOf(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *SDK) GetInstance(ctx context.Context, req *computepb.GetInstanceRequest) (*computepb.Instance, error) {
	return s.instances.Get(ctx, req)
}

func (s *SDK) GetRegion(ctx context.Context, req *computepb.GetRegionRequest) (*computepb.Region, error) {
	return s.regions.Get(ctx, req)
}

func (s *SDK) GetProject(ctx context.Context, req *computepb.GetProjectRequest) (*computepb.Project, error) {
	return s.projects.Get(ctx, req)
}

func (s *SDK) GetImage(ctx context.Context, req *computepb.GetImageRequest) (*computepb.Image, error) {
	return s.images.Get(ctx, req)
}

func (s *SDK) GetImageFromFamily(ctx context.Context, req *computepb.GetFromFamilyImageRequest) (*computepb.Image, error) {
	return s.images.GetFromFamily(ctx, req)
}

func (s *SDK) GetSubnetwork(ctx context.Context, req *computepb.GetSubnetworkRequest) (*computepb.Subnetwork, error) {
	return s.subnetworks.Get(ctx, req)
}

func (s *SDK) GetNetwork(ctx context.Context, req *computepb.GetNetworkRequest) (*computepb.Network, error) {
	return s.networks.Get(ctx, req)
}

func (s *SDK) GetResourcePolicy(ctx context.Context, req *computepb.GetResourcePolicyRequest) (*computepb.ResourcePolicy, error) {
	return s.policies.Get(ctx, req)
}

func (s *SDK) WaitZoneOperation(ctx context.Context, req *computepb.WaitZoneOperationRequest) (*computepb.Operation, error) {
	return s.zoneOps.Wait(ctx, req)
}

func (s *SDK) GetZoneOperation(ctx context.Context, req *computepb.GetZoneOperationRequest) (*computepb.Operation, error) {
	return s.zoneOps.Get(ctx, req)
}

func (s *SDK) WaitRegionOperation(ctx context.Context, req *computepb.WaitRegionOperationRequest) (*computepb.Operation, error) {
	return s.regionOps.Wait(ctx, req)
}

func (s *SDK) GetRegionOperation(ctx context.Context, req *computepb.GetRegionOperationRequest) (*computepb.Operation, error) {
	return s.regionOps.Get(ctx, req)
}

func (s *SDK) WaitGlobalOperation(ctx context.Context, req *computepb.WaitGlobalOperationRequest) (*computepb.Operation, error) {
	return s.globalOps.Wait(ctx, req)
}

func (s *SDK) GetGlobalOperation(ctx context.Context, req *computepb.GetGlobalOperationRequest) (*computepb.Operation, error) {
	return s.globalOps.Get(ctx, req)
}

func (s *SDK) InsertInstance(ctx context.Context, req *computepb.InsertInstanceRequest) (*computepb.Operation, error) {
	op, err := s.instances.Insert(ctx, req)
	if err != nil {
		return nil, err
	}
	return op.Proto(), nil
}

func (s *SDK) DeleteInstance(ctx context.Context, req *computepb.DeleteInstanceRequest) (*computepb.Operation, error) {
	op, err := s.instances.Delete(ctx, req)
	if err != nil {
		return nil, err
	}
	return op.Proto(), nil
}

func (s *SDK) InsertSubnetwork(ctx context.Context, req *computepb.InsertSubnetworkRequest) (*computepb.Operation, error) {
	op, err := s.subnetworks.Insert(ctx, req)
	if err != nil {
		return nil, err
	}
	return op.Proto(), nil
}

func (s *SDK) DeleteSubnetwork(ctx context.Context, req *computepb.DeleteSubnetworkRequest) (*computepb.Operation, error) {
	op, err := s.subnetworks.Delete(ctx, req)
	if err != nil {
		return nil, err
	}
	return op.Proto(), nil
}

func (s *SDK) InsertNetwork(ctx context.Context, req *computepb.InsertNetworkRequest) (*computepb.Operation, error) {
	op, err := s.networks.Insert(ctx, req)
	if err != nil {
		return nil, err
	}
	return op.Proto(), nil
}

func (s *SDK) DeleteNetwork(ctx context.Context, req *computepb.DeleteNetworkRequest) (*computepb.Operation, error) {
	op, err := s.networks.Delete(ctx, req)
	if err != nil {
		return nil, err
	}
	return op.Proto(), nil
}

func (s *SDK) InsertFirewall(ctx context.Context, req *computepb.InsertFirewallRequest) (*computepb.Operation, error) {
	op, err := s.firewalls.Insert(ctx, req)
	if err != nil {
		return nil, err
	}
	return op.Proto(), nil
}

func (s *SDK) DeleteFirewall(ctx context.Context, req *computepb.DeleteFirewallRequest) (*computepb.Operation, error) {
	op, err := s.firewalls.Delete(ctx, req)
	if err != nil {
		return nil, err
	}
	return op.Proto(), nil
}

func (s *SDK) InsertResourcePolicy(ctx context.Context, req *computepb.InsertResourcePolicyRequest) (*computepb.Operation, error) {
	op, err := s.policies.Insert(ctx, req)
	if err != nil {
		return nil, err
	}
	return op.Proto(), nil
}

func (s *SDK) DeleteResourcePolicy(ctx context.Context, req *computepb.DeleteResourcePolicyRequest) (*computepb.Operation, error) {
	op, err := s.policies.Delete(ctx, req)
	if err != nil {
		return nil, err
	}
	return op.Proto(), nil
}

func (s *SDK) ListInstances(ctx context.Context, req *computepb.ListInstancesRequest) (*computepb.InstanceList, error) {
	items, next, err := page[*computepb.Instance](s.instances.List(ctx, req), req.MaxResults, req.PageToken)
	if err != nil {
		return nil, err
	}
	return &computepb.InstanceList{Items: items, NextPageToken: nextOf(next)}, nil
}

func (s *SDK) ListZones(ctx context.Context, req *computepb.ListZonesRequest) (*computepb.ZoneList, error) {
	items, next, err := page[*computepb.Zone](s.zones.List(ctx, req), req.MaxResults, req.PageToken)
	if err != nil {
		return nil, err
	}
	return &computepb.ZoneList{Items: items, NextPageToken: nextOf(next)}, nil
}

func (s *SDK) ListRegions(ctx context.Context, req *computepb.ListRegionsRequest) (*computepb.RegionList, error) {
	items, next, err := page[*computepb.Region](s.regions.List(ctx, req), req.MaxResults, req.PageToken)
	if err != nil {
		return nil, err
	}
	return &computepb.RegionList{Items: items, NextPageToken: nextOf(next)}, nil
}

func (s *SDK) ListMachineTypes(ctx context.Context, req *computepb.ListMachineTypesRequest) (*computepb.MachineTypeList, error) {
	items, next, err := page[*computepb.MachineType](s.machineTypes.List(ctx, req), req.MaxResults, req.PageToken)
	if err != nil {
		return nil, err
	}
	return &computepb.MachineTypeList{Items: items, NextPageToken: nextOf(next)}, nil
}

func (s *SDK) ListSubnetworks(ctx context.Context, req *computepb.ListSubnetworksRequest) (*computepb.SubnetworkList, error) {
	items, next, err := page[*computepb.Subnetwork](s.subnetworks.List(ctx, req), req.MaxResults, req.PageToken)
	if err != nil {
		return nil, err
	}
	return &computepb.SubnetworkList{Items: items, NextPageToken: nextOf(next)}, nil
}

func (s *SDK) ListNetworks(ctx context.Context, req *computepb.ListNetworksRequest) (*computepb.NetworkList, error) {
	items, next, err := page[*computepb.Network](s.networks.List(ctx, req), req.MaxResults, req.PageToken)
	if err != nil {
		return nil, err
	}
	return &computepb.NetworkList{Items: items, NextPageToken: nextOf(next)}, nil
}

func (s *SDK) ListFirewalls(ctx context.Context, req *computepb.ListFirewallsRequest) (*computepb.FirewallList, error) {
	items, next, err := page[*computepb.Firewall](s.firewalls.List(ctx, req), req.MaxResults, req.PageToken)
	if err != nil {
		return nil, err
	}
	return &computepb.FirewallList{Items: items, NextPageToken: nextOf(next)}, nil
}

func (s *SDK) ListResourcePolicies(ctx context.Context, req *computepb.ListResourcePoliciesRequest) (*computepb.ResourcePolicyList, error) {
	items, next, err := page[*computepb.ResourcePolicy](s.policies.List(ctx, req), req.MaxResults, req.PageToken)
	if err != nil {
		return nil, err
	}
	return &computepb.ResourcePolicyList{Items: items, NextPageToken: nextOf(next)}, nil
}

func (s *SDK) AggregatedListInstances(ctx context.Context, req *computepb.AggregatedListInstancesRequest) (*computepb.InstanceAggregatedList, error) {
	pairs, next, err := page[compute.InstancesScopedListPair](s.instances.AggregatedList(ctx, req), req.MaxResults, req.PageToken)
	if err != nil {
		return nil, err
	}
	out := &computepb.InstanceAggregatedList{Items: map[string]*computepb.InstancesScopedList{}, NextPageToken: nextOf(next)}
	for _, p := range pairs {
		out.Items[p.Key] = p.Value
	}
	return out, nil
}
