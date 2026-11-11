/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/vultr/govultr/v3"
	"golang.org/x/time/rate"

	"github.com/SpareCores/scarpenter/internal/version"
)

// Client defaults.
const (
	// DefaultCallTimeout bounds one API call, including govultr's retries of it
	// (plan 04 review N4).
	DefaultCallTimeout = 45 * time.Second
	// DefaultCreateTimeout bounds Instance.Create.
	DefaultCreateTimeout = 90 * time.Second
	// DefaultRatePerSecond is the client-side limit; Vultr's is 30 per second per
	// source IP, with HTTP 429 above it.
	DefaultRatePerSecond = 10
	// DefaultRetries is govultr's retry count for 429 and 5xx.
	DefaultRetries = 3
)

// ClientConfig configures NewClient.
type ClientConfig struct {
	// APIKey is VULTR_API_KEY. Never logged.
	APIKey string
	// BaseURL overrides https://api.vultr.com (tests).
	BaseURL string
	// UserAgent defaults to scarpenter/<version>.
	UserAgent string
	// Transport is the base round tripper (tests); default http.DefaultTransport.
	Transport http.RoundTripper
	// RatePerSecond, CallTimeout, CreateTimeout and Retries default as above;
	// Retries < 0 disables govultr's retries.
	RatePerSecond float64
	CallTimeout   time.Duration
	CreateTimeout time.Duration
	Retries       int
	// RetryWait is govultr's backoff base (its "rate limit"), default 500ms.
	RetryWait time.Duration
}

// NewClient builds the real API client: govultr behind the API interface, with
// a transport that authenticates, limits the request rate and records the HTTP
// status per call (govultr drops it from its errors).
func NewClient(cfg ClientConfig) (API, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("vultr: no API key")
	}
	c := cfg
	if c.UserAgent == "" {
		c.UserAgent = "scarpenter/" + version.Version
	}
	if c.Transport == nil {
		c.Transport = http.DefaultTransport
	}
	if c.RatePerSecond <= 0 {
		c.RatePerSecond = DefaultRatePerSecond
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = DefaultCallTimeout
	}
	if c.CreateTimeout <= 0 {
		c.CreateTimeout = DefaultCreateTimeout
	}
	if c.Retries == 0 {
		c.Retries = DefaultRetries
	}
	if c.RetryWait <= 0 {
		c.RetryWait = 500 * time.Millisecond
	}
	httpClient := &http.Client{Transport: &transport{
		base: c.Transport, key: c.APIKey, limiter: rate.NewLimiter(rate.Limit(c.RatePerSecond), int(c.RatePerSecond)),
	}}
	g := govultr.NewClient(httpClient)
	if c.BaseURL != "" {
		if err := g.SetBaseURL(c.BaseURL); err != nil {
			return nil, err
		}
	}
	g.SetUserAgent(c.UserAgent)
	g.SetRetryLimit(max(c.Retries, 0))
	g.SetRateLimit(c.RetryWait)
	return &sdkClient{g: g, call: c.CallTimeout, create: c.CreateTimeout}, nil
}

// statusKey carries a *statusRecorder in the request context.
type statusKey struct{}

// statusRecorder holds the status of the last response of one call.
type statusRecorder struct {
	mu     sync.Mutex
	status int
}

func (r *statusRecorder) set(s int) {
	r.mu.Lock()
	r.status = s
	r.mu.Unlock()
}

func (r *statusRecorder) get() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// transport adds the key, waits for the limiter and records the status of each
// response in the call's recorder. govultr's retries pass through it, so the
// recorder ends with the status of the last attempt.
type transport struct {
	base    http.RoundTripper
	key     string
	limiter *rate.Limiter
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.limiter.Wait(req.Context()); err != nil {
		return nil, err
	}
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+t.key)
	resp, err := t.base.RoundTrip(req)
	if rec, ok := req.Context().Value(statusKey{}).(*statusRecorder); ok {
		if err != nil {
			rec.set(0)
		} else {
			rec.set(resp.StatusCode)
		}
	}
	return resp, err
}

// sdkClient implements API over govultr.
type sdkClient struct {
	g            *govultr.Client
	call, create time.Duration
}

var _ API = (*sdkClient)(nil)

// apiErrorPrefix is how govultr starts the message of an API error.
const apiErrorPrefix = "error returned from api : "

