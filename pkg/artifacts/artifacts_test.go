/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package artifacts_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	clocktesting "k8s.io/utils/clock/testing"

	"github.com/SpareCores/scarpenter/pkg/artifacts"
)

// The fixtures are the real SHA256SUMS of the two releases, downloaded from
// the sysext-bakery on 2026-11-06.
const (
	k8sAMD64 = "75072a32a1d87cfbe67907a0f6ebe3f0a860c2b2e15dbf1d8a354d5162e63b3b"
	k8sARM64 = "2e2a6faf04230c6b719656dbea20b76f1fbf9a5b3e61c0787c0850f052e40f6b"
	tsAMD64  = "d28c68ae6ed525bbeed0e95ef68b9f660b3e79eb0ca834ebf481ac86f7a886d4"
	tsARM64  = "580eade1a34f60161427f17d11c14581164c02133811670a5f554f4ff05d7ce6"
)

// server serves testdata like the sysext-bakery serves releases and counts the
// requests per path.
func server(t *testing.T) (*httptest.Server, *map[string]*atomic.Int32) {
	t.Helper()
	counts := map[string]*atomic.Int32{}
	for _, rel := range []string{"kubernetes-v1.37.1", "tailscale-v1.98.10"} {
		counts["/"+rel+"/SHA256SUMS"] = &atomic.Int32{}
	}
	fs := http.FileServer(http.Dir("testdata"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, ok := counts[r.URL.Path]; ok {
			c.Add(1)
		}
		fs.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &counts
}

func TestSysextURLs(t *testing.T) {
	r := artifacts.New(artifacts.Config{})
	tests := []struct {
		name, version, arch, want string
		wantErr                   string
	}{
		{"kubernetes", "v1.37.1", "amd64",
			"https://github.com/flatcar/sysext-bakery/releases/download/kubernetes-v1.37.1/kubernetes-v1.37.1-x86-64.raw", ""},
		{"tailscale", "v1.98.10", "arm64",
			"https://github.com/flatcar/sysext-bakery/releases/download/tailscale-v1.98.10/tailscale-v1.98.10-arm64.raw", ""},
		{"kubernetes", "v1.37.1", "riscv", "", "unsupported architecture"},
		{"Kube/../x", "v1.37.1", "amd64", "", "invalid sysext name"},
		{"kubernetes", "latest", "amd64", "", "invalid sysext version"},
		{"kubernetes", "v1/../2", "amd64", "", "invalid sysext version"},
	}
	for _, tc := range tests {
		got, err := r.SysextURL(tc.name, tc.version, tc.arch)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("SysextURL(%q, %q, %q) error = %v, want %q", tc.name, tc.version, tc.arch, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("SysextURL(%q, %q, %q) = %q, %v; want %q", tc.name, tc.version, tc.arch, got, err, tc.want)
		}
	}
}

func TestConfigurableBase(t *testing.T) {
	r := artifacts.New(artifacts.Config{SysextBase: "https://mirror.example.com/sysexts/", FlatcarBase: "https://mirror.example.com/flatcar"})
	got, err := r.SysextURL("kubernetes", "v1.37.1", "amd64")
	if want := "https://mirror.example.com/sysexts/kubernetes-v1.37.1/kubernetes-v1.37.1-x86-64.raw"; err != nil || got != want {
		t.Errorf("SysextURL = %q, %v; want %q", got, err, want)
	}
	sums, _ := r.ChecksumsURL("kubernetes", "v1.37.1")
	if want := "https://mirror.example.com/sysexts/kubernetes-v1.37.1/SHA256SUMS"; sums != want {
		t.Errorf("ChecksumsURL = %q, want %q", sums, want)
	}
	got, err = r.FlatcarURL("stable", "amd64", "4757.2.3", artifacts.FlatcarVersionFile)
	if want := "https://mirror.example.com/flatcar/amd64-usr/4757.2.3/version.txt"; err != nil || got != want {
		t.Errorf("FlatcarURL = %q, %v; want %q", got, err, want)
	}
}

func TestFlatcarURL(t *testing.T) {
	r := artifacts.New(artifacts.Config{})
	got, err := r.FlatcarURL("lts", "arm64", "4757.2.3", artifacts.FlatcarImage)
	want := "https://lts.release.flatcar-linux.net/arm64-usr/4757.2.3/flatcar_production_qemu_uefi_image.img.bz2"
	if err != nil || got != want {
		t.Errorf("FlatcarURL = %q, %v; want %q", got, err, want)
	}
	for _, tc := range []struct{ channel, arch, version, file string }{
		{"nightly", "amd64", "4757.2.3", "version.txt"},
		{"stable", "s390x", "4757.2.3", "version.txt"},
		{"stable", "amd64", "4757.2", "version.txt"},
		{"stable", "amd64", "4757.2.3", "../etc/passwd"},
	} {
		if _, err := r.FlatcarURL(tc.channel, tc.arch, tc.version, tc.file); err == nil {
			t.Errorf("FlatcarURL(%+v) succeeded", tc)
		}
	}
}

func TestResolveFromRealFixtures(t *testing.T) {
	srv, counts := server(t)
	r := artifacts.New(artifacts.Config{SysextBase: srv.URL})
	ctx := context.Background()
	for _, tc := range []struct {
		name, version, arch, file, sum string
	}{
		{"kubernetes", "v1.37.1", "amd64", "kubernetes-v1.37.1-x86-64.raw", k8sAMD64},
		{"kubernetes", "v1.37.1", "arm64", "kubernetes-v1.37.1-arm64.raw", k8sARM64},
		{"tailscale", "v1.98.10", "amd64", "tailscale-v1.98.10-x86-64.raw", tsAMD64},
		{"tailscale", "v1.98.10", "arm64", "tailscale-v1.98.10-arm64.raw", tsARM64},
	} {
		got, err := r.Sysext(ctx, tc.name, tc.version, tc.arch)
		if err != nil {
			t.Fatalf("Sysext(%s %s %s): %v", tc.name, tc.version, tc.arch, err)
		}
		if want := srv.URL + "/" + tc.name + "-" + tc.version + "/" + tc.file; got.URL != want || got.SHA256 != tc.sum {
			t.Errorf("Sysext(%s %s %s) = %+v, want %s %s", tc.name, tc.version, tc.arch, got, want, tc.sum)
		}
	}
	for path, n := range *counts {
		if n.Load() != 1 {
			t.Errorf("%s fetched %d times, want once (cached by URL)", path, n.Load())
		}
	}
}

func TestCacheExpiryAndErrors(t *testing.T) {
	srv, counts := server(t)
	clk := clocktesting.NewFakeClock(time.Now())
	r := artifacts.New(artifacts.Config{SysextBase: srv.URL, Clock: clk, TTL: time.Hour, ErrorTTL: time.Minute})
	ctx := context.Background()

	fetch := func() error { _, err := r.Sysext(ctx, "kubernetes", "v1.37.1", "amd64"); return err }
	if err := fetch(); err != nil {
		t.Fatal(err)
	}
	clk.Step(59 * time.Minute)
	if err := fetch(); err != nil {
		t.Fatal(err)
	}
	if n := (*counts)["/kubernetes-v1.37.1/SHA256SUMS"].Load(); n != 1 {
		t.Errorf("fetched %d times within the TTL, want 1", n)
	}
	clk.Step(2 * time.Minute)
	if err := fetch(); err != nil {
		t.Fatal(err)
	}
	if n := (*counts)["/kubernetes-v1.37.1/SHA256SUMS"].Load(); n != 2 {
		t.Errorf("fetched %d times after the TTL, want 2", n)
	}

	// A release that is not published is ErrNotFound, remembered briefly.
	_, err := r.Sysext(ctx, "kubernetes", "v9.9.9", "amd64")
	if !errors.Is(err, artifacts.ErrNotFound) {
		t.Fatalf("missing release error = %v, want ErrNotFound", err)
	}
	if _, err2 := r.Sysext(ctx, "kubernetes", "v9.9.9", "amd64"); !errors.Is(err2, artifacts.ErrNotFound) {
		t.Fatalf("cached error = %v", err2)
	}
}

func TestServerErrorAndRetryAfterErrorTTL(t *testing.T) {
	var fail atomic.Bool
	var hits atomic.Int32
	fail.Store(true)
	fs := http.FileServer(http.Dir("testdata"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if fail.Load() {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		fs.ServeHTTP(w, r)
	}))
	defer srv.Close()
	clk := clocktesting.NewFakeClock(time.Now())
	r := artifacts.New(artifacts.Config{SysextBase: srv.URL, Clock: clk})
	ctx := context.Background()
	_, err := r.Sysext(ctx, "kubernetes", "v1.37.1", "amd64")
	if err == nil || errors.Is(err, artifacts.ErrNotFound) || !strings.Contains(err.Error(), "502") {
		t.Fatalf("error = %v, want a 502 that is not ErrNotFound", err)
	}
	_, _ = r.Sysext(ctx, "kubernetes", "v1.37.1", "amd64")
	if hits.Load() != 1 {
		t.Errorf("hits = %d, want 1: the failure is cached for the error TTL", hits.Load())
	}
	fail.Store(false)
	clk.Step(artifacts.DefaultErrorTTL + time.Second)
	if _, err := r.Sysext(ctx, "kubernetes", "v1.37.1", "amd64"); err != nil {
		t.Fatalf("after the error TTL: %v", err)
	}
}

func TestChecksumMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(k8sAMD64 + "  kubernetes-v1.37.1-x86-64.raw\n"))
	}))
	defer srv.Close()
	r := artifacts.New(artifacts.Config{SysextBase: srv.URL})
	_, err := r.Sysext(context.Background(), "kubernetes", "v1.37.1", "arm64")
	if !errors.Is(err, artifacts.ErrChecksumMissing) {
		t.Fatalf("error = %v, want ErrChecksumMissing", err)
	}
}

