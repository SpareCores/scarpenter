/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package main

import (
	"context"
	"strings"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"

	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/api"
)

// The read-only guard of the UpCloud canary. UpCloud has no policy language: a token can
// narrow what a sub-account may do by resource type, not by operation, so the canary's
// token cannot be made read-only on the server side. This guard is the wall: the driver is
// given an api.API whose every method is written out here, reads (Get*) passed to the real
// client and recorded, everything else refused before it reaches the network. A method added
// to api.API breaks the build of this file; guard_upcloud_test.go checks by reflection that
// every method is either a read that reaches the inner client or a refused write.

// roUpCloud implements api.API.
type roUpCloud struct {
	inner api.API
	audit *audit
}

// readOnlyUpCloud wraps an API so that only reads go through. The audit records them as
// upcloud:<method> and the refusals the same way.
func readOnlyUpCloud(inner api.API, a *audit) api.API { return &roUpCloud{inner: inner, audit: a} }

var _ api.API = (*roUpCloud)(nil)

// pass records a read; deny refuses a write. The names are the API's method names.
func (g *roUpCloud) pass(name string) { g.audit.read("upcloud:" + name) }

func (g *roUpCloud) deny(name string) error { return g.audit.deny("upcloud:" + name) }

// isUpCloudRead says whether a method of api.API is a read: the Get calls. WaitForServerState
// is not (it only follows a write, which the canary never makes).
func isUpCloudRead(name string) bool { return strings.HasPrefix(name, "Get") }

func (g *roUpCloud) GetZones(ctx context.Context) (*upcloud.Zones, error) {
	g.pass("GetZones")
	return g.inner.GetZones(ctx)
}

func (g *roUpCloud) GetPlans(ctx context.Context) (*upcloud.Plans, error) {
	g.pass("GetPlans")
	return g.inner.GetPlans(ctx)
}

func (g *roUpCloud) GetAccount(ctx context.Context) (*upcloud.Account, error) {
	g.pass("GetAccount")
	return g.inner.GetAccount(ctx)
}

func (g *roUpCloud) GetServersWithFilters(
	ctx context.Context, r *request.GetServersWithFiltersRequest,
) (*upcloud.Servers, error) {
	g.pass("GetServersWithFilters")
	return g.inner.GetServersWithFilters(ctx, r)
}

func (g *roUpCloud) GetServerDetails(
	ctx context.Context, r *request.GetServerDetailsRequest,
) (*upcloud.ServerDetails, error) {
	g.pass("GetServerDetails")
	return g.inner.GetServerDetails(ctx, r)
}

func (g *roUpCloud) CreateServer(
	context.Context, *request.CreateServerRequest, ...upcloud.Label,
) (*upcloud.ServerDetails, error) {
	return nil, g.deny("CreateServer")
}

func (g *roUpCloud) StopServer(context.Context, *request.StopServerRequest) (*upcloud.ServerDetails, error) {
	return nil, g.deny("StopServer")
}

func (g *roUpCloud) ModifyServer(context.Context, *request.ModifyServerRequest) (*upcloud.ServerDetails, error) {
	return nil, g.deny("ModifyServer")
}

func (g *roUpCloud) DeleteServerAndStorages(context.Context, *request.DeleteServerAndStoragesRequest) error {
	return g.deny("DeleteServerAndStorages")
}

func (g *roUpCloud) WaitForServerState(
	context.Context, *request.WaitForServerStateRequest,
) (*upcloud.ServerDetails, error) {
	return nil, g.deny("WaitForServerState")
}

func (g *roUpCloud) GetStorages(ctx context.Context, r *request.GetStoragesRequest) (*upcloud.Storages, error) {
	g.pass("GetStorages")
	return g.inner.GetStorages(ctx, r)
}

