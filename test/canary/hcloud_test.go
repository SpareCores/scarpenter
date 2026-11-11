/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/SpareCores/scarpenter/pkg/vendors/hcloud"
	"github.com/SpareCores/scarpenter/pkg/vendors/hcloud/fake"
)

func hcloudConfig(srv *httptest.Server) *config {
	return &config{
		vendor: "hcloud", region: "fsn1,nbg1", flatcarChannel: "stable", flatcarVersion: "4757.2.3",
		kubernetesVersion: "v1.37.1", tailscaleVersion: "v1.98.10", k3sVersion: "v1.37.2+k3s1",
		types:            []string{"hcloud.cx23", "hcloud.cax11", "hcloud.cpx22"},
		flatcarImageFile: "flatcar_production_hetzner_image.bin.bz2",
		sysextBase:       srv.URL, k3sBase: srv.URL, flatcarBase: srv.URL,
	}
}

// flatcarServer serves the release files of the Flatcar server on top of releases(): a missing
// path answers 404.
func flatcarServer(t *testing.T, missing ...string) *httptest.Server {
	t.Helper()
	base := releases(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, m := range missing {
			if strings.Contains(r.URL.Path, m) {
				http.NotFound(w, r)
				return
			}
		}
		if strings.Contains(r.URL.Path, "flatcar_production_hetzner_image.bin.bz2") {
			w.Header().Set("Content-Length", "573741824")
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte("digests"))
			}
			return
		}
		base.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func hcloudProject(t *testing.T) *fake.Fake {
	t.Helper()
	f := fake.New()
	f.AddFlatcarImages("stable", "4757.2.3")
	return f
}

func runHCloudCanary(t *testing.T, f *fake.Fake, cfg *config) (*report, *audit) {
	t.Helper()
	rep := &report{}
	a := newAudit()
	checkArtifacts(context.Background(), cfg, rep)
	runHCloud(context.Background(), cfg, rep, &roAPI{API: f, audit: a}, a)
	return rep, a
}

func TestHCloudCanaryPassesOnAHealthyWorld(t *testing.T) {
	cfg := hcloudConfig(flatcarServer(t))
	rep, a := runHCloudCanary(t, hcloudProject(t), cfg)
	var b bytes.Buffer
	rep.write(&b)
	if f := failed(rep); len(f) > 0 {
		t.Fatalf("failed checks %v:\n%s", f, b.String())
	}
	for _, want := range []string{"Flatcar release stable 4757.2.3 amd64", "Flatcar release stable 4757.2.3 arm64",
		"Flatcar snapshot stable 4757.2.3 arm64", "locations fsn1,nbg1", "server types in", "API rate limit", "read-only guard"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("the table lacks %q:\n%s", want, b.String())
		}
	}
	if len(a.refusals()) != 0 {
		t.Errorf("refusals: %v", a.refusals())
	}
}

