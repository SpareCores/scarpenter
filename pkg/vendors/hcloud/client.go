/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/
package hcloud

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"
	"github.com/prometheus/client_golang/prometheus"
)

// Per-call deadlines (plan 15, "Client layer"): every call runs under its own,
// the wait for the create action under a longer one. A call that runs into one
// fails with a Retryable error, code CallTimeout.
const (
	DefaultCallTimeout   = 30 * time.Second
	DefaultCreateTimeout = 90 * time.Second
)

// Endpoint is the API's base URL; HCLOUD_ENDPOINT replaces it for tests.
const Endpoint = hc.Endpoint

// ClientConfig configures NewClient.
type ClientConfig struct {
	// Token is the project's API token (HCLOUD_TOKEN).
	Token string
	// Endpoint overrides the API base URL (HCLOUD_ENDPOINT); empty is the
	// production API.
	Endpoint string
	// Version is the controller version for the user agent, which reads
	// scarpenter/<version>.
	Version string
	// Registry receives the SDK's request metrics when set.
	Registry prometheus.Registerer
	// CallTimeout and ActionTimeout override the defaults (zero keeps them).
	CallTimeout, ActionTimeout time.Duration
	// HTTPClient replaces the transport (tests); the rate-limit headers are
	// read from it either way.
	HTTPClient *http.Client
}

// Client is the real API: the adapter over hcloud-go. The SDK retries
// rate_limit_exceeded, conflict, bad_gateway and timeout with exponential
// backoff by itself.
type Client struct {
	c             *hc.Client
	callTimeout   time.Duration
	actionTimeout time.Duration
	rl            *rateLimitTransport
}

var _ API = (*Client)(nil)

// NewClient builds the adapter. It makes no call.
func NewClient(cfg ClientConfig) *Client {
	rt := &rateLimitTransport{}
	base := cfg.HTTPClient
	if base == nil {
		base = &http.Client{}
	}
	cp := *base
	rt.next = cp.Transport
	if rt.next == nil {
		rt.next = http.DefaultTransport
	}
	cp.Transport = rt
	version := cfg.Version
	if version == "" {
		version = "dev"
	}
	opts := []hc.ClientOption{
		hc.WithToken(cfg.Token),
		hc.WithApplication("scarpenter", version),
		hc.WithHTTPClient(&cp),
	}
	if cfg.Endpoint != "" {
		opts = append(opts, hc.WithEndpoint(cfg.Endpoint))
	}
	if cfg.Registry != nil {
		opts = append(opts, hc.WithInstrumentation(cfg.Registry))
	}
	cl := &Client{c: hc.NewClient(opts...), callTimeout: cfg.CallTimeout, actionTimeout: cfg.ActionTimeout, rl: rt}
	if cl.callTimeout == 0 {
		cl.callTimeout = DefaultCallTimeout
	}
	if cl.actionTimeout == 0 {
		cl.actionTimeout = DefaultCreateTimeout
	}
	return cl
}

// rateLimitTransport records the RateLimit-* headers of every response.
type rateLimitTransport struct {
	next http.RoundTripper
	mu   sync.Mutex
	last RateLimit
}

func (t *rateLimitTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(r)
	if err != nil || resp == nil {
		return resp, err
	}
	if h := resp.Header.Get("RateLimit-Limit"); h != "" {
		rl := RateLimit{}
		rl.Limit, _ = strconv.Atoi(h)
		rl.Remaining, _ = strconv.Atoi(resp.Header.Get("RateLimit-Remaining"))
		rl.Reset, _ = strconv.ParseInt(resp.Header.Get("RateLimit-Reset"), 10, 64)
		t.mu.Lock()
		t.last = rl
		t.mu.Unlock()
	}
	return resp, nil
}

// RateLimit implements API.
func (c *Client) RateLimit() RateLimit {
	c.rl.mu.Lock()
	defer c.rl.mu.Unlock()
	return c.rl.last
}

func (c *Client) ctx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.callTimeout)
}

// Locations implements API.
func (c *Client) Locations(ctx context.Context) ([]*hc.Location, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	return c.c.Location.All(ctx)
}

// ServerTypes implements API.
func (c *Client) ServerTypes(ctx context.Context) ([]*hc.ServerType, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	return c.c.ServerType.All(ctx)
}

// Images implements API.
func (c *Client) Images(ctx context.Context, opts hc.ImageListOpts) ([]*hc.Image, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	return c.c.Image.AllWithOpts(ctx, opts)
}

// CreateServer implements API.
func (c *Client) CreateServer(ctx context.Context, opts hc.ServerCreateOpts) (hc.ServerCreateResult, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	res, _, err := c.c.Server.Create(ctx, opts)
	return res, err
}

