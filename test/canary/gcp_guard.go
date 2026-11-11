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
// an operation read before it reaches the SDK. roCompute implements gcp.ComputeAPI method by
// method (a method added to the interface breaks the build here), and gcp_guard_test.go checks
// by reflection that every method of the interface is either passed through as a read or
// refused, and that every method has the IAM permission the audit and canary-policy.sh speak in.

// permissions maps each method of gcp.ComputeAPI to the IAM permission its call needs
// (docs/plans/v1/wip/16-gcp.md section 8). The writes are listed too: a refusal is recorded with
// the permission it would have needed.
var permissions = map[string]string{
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

// permissionOf is the IAM permission the audit records for a method; a method the table
// does not know is recorded as unmapped (and guard_test.go fails on it).
func permissionOf(method string) string {
	if p, ok := permissions[method]; ok {
		return p
	}
	return "unmapped:" + method
}

// roCompute is a gcp.ComputeAPI that passes the reads on and refuses the writes.
type roCompute struct {
	inner gcp.ComputeAPI
	audit *audit
}

var _ gcp.ComputeAPI = (*roCompute)(nil)

// readOnlyCompute wraps the API: every call it makes is guarded. The audit is the one of the
// AWS guard (aws_guard.go): its actions here are IAM permissions.
func readOnlyCompute(inner gcp.ComputeAPI, a *audit) gcp.ComputeAPI {
	return &roCompute{inner: inner, audit: a}
}

func (g *roCompute) InsertInstance(context.Context, *computepb.InsertInstanceRequest) (*computepb.Operation, error) {
	return nil, g.audit.deny(permissionOf("InsertInstance"))
}

func (g *roCompute) DeleteInstance(context.Context, *computepb.DeleteInstanceRequest) (*computepb.Operation, error) {
	return nil, g.audit.deny(permissionOf("DeleteInstance"))
}

func (g *roCompute) GetInstance(ctx context.Context, req *computepb.GetInstanceRequest) (*computepb.Instance, error) {
	g.audit.read(permissionOf("GetInstance"))
	return g.inner.GetInstance(ctx, req)
}

func (g *roCompute) ListInstances(
	ctx context.Context, req *computepb.ListInstancesRequest,
) (*computepb.InstanceList, error) {
	g.audit.read(permissionOf("ListInstances"))
	return g.inner.ListInstances(ctx, req)
}

func (g *roCompute) AggregatedListInstances(
	ctx context.Context, req *computepb.AggregatedListInstancesRequest,
) (*computepb.InstanceAggregatedList, error) {
	g.audit.read(permissionOf("AggregatedListInstances"))
	return g.inner.AggregatedListInstances(ctx, req)
}

func (g *roCompute) ListZones(ctx context.Context, req *computepb.ListZonesRequest) (*computepb.ZoneList, error) {
	g.audit.read(permissionOf("ListZones"))
	return g.inner.ListZones(ctx, req)
}

func (g *roCompute) ListImages(ctx context.Context, req *computepb.ListImagesRequest) (*computepb.ImageList, error) {
	g.audit.read(permissionOf("ListImages"))
	return g.inner.ListImages(ctx, req)
}

func (g *roCompute) GetRegion(ctx context.Context, req *computepb.GetRegionRequest) (*computepb.Region, error) {
	g.audit.read(permissionOf("GetRegion"))
	return g.inner.GetRegion(ctx, req)
}

func (g *roCompute) ListRegions(ctx context.Context, req *computepb.ListRegionsRequest) (*computepb.RegionList, error) {
	g.audit.read(permissionOf("ListRegions"))
	return g.inner.ListRegions(ctx, req)
}

func (g *roCompute) ListMachineTypes(
	ctx context.Context, req *computepb.ListMachineTypesRequest,
) (*computepb.MachineTypeList, error) {
	g.audit.read(permissionOf("ListMachineTypes"))
	return g.inner.ListMachineTypes(ctx, req)
}

func (g *roCompute) GetProject(ctx context.Context, req *computepb.GetProjectRequest) (*computepb.Project, error) {
	g.audit.read(permissionOf("GetProject"))
	return g.inner.GetProject(ctx, req)
}

func (g *roCompute) GetImage(ctx context.Context, req *computepb.GetImageRequest) (*computepb.Image, error) {
	g.audit.read(permissionOf("GetImage"))
	return g.inner.GetImage(ctx, req)
}

func (g *roCompute) GetImageFromFamily(
	ctx context.Context, req *computepb.GetFromFamilyImageRequest,
) (*computepb.Image, error) {
	g.audit.read(permissionOf("GetImageFromFamily"))
	return g.inner.GetImageFromFamily(ctx, req)
}

func (g *roCompute) GetSubnetwork(
	ctx context.Context, req *computepb.GetSubnetworkRequest,
) (*computepb.Subnetwork, error) {
	g.audit.read(permissionOf("GetSubnetwork"))
	return g.inner.GetSubnetwork(ctx, req)
}

func (g *roCompute) ListSubnetworks(
	ctx context.Context, req *computepb.ListSubnetworksRequest,
) (*computepb.SubnetworkList, error) {
	g.audit.read(permissionOf("ListSubnetworks"))
	return g.inner.ListSubnetworks(ctx, req)
}

func (g *roCompute) InsertSubnetwork(
	context.Context, *computepb.InsertSubnetworkRequest,
) (*computepb.Operation, error) {
	return nil, g.audit.deny(permissionOf("InsertSubnetwork"))
}

func (g *roCompute) DeleteSubnetwork(
	context.Context, *computepb.DeleteSubnetworkRequest,
) (*computepb.Operation, error) {
	return nil, g.audit.deny(permissionOf("DeleteSubnetwork"))
}

func (g *roCompute) GetNetwork(ctx context.Context, req *computepb.GetNetworkRequest) (*computepb.Network, error) {
	g.audit.read(permissionOf("GetNetwork"))
	return g.inner.GetNetwork(ctx, req)
}

func (g *roCompute) ListNetworks(
	ctx context.Context, req *computepb.ListNetworksRequest,
) (*computepb.NetworkList, error) {
	g.audit.read(permissionOf("ListNetworks"))
	return g.inner.ListNetworks(ctx, req)
}

func (g *roCompute) InsertNetwork(context.Context, *computepb.InsertNetworkRequest) (*computepb.Operation, error) {
	return nil, g.audit.deny(permissionOf("InsertNetwork"))
}

func (g *roCompute) DeleteNetwork(context.Context, *computepb.DeleteNetworkRequest) (*computepb.Operation, error) {
	return nil, g.audit.deny(permissionOf("DeleteNetwork"))
}

func (g *roCompute) ListFirewalls(
	ctx context.Context, req *computepb.ListFirewallsRequest,
) (*computepb.FirewallList, error) {
	g.audit.read(permissionOf("ListFirewalls"))
	return g.inner.ListFirewalls(ctx, req)
}

func (g *roCompute) InsertFirewall(context.Context, *computepb.InsertFirewallRequest) (*computepb.Operation, error) {
	return nil, g.audit.deny(permissionOf("InsertFirewall"))
}

func (g *roCompute) DeleteFirewall(context.Context, *computepb.DeleteFirewallRequest) (*computepb.Operation, error) {
	return nil, g.audit.deny(permissionOf("DeleteFirewall"))
}

func (g *roCompute) GetResourcePolicy(
	ctx context.Context, req *computepb.GetResourcePolicyRequest,
) (*computepb.ResourcePolicy, error) {
	g.audit.read(permissionOf("GetResourcePolicy"))
	return g.inner.GetResourcePolicy(ctx, req)
}

func (g *roCompute) ListResourcePolicies(
	ctx context.Context, req *computepb.ListResourcePoliciesRequest,
) (*computepb.ResourcePolicyList, error) {
	g.audit.read(permissionOf("ListResourcePolicies"))
	return g.inner.ListResourcePolicies(ctx, req)
}

func (g *roCompute) InsertResourcePolicy(
	context.Context, *computepb.InsertResourcePolicyRequest,
) (*computepb.Operation, error) {
	return nil, g.audit.deny(permissionOf("InsertResourcePolicy"))
}

func (g *roCompute) DeleteResourcePolicy(
	context.Context, *computepb.DeleteResourcePolicyRequest,
) (*computepb.Operation, error) {
	return nil, g.audit.deny(permissionOf("DeleteResourcePolicy"))
}

func (g *roCompute) WaitZoneOperation(
	ctx context.Context, req *computepb.WaitZoneOperationRequest,
) (*computepb.Operation, error) {
	g.audit.read(permissionOf("WaitZoneOperation"))
	return g.inner.WaitZoneOperation(ctx, req)
}

func (g *roCompute) GetZoneOperation(
	ctx context.Context, req *computepb.GetZoneOperationRequest,
) (*computepb.Operation, error) {
	g.audit.read(permissionOf("GetZoneOperation"))
	return g.inner.GetZoneOperation(ctx, req)
}

func (g *roCompute) WaitRegionOperation(
	ctx context.Context, req *computepb.WaitRegionOperationRequest,
) (*computepb.Operation, error) {
	g.audit.read(permissionOf("WaitRegionOperation"))
	return g.inner.WaitRegionOperation(ctx, req)
}

func (g *roCompute) GetRegionOperation(
	ctx context.Context, req *computepb.GetRegionOperationRequest,
) (*computepb.Operation, error) {
	g.audit.read(permissionOf("GetRegionOperation"))
	return g.inner.GetRegionOperation(ctx, req)
}

func (g *roCompute) WaitGlobalOperation(
	ctx context.Context, req *computepb.WaitGlobalOperationRequest,
) (*computepb.Operation, error) {
	g.audit.read(permissionOf("WaitGlobalOperation"))
	return g.inner.WaitGlobalOperation(ctx, req)
}

func (g *roCompute) GetGlobalOperation(
	ctx context.Context, req *computepb.GetGlobalOperationRequest,
) (*computepb.Operation, error) {
	g.audit.read(permissionOf("GetGlobalOperation"))
	return g.inner.GetGlobalOperation(ctx, req)
}
