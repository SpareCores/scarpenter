/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package tailnet_test

import (
	"context"
	"errors"
	"net/netip"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/SpareCores/scarpenter/pkg/tailnet"
	"github.com/SpareCores/scarpenter/pkg/tailnet/tailnettest"
)

const (
	testAPIKey       = "hs-api-key-secret-value"
	testClientID     = "client-id-value"
	testClientSecret = "tskey-client-secret-value"
)

var testTags = []string{"tag:scarpenter-bench"}

// clock is a fake clock whose Sleep advances it, so retries cost no time.
type clock struct {
	mu     sync.Mutex
	t      time.Time
	sleeps []time.Duration
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *clock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, d)
	c.t = c.t.Add(d)
	return nil
}

func (c *clock) Sleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}

// rig is one backend with its fake, seen through what the contract tests need.
type rig struct {
	name    string
	backend tailnet.Backend
	clock   *clock
	// faults is the fake's fault injection and call log.
	failNext   func(n, status int, retryAfter string)
	setDelay   func(time.Duration)
	count      func(method, path string) int
	calls      func() int
	mintPath   string
	deletePath func(id string) string
	// fake state
	validKeys func() []string // IDs of keys a node could still join with
	deviceIDs func() []string
	join      func(secret, hostname string) (deviceID string, ip netip.Addr, err error)
	keyProps  func(id string) (tags []string, ephemeral, reusable bool)
	// byKey is true when the backend names the admitting key of a device.
	byKey bool
	url   string
	// tsFake is set for the Tailscale rig.
	tsFake *tailnettest.Tailscale
}

func (r *rig) now() time.Time { return r.clock.Now() }

func newTailscaleRig(t *testing.T, cfg tailnet.Config) *rig {
	t.Helper()
	clk := newClock()
	fake := tailnettest.NewTailscale(t, testClientID, testClientSecret)
	fake.SetNow(clk.Now)
	cfg.BaseURL = fake.URL()
	cfg.Now, cfg.Sleep = clk.Now, clk.Sleep
	b, err := tailnet.NewTailscale(tailnet.TailscaleConfig{
		Config: cfg,
		Credentials: func() (string, tailnet.Secret, error) {
			return testClientID, testClientSecret, nil
		},
	})
	require.NoError(t, err)
	r := &rig{
		name: "tailscale", backend: b, clock: clk, url: fake.URL(), tsFake: fake,
		failNext: fake.FailNext, setDelay: fake.SetDelay, count: fake.CountCalls,
		calls:      func() int { return len(fake.Calls()) },
		mintPath:   "/api/v2/tailnet/-/keys",
		deletePath: func(id string) string { return "/api/v2/device/" + id },
		validKeys: func() []string {
			ids := make([]string, 0, 4)
			for _, k := range fake.Keys() {
				ids = append(ids, k.ID)
			}
			return ids
		},
		deviceIDs: func() []string {
			ids := make([]string, 0, 4)
			for _, d := range fake.Devices() {
				ids = append(ids, d.NodeID)
			}
			return ids
		},
		join: func(secret, host string) (string, netip.Addr, error) {
			d, err := fake.Join(secret, host)
			if err != nil {
				return "", netip.Addr{}, err
			}
			return d.NodeID, netip.MustParseAddr(d.Addresses[0]), nil
		},
		keyProps: func(id string) ([]string, bool, bool) {
			for _, k := range fake.Keys() {
				if k.ID == id {
					require.True(t, k.Preauthorized)
					return k.Tags, k.Ephemeral, k.Reusable
				}
			}
			t.Fatalf("key %s not found", id)
			return nil, false, false
		},
	}
	return r
}

func newHeadscaleRig(t *testing.T, cfg tailnet.Config) *rig {
	t.Helper()
	clk := newClock()
	fake := tailnettest.NewHeadscale(t, testAPIKey)
	fake.SetNow(clk.Now)
	cfg.BaseURL = fake.URL()
	cfg.Now, cfg.Sleep = clk.Now, clk.Sleep
	b, err := tailnet.NewHeadscale(tailnet.HeadscaleConfig{
		Config: cfg,
		APIKey: func() (tailnet.Secret, error) { return testAPIKey, nil },
	})
	require.NoError(t, err)
	return &rig{
		name: "headscale", backend: b, clock: clk, byKey: true, url: fake.URL(),
		failNext: fake.FailNext, setDelay: fake.SetDelay, count: fake.CountCalls,
		calls:      func() int { return len(fake.Calls()) },
		mintPath:   "/api/v1/preauthkey",
		deletePath: func(id string) string { return "/api/v1/node/" + id },
		validKeys: func() []string {
			ids := make([]string, 0, 4)
			for _, k := range fake.ValidKeys() {
				ids = append(ids, itoa(k.ID))
			}
			return ids
		},
		deviceIDs: func() []string {
			ids := make([]string, 0, 4)
			for _, n := range fake.Nodes() {
				ids = append(ids, itoa(n.ID))
			}
			return ids
		},
		join: func(secret, host string) (string, netip.Addr, error) {
			n, err := fake.Join(secret, host)
			if err != nil {
				return "", netip.Addr{}, err
			}
			return itoa(n.ID), netip.MustParseAddr(n.IPs[0]), nil
		},
		keyProps: func(id string) ([]string, bool, bool) {
			for _, k := range fake.Keys() {
				if itoa(k.ID) == id {
					return k.Tags, k.Ephemeral, k.Reusable
				}
			}
			t.Fatalf("key %s not found", id)
			return nil, false, false
		},
	}
}

func itoa(n uint64) string { return strconv.FormatUint(n, 10) }

// eachBackend runs the test against both backends.
func eachBackend(t *testing.T, cfg tailnet.Config, fn func(t *testing.T, r *rig)) {
	t.Helper()
	for _, mk := range []func(*testing.T, tailnet.Config) *rig{newTailscaleRig, newHeadscaleRig} {
		r := mk(t, cfg)
		t.Run(r.name, func(t *testing.T) { fn(t, r) })
	}
}

func mint(t *testing.T, r *rig) tailnet.Key {
	t.Helper()
	k, err := r.backend.MintKey(t.Context(), tailnet.KeyRequest{Tags: testTags, Expiry: time.Hour, Description: "scarpenter nc-1"})
	require.NoError(t, err)
	return k
}

func requireClass(t *testing.T, err error, c tailnet.Class) {
	t.Helper()
	_ = requireError(t, err, c)
}

func requireError(t *testing.T, err error, c tailnet.Class) *tailnet.Error {
	t.Helper()
	require.Error(t, err)
	var e *tailnet.Error
	require.True(t, errors.As(err, &e), "not a *tailnet.Error: %v", err)
	require.Equal(t, c, e.Class, err.Error())
	return e
}
