/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package main

import (
	"context"
	"strings"

	"cloud.google.com/go/compute/apiv1/computepb"

	"github.com/SpareCores/scarpenter/pkg/vendors/gcp"
)

// The read-only guard of the GCP canary (the audit and errReadOnly are aws_guard.go's). The
// canary runs the driver's own read paths with an identity that cannot write; this guard is the
// second wall: the driver is given a client that refuses every call that is not a get, a list or
// an operation read before it reaches the SDK. gcpROCompute implements gcp.ComputeAPI method by
// method (a method added to the interface breaks the build here), and gcp_guard_test.go checks
// by reflection that every method of the interface is either passed through as a read or
// refused, and that every method has the IAM permission the audit and canary-policy.sh speak in.

// gcpPermissions maps each method of gcp.ComputeAPI to the IAM permission its call needs
// (docs/plans/v1/wip/16-gcp.md section 8). The writes are listed too: a refusal is recorded with
// the permission it would have needed.
var gcpPermissions = map[string]string{
	"InsertInstance":          "compute.instances.create",
	"DeleteInstance":          "compute.instances.delete",
	"GetInstance":             "compute.instances.get",
	"ListInstances":           "compute.instances.list",
	"AggregatedListInstances": "compute.instances.list",
	"ListZones":               "compute.zones.list",
	"GetRegion":               "compute.regions.get",
	"ListRegions":             "compute.regions.list",
	"ListMachineTypes":        "compute.machineTypes.list",
	"GetProject":              "compute.projects.get",
	"GetImage":                "compute.images.get",
	"GetImageFromFamily":      "compute.images.getFromFamily",
	"ListImages":              "compute.images.list",
	"GetSubnetwork":           "compute.subnetworks.get",
	"ListSubnetworks":         "compute.subnetworks.list",
	"InsertSubnetwork":        "compute.subnetworks.create",
	"DeleteSubnetwork":        "compute.subnetworks.delete",
	"GetNetwork":              "compute.networks.get",
	"ListNetworks":            "compute.networks.list",
	"InsertNetwork":           "compute.networks.create",
	"DeleteNetwork":           "compute.networks.delete",
	"ListFirewalls":           "compute.firewalls.list",
	"InsertFirewall":          "compute.firewalls.create",
	"DeleteFirewall":          "compute.firewalls.delete",
	"GetResourcePolicy":       "compute.resourcePolicies.get",
	"ListResourcePolicies":    "compute.resourcePolicies.list",
	"InsertResourcePolicy":    "compute.resourcePolicies.create",
	"DeleteResourcePolicy":    "compute.resourcePolicies.delete",
	"WaitZoneOperation":       "compute.zoneOperations.get",
	"GetZoneOperation":        "compute.zoneOperations.get",
	"WaitRegionOperation":     "compute.regionOperations.get",
	"GetRegionOperation":      "compute.regionOperations.get",
	"WaitGlobalOperation":     "compute.globalOperations.get",
	"GetGlobalOperation":      "compute.globalOperations.get",
}

// isGCPRead reports whether a method of the API is a read: a get, a list (also the
// aggregated one) or the read of an operation. Everything else (Insert, Delete, and any
// verb a later version of the interface adds) is refused.
func isGCPRead(method string) bool {
	for _, p := range []string{"Get", "List", "AggregatedList", "Wait"} {
		if strings.HasPrefix(method, p) {
			return true
		}
	}
	return false
}

// gcpPermissionOf is the IAM permission the audit records for a method; a method the table
// does not know is recorded as unmapped (and guard_test.go fails on it).
func gcpPermissionOf(method string) string {
	if p, ok := gcpPermissions[method]; ok {
		return p
	}
	return "unmapped:" + method
}

// gcpROCompute is a gcp.ComputeAPI that passes the reads on and refuses the writes.
type gcpROCompute struct {
	inner gcp.ComputeAPI
	audit *audit
}

var _ gcp.ComputeAPI = (*gcpROCompute)(nil)

// gcpReadOnlyCompute wraps the API: every call it makes is guarded. The audit is the one of the
// AWS guard (aws_guard.go): its actions here are IAM permissions.
func gcpReadOnlyCompute(inner gcp.ComputeAPI, a *audit) gcp.ComputeAPI {
	return &gcpROCompute{inner: inner, audit: a}
}

func (g *gcpROCompute) InsertInstance(context.Context, *computepb.InsertInstanceRequest) (*computepb.Operation, error) {
	return nil, g.audit.deny(gcpPermissionOf("InsertInstance"))
}

