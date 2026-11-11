/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vultr/govultr/v3"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/vultr"
)

// newClient talks to a local server: no Vultr call is made.
func newClient(t *testing.T, h http.HandlerFunc, edit func(*vultr.ClientConfig)) vultr.API {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg := vultr.ClientConfig{APIKey: "test-key", BaseURL: srv.URL, RetryWait: time.Millisecond, RatePerSecond: 1000}
	if edit != nil {
		edit(&cfg)
	}
	c, err := vultr.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func jsonReply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func TestClientNeedsAKey(t *testing.T) {
	if _, err := vultr.NewClient(vultr.ClientConfig{}); err == nil {
		t.Fatal("a client without a key")
	}
}

func TestClientRequest(t *testing.T) {
	var got *http.Request
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		jsonReply(w, 200, `{"instances":[{"id":"i-1","region":"ewr","tags":["a=b"]}],"meta":{"total":1,"links":{"next":"","prev":""}}}`)
	}, nil)
	list, meta, err := c.ListInstances(context.Background(), &govultr.ListOptions{Tag: "scarpenter.sparecores.com/cluster=my-cluster", Region: "ewr", PerPage: 500})
	if err != nil || len(list) != 1 || list[0].ID != "i-1" || meta.Total != 1 {
		t.Fatalf("got %+v %+v %v", list, meta, err)
	}
	if got.URL.Path != "/v2/instances" || got.Header.Get("Authorization") != "Bearer test-key" {
		t.Errorf("path %q, auth %q", got.URL.Path, got.Header.Get("Authorization"))
	}
	if ua := got.Header.Get("User-Agent"); !strings.HasPrefix(ua, "scarpenter/") {
		t.Errorf("user agent %q", ua)
	}
	q := got.URL.Query()
	if q.Get("tag") != "scarpenter.sparecores.com/cluster=my-cluster" || q.Get("region") != "ewr" || q.Get("per_page") != "500" {
		t.Errorf("query %v", q)
	}
}

func TestClientCreateSendsTheBody(t *testing.T) {
	var body map[string]any
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/instances" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		jsonReply(w, 202, `{"instance":{"id":"i-1","region":"ewr","status":"pending","main_ip":"0.0.0.0","default_password":"x"}}`)
	}, nil)
	req := govultr.InstanceCreateReq{Region: "ewr", Plan: "vc2-1c-1gb", SnapshotID: "s", UserData: "e30=", Tags: []string{"a=b"}}
	in, err := c.CreateInstance(context.Background(), &req)
	if err != nil || in.ID != "i-1" {
		t.Fatalf("got %+v %v", in, err)
	}
	if body["region"] != "ewr" || body["plan"] != "vc2-1c-1gb" || body["user_data"] != "e30=" || body["snapshot_id"] != "s" {
		t.Errorf("body %v", body)
	}
}

func TestClientCapturesTheStatusGovultrDrops(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		op     func(vultr.API) error
	}{
		{"404 on get", 404, `{"error":"Invalid instance ID.","status":404}`, func(c vultr.API) error {
			_, err := c.GetInstance(context.Background(), "x")
			return err
		}},
		{"400 stock on create", 400, `{"error":"Plan is currently unavailable in this location.","status":400}`, func(c vultr.API) error {
			_, err := c.CreateInstance(context.Background(), &govultr.InstanceCreateReq{})
			return err
		}},
		{"400 limit on create", 400, `{"error":"You have reached the account limit of instances.","status":400}`, func(c vultr.API) error {
			_, err := c.CreateInstance(context.Background(), &govultr.InstanceCreateReq{})
			return err
		}},
		{"401", 401, `{"error":"Invalid API token.","status":401}`, func(c vultr.API) error {
			_, _, err := c.ListInstances(context.Background(), nil)
			return err
		}},
		{"a body that is not JSON", 502, `<html>bad gateway</html>`, func(c vultr.API) error {
			_, _, err := c.ListInstances(context.Background(), nil)
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cl := newClient(t, func(w http.ResponseWriter, _ *http.Request) { jsonReply(w, c.status, c.body) }, func(cfg *vultr.ClientConfig) { cfg.Retries = -1 })
			err := c.op(cl)
			var he vultr.HTTPStatus
			if !errors.As(err, &he) || he.HTTPStatus() != c.status {
				t.Fatalf("no status %d on %v", c.status, err)
			}
			if strings.Contains(err.Error(), "error returned from api") {
				t.Errorf("the SDK prefix stayed in %q", err)
			}
		})
	}
}

