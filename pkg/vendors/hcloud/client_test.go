/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/
package hcloud_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud/exp/mockutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/hcloud"
)

// TestCreateServerGolden asserts every field of the POST /servers body through
// the real SDK, so a changed SDK schema fails here too (plan 15, fake section).
func TestCreateServerGolden(t *testing.T) {
	spec := hcloud.LaunchSpec{
		Name: "nc-1", ServerType: "cpx22", Location: "fsn1", ImageID: 99, UserData: []byte(`{"ignition":{"version":"3.4.0"}}`),
		Tags:           map[vendors.TagKey]string{vendors.TagCluster: "c", vendors.TagManagedBy: "scarpenter"},
		ExtraTags:      map[string]string{"team": "infra"},
		IdempotencyKey: "uid-1", PublicIPv4: true, PublicIPv6: false,
		NetworkID: 11, FirewallID: 12, PlacementGroupID: 13, VolumeIDs: []int64{21, 22}, SSHKeyID: 5,
	}
	opts, err := hcloud.BuildCreateOpts(spec)
	require.NoError(t, err)

	srv := mockutil.NewServer(t, []mockutil.Request{{
		Method: "POST", Path: "/servers",
		Want: func(t *testing.T, r *http.Request) {
			var got map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
			assert.Equal(t, map[string]any{
				"name":               "nc-1",
				"server_type":        "cpx22",
				"location":           "fsn1",
				"image":              float64(99),
				"user_data":          `{"ignition":{"version":"3.4.0"}}`,
				"start_after_create": true,
				"automount":          false,
				"labels": map[string]any{
					"scarpenter.sparecores.com/cluster":         "c",
					"scarpenter.sparecores.com/managed-by":      "scarpenter",
					"scarpenter.sparecores.com/idempotency-key": "uid-1",
					"scarpenter.sparecores.com/capacity-type":   "on-demand",
					"team": "infra",
				},
				"public_net":      map[string]any{"enable_ipv4": true, "enable_ipv6": false},
				"networks":        []any{float64(11)},
				"firewalls":       []any{map[string]any{"firewall": float64(12)}},
				"placement_group": float64(13),
				"volumes":         []any{float64(21), float64(22)},
				"ssh_keys":        []any{float64(5)},
			}, got)
		},
		Status: 201,
		JSON: map[string]any{
			"server": map[string]any{"id": 1, "name": "nc-1", "status": "initializing", "labels": map[string]string{}},
			"action": map[string]any{"id": 2, "status": "running", "command": "create_server"},
		},
	}})
	c := hcloud.NewClient(hcloud.ClientConfig{Token: "t", Endpoint: srv.URL, Version: "1.2.3"})
	res, err := c.CreateServer(context.Background(), opts)
	require.NoError(t, err)
	assert.Equal(t, int64(1), res.Server.ID)
	assert.Equal(t, int64(2), res.Action.ID)
}

// TestClientHeadersAndRateLimit: user agent, bearer token, the RateLimit-*
// headers of every response and the per-call error body.
func TestClientHeadersAndRateLimit(t *testing.T) {
	var ua, auth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua, auth = r.UserAgent(), r.Header.Get("Authorization")
		w.Header().Set("RateLimit-Limit", "3600")
		w.Header().Set("RateLimit-Remaining", "2000")
		w.Header().Set("RateLimit-Reset", "1760000000")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"locations":[{"id":1,"name":"fsn1","network_zone":"eu-central"}],"meta":{"pagination":{"page":1,"per_page":50,"last_page":1,"total_entries":1}}}`))
	}))
	defer ts.Close()
	c := hcloud.NewClient(hcloud.ClientConfig{Token: "secret", Endpoint: ts.URL, Version: "9.9.9"})
	assert.False(t, c.RateLimit().Known())
	locs, err := c.Locations(context.Background())
	require.NoError(t, err)
	require.Len(t, locs, 1)
	assert.Equal(t, "fsn1", locs[0].Name)
	assert.Contains(t, ua, "scarpenter/9.9.9")
	assert.Equal(t, "Bearer secret", auth)
	rl := c.RateLimit()
	assert.Equal(t, hcloud.RateLimit{Limit: 3600, Remaining: 2000, Reset: 1760000000}, rl)
	assert.False(t, rl.LowRemaining())
	assert.True(t, hcloud.RateLimit{Limit: 3600, Remaining: 700}.LowRemaining())
	assert.False(t, hcloud.RateLimit{}.LowRemaining())
}

func TestClientErrorClassification(t *testing.T) {
	srv := mockutil.NewServer(t, []mockutil.Request{{
		Method: "POST", Path: "/servers", Status: 412,
		JSONRaw: `{"error":{"code":"resource_limit_exceeded","message":"server limit reached"}}`,
	}})
	c := hcloud.NewClient(hcloud.ClientConfig{Token: "t", Endpoint: srv.URL})
	p := hcloud.NewProject(c, "c")
	_, err := p.Create(context.Background(), hcloud.LaunchSpec{Name: "n", ServerType: "cx23", Location: "fsn1", ImageID: 1, PublicIPv4: true})
	assert.Equal(t, vendors.QuotaExceeded, vendors.ClassOf(err))
	assert.True(t, vendors.IsAccountWide(err))
}

func TestClientCallTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer ts.Close()
	c := hcloud.NewClient(hcloud.ClientConfig{Token: "t", Endpoint: ts.URL, CallTimeout: 50 * time.Millisecond})
	p := hcloud.NewProject(c, "c")
	_, err := p.Regions(context.Background())
	require.Error(t, err)
	assert.Equal(t, vendors.Retryable, vendors.ClassOf(err))
	assert.Equal(t, hcloud.CodeTimeout, vendors.CodeOf(err))
}

func TestGetUnknownIsNotFound(t *testing.T) {
	srv := mockutil.NewServer(t, []mockutil.Request{{
		Method: "GET", Path: "/servers/5", Status: 404,
		JSONRaw: `{"error":{"code":"not_found","message":"server not found"}}`,
	}})
	p := hcloud.NewProject(hcloud.NewClient(hcloud.ClientConfig{Token: "t", Endpoint: srv.URL}), "c")
	_, err := p.Get(context.Background(), hcloud.NewRef("fsn1", 5))
	assert.Equal(t, vendors.NotFound, vendors.ClassOf(err))
}

var ctx = context.Background()

// TestClientBareStatus: responses without a JSON error body, through the real
// SDK: 429 is Retryable RateLimited, 503 Retryable, 403 Fatal.
func TestClientBareStatus(t *testing.T) {
	for status, want := range map[int]vendors.Class{429: vendors.Retryable, 503: vendors.Retryable, 403: vendors.Fatal} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(status)
			_, _ = w.Write([]byte("slow down"))
		}))
		c := hcloud.NewClient(hcloud.ClientConfig{Token: "t", Endpoint: ts.URL})
		_, err := hcloud.NewProject(c, "c").Get(context.Background(), "fsn1/1")
		assert.Equal(t, want, vendors.ClassOf(err), "status %d: %v", status, err)
		if status == 429 {
			assert.Equal(t, hcloud.CodeRateLimited, vendors.CodeOf(err))
		}
		ts.Close()
	}
}