func (g *gcpROCompute) DeleteInstance(context.Context, *computepb.DeleteInstanceRequest) (*computepb.Operation, error) {
	return nil, g.audit.deny(gcpPermissionOf("DeleteInstance"))
}

func (g *gcpROCompute) GetInstance(ctx context.Context, req *computepb.GetInstanceRequest) (*computepb.Instance, error) {
	g.audit.read(gcpPermissionOf("GetInstance"))
	return g.inner.GetInstance(ctx, req)
}

func (g *gcpROCompute) ListInstances(
	ctx context.Context, req *computepb.ListInstancesRequest,
) (*computepb.InstanceList, error) {
	g.audit.read(gcpPermissionOf("ListInstances"))
	return g.inner.ListInstances(ctx, req)
}

func (g *gcpROCompute) AggregatedListInstances(
	ctx context.Context, req *computepb.AggregatedListInstancesRequest,
) (*computepb.InstanceAggregatedList, error) {
	g.audit.read(gcpPermissionOf("AggregatedListInstances"))
	return g.inner.AggregatedListInstances(ctx, req)
}

func (g *gcpROCompute) ListZones(ctx context.Context, req *computepb.ListZonesRequest) (*computepb.ZoneList, error) {
	g.audit.read(gcpPermissionOf("ListZones"))
	return g.inner.ListZones(ctx, req)
}

func (g *gcpROCompute) ListImages(ctx context.Context, req *computepb.ListImagesRequest) (*computepb.ImageList, error) {
	g.audit.read(gcpPermissionOf("ListImages"))
	return g.inner.ListImages(ctx, req)
}

func (g *gcpROCompute) GetRegion(ctx context.Context, req *computepb.GetRegionRequest) (*computepb.Region, error) {
	g.audit.read(gcpPermissionOf("GetRegion"))
	return g.inner.GetRegion(ctx, req)
}

func (g *gcpROCompute) ListRegions(ctx context.Context, req *computepb.ListRegionsRequest) (*computepb.RegionList, error) {
	g.audit.read(gcpPermissionOf("ListRegions"))
	return g.inner.ListRegions(ctx, req)
}

func (g *gcpROCompute) ListMachineTypes(
	ctx context.Context, req *computepb.ListMachineTypesRequest,
) (*computepb.MachineTypeList, error) {
	g.audit.read(gcpPermissionOf("ListMachineTypes"))
	return g.inner.ListMachineTypes(ctx, req)
}

func (g *gcpROCompute) GetProject(ctx context.Context, req *computepb.GetProjectRequest) (*computepb.Project, error) {
	g.audit.read(gcpPermissionOf("GetProject"))
	return g.inner.GetProject(ctx, req)
}

func (g *gcpROCompute) GetImage(ctx context.Context, req *computepb.GetImageRequest) (*computepb.Image, error) {
	g.audit.read(gcpPermissionOf("GetImage"))
	return g.inner.GetImage(ctx, req)
}

func (g *gcpROCompute) GetImageFromFamily(
	ctx context.Context, req *computepb.GetFromFamilyImageRequest,
) (*computepb.Image, error) {
	g.audit.read(gcpPermissionOf("GetImageFromFamily"))
	return g.inner.GetImageFromFamily(ctx, req)
}

func (g *gcpROCompute) GetSubnetwork(
	ctx context.Context, req *computepb.GetSubnetworkRequest,
) (*computepb.Subnetwork, error) {
	g.audit.read(gcpPermissionOf("GetSubnetwork"))
	return g.inner.GetSubnetwork(ctx, req)
}

func (g *gcpROCompute) ListSubnetworks(
	ctx context.Context, req *computepb.ListSubnetworksRequest,
) (*computepb.SubnetworkList, error) {
	g.audit.read(gcpPermissionOf("ListSubnetworks"))
	return g.inner.ListSubnetworks(ctx, req)
}

func (g *gcpROCompute) InsertSubnetwork(
	context.Context, *computepb.InsertSubnetworkRequest,
) (*computepb.Operation, error) {
	return nil, g.audit.deny(gcpPermissionOf("InsertSubnetwork"))
}

func (g *gcpROCompute) DeleteSubnetwork(
	context.Context, *computepb.DeleteSubnetworkRequest,
) (*computepb.Operation, error) {
	return nil, g.audit.deny(gcpPermissionOf("DeleteSubnetwork"))
}

func (g *gcpROCompute) GetNetwork(ctx context.Context, req *computepb.GetNetworkRequest) (*computepb.Network, error) {
	g.audit.read(gcpPermissionOf("GetNetwork"))
	return g.inner.GetNetwork(ctx, req)
}

