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
    enabled: false
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
  nameservers:
    global: []
unix_socket: %[1]s/headscale.sock
`, dir, url, port, freePort(t), freePort(t)), 0o600))

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

	listed := func() map[string]any {
		out, err := exec.Command(bin, "-c", cfg, "preauthkeys", "list", "-o", "json").Output()
		require.NoError(t, err)
		var keys []map[string]any
		require.NoError(t, json.Unmarshal(out, &keys))
		for _, key := range keys {
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
	require.Equal(t, []any{testTags[0]}, key["acl_tags"])

	require.NoError(t, b.RevokeKey(t.Context(), k.ID))
	exp, err := time.Parse(time.RFC3339, fmt.Sprint(listed()["expiration"]))
	require.NoError(t, err)
	require.WithinDuration(t, time.Now(), exp, time.Minute, "expire sets the expiration to now")
	require.NoError(t, b.RevokeKey(t.Context(), "99999"), "an unknown key is success")

	_, err = b.FindDevice(t.Context(), tailnet.DeviceQuery{KeyIDs: []string{k.ID}})
	requireClass(t, err, tailnet.NotFound)
	require.NoError(t, b.DeleteDevice(t.Context(), "99999"), "an unknown node is success")
}