// wrap makes an APIError of a failed call with the status the transport saw.
// Context errors pass through, so errors.Is keeps working.
func wrap(err error, status int) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &APIError{Status: status, Message: strings.TrimPrefix(err.Error(), apiErrorPrefix), Err: err}
}

// do runs f under the deadline with a status recorder.
func do[T any](ctx context.Context, timeout time.Duration, f func(ctx context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	rec := &statusRecorder{}
	v, err := f(context.WithValue(ctx, statusKey{}, rec))
	if err != nil && ctx.Err() != nil {
		// govultr's retry handler flattens the context error into text
		// ("gave up after 1 attempts, last error : context canceled"); restore it.
		err = fmt.Errorf("%w: %v", ctx.Err(), err)
	}
	return v, wrap(err, rec.get())
}

// run is do for calls without a result.
func (c *sdkClient) run(ctx context.Context, f func(ctx context.Context) error) error {
	_, err := do(ctx, c.call, func(ctx context.Context) (struct{}, error) { return struct{}{}, f(ctx) })
	return err
}

func (c *sdkClient) ListRegions(ctx context.Context, opts *govultr.ListOptions) ([]govultr.Region, *govultr.Meta, error) {
	type r struct {
		v []govultr.Region
		m *govultr.Meta
	}
	out, err := do(ctx, c.call, func(ctx context.Context) (r, error) {
		v, m, _, err := c.g.Region.List(ctx, opts)
		return r{v, m}, err
	})
	return out.v, out.m, err
}

func (c *sdkClient) RegionAvailability(ctx context.Context, regionID, planType string) (*govultr.PlanAvailability, error) {
	return do(ctx, c.call, func(ctx context.Context) (*govultr.PlanAvailability, error) {
		v, _, err := c.g.Region.Availability(ctx, regionID, planType)
		return v, err
	})
}

func (c *sdkClient) ListPlans(ctx context.Context, planType string, opts *govultr.ListOptions) ([]govultr.Plan, *govultr.Meta, error) {
	type r struct {
		v []govultr.Plan
		m *govultr.Meta
	}
	out, err := do(ctx, c.call, func(ctx context.Context) (r, error) {
		v, m, _, err := c.g.Plan.List(ctx, planType, opts)
		return r{v, m}, err
	})
	return out.v, out.m, err
}

func (c *sdkClient) ListSnapshots(ctx context.Context, opts *govultr.ListOptions) ([]govultr.Snapshot, *govultr.Meta, error) {
	type r struct {
		v []govultr.Snapshot
		m *govultr.Meta
	}
	out, err := do(ctx, c.call, func(ctx context.Context) (r, error) {
		v, m, _, err := c.g.Snapshot.List(ctx, opts)
		return r{v, m}, err
	})
	return out.v, out.m, err
}

func (c *sdkClient) GetSnapshot(ctx context.Context, id string) (*govultr.Snapshot, error) {
	return do(ctx, c.call, func(ctx context.Context) (*govultr.Snapshot, error) {
		v, _, err := c.g.Snapshot.Get(ctx, id)
		return v, err
	})
}

func (c *sdkClient) CreateInstance(ctx context.Context, req *govultr.InstanceCreateReq) (*govultr.Instance, error) {
	return do(ctx, c.create, func(ctx context.Context) (*govultr.Instance, error) {
		v, _, err := c.g.Instance.Create(ctx, req)
		return v, err
	})
}

func (c *sdkClient) GetInstance(ctx context.Context, id string) (*govultr.Instance, error) {
	return do(ctx, c.call, func(ctx context.Context) (*govultr.Instance, error) {
		v, _, err := c.g.Instance.Get(ctx, id)
		return v, err
	})
}

func (c *sdkClient) DeleteInstance(ctx context.Context, id string) error {
	return c.run(ctx, func(ctx context.Context) error { return c.g.Instance.Delete(ctx, id) })
}

func (c *sdkClient) ListInstances(ctx context.Context, opts *govultr.ListOptions) ([]govultr.Instance, *govultr.Meta, error) {
	type r struct {
		v []govultr.Instance
		m *govultr.Meta
	}
	out, err := do(ctx, c.call, func(ctx context.Context) (r, error) {
		v, m, _, err := c.g.Instance.List(ctx, opts)
		return r{v, m}, err
	})
	return out.v, out.m, err
}

func (c *sdkClient) CreateFirewallGroup(ctx context.Context, req *govultr.FirewallGroupReq) (*govultr.FirewallGroup, error) {
	return do(ctx, c.call, func(ctx context.Context) (*govultr.FirewallGroup, error) {
		v, _, err := c.g.FirewallGroup.Create(ctx, req)
		return v, err
	})
}

func (c *sdkClient) GetFirewallGroup(ctx context.Context, id string) (*govultr.FirewallGroup, error) {
	return do(ctx, c.call, func(ctx context.Context) (*govultr.FirewallGroup, error) {
		v, _, err := c.g.FirewallGroup.Get(ctx, id)
		return v, err
	})
}

func (c *sdkClient) DeleteFirewallGroup(ctx context.Context, id string) error {
	return c.run(ctx, func(ctx context.Context) error { return c.g.FirewallGroup.Delete(ctx, id) })
}

func (c *sdkClient) ListFirewallGroups(ctx context.Context, opts *govultr.ListOptions) ([]govultr.FirewallGroup, *govultr.Meta, error) {
	type r struct {
		v []govultr.FirewallGroup
		m *govultr.Meta
	}
	out, err := do(ctx, c.call, func(ctx context.Context) (r, error) {
		v, m, _, err := c.g.FirewallGroup.List(ctx, opts)
		return r{v, m}, err
	})
	return out.v, out.m, err
}

func (c *sdkClient) CreateFirewallRule(ctx context.Context, groupID string, req *govultr.FirewallRuleReq) (*govultr.FirewallRule, error) {
	return do(ctx, c.call, func(ctx context.Context) (*govultr.FirewallRule, error) {
		v, _, err := c.g.FirewallRule.Create(ctx, groupID, req)
		return v, err
	})
}

func (c *sdkClient) ListFirewallRules(ctx context.Context, groupID string, opts *govultr.ListOptions) ([]govultr.FirewallRule, *govultr.Meta, error) {
	type r struct {
		v []govultr.FirewallRule
		m *govultr.Meta
	}
	out, err := do(ctx, c.call, func(ctx context.Context) (r, error) {
		v, m, _, err := c.g.FirewallRule.List(ctx, groupID, opts)
		return r{v, m}, err
	})
	return out.v, out.m, err
}

func (c *sdkClient) DeleteFirewallRule(ctx context.Context, groupID string, ruleID int) error {
	return c.run(ctx, func(ctx context.Context) error { return c.g.FirewallRule.Delete(ctx, groupID, ruleID) })
}

func (c *sdkClient) CreateVPC(ctx context.Context, req *govultr.VPCReq) (*govultr.VPC, error) {
	return do(ctx, c.call, func(ctx context.Context) (*govultr.VPC, error) {
		v, _, err := c.g.VPC.Create(ctx, req)
		return v, err
	})
}

func (c *sdkClient) GetVPC(ctx context.Context, id string) (*govultr.VPC, error) {
	return do(ctx, c.call, func(ctx context.Context) (*govultr.VPC, error) {
		v, _, err := c.g.VPC.Get(ctx, id)
		return v, err
	})
}

func (c *sdkClient) DeleteVPC(ctx context.Context, id string) error {
	return c.run(ctx, func(ctx context.Context) error { return c.g.VPC.Delete(ctx, id) })
}

func (c *sdkClient) ListVPCs(ctx context.Context, opts *govultr.ListOptions) ([]govultr.VPC, *govultr.Meta, error) {
	type r struct {
		v []govultr.VPC
		m *govultr.Meta
	}
	out, err := do(ctx, c.call, func(ctx context.Context) (r, error) {
		v, m, _, err := c.g.VPC.List(ctx, opts)
		return r{v, m}, err
	})
	return out.v, out.m, err
}

func (c *sdkClient) ListVPCAttachments(ctx context.Context, vpcID string, opts *govultr.ListOptions) ([]govultr.VPCAttachment, *govultr.Meta, error) {
	type r struct {
		v []govultr.VPCAttachment
		m *govultr.Meta
	}
	out, err := do(ctx, c.call, func(ctx context.Context) (r, error) {
		v, m, _, err := c.g.VPC.ListAttachments(ctx, vpcID, opts)
		return r{v, m}, err
	})
	return out.v, out.m, err
}

// String keeps the key out of logs that print the client.
func (c *sdkClient) String() string { return fmt.Sprintf("vultr client (%s)", c.g.BaseURL) }