func TestClientStatusClassifiedThroughOps(t *testing.T) {
	// The whole path: a real client against a server that answers like Vultr
	// (text only, no code), through Ops.
	var creates atomic.Int32
	h := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/instances" && r.Method == http.MethodGet:
			jsonReply(w, 200, `{"instances":[],"meta":{"total":0,"links":{"next":"","prev":""}}}`)
		case strings.HasSuffix(r.URL.Path, "/availability"):
			jsonReply(w, 200, `{"available_plans":["vc2-1c-1gb"]}`)
		case r.Method == http.MethodPost:
			creates.Add(1)
			jsonReply(w, 400, `{"error":"You have reached the account limit of instances.","status":400}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v2/instances/"):
			jsonReply(w, 404, `{"error":"Invalid instance ID.","status":404}`)
		default:
			jsonReply(w, 500, `{"error":"unexpected","status":500}`)
		}
	}
	e := newEnv(t)
	cl := newClient(t, h, func(cfg *vultr.ClientConfig) { cfg.Retries = -1 })
	ops := vultr.NewOps(cl, cluster)
	_, err := ops.Launch(context.Background(), e.request("claim-1", "uid-1"), e.launchOpts())
	if !vendors.IsClass(err, vendors.QuotaExceeded) || !vendors.IsAccountWide(err) {
		t.Fatalf("launch: %v", err)
	}
	if creates.Load() != 1 {
		t.Errorf("%d creates", creates.Load())
	}
	if _, err := ops.GetInstance(context.Background(), "ewr/gone"); !vendors.IsClass(err, vendors.NotFound) {
		t.Fatalf("get: %v", err)
	}
	if err := ops.DeleteInstance(context.Background(), "ewr/gone"); err != nil {
		t.Fatalf("delete of a gone instance: %v", err)
	}
}

func TestClientRetriesThenSucceeds(t *testing.T) {
	var n atomic.Int32
	c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) < 3 {
			jsonReply(w, 503, `{"error":"try later","status":503}`)
			return
		}
		jsonReply(w, 200, `{"regions":[{"id":"ewr"}],"meta":{"total":1,"links":{}}}`)
	}, nil)
	regions, _, err := c.ListRegions(context.Background(), nil)
	if err != nil || len(regions) != 1 || n.Load() != 3 {
		t.Fatalf("got %v %v after %d calls", regions, err, n.Load())
	}
}

// govultr drops the response when the retries run out; the transport still saw
// the 429, so the error keeps the status.
func TestClientRetriesExhaustedKeepsTheStatus(t *testing.T) {
	var n atomic.Int32
	c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		jsonReply(w, 429, `{"error":"Rate limit exceeded.","status":429}`)
	}, func(cfg *vultr.ClientConfig) { cfg.Retries = 2 })
	_, _, err := c.ListRegions(context.Background(), nil)
	var he vultr.HTTPStatus
	if !errors.As(err, &he) || he.HTTPStatus() != 429 || n.Load() != 3 {
		t.Fatalf("got %v after %d calls", err, n.Load())
	}
	ops := vultr.NewOps(c, cluster)
	if _, err := ops.Regions(context.Background(), nil); !vendors.IsClass(err, vendors.Retryable) || vendors.CodeOf(err) != "RateLimited" {
		t.Fatalf("classified %v", err)
	}
}

func TestClientCallTimeout(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		jsonReply(w, 200, `{}`)
	}, func(cfg *vultr.ClientConfig) { cfg.CallTimeout = 50 * time.Millisecond; cfg.Retries = -1 })
	start := time.Now()
	_, _, err := c.ListRegions(context.Background(), nil)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
		t.Fatalf("got %v after %v", err, time.Since(start))
	}
	ops := vultr.NewOps(c, cluster)
	if _, err := ops.Regions(context.Background(), nil); vendors.CodeOf(err) != "CallTimeout" || !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("classified %v", err)
	}
}

func TestClientCanceledContext(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, _ *http.Request) { jsonReply(w, 200, `{}`) }, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := c.ListRegions(ctx, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestClientLimitsTheRequestRate(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		jsonReply(w, 200, `{"regions":[],"meta":{"total":0,"links":{}}}`)
	}, func(cfg *vultr.ClientConfig) { cfg.RatePerSecond = 20 })
	start := time.Now()
	for range 30 {
		if _, _, err := c.ListRegions(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
	}
	// A burst of 20, then 10 more at 20 per second: at least 0.4 seconds.
	if d := time.Since(start); d < 400*time.Millisecond {
		t.Fatalf("30 calls took %v", d)
	}
}

func TestClientDoesNotPrintTheKey(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, _ *http.Request) { jsonReply(w, 200, `{}`) }, nil)
	if s := fmt.Sprintf("%v %+v %s", c, c, c); strings.Contains(s, "test-key") {
		t.Fatalf("the key is in %q", s)
	}
}

// Every call of the API interface goes through the client's wrapper.
func TestClientCallsEveryEndpoint(t *testing.T) {
	var paths []string
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		jsonReply(w, 200, `{"meta":{"total":0,"links":{}},"available_plans":[],"snapshot":{"id":"s"},"firewall_group":{"id":"g"},"firewall_rule":{"id":1},"vpc":{"id":"v"},"instance":{"id":"i"}}`)
	}, nil)
	ctx := context.Background()
	var errs []error
	add := func(err error) { errs = append(errs, err) }
	_, _, err := c.ListRegions(ctx, nil)
	add(err)
	_, err = c.RegionAvailability(ctx, "ewr", "all")
	add(err)
	_, _, err = c.ListPlans(ctx, "all", nil)
	add(err)
	_, _, err = c.ListSnapshots(ctx, nil)
	add(err)
	_, err = c.GetSnapshot(ctx, "s")
	add(err)
	_, err = c.CreateInstance(ctx, &govultr.InstanceCreateReq{})
	add(err)
	_, err = c.GetInstance(ctx, "i")
	add(err)
	add(c.DeleteInstance(ctx, "i"))
	_, _, err = c.ListInstances(ctx, nil)
	add(err)
	_, err = c.CreateFirewallGroup(ctx, &govultr.FirewallGroupReq{})
	add(err)
	_, err = c.GetFirewallGroup(ctx, "g")
	add(err)
	add(c.DeleteFirewallGroup(ctx, "g"))
	_, _, err = c.ListFirewallGroups(ctx, nil)
	add(err)
	_, err = c.CreateFirewallRule(ctx, "g", &govultr.FirewallRuleReq{})
	add(err)
	_, _, err = c.ListFirewallRules(ctx, "g", nil)
	add(err)
	add(c.DeleteFirewallRule(ctx, "g", 1))
	_, err = c.CreateVPC(ctx, &govultr.VPCReq{})
	add(err)
	_, err = c.GetVPC(ctx, "v")
	add(err)
	add(c.DeleteVPC(ctx, "v"))
	_, _, err = c.ListVPCs(ctx, nil)
	add(err)
	_, _, err = c.ListVPCAttachments(ctx, "v", nil)
	add(err)
	for i, err := range errs {
		if err != nil {
			t.Errorf("call %d: %v", i, err)
		}
	}
	want := []string{
		"GET /v2/regions", "GET /v2/regions/ewr/availability", "GET /v2/plans", "GET /v2/snapshots", "GET /v2/snapshots/s",
		"POST /v2/instances", "GET /v2/instances/i", "DELETE /v2/instances/i", "GET /v2/instances",
		"POST /v2/firewalls", "GET /v2/firewalls/g", "DELETE /v2/firewalls/g", "GET /v2/firewalls",
		"POST /v2/firewalls/g/rules", "GET /v2/firewalls/g/rules", "DELETE /v2/firewalls/g/rules/1",
		"POST /v2/vpcs", "GET /v2/vpcs/v", "DELETE /v2/vpcs/v", "GET /v2/vpcs", "GET /v2/vpcs/v/attachments",
	}
	if strings.Join(paths, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests:\n%s\nwant:\n%s", strings.Join(paths, "\n"), strings.Join(want, "\n"))
	}
}