func (g *roUpCloud) GetStorageDetails(
	ctx context.Context, r *request.GetStorageDetailsRequest,
) (*upcloud.StorageDetails, error) {
	g.pass("GetStorageDetails")
	return g.inner.GetStorageDetails(ctx, r)
}

func (g *roUpCloud) ModifyStorage(context.Context, *request.ModifyStorageRequest) (*upcloud.StorageDetails, error) {
	return nil, g.deny("ModifyStorage")
}

func (g *roUpCloud) GetNetworks(ctx context.Context, f ...request.QueryFilter) (*upcloud.Networks, error) {
	g.pass("GetNetworks")
	return g.inner.GetNetworks(ctx, f...)
}

func (g *roUpCloud) GetNetworkDetails(
	ctx context.Context, r *request.GetNetworkDetailsRequest,
) (*upcloud.Network, error) {
	g.pass("GetNetworkDetails")
	return g.inner.GetNetworkDetails(ctx, r)
}

func (g *roUpCloud) CreateNetwork(context.Context, *request.CreateNetworkRequest) (*upcloud.Network, error) {
	return nil, g.deny("CreateNetwork")
}

func (g *roUpCloud) DeleteNetwork(context.Context, *request.DeleteNetworkRequest) error {
	return g.deny("DeleteNetwork")
}

func (g *roUpCloud) GetFirewallRules(
	ctx context.Context, r *request.GetFirewallRulesRequest,
) (*upcloud.FirewallRules, error) {
	g.pass("GetFirewallRules")
	return g.inner.GetFirewallRules(ctx, r)
}

func (g *roUpCloud) CreateFirewallRules(context.Context, *request.CreateFirewallRulesRequest) error {
	return g.deny("CreateFirewallRules")
}

func (g *roUpCloud) GetServerGroups(
	ctx context.Context, r *request.GetServerGroupsRequest,
) (upcloud.ServerGroups, error) {
	g.pass("GetServerGroups")
	return g.inner.GetServerGroups(ctx, r)
}

func (g *roUpCloud) GetServerGroup(
	ctx context.Context, r *request.GetServerGroupRequest,
) (*upcloud.ServerGroup, error) {
	g.pass("GetServerGroup")
	return g.inner.GetServerGroup(ctx, r)
}

func (g *roUpCloud) CreateServerGroup(
	context.Context, *request.CreateServerGroupRequest,
) (*upcloud.ServerGroup, error) {
	return nil, g.deny("CreateServerGroup")
}

func (g *roUpCloud) DeleteServerGroup(context.Context, *request.DeleteServerGroupRequest) error {
	return g.deny("DeleteServerGroup")
}

func (g *roUpCloud) GetIPAddresses(ctx context.Context) (*upcloud.IPAddresses, error) {
	g.pass("GetIPAddresses")
	return g.inner.GetIPAddresses(ctx)
}

func (g *roUpCloud) GetFirewallRulesets(ctx context.Context) ([]api.Ruleset, error) {
	g.pass("GetFirewallRulesets")
	return g.inner.GetFirewallRulesets(ctx)
}

func (g *roUpCloud) GetFirewallRuleset(ctx context.Context, uuid string) (*api.Ruleset, error) {
	g.pass("GetFirewallRuleset")
	return g.inner.GetFirewallRuleset(ctx, uuid)
}

func (g *roUpCloud) CreateFirewallRuleset(context.Context, api.CreateRulesetRequest) (*api.Ruleset, error) {
	return nil, g.deny("CreateFirewallRuleset")
}

func (g *roUpCloud) DeleteFirewallRuleset(context.Context, string) error {
	return g.deny("DeleteFirewallRuleset")
}

func (g *roUpCloud) AttachPrivateFirewallRuleset(context.Context, string, string) error {
	return g.deny("AttachPrivateFirewallRuleset")
}

func (g *roUpCloud) DetachPrivateFirewallRuleset(context.Context, string, string) error {
	return g.deny("DetachPrivateFirewallRuleset")
}
