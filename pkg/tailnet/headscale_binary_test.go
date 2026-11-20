/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package tailnet_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/SpareCores/scarpenter/pkg/tailnet"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// TestHeadscaleBinary runs the Headscale backend against a real Headscale (0.28 or newer)
// when HEADSCALE_BIN names its binary (the L4 lane caches the pinned one); it is skipped
// otherwise. It settles that the server accepts a key without a user and with tags over the
// REST API, the JSON names the backend reads, and that expire and delete answer as the
// backend assumes. Joining a node needs a Tailscale client and is the L4 lane's job.
func TestHeadscaleBinary(t *testing.T) {
	bin := os.Getenv("HEADSCALE_BIN")
	if bin == "" {
		t.Skip("HEADSCALE_BIN is not set")
	}
	dir := t.TempDir()
	port := freePort(t)
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	cfg := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfg, fmt.Appendf(nil, `
server_url: %[2]s
listen_addr: 127.0.0.1:%[3]d
metrics_listen_addr: 127.0.0.1:%[4]d
grpc_listen_addr: 127.0.0.1:%[5]d
grpc_allow_insecure: true
noise:
  private_key_path: %[1]s/noise_private.key
prefixes:
  v4: 100.64.0.0/10
  v6: fd7a:115c:a1e0::/48
derp:
  server:
    enabled: true
    region_id: 999
    region_code: test
    region_name: test
    private_key_path: %[1]s/derp_private.key
    stun_listen_addr: 127.0.0.1:%[6]d
    verify_clients: false
    ipv4: 127.0.0.1
  urls: []
  paths: []
  auto_update_enabled: false
database:
  type: sqlite
  sqlite:
    path: %[1]s/db.sqlite
dns:
  magic_dns: false
  base_domain: example.com
  override_local_dns: false
  nameservers:
    global: []
unix_socket: %[1]s/headscale.sock
`, dir, url, port, freePort(t), freePort(t), freePort(t)), 0o600))

	ctx, cancel := context.WithCancel(t.Context())
	srv := exec.CommandContext(ctx, bin, "serve", "-c", cfg)
	var srvLog bytes.Buffer
	srv.Stdout, srv.Stderr = &srvLog, &srvLog
	require.NoError(t, srv.Start())
	t.Cleanup(func() {
		cancel()
		_ = srv.Wait()
		if t.Failed() {
			t.Log(srvLog.String())
		}
	})
	require.Eventually(t, func() bool {
		resp, err := http.Get(url + "/health") //nolint:noctx // local readiness probe
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 30*time.Second, 200*time.Millisecond, "headscale did not start")

	out, err := exec.Command(bin, "-c", cfg, "apikeys", "create", "--expiration", "1h").Output()
	require.NoError(t, err)
	apiKey := strings.TrimSpace(string(out))

	b, err := tailnet.NewHeadscale(tailnet.HeadscaleConfig{
		Config: tailnet.Config{BaseURL: url},
		APIKey: func() (tailnet.Secret, error) { return tailnet.Secret(apiKey), nil },
	})
	require.NoError(t, err)

	k, err := b.MintKey(t.Context(), tailnet.KeyRequest{Tags: testTags, Expiry: time.Hour})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(k.Secret.Reveal(), "hskey-auth-"))
	require.WithinDuration(t, time.Now().Add(time.Hour), k.Expires, time.Minute)

	// The REST listing, so that the gateway's JSON names are checked too.
	listed := func() map[string]any {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url+"/api/v1/preauthkey", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var doc struct {
			PreAuthKeys []map[string]any `json:"preAuthKeys"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&doc))
		for _, key := range doc.PreAuthKeys {
			if fmt.Sprint(key["id"]) == k.ID {
				return key
			}
		}
		t.Fatalf("key %s is not listed", k.ID)
		return nil
	}
	key := listed()
	require.Equal(t, true, key["ephemeral"])
	require.Equal(t, false, key["reusable"])
	require.Equal(t, []any{testTags[0]}, key["aclTags"])
	require.Nil(t, key["user"], "a tagged key has no user")

	require.NoError(t, b.RevokeKey(t.Context(), k.ID))
	exp, err := time.Parse(time.RFC3339, fmt.Sprint(listed()["expiration"]))
	require.NoError(t, err)
	require.WithinDuration(t, time.Now(), exp, time.Minute, "expire sets the expiration to now")
	require.NoError(t, b.RevokeKey(t.Context(), "99999"), "an unknown key is success")

	_, err = b.FindDevice(t.Context(), tailnet.DeviceQuery{KeyIDs: []string{k.ID}})
	requireClass(t, err, tailnet.NotFound)
	require.NoError(t, b.DeleteDevice(t.Context(), "99999"), "an unknown node is success")

	headscaleJoin(t, b, url)
}

// headscaleJoin joins a userspace tailscaled with a minted key when TAILSCALE_DIR names a
// directory with the tailscale and tailscaled binaries (the L4 lane caches them), and checks
// that FindDevice finds the node by its key and DeleteDevice removes it.
func headscaleJoin(t *testing.T, b *tailnet.Headscale, url string) {
	t.Helper()
	tsDir := os.Getenv("TAILSCALE_DIR")
	if tsDir == "" {
		t.Log("TAILSCALE_DIR is not set: no node joins")
		return
	}
	k, err := b.MintKey(t.Context(), tailnet.KeyRequest{Tags: testTags, Expiry: time.Hour})
	require.NoError(t, err)

	dir := t.TempDir()
	sock := filepath.Join(dir, "tailscaled.sock")
	ctx, cancel := context.WithCancel(t.Context())
	tsd := exec.CommandContext(ctx, filepath.Join(tsDir, "tailscaled"),
		"--tun=userspace-networking", "--state=mem:", "--statedir="+dir, "--socket="+sock, "--port=0")
	var tsdLog bytes.Buffer
	tsd.Stdout, tsd.Stderr = &tsdLog, &tsdLog
	require.NoError(t, tsd.Start())
	t.Cleanup(func() {
		cancel()
		_ = tsd.Wait()
		if t.Failed() {
			t.Log(tsdLog.String())
		}
	})
	start := time.Now().Add(-time.Minute)
	up := exec.CommandContext(t.Context(), filepath.Join(tsDir, "tailscale"), "--socket="+sock, "up",
		"--login-server="+url, "--auth-key="+k.Secret.Reveal(), "--hostname=hs-join-test", "--timeout=60s")
	out, err := up.CombinedOutput()
	require.NoError(t, err, "tailscale up: %s", out)

	d, err := b.FindDevice(t.Context(), tailnet.DeviceQuery{KeyIDs: []string{k.ID}})
	require.NoError(t, err)
	require.Equal(t, "hs-join-test", d.Hostname)
	require.Equal(t, testTags, d.Tags)
	require.NotEmpty(t, d.Addresses)
	require.True(t, d.Created.After(start), "created %s", d.Created)

	byIP, err := b.FindDevice(t.Context(), tailnet.DeviceQuery{IP: d.Addresses[0], Hostname: "hs-join-test"})
	require.NoError(t, err)
	require.Equal(t, d.ID, byIP.ID)

	require.NoError(t, b.DeleteDevice(t.Context(), d.ID))
	_, err = b.FindDevice(t.Context(), tailnet.DeviceQuery{KeyIDs: []string{k.ID}})
	requireClass(t, err, tailnet.NotFound)
}
