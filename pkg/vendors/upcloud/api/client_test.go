/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/client"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"
)

type seen struct {
	method, path, auth, agent string
	body                      map[string]any
}

// testClient serves canned answers by "METHOD path" and records the requests.
func testClient(t *testing.T, answers map[string]func(w http.ResponseWriter)) (*Client, *[]seen) {
	t.Helper()
	var reqs []seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := seen{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), agent: r.UserAgent()}
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &s.body)
		}
		reqs = append(reqs, s)
		if f, ok := answers[r.Method+" "+r.URL.Path]; ok {
			f(w)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return New("tok-123", "scarpenter/test", client.WithBaseURL(srv.URL)), &reqs
}

func jsonAnswer(status int, body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func problemAnswer(status int, body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func TestNewFromEnv(t *testing.T) {
	t.Setenv(TokenEnv, "")
	if _, err := NewFromEnv("x"); !errors.Is(err, ErrNoToken) {
		t.Errorf("empty token: %v", err)
	}
	t.Setenv(TokenEnv, "abc")
	if c, err := NewFromEnv("x"); err != nil || c == nil {
		t.Errorf("token set: %v", err)
	}
}

func TestRulesetCalls(t *testing.T) {
	c, reqs := testClient(t, map[string]func(http.ResponseWriter){
		"GET /1.3/firewall-ruleset": jsonAnswer(200, `[{"uuid":"u1","name":"a","stateful":true,"enabled":true,"version":3,
			"labels":[{"key":"scarpenter-use","value":"true"}]},{"uuid":"u2","name":"b"}]`),
		"GET /1.3/firewall-ruleset/u1":                        jsonAnswer(200, `{"uuid":"u1","name":"a","description":"d"}`),
		"POST /1.3/firewall-ruleset":                          jsonAnswer(201, `{"uuid":"new","name":"n","stateful":true}`),
		"POST /1.3/firewall-ruleset/new/rule":                 jsonAnswer(201, `{}`),
		"DELETE /1.3/firewall-ruleset/u1":                     jsonAnswer(204, ``),
		"POST /1.3/server/s1/attach_firewall_ruleset/private": jsonAnswer(200, `{}`),
		"POST /1.3/server/s1/detach_firewall_ruleset/private": jsonAnswer(200, `{}`),
	})
	ctx := context.Background()
	list, err := c.GetFirewallRulesets(ctx)
	if err != nil || len(list) != 2 || list[0].UUID != "u1" || !list[0].Stateful || list[0].Version != 3 || list[0].Labels[0].Key != "scarpenter-use" {
		t.Fatalf("list = %+v %v", list, err)
	}
	if r, err := c.GetFirewallRuleset(ctx, "u1"); err != nil || r.Description != "d" {
		t.Fatalf("get = %+v %v", r, err)
	}
	created, err := c.CreateFirewallRuleset(ctx, CreateRulesetRequest{
		Name: "n", Stateful: true, Labels: []upcloud.Label{{Key: "scarpenter-use", Value: "true"}},
		Rules: []RulesetRule{
			{Action: "accept", Direction: "in", Family: "IPv4", Protocol: "tcp", CIDR: "10.0.0.0/8", PortStart: 22, PortEnd: 22},
			{Action: "accept", Direction: "out", Family: "IPv4"},
		},
	})
	if err != nil || created.UUID != "new" {
		t.Fatalf("create = %+v %v", created, err)
	}
	if err := c.DeleteFirewallRuleset(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	if err := c.AttachPrivateFirewallRuleset(ctx, "s1", "u1"); err != nil {
		t.Fatal(err)
	}
	if err := c.DetachPrivateFirewallRuleset(ctx, "s1", "u1"); err != nil {
		t.Fatal(err)
	}
	for _, r := range *reqs {
		if r.auth != "Bearer tok-123" || r.agent != "scarpenter/test" {
			t.Errorf("%s %s: auth %q agent %q", r.method, r.path, r.auth, r.agent)
		}
	}
	got := *reqs
	// The create call sends the ruleset, then its rules in one request.
	if got[2].body["name"] != "n" || got[2].body["stateful"] != true {
		t.Errorf("create body = %v", got[2].body)
	}
	rules := got[3].body["rules"].([]any)
	if len(rules) != 2 || rules[0].(map[string]any)["source_address_cidr"] != "10.0.0.0/8" ||
		rules[1].(map[string]any)["destination_address_cidr"] != nil {
		t.Errorf("rules body = %v", got[3].body)
	}
	att := got[5].body["firewall_ruleset"].(map[string]any)
	if att["firewall_ruleset_uuid"] != "u1" {
		t.Errorf("attach body = %v", got[5].body)
	}
}

func TestRulesetErrorsAreProblems(t *testing.T) {
	c, _ := testClient(t, map[string]func(http.ResponseWriter){
		"GET /1.3/firewall-ruleset/missing": problemAnswer(404, `{"type":"https://developers.upcloud.com/1.3/errors#ERROR_NOT_FOUND","title":"gone","status":404}`),
		"DELETE /1.3/firewall-ruleset/busy": jsonAnswer(409, `{"error":{"error_code":"FIREWALL_RULESET_IN_USE","error_message":"attached"}}`),
		"GET /1.3/firewall-ruleset":         problemAnswer(500, `{"type":"https://developers.upcloud.com/1.3/errors#ERROR_GENERAL_FAILURE","title":"x"}`),
	})
	ctx := context.Background()
	_, err := c.GetFirewallRuleset(ctx, "missing")
	if p, ok := errors.AsType[*upcloud.Problem](err); !ok || p.ErrorCode() != "NOT_FOUND" || p.Status != 404 {
		t.Errorf("problem+json: %v", err)
	}
	err = c.DeleteFirewallRuleset(ctx, "busy")
	if p, ok := errors.AsType[*upcloud.Problem](err); !ok || p.ErrorCode() != "FIREWALL_RULESET_IN_USE" || p.Status != 409 {
		t.Errorf("legacy: %v", err)
	}
	_, err = c.GetFirewallRulesets(ctx)
	if p, ok := errors.AsType[*upcloud.Problem](err); !ok || p.Status != 500 {
		t.Errorf("status fill-in: %v", err)
	}
}

func TestParseErrorPassThrough(t *testing.T) {
	if ParseError(nil) != nil {
		t.Error("nil")
	}
	plain := errors.New("x")
	if ParseError(plain) != plain {
		t.Error("plain error changed")
	}
	bad := &client.Error{ErrorCode: 502, ResponseBody: []byte("<html>"), Type: client.ErrorTypeError}
	if ParseError(bad) != error(bad) {
		t.Error("an unparsable body must pass through")
	}
}

func TestCreateServerStorageLabels(t *testing.T) {
	c, reqs := testClient(t, map[string]func(http.ResponseWriter){
		"POST /1.3/server": jsonAnswer(202, `{"server":{"uuid":"s1","state":"maintenance"}}`),
	})
	r := &request.CreateServerRequest{
		Zone: "fi-hel1", Plan: "1xCPU-2GB", Hostname: "h", Title: "h",
		StorageDevices: request.CreateServerStorageDeviceSlice{
			{Action: request.CreateServerStorageDeviceActionClone, Storage: "tpl", Size: 20},
			{Action: request.CreateServerStorageDeviceActionCreate, Size: 10, Tier: "maxiops"},
			{Action: request.CreateServerStorageDeviceActionAttach, Storage: "other"},
		},
	}
	labels := []upcloud.Label{{Key: "scarpenter-cluster", Value: "c1"}}
	d, err := c.CreateServer(context.Background(), r, labels...)
	if err != nil || d.UUID != "s1" {
		t.Fatalf("create: %v %+v", err, d)
	}
	server := (*reqs)[0].body["server"].(map[string]any)
	devs := server["storage_devices"].(map[string]any)["storage_device"].([]any)
	for i, want := range []bool{true, true, false} {
		dev := devs[i].(map[string]any)
		ls, has := dev["labels"].([]any)
		if has != want || (has && (len(ls) != 1 || ls[0].(map[string]any)["key"] != "scarpenter-cluster")) {
			t.Errorf("device %d (%v): labels %v", i, dev["action"], dev["labels"])
		}
	}
	if server["zone"] != "fi-hel1" || server["plan"] != "1xCPU-2GB" || devs[0].(map[string]any)["storage"] != "tpl" {
		t.Errorf("the SDK's fields were lost: %v", server)
	}
	// An error answer is the same Problem the SDK returns.
	c, _ = testClient(t, map[string]func(http.ResponseWriter){
		"POST /1.3/server": jsonAnswer(409, `{"error":{"error_code":"SERVER_CREATING_LIMIT_REACHED","error_message":"x"}}`),
	})
	_, err = c.CreateServer(context.Background(), r, labels...)
	if p, ok := errors.AsType[*upcloud.Problem](err); !ok || p.ErrorCode() != "SERVER_CREATING_LIMIT_REACHED" {
		t.Errorf("error = %v", err)
	}
}