func TestContextCancelIsNotCached(t *testing.T) {
	srv, _ := server(t)
	r := artifacts.New(artifacts.Config{SysextBase: srv.URL})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Sysext(ctx, "kubernetes", "v1.37.1", "amd64"); err == nil {
		t.Fatal("a cancelled context succeeded")
	}
	if _, err := r.Sysext(context.Background(), "kubernetes", "v1.37.1", "amd64"); err != nil {
		t.Fatalf("the cancellation was cached: %v", err)
	}
}

func TestParseSHA256SUMS(t *testing.T) {
	good := k8sAMD64 + "  a.raw\n\n# comment\n" + strings.ToUpper(k8sARM64) + " *b.raw\n"
	got, err := artifacts.ParseSHA256SUMS(strings.NewReader(good))
	if err != nil || len(got) != 2 || got["a.raw"] != k8sAMD64 || got["b.raw"] != k8sARM64 {
		t.Fatalf("ParseSHA256SUMS = %v, %v", got, err)
	}
	for name, in := range map[string]string{
		"empty":     "",
		"short sum": "abc  a.raw\n",
		"no file":   k8sAMD64 + "\n",
		"conflict":  k8sAMD64 + "  a.raw\n" + k8sARM64 + "  a.raw\n",
		"not hex":   strings.Repeat("g", 64) + "  a.raw\n",
	} {
		if _, err := artifacts.ParseSHA256SUMS(strings.NewReader(in)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}
