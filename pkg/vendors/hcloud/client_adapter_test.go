/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package hcloud_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/SpareCores/scarpenter/pkg/vendors/hcloud"
)

// TestClientAdapterRoutes calls every method of the real client against a stub
// that knows the API's paths: the adapter sends what the interface says to the
// endpoint the API documents, and returns what the SDK decoded.
func TestClientAdapterRoutes(t *testing.T) {
	page := `,"meta":{"pagination":{"page":1,"per_page":50,"last_page":1,"total_entries":1}}`
	act := `{"id":7,"status":"success","command":"x","progress":100}`
	routes := map[string]string{
		"GET /server_types":          `{"server_types":[{"id":1,"name":"cx23","cores":2,"memory":4,"disk":40,"architecture":"x86"}]` + page + `}`,
		"GET /images":                `{"images":[{"id":5,"type":"snapshot","status":"available","architecture":"x86"}]` + page + `}`,
		"GET /servers":               `{"servers":[{"id":3,"name":"a"}]` + page + `}`,
		"GET /servers/3":             `{"server":{"id":3,"name":"a"}}`,
		"DELETE /servers/3":          `{"action":` + act + `}`,
		"GET /actions/7":             `{"action":` + act + `}`,
		"GET /networks/4":            `{"network":{"id":4}}`,
		"GET /networks":              `{"networks":[{"id":4}]` + page + `}`,
		"GET /firewalls/6":           `{"firewall":{"id":6}}`,
		"GET /firewalls":             `{"firewalls":[{"id":6}]` + page + `}`,
		"POST /firewalls":            `{"firewall":{"id":6},"actions":[]}`,
		"DELETE /firewalls/6":        `{}`,
		"GET /placement_groups/8":    `{"placement_group":{"id":8}}`,
		"GET /placement_groups":      `{"placement_groups":[{"id":8}]` + page + `}`,
		"POST /placement_groups":     `{"placement_group":{"id":8}}`,
		"DELETE /placement_groups/8": `{}`,
		"POST /volumes":              `{"volume":{"id":9},"action":` + act + `,"next_actions":[]}`,
		"GET /volumes/9":             `{"volume":{"id":9}}`,
		"GET /volumes":               `{"volumes":[{"id":9}]` + page + `}`,
		"DELETE /volumes/9":          `{}`,
		"GET /primary_ips/10":        `{"primary_ip":{"id":10}}`,
		"DELETE /primary_ips/10":     `{}`,
		"GET /ssh_keys/11":           `{"ssh_key":{"id":11,"name":"k"}}`,
		"GET /locations":             `{"locations":[{"id":1,"name":"fsn1"}]` + page + `}`,
	}
	var seen []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		seen = append(seen, key)
		body, ok := routes[key]
		if !ok {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"no route ` + key + `"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()
	c := hcloud.NewClient(hcloud.ClientConfig{Token: "t", Endpoint: ts.URL})
	ctx := context.Background()
	check := func(name string, err error) {
		t.Helper()
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	_, err := c.ServerTypes(ctx)
	check("ServerTypes", err)
	_, err = c.Images(ctx, hc.ImageListOpts{})
	check("Images", err)
	_, err = c.Servers(ctx, hc.ServerListOpts{})
	check("Servers", err)
	_, err = c.GetServer(ctx, 3)
	check("GetServer", err)
	a, err := c.DeleteServer(ctx, &hc.Server{ID: 3})
	check("DeleteServer", err)
	if a == nil || a.ID != 7 {
		t.Errorf("DeleteServer returns its action: %+v", a)
	}
	check("WaitForAction", c.WaitForAction(ctx, a))
	_, err = c.GetNetwork(ctx, 4)
	check("GetNetwork", err)
	_, err = c.Networks(ctx, hc.NetworkListOpts{})
	check("Networks", err)
	_, err = c.GetFirewall(ctx, 6)
	check("GetFirewall", err)
	_, err = c.Firewalls(ctx, hc.FirewallListOpts{})
	check("Firewalls", err)
	fw, err := c.CreateFirewall(ctx, hc.FirewallCreateOpts{Name: "f"})
	check("CreateFirewall", err)
	check("DeleteFirewall", c.DeleteFirewall(ctx, fw))
	_, err = c.GetPlacementGroup(ctx, 8)
	check("GetPlacementGroup", err)
	_, err = c.PlacementGroups(ctx, hc.PlacementGroupListOpts{})
	check("PlacementGroups", err)
	g, err := c.CreatePlacementGroup(ctx, hc.PlacementGroupCreateOpts{Name: "g", Type: hc.PlacementGroupTypeSpread})
	check("CreatePlacementGroup", err)
	check("DeletePlacementGroup", c.DeletePlacementGroup(ctx, g))
	v, err := c.CreateVolume(ctx, hc.VolumeCreateOpts{Name: "v", Size: 10, Location: &hc.Location{Name: "fsn1"}})
	check("CreateVolume", err)
	_, err = c.GetVolume(ctx, 9)
	check("GetVolume", err)
	_, err = c.Volumes(ctx, hc.VolumeListOpts{})
	check("Volumes", err)
	check("DeleteVolume", c.DeleteVolume(ctx, v))
	_, err = c.GetPrimaryIP(ctx, 10)
	check("GetPrimaryIP", err)
	check("DeletePrimaryIP", c.DeletePrimaryIP(ctx, &hc.PrimaryIP{ID: 10}))
	k, err := c.SSHKey(ctx, "11")
	check("SSHKey", err)
	if k == nil || k.ID != 11 {
		t.Errorf("SSHKey: %+v", k)
	}
	// A failed delete returns no action and the error.
	if _, err := c.DeleteServer(ctx, &hc.Server{ID: 99}); err == nil {
		t.Errorf("DeleteServer of an unknown server: %v", err)
	}
	if len(seen) < 22 {
		t.Errorf("only %d calls reached the stub: %v", len(seen), seen)
	}
}
