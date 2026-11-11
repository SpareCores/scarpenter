/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package api is the UpCloud API as the driver sees it: a small interface over
// the calls the driver needs, in the shape of upcloud-go-api v8's service
// methods, plus the firewall ruleset calls that v8 lacks (a thin hand-written
// client, plan 19, open question 3). The driver and its tests never touch the
// SDK client directly; the in-memory fake (pkg/vendors/upcloud/fake) implements
// exactly this interface, and so does the real Client.
package api

import (
	"context"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"
)

// API is the part of the UpCloud API the driver calls. Every method but the
// ruleset ones and CreateServer is the SDK service method of the same name. GPU stock
// (GetDevicesAvailability) is not part of it: v1 launches no GPU plans.
type API interface {
	GetZones(ctx context.Context) (*upcloud.Zones, error)
	GetPlans(ctx context.Context) (*upcloud.Plans, error)
	GetAccount(ctx context.Context) (*upcloud.Account, error)

	GetServersWithFilters(ctx context.Context, r *request.GetServersWithFiltersRequest) (*upcloud.Servers, error)
	GetServerDetails(ctx context.Context, r *request.GetServerDetailsRequest) (*upcloud.ServerDetails, error)
	// CreateServer is the SDK's call plus storageLabels, set on every storage the
	// call creates or clones: the API takes labels per storage device, the SDK's
	// device type has no field for them (Client.CreateServer adds them to the body).
	CreateServer(
		ctx context.Context, r *request.CreateServerRequest, storageLabels ...upcloud.Label,
	) (*upcloud.ServerDetails, error)
	StopServer(ctx context.Context, r *request.StopServerRequest) (*upcloud.ServerDetails, error)
	ModifyServer(ctx context.Context, r *request.ModifyServerRequest) (*upcloud.ServerDetails, error)
	DeleteServerAndStorages(ctx context.Context, r *request.DeleteServerAndStoragesRequest) error
	WaitForServerState(ctx context.Context, r *request.WaitForServerStateRequest) (*upcloud.ServerDetails, error)

	GetStorages(ctx context.Context, r *request.GetStoragesRequest) (*upcloud.Storages, error)
	GetStorageDetails(ctx context.Context, r *request.GetStorageDetailsRequest) (*upcloud.StorageDetails, error)
	ModifyStorage(ctx context.Context, r *request.ModifyStorageRequest) (*upcloud.StorageDetails, error)

	GetNetworks(ctx context.Context, f ...request.QueryFilter) (*upcloud.Networks, error)
	GetNetworkDetails(ctx context.Context, r *request.GetNetworkDetailsRequest) (*upcloud.Network, error)
	CreateNetwork(ctx context.Context, r *request.CreateNetworkRequest) (*upcloud.Network, error)
	DeleteNetwork(ctx context.Context, r *request.DeleteNetworkRequest) error

	GetFirewallRules(ctx context.Context, r *request.GetFirewallRulesRequest) (*upcloud.FirewallRules, error)
	CreateFirewallRules(ctx context.Context, r *request.CreateFirewallRulesRequest) error

	GetServerGroups(ctx context.Context, r *request.GetServerGroupsRequest) (upcloud.ServerGroups, error)
	GetServerGroup(ctx context.Context, r *request.GetServerGroupRequest) (*upcloud.ServerGroup, error)
	CreateServerGroup(ctx context.Context, r *request.CreateServerGroupRequest) (*upcloud.ServerGroup, error)
	DeleteServerGroup(ctx context.Context, r *request.DeleteServerGroupRequest) error

	GetIPAddresses(ctx context.Context) (*upcloud.IPAddresses, error)

	Rulesets
}

// Rulesets is the standalone firewall ruleset surface (the SDN firewall, which
// filters private interfaces only), absent from upcloud-go-api v8.42.0. The
// real client talks to /firewall-ruleset and
// /server/<uuid>/{attach,detach}_firewall_ruleset/private (the paths of the
// OpenAPI-generated v9 module, alpha); none of it has been called against UpCloud.
type Rulesets interface {
	GetFirewallRulesets(ctx context.Context) ([]Ruleset, error)
	GetFirewallRuleset(ctx context.Context, uuid string) (*Ruleset, error)
	CreateFirewallRuleset(ctx context.Context, r CreateRulesetRequest) (*Ruleset, error)
	DeleteFirewallRuleset(ctx context.Context, uuid string) error
	AttachPrivateFirewallRuleset(ctx context.Context, serverUUID, rulesetUUID string) error
	DetachPrivateFirewallRuleset(ctx context.Context, serverUUID, rulesetUUID string) error
}

// Ruleset is a standalone firewall ruleset.
type Ruleset struct {
	UUID        string
	Name        string
	Description string
	Stateful    bool
	Enabled     bool
	Version     int
	// Labels: whether the list response carries them is unverified (plan 19).
	Labels []upcloud.Label
	// Rules is filled by the fake and by a get, not by a list.
	Rules []RulesetRule
}

// RulesetRule is one rule of a ruleset.
type RulesetRule struct {
	Action    string // accept, drop
	Direction string // in, out
	Family    string // IPv4, IPv6
	Protocol  string // tcp, udp, icmp, or empty for any
	// Source (inbound) or destination (outbound) network, empty for any.
	CIDR      string
	PortStart int
	PortEnd   int
	Comment   string
}

// CreateRulesetRequest creates a ruleset and its rules.
type CreateRulesetRequest struct {
	Name        string
	Description string
	Stateful    bool
	Labels      []upcloud.Label
	Rules       []RulesetRule
}

var _ API = (*Client)(nil)