func (g *gcpROCompute) ListNetworks(
	ctx context.Context, req *computepb.ListNetworksRequest,
) (*computepb.NetworkList, error) {
	g.audit.read(gcpPermissionOf("ListNetworks"))
	return g.inner.ListNetworks(ctx, req)
}

func (g *gcpROCompute) InsertNetwork(context.Context, *computepb.InsertNetworkRequest) (*computepb.Operation, error) {
	return nil, g.audit.deny(gcpPermissionOf("InsertNetwork"))
}

func (g *gcpROCompute) DeleteNetwork(context.Context, *computepb.DeleteNetworkRequest) (*computepb.Operation, error) {
	return nil, g.audit.deny(gcpPermissionOf("DeleteNetwork"))
}

func (g *gcpROCompute) ListFirewalls(
	ctx context.Context, req *computepb.ListFirewallsRequest,
) (*computepb.FirewallList, error) {
	g.audit.read(gcpPermissionOf("ListFirewalls"))
	return g.inner.ListFirewalls(ctx, req)
}

func (g *gcpROCompute) InsertFirewall(context.Context, *computepb.InsertFirewallRequest) (*computepb.Operation, error) {
	return nil, g.audit.deny(gcpPermissionOf("InsertFirewall"))
}

func (g *gcpROCompute) DeleteFirewall(context.Context, *computepb.DeleteFirewallRequest) (*computepb.Operation, error) {
	return nil, g.audit.deny(gcpPermissionOf("DeleteFirewall"))
}

func (g *gcpROCompute) GetResourcePolicy(
	ctx context.Context, req *computepb.GetResourcePolicyRequest,
) (*computepb.ResourcePolicy, error) {
	g.audit.read(gcpPermissionOf("GetResourcePolicy"))
	return g.inner.GetResourcePolicy(ctx, req)
}

func (g *gcpROCompute) ListResourcePolicies(
	ctx context.Context, req *computepb.ListResourcePoliciesRequest,
) (*computepb.ResourcePolicyList, error) {
	g.audit.read(gcpPermissionOf("ListResourcePolicies"))
	return g.inner.ListResourcePolicies(ctx, req)
}

func (g *gcpROCompute) InsertResourcePolicy(
	context.Context, *computepb.InsertResourcePolicyRequest,
) (*computepb.Operation, error) {
	return nil, g.audit.deny(gcpPermissionOf("InsertResourcePolicy"))
}

func (g *gcpROCompute) DeleteResourcePolicy(
	context.Context, *computepb.DeleteResourcePolicyRequest,
) (*computepb.Operation, error) {
	return nil, g.audit.deny(gcpPermissionOf("DeleteResourcePolicy"))
}

func (g *gcpROCompute) WaitZoneOperation(
	ctx context.Context, req *computepb.WaitZoneOperationRequest,
) (*computepb.Operation, error) {
	g.audit.read(gcpPermissionOf("WaitZoneOperation"))
	return g.inner.WaitZoneOperation(ctx, req)
}

func (g *gcpROCompute) GetZoneOperation(
	ctx context.Context, req *computepb.GetZoneOperationRequest,
) (*computepb.Operation, error) {
	g.audit.read(gcpPermissionOf("GetZoneOperation"))
	return g.inner.GetZoneOperation(ctx, req)
}

func (g *gcpROCompute) WaitRegionOperation(
	ctx context.Context, req *computepb.WaitRegionOperationRequest,
) (*computepb.Operation, error) {
	g.audit.read(gcpPermissionOf("WaitRegionOperation"))
	return g.inner.WaitRegionOperation(ctx, req)
}

func (g *gcpROCompute) GetRegionOperation(
	ctx context.Context, req *computepb.GetRegionOperationRequest,
) (*computepb.Operation, error) {
	g.audit.read(gcpPermissionOf("GetRegionOperation"))
	return g.inner.GetRegionOperation(ctx, req)
}

func (g *gcpROCompute) WaitGlobalOperation(
	ctx context.Context, req *computepb.WaitGlobalOperationRequest,
) (*computepb.Operation, error) {
	g.audit.read(gcpPermissionOf("WaitGlobalOperation"))
	return g.inner.WaitGlobalOperation(ctx, req)
}

func (g *gcpROCompute) GetGlobalOperation(
	ctx context.Context, req *computepb.GetGlobalOperationRequest,
) (*computepb.Operation, error) {
	g.audit.read(gcpPermissionOf("GetGlobalOperation"))
	return g.inner.GetGlobalOperation(ctx, req)
}