func TestHCloudCanaryFailsWhenTheWorldMoved(t *testing.T) {
	base := hcloudConfig(flatcarServer(t))
	for name, tc := range map[string]struct {
		setup func(*fake.Fake, *config) *config
		want  string
	}{
		"a snapshot is gone": {func(f *fake.Fake, c *config) *config {
			c.flatcarVersion = "4800.0.0"
			return c
		}, "Flatcar snapshot stable 4800.0.0 amd64"},
		"two snapshots": {func(f *fake.Fake, c *config) *config {
			f.AddFlatcarImages("stable", "4757.2.3")
			return c
		}, "Flatcar snapshot stable 4757.2.3 amd64"},
		"a lane location is gone": {func(_ *fake.Fake, c *config) *config {
			c.region = "fsn1,xxx9"
			return c
		}, "locations fsn1,xxx9"},
		"a type is not offered": {func(_ *fake.Fake, c *config) *config {
			c.types = append(c.types, "hcloud.nosuchtype")
			return c
		}, "server types in"},
		"a type is past its deprecation": {func(f *fake.Fake, c *config) *config {
			f.SetDeprecation("cx23", "fsn1", time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour))
			f.SetDeprecation("cx23", "nbg1", time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour))
			f.SetDeprecation("cx23", "hel1", time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour))
			return c
		}, "server types in"},
		"the rate limit is nearly used": {func(f *fake.Fake, c *config) *config {
			for range 2700 {
				_, _ = f.GetServer(context.Background(), 1)
			}
			return c
		}, "API rate limit"},
		"the release server lost the image": {func(_ *fake.Fake, c *config) *config {
			c.flatcarBase = flatcarServer(t, "arm64-usr").URL
			return c
		}, "Flatcar release stable 4757.2.3 arm64"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := *base
			f := hcloudProject(t)
			rep, _ := runHCloudCanary(t, f, tc.setup(f, &cfg))
			got := failed(rep)
			if len(got) == 0 {
				t.Fatalf("nothing failed, want %q", tc.want)
			}
			for _, g := range got {
				// A missing or doubled snapshot fails both architectures.
				if !strings.HasPrefix(g, strings.TrimSuffix(tc.want, " amd64")) {
					t.Errorf("failed checks %v, want only %q", got, tc.want)
				}
			}
		})
	}
}

func TestHCloudCanaryOutOfStockIsNotAFailure(t *testing.T) {
	f := hcloudProject(t)
	f.SetAvailable("cx23", "fsn1", false)
	rep, _ := runHCloudCanary(t, f, hcloudConfig(flatcarServer(t)))
	if got := failed(rep); len(got) != 0 {
		t.Errorf("%v", got)
	}
	var b bytes.Buffer
	rep.write(&b)
	if !strings.Contains(b.String(), "temporarily unavailable: cx23@fsn1") {
		t.Errorf("the table does not say it:\n%s", b.String())
	}
}

func TestHCloudConfigNeedsNoQuota(t *testing.T) {
	env := map[string]string{
		"E2E_HCLOUD_LOCATIONS": "fsn1,nbg1", "E2E_FLATCAR_CHANNEL": "stable", "E2E_FLATCAR_VERSION": "4757.2.3",
		"E2E_KUBERNETES_VERSION": "v1.37.1", "E2E_TAILSCALE_VERSION": "v1.98.10", "E2E_K3S_VERSION": "v1.37.2+k3s1",
		"E2E_TYPE_AMD64": "hcloud.cx23", "E2E_TYPE_ARM64": "hcloud.cax11", "E2E_TYPE_DB": "hcloud.cx23",
		"E2E_TYPES_DESCRIBED": "hcloud.cx23,hcloud.cpx22",
	}
	cfg, err := loadConfig("hcloud", func(k string) string { return env[k] })
	if err != nil || cfg.region != "fsn1,nbg1" || cfg.flatcarImageFile == "" || cfg.minOnDemand != 0 {
		t.Fatalf("%+v, %v", cfg, err)
	}
	delete(env, "E2E_HCLOUD_LOCATIONS")
	if _, err := loadConfig("hcloud", func(k string) string { return env[k] }); err == nil {
		t.Error("the locations are required")
	}
}

// --- the guards ------------------------------------------------------------

// Every request that is not a GET is refused before the network; a GET is passed on and recorded.
func TestTransportGuardRefusesEveryMethodButGET(t *testing.T) {
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.Method]++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	a := newAudit()
	c := guardedHTTPClient(a)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead, http.MethodOptions, "PROPFIND"} {
		req, _ := http.NewRequestWithContext(context.Background(), m, srv.URL+"/v1/servers/42", nil)
		if resp, err := c.Do(req); err == nil || !errors.Is(err, errReadOnly) {
			if resp != nil {
				_ = resp.Body.Close()
			}
			t.Errorf("%s was not refused: %v", m, err)
		}
	}
	if len(hits) != 0 {
		t.Errorf("refused requests reached the server: %v", hits)
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/v1/servers/42", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if hits[http.MethodGet] != 1 || len(a.refusals()) != 7 || len(a.actions()) != 1 || a.actions()[0] != "GET servers" {
		t.Errorf("hits %v, refusals %v, reads %v", hits, a.refusals(), a.actions())
	}
}

