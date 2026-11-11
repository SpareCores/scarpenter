/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package main

import (
	"context"
	"net/http"
	"strings"

	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/SpareCores/scarpenter/pkg/vendors/hcloud"
)

// The read-only guard of the Hetzner canary. A Hetzner token is read or read-write for the whole
// project and cannot be narrowed, so the canary is given a read-only one (created as such in the
// Console) and two walls of its own: the HTTP transport of the real client refuses every method
// but GET before it reaches the network (roTransport), and the driver is given an API that refuses
// every Create* and Delete* call (roAPI). guard_test.go checks both: a request with another method
// never leaves, and every method of hcloud.API is either a read that is passed on or a refused write.

// roTransport refuses every request that is not a GET.
type roTransport struct {
	inner http.RoundTripper
	audit *audit
}

func (t *roTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodGet {
		return nil, t.audit.deny(r.Method + " " + r.URL.Path)
	}
	t.audit.read("GET " + apiPath(r.URL.Path))
	return t.inner.RoundTrip(r)
}

// apiPath keeps the kind of a path, not the ID: /servers/42 is /servers.
func apiPath(p string) string {
	p = strings.TrimPrefix(p, "/v1/")
	if i := strings.Index(p, "/"); i > 0 {
		return p[:i]
	}
	return p
}

// guardedHTTPClient is the client of the real API: every request goes through roTransport.
func guardedHTTPClient(a *audit) *http.Client {
	return &http.Client{Transport: &roTransport{inner: http.DefaultTransport, audit: a}}
}

// isHCloudRead says whether a method of hcloud.API is a read: everything but Create* and Delete*.
func isHCloudRead(name string) bool {
	return !strings.HasPrefix(name, "Create") && !strings.HasPrefix(name, "Delete")
}

// roAPI wraps an hcloud.API: reads are recorded and passed on, writes are refused.
type roAPI struct {
	hcloud.API
	audit *audit
}

var _ hcloud.API = (*roAPI)(nil)

func (g *roAPI) Locations(ctx context.Context) ([]*hc.Location, error) {
	g.audit.read("hcloud:Locations")
	return g.API.Locations(ctx)
}

func (g *roAPI) ServerTypes(ctx context.Context) ([]*hc.ServerType, error) {
	g.audit.read("hcloud:ServerTypes")
	return g.API.ServerTypes(ctx)
}

func (g *roAPI) Images(ctx context.Context, o hc.ImageListOpts) ([]*hc.Image, error) {
	g.audit.read("hcloud:Images")
	return g.API.Images(ctx, o)
}

func (g *roAPI) CreateServer(context.Context, hc.ServerCreateOpts) (hc.ServerCreateResult, error) {
	return hc.ServerCreateResult{}, g.audit.deny("hcloud:CreateServer")
}

func (g *roAPI) GetServer(ctx context.Context, id int64) (*hc.Server, error) {
	g.audit.read("hcloud:GetServer")
	return g.API.GetServer(ctx, id)
}

func (g *roAPI) Servers(ctx context.Context, o hc.ServerListOpts) ([]*hc.Server, error) {
	g.audit.read("hcloud:Servers")
	return g.API.Servers(ctx, o)
}

func (g *roAPI) DeleteServer(context.Context, *hc.Server) (*hc.Action, error) {
	return nil, g.audit.deny("hcloud:DeleteServer")
}

func (g *roAPI) WaitForAction(ctx context.Context, a *hc.Action) error {
	g.audit.read("hcloud:WaitForAction")
	return g.API.WaitForAction(ctx, a)
}

func (g *roAPI) GetNetwork(ctx context.Context, id int64) (*hc.Network, error) {
	g.audit.read("hcloud:GetNetwork")
	return g.API.GetNetwork(ctx, id)
}

func (g *roAPI) Networks(ctx context.Context, o hc.NetworkListOpts) ([]*hc.Network, error) {
	g.audit.read("hcloud:Networks")
	return g.API.Networks(ctx, o)
}

func (g *roAPI) GetFirewall(ctx context.Context, id int64) (*hc.Firewall, error) {
	g.audit.read("hcloud:GetFirewall")
	return g.API.GetFirewall(ctx, id)
}

func (g *roAPI) Firewalls(ctx context.Context, o hc.FirewallListOpts) ([]*hc.Firewall, error) {
	g.audit.read("hcloud:Firewalls")
	return g.API.Firewalls(ctx, o)
}

func (g *roAPI) CreateFirewall(context.Context, hc.FirewallCreateOpts) (*hc.Firewall, error) {
	return nil, g.audit.deny("hcloud:CreateFirewall")
}

func (g *roAPI) DeleteFirewall(context.Context, *hc.Firewall) error {
	return g.audit.deny("hcloud:DeleteFirewall")
}

func (g *roAPI) GetPlacementGroup(ctx context.Context, id int64) (*hc.PlacementGroup, error) {
	g.audit.read("hcloud:GetPlacementGroup")
	return g.API.GetPlacementGroup(ctx, id)
}

func (g *roAPI) PlacementGroups(ctx context.Context, o hc.PlacementGroupListOpts) ([]*hc.PlacementGroup, error) {
	g.audit.read("hcloud:PlacementGroups")
	return g.API.PlacementGroups(ctx, o)
}

func (g *roAPI) CreatePlacementGroup(context.Context, hc.PlacementGroupCreateOpts) (*hc.PlacementGroup, error) {
	return nil, g.audit.deny("hcloud:CreatePlacementGroup")
}

func (g *roAPI) DeletePlacementGroup(context.Context, *hc.PlacementGroup) error {
	return g.audit.deny("hcloud:DeletePlacementGroup")
}

func (g *roAPI) CreateVolume(context.Context, hc.VolumeCreateOpts) (*hc.Volume, error) {
	return nil, g.audit.deny("hcloud:CreateVolume")
}

func (g *roAPI) GetVolume(ctx context.Context, id int64) (*hc.Volume, error) {
	g.audit.read("hcloud:GetVolume")
	return g.API.GetVolume(ctx, id)
}

func (g *roAPI) Volumes(ctx context.Context, o hc.VolumeListOpts) ([]*hc.Volume, error) {
	g.audit.read("hcloud:Volumes")
	return g.API.Volumes(ctx, o)
}

func (g *roAPI) DeleteVolume(context.Context, *hc.Volume) error {
	return g.audit.deny("hcloud:DeleteVolume")
}

func (g *roAPI) GetPrimaryIP(ctx context.Context, id int64) (*hc.PrimaryIP, error) {
	g.audit.read("hcloud:GetPrimaryIP")
	return g.API.GetPrimaryIP(ctx, id)
}

func (g *roAPI) DeletePrimaryIP(context.Context, *hc.PrimaryIP) error {
	return g.audit.deny("hcloud:DeletePrimaryIP")
}

func (g *roAPI) SSHKey(ctx context.Context, idOrName string) (*hc.SSHKey, error) {
	g.audit.read("hcloud:SSHKey")
	return g.API.SSHKey(ctx, idOrName)
}
