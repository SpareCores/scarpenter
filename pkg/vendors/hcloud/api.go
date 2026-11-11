/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package hcloud is the lower layer of the Hetzner Cloud driver (plan 15): the
// narrow API interface over hcloud-go and its in-memory fake (subpackage fake),
// error classification, the pure mapping code (labels, names, provider IDs,
// states, the server create request), the shared server list and the optional
// project server limit. The vendors.Driver implementation, the node options and
// the managed resources build on it (TODO(plan 14)).
package hcloud

import (
	"context"

	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// API is the part of the Hetzner Cloud API the driver calls (plan 15, "Client
// layer"): one client for the whole project, flat methods over the SDK's value
// types. The in-memory fake (pkg/vendors/hcloud/fake) implements exactly this,
// and so does Client, the adapter over hcloud-go.
//
// Conventions, both implementations alike: the Get methods return nil (and no
// error) for an object the API does not know, as the SDK's GetByID does; List
// methods return every page (50 objects per page, followed to the end); errors
// are the SDK's (hc.Error for API errors, carrying the API error code).
type API interface {
	Locations(ctx context.Context) ([]*hc.Location, error)
	ServerTypes(ctx context.Context) ([]*hc.ServerType, error)
	Images(ctx context.Context, opts hc.ImageListOpts) ([]*hc.Image, error)

	CreateServer(ctx context.Context, opts hc.ServerCreateOpts) (hc.ServerCreateResult, error)
	GetServer(ctx context.Context, id int64) (*hc.Server, error)
	Servers(ctx context.Context, opts hc.ServerListOpts) ([]*hc.Server, error)
	// DeleteServer starts the deletion and returns its action (not awaited).
	DeleteServer(ctx context.Context, s *hc.Server) (*hc.Action, error)
	// WaitForAction waits for an action to succeed; a failed action is an
	// hc.ActionError.
	WaitForAction(ctx context.Context, a *hc.Action) error

	GetNetwork(ctx context.Context, id int64) (*hc.Network, error)
	Networks(ctx context.Context, opts hc.NetworkListOpts) ([]*hc.Network, error)

	GetFirewall(ctx context.Context, id int64) (*hc.Firewall, error)
	Firewalls(ctx context.Context, opts hc.FirewallListOpts) ([]*hc.Firewall, error)
	CreateFirewall(ctx context.Context, opts hc.FirewallCreateOpts) (*hc.Firewall, error)
	DeleteFirewall(ctx context.Context, f *hc.Firewall) error

	GetPlacementGroup(ctx context.Context, id int64) (*hc.PlacementGroup, error)
	PlacementGroups(ctx context.Context, opts hc.PlacementGroupListOpts) ([]*hc.PlacementGroup, error)
	CreatePlacementGroup(ctx context.Context, opts hc.PlacementGroupCreateOpts) (*hc.PlacementGroup, error)
	DeletePlacementGroup(ctx context.Context, g *hc.PlacementGroup) error

	CreateVolume(ctx context.Context, opts hc.VolumeCreateOpts) (*hc.Volume, error)
	GetVolume(ctx context.Context, id int64) (*hc.Volume, error)
	Volumes(ctx context.Context, opts hc.VolumeListOpts) ([]*hc.Volume, error)
	DeleteVolume(ctx context.Context, v *hc.Volume) error

	GetPrimaryIP(ctx context.Context, id int64) (*hc.PrimaryIP, error)
	DeletePrimaryIP(ctx context.Context, ip *hc.PrimaryIP) error

	// SSHKey finds a key by name or ID.
	SSHKey(ctx context.Context, idOrName string) (*hc.SSHKey, error)

	// RateLimit is the last RateLimit-* headers the API sent (zero before the
	// first call).
	RateLimit() RateLimit
}

// RateLimit is the project's rate limit state from the RateLimit-* headers
// (3600 requests per hour, refilled one per second).
type RateLimit struct {
	Limit     int
	Remaining int
	// Reset is the Unix time at which the bucket is full again (0 unknown).
	Reset int64
}

// Known reports whether the API has sent the headers yet.
func (r RateLimit) Known() bool { return r.Limit > 0 }

// LowRemaining reports whether less than 20 % of the limit is left (the plan's
// warning threshold).
func (r RateLimit) LowRemaining() bool { return r.Known() && r.Remaining*5 < r.Limit }