// The real client through the guard: a write the driver would make never leaves.
func TestRealClientCannotWriteThroughTheGuard(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hit = true }))
	defer srv.Close()
	a := newAudit()
	c := hcloud.NewClient(hcloud.ClientConfig{Token: "t", Endpoint: srv.URL, HTTPClient: guardedHTTPClient(a)})
	if _, err := c.CreateServer(context.Background(), hc.ServerCreateOpts{Name: "x", ServerType: &hc.ServerType{Name: "cx23"}, Image: &hc.Image{ID: 1}}); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("CreateServer: %v", err)
	}
	if _, err := c.DeleteServer(context.Background(), &hc.Server{ID: 1}); err == nil {
		t.Error("DeleteServer")
	}
	if hit {
		t.Error("a write reached the server")
	}
}

// Every method of hcloud.API is either a read that reaches the inner API or a write that is
// refused before it: the guard cannot let a write through by omission, and a method added to the
// interface that is a write by its name is refused by the embedded-interface rule below.
func TestAPIGuardRefusesEveryWriteAndPassesEveryRead(t *testing.T) {
	typ := reflect.TypeFor[hcloud.API]()
	if typ.NumMethod() < 20 {
		t.Fatalf("the interface has only %d methods: is the reflection broken?", typ.NumMethod())
	}
	for method := range typ.Methods() {
		name := method.Name
		if name == "RateLimit" {
			continue // a counter of the client, no request
		}
		t.Run(name, func(t *testing.T) {
			f := fake.New()
			a := newAudit()
			g := &roAPI{API: f, audit: a}
			m := reflect.ValueOf(g).MethodByName(name)
			args := []reflect.Value{reflect.ValueOf(context.Background())}
			for i := 1; i < m.Type().NumIn(); i++ {
				args = append(args, reflect.New(m.Type().In(i)).Elem())
			}
			var err error
			func() {
				defer func() { _ = recover() }() // the fake on an empty input is not what is tested
				out := m.Call(args)
				if e, ok := reflect.TypeAssert[error](out[len(out)-1]); ok {
					err = e
				}
			}()
			if isHCloudRead(name) {
				if errors.Is(err, errReadOnly) {
					t.Fatalf("the read %s was refused: %v", name, err)
				}
				if got := a.actions(); len(got) != 1 || got[0] != "hcloud:"+name {
					t.Fatalf("the read %s was not recorded: %v", name, got)
				}
				return
			}
			if !errors.Is(err, errReadOnly) {
				t.Fatalf("the write %s was not refused: %v", name, err)
			}
			if f.Calls(name) != 0 {
				t.Fatalf("the write %s reached the inner API", name)
			}
			if got := a.refusals(); len(got) != 1 || got[0] != "hcloud:"+name {
				t.Fatalf("the refusal of %s was not recorded: %v", name, got)
			}
		})
	}
}

func TestIsHCloudRead(t *testing.T) {
	for op, want := range map[string]bool{
		"Locations": true, "GetServer": true, "Servers": true, "WaitForAction": true, "SSHKey": true,
		"CreateServer": false, "DeleteVolume": false, "DeletePrimaryIP": false, "CreateFirewall": false,
	} {
		if got := isHCloudRead(op); got != want {
			t.Errorf("isHCloudRead(%q) = %t, want %t", op, got, want)
		}
	}
}

func TestMainKnowsTheVendor(t *testing.T) {
	if _, ok := vendorChecks["hcloud"]; !ok {
		t.Error("hcloud is not a canary vendor")
	}
}