// GetServer implements API.
func (c *Client) GetServer(ctx context.Context, id int64) (*hc.Server, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	s, _, err := c.c.Server.GetByID(ctx, id)
	return s, err
}

// Servers implements API.
func (c *Client) Servers(ctx context.Context, opts hc.ServerListOpts) ([]*hc.Server, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	return c.c.Server.AllWithOpts(ctx, opts)
}

// DeleteServer implements API.
func (c *Client) DeleteServer(ctx context.Context, s *hc.Server) (*hc.Action, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	res, _, err := c.c.Server.DeleteWithResult(ctx, s)
	if res == nil {
		return nil, err
	}
	return res.Action, err
}

// WaitForAction implements API.
func (c *Client) WaitForAction(ctx context.Context, a *hc.Action) error {
	ctx, cancel := context.WithTimeout(ctx, c.actionTimeout)
	defer cancel()
	return c.c.Action.WaitFor(ctx, a)
}

// GetNetwork implements API.
func (c *Client) GetNetwork(ctx context.Context, id int64) (*hc.Network, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	n, _, err := c.c.Network.GetByID(ctx, id)
	return n, err
}

// Networks implements API.
func (c *Client) Networks(ctx context.Context, opts hc.NetworkListOpts) ([]*hc.Network, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	return c.c.Network.AllWithOpts(ctx, opts)
}

// GetFirewall implements API.
func (c *Client) GetFirewall(ctx context.Context, id int64) (*hc.Firewall, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	f, _, err := c.c.Firewall.GetByID(ctx, id)
	return f, err
}

// Firewalls implements API.
func (c *Client) Firewalls(ctx context.Context, opts hc.FirewallListOpts) ([]*hc.Firewall, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	return c.c.Firewall.AllWithOpts(ctx, opts)
}

// CreateFirewall implements API.
func (c *Client) CreateFirewall(ctx context.Context, opts hc.FirewallCreateOpts) (*hc.Firewall, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	res, _, err := c.c.Firewall.Create(ctx, opts)
	return res.Firewall, err
}

// DeleteFirewall implements API.
func (c *Client) DeleteFirewall(ctx context.Context, f *hc.Firewall) error {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	_, err := c.c.Firewall.Delete(ctx, f)
	return err
}

// GetPlacementGroup implements API.
func (c *Client) GetPlacementGroup(ctx context.Context, id int64) (*hc.PlacementGroup, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	g, _, err := c.c.PlacementGroup.GetByID(ctx, id)
	return g, err
}

// PlacementGroups implements API.
func (c *Client) PlacementGroups(ctx context.Context, opts hc.PlacementGroupListOpts) ([]*hc.PlacementGroup, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	return c.c.PlacementGroup.AllWithOpts(ctx, opts)
}

// CreatePlacementGroup implements API.
func (c *Client) CreatePlacementGroup(
	ctx context.Context, opts hc.PlacementGroupCreateOpts,
) (*hc.PlacementGroup, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	res, _, err := c.c.PlacementGroup.Create(ctx, opts)
	return res.PlacementGroup, err
}

// DeletePlacementGroup implements API.
func (c *Client) DeletePlacementGroup(ctx context.Context, g *hc.PlacementGroup) error {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	_, err := c.c.PlacementGroup.Delete(ctx, g)
	return err
}

// CreateVolume implements API.
func (c *Client) CreateVolume(ctx context.Context, opts hc.VolumeCreateOpts) (*hc.Volume, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	res, _, err := c.c.Volume.Create(ctx, opts)
	return res.Volume, err
}

// GetVolume implements API.
func (c *Client) GetVolume(ctx context.Context, id int64) (*hc.Volume, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	v, _, err := c.c.Volume.GetByID(ctx, id)
	return v, err
}

// Volumes implements API.
func (c *Client) Volumes(ctx context.Context, opts hc.VolumeListOpts) ([]*hc.Volume, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	return c.c.Volume.AllWithOpts(ctx, opts)
}

// DeleteVolume implements API.
func (c *Client) DeleteVolume(ctx context.Context, v *hc.Volume) error {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	_, err := c.c.Volume.Delete(ctx, v)
	return err
}

// GetPrimaryIP implements API.
func (c *Client) GetPrimaryIP(ctx context.Context, id int64) (*hc.PrimaryIP, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	ip, _, err := c.c.PrimaryIP.GetByID(ctx, id)
	return ip, err
}

// DeletePrimaryIP implements API.
func (c *Client) DeletePrimaryIP(ctx context.Context, ip *hc.PrimaryIP) error {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	_, err := c.c.PrimaryIP.Delete(ctx, ip)
	return err
}

// SSHKey implements API.
func (c *Client) SSHKey(ctx context.Context, idOrName string) (*hc.SSHKey, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	k, _, err := c.c.SSHKey.Get(ctx, idOrName)
	return k, err
}
