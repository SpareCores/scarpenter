/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package tailnet_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/SpareCores/scarpenter/pkg/tailnet"
)

func TestMintKey(t *testing.T) {
	eachBackend(t, tailnet.Config{}, func(t *testing.T, r *rig) {
		k := mint(t, r)
		require.NotEmpty(t, k.ID)
		require.NotEmpty(t, k.Secret.Reveal())
		require.WithinDuration(t, r.now().Add(time.Hour), k.Expires, 2*time.Second)

		tags, ephemeral, reusable := r.keyProps(k.ID)
		require.Equal(t, testTags, tags)
		require.True(t, ephemeral)
		require.False(t, reusable)
		require.Equal(t, []string{k.ID}, r.validKeys())
	})
}

func TestMintKeyRejectsBadRequestsLocally(t *testing.T) {
	eachBackend(t, tailnet.Config{}, func(t *testing.T, r *rig) {
		for name, req := range map[string]tailnet.KeyRequest{
			"no tags":    {Expiry: time.Hour},
			"no prefix":  {Tags: []string{"bench"}, Expiry: time.Hour},
			"upper case": {Tags: []string{"tag:Bench"}, Expiry: time.Hour},
			"no expiry":  {Tags: testTags},
		} {
			_, err := r.backend.MintKey(t.Context(), req)
			requireClass(t, err, tailnet.Fatal)
			require.Zero(t, r.calls(), name)
		}
	})
}

func TestKeyIsSingleUse(t *testing.T) {
	eachBackend(t, tailnet.Config{}, func(t *testing.T, r *rig) {
		k := mint(t, r)
		_, _, err := r.join(k.Secret.Reveal(), "node-a")
		require.NoError(t, err)
		_, _, err = r.join(k.Secret.Reveal(), "node-b")
		require.Error(t, err)
		require.Len(t, r.deviceIDs(), 1)
	})
}

func TestKeyExpires(t *testing.T) {
	eachBackend(t, tailnet.Config{}, func(t *testing.T, r *rig) {
		k := mint(t, r)
		r.clock.Advance(time.Hour + time.Second)
		_, _, err := r.join(k.Secret.Reveal(), "node-a")
		require.Error(t, err)
	})
}

func TestRevokeKey(t *testing.T) {
	eachBackend(t, tailnet.Config{}, func(t *testing.T, r *rig) {
		k := mint(t, r)
		require.NoError(t, r.backend.RevokeKey(t.Context(), k.ID))
		require.Empty(t, r.validKeys())
		_, _, err := r.join(k.Secret.Reveal(), "late")
		require.Error(t, err, "a revoked key admits nothing")

		// Idempotent: again, a key already used, a key that never existed.
		require.NoError(t, r.backend.RevokeKey(t.Context(), k.ID))
		used := mint(t, r)
		_, _, err = r.join(used.Secret.Reveal(), "node-a")
		require.NoError(t, err)
		require.NoError(t, r.backend.RevokeKey(t.Context(), used.ID))
		require.NoError(t, r.backend.RevokeKey(t.Context(), "9999"))
	})
}

func joinNode(t *testing.T, r *rig, host string) (string, netip.Addr, tailnet.Key) {
	t.Helper()
	k := mint(t, r)
	id, ip, err := r.join(k.Secret.Reveal(), host)
	require.NoError(t, err)
	return id, ip, k
}

func TestFindDevice(t *testing.T) {
	eachBackend(t, tailnet.Config{}, func(t *testing.T, r *rig) {
		before := r.now().Add(-time.Minute)
		idA, ipA, keyA := joinNode(t, r, "nc-a")
		idB, ipB, keyB := joinNode(t, r, "nc-b")

		d, err := r.backend.FindDevice(t.Context(), tailnet.DeviceQuery{IP: ipA, Hostname: "nc-a"})
		require.NoError(t, err)
		require.Equal(t, idA, d.ID)
		require.Equal(t, "nc-a", d.Hostname)
		require.Equal(t, testTags, d.Tags)
		require.Contains(t, d.Addresses, ipA)

		d, err = r.backend.FindDevice(t.Context(), tailnet.DeviceQuery{Hostname: "nc-b", Tags: testTags, CreatedAfter: before})
		require.NoError(t, err)
		require.Equal(t, idB, d.ID)
		require.Contains(t, d.Addresses, ipB)

		// The IP and the hostname must agree.
		_, err = r.backend.FindDevice(t.Context(), tailnet.DeviceQuery{IP: ipA, Hostname: "nc-b"})
		requireClass(t, err, tailnet.NotFound)
		_, err = r.backend.FindDevice(t.Context(), tailnet.DeviceQuery{Hostname: "nc-a", Tags: []string{"tag:other"}})
		requireClass(t, err, tailnet.NotFound)
		_, err = r.backend.FindDevice(t.Context(), tailnet.DeviceQuery{Hostname: "nc-a", CreatedAfter: r.now().Add(time.Hour)})
		requireClass(t, err, tailnet.NotFound)

		_, err = r.backend.FindDevice(t.Context(), tailnet.DeviceQuery{})
		requireClass(t, err, tailnet.Fatal)

		if r.byKey {
			d, err = r.backend.FindDevice(t.Context(), tailnet.DeviceQuery{KeyIDs: []string{"9999", keyB.ID}})
			require.NoError(t, err)
			require.Equal(t, idB, d.ID)
			// An expired key still names the node it admitted.
			require.NoError(t, r.backend.RevokeKey(t.Context(), keyA.ID))
			d, err = r.backend.FindDevice(t.Context(), tailnet.DeviceQuery{KeyIDs: []string{keyA.ID}})
			require.NoError(t, err)
			require.Equal(t, idA, d.ID)
		} else {
			_, err = r.backend.FindDevice(t.Context(), tailnet.DeviceQuery{KeyIDs: []string{keyA.ID}})
			requireClass(t, err, tailnet.Fatal)
		}
	})
}

func TestFindDeviceAmbiguous(t *testing.T) {
	eachBackend(t, tailnet.Config{}, func(t *testing.T, r *rig) {
		joinNode(t, r, "same-name")
		joinNode(t, r, "same-name")
		_, err := r.backend.FindDevice(t.Context(), tailnet.DeviceQuery{Hostname: "same-name", Tags: testTags})
		requireClass(t, err, tailnet.Ambiguous)
		require.Len(t, r.deviceIDs(), 2, "nothing is deleted by a find")
	})
}

func TestDeleteDevice(t *testing.T) {
	eachBackend(t, tailnet.Config{}, func(t *testing.T, r *rig) {
		id, _, _ := joinNode(t, r, "nc-a")
		require.NoError(t, r.backend.DeleteDevice(t.Context(), id))
		require.Empty(t, r.deviceIDs())
		require.NoError(t, r.backend.DeleteDevice(t.Context(), id), "gone is success")
		require.Equal(t, 2, r.count("DELETE", r.deletePath(id)))
	})
}

func TestRetriesServerErrors(t *testing.T) {
	eachBackend(t, tailnet.Config{MaxAttempts: 3, Backoff: time.Second}, func(t *testing.T, r *rig) {
		r.failNext(2, 503, "")
		k := mint(t, r)
		require.NotEmpty(t, k.ID)
		sleeps := r.clock.Sleeps()
		require.Len(t, sleeps, 2)
		require.GreaterOrEqual(t, sleeps[0], 500*time.Millisecond)
		require.GreaterOrEqual(t, sleeps[1], time.Second)

		// Exhausted: Retryable, no more than MaxAttempts requests.
		before := r.count("POST", r.mintPath)
		r.failNext(3, 500, "")
		_, err := r.backend.MintKey(t.Context(), tailnet.KeyRequest{Tags: testTags, Expiry: time.Hour})
		e := requireError(t, err, tailnet.Retryable)
		require.Equal(t, 500, e.Status)
		require.Equal(t, before+3, r.count("POST", r.mintPath))
	})
}

func TestRateLimitWithShortRetryAfterIsWaitedOut(t *testing.T) {
	eachBackend(t, tailnet.Config{}, func(t *testing.T, r *rig) {
		r.failNext(1, 429, "2")
		k := mint(t, r)
		require.NotEmpty(t, k.ID)
		require.Equal(t, []time.Duration{2 * time.Second}, r.clock.Sleeps())
	})
}

func TestRateLimitWithoutRetryAfterBacksOffThenGates(t *testing.T) {
	eachBackend(t, tailnet.Config{}, func(t *testing.T, r *rig) {
		r.failNext(1, 429, "")
		k := mint(t, r)
		require.NotEmpty(t, k.ID)
		require.Len(t, r.clock.Sleeps(), 1)

		// The default 10 s gate follows a 429: the next call does not reach the API.
		n := r.calls()
		_, err := r.backend.MintKey(t.Context(), tailnet.KeyRequest{Tags: testTags, Expiry: time.Hour})
		e := requireError(t, err, tailnet.Retryable)
		require.Equal(t, 429, e.Status)
		require.Positive(t, e.RetryAfter)
		require.Equal(t, n, r.calls())

		r.clock.Advance(11 * time.Second)
		mint(t, r)
	})
}

func TestRateLimitWithLongRetryAfterFailsFast(t *testing.T) {
	eachBackend(t, tailnet.Config{}, func(t *testing.T, r *rig) {
		r.failNext(1, 429, "30")
		_, err := r.backend.MintKey(t.Context(), tailnet.KeyRequest{Tags: testTags, Expiry: time.Hour})
		e := requireError(t, err, tailnet.Retryable)
		require.Equal(t, 30*time.Second, e.RetryAfter)
		require.Equal(t, 30*time.Second, tailnet.RetryAfterOf(err))
		require.Empty(t, r.clock.Sleeps(), "the caller requeues; the call does not sleep")

		// Every call fails fast, without a request, until Retry-After has passed.
		n := r.calls()
		require.Equal(t, tailnet.Retryable, tailnet.ClassOf(r.backend.RevokeKey(t.Context(), "1")))
		_, err = r.backend.FindDevice(t.Context(), tailnet.DeviceQuery{Hostname: "x"})
		requireClass(t, err, tailnet.Retryable)
		require.Equal(t, n, r.calls())

		r.clock.Advance(31 * time.Second)
		mint(t, r)
	})
}

func TestDeadlineOnEveryAttempt(t *testing.T) {
	eachBackend(t, tailnet.Config{CallTimeout: 50 * time.Millisecond, MaxAttempts: 2}, func(t *testing.T, r *rig) {
		r.setDelay(500 * time.Millisecond)
		start := time.Now()
		_, err := r.backend.MintKey(t.Context(), tailnet.KeyRequest{Tags: testTags, Expiry: time.Hour})
		requireClass(t, err, tailnet.Retryable)
		require.Less(t, time.Since(start), 2*time.Second)
	})
}

func TestCallerContextIsHonoured(t *testing.T) {
	eachBackend(t, tailnet.Config{}, func(t *testing.T, r *rig) {
		r.setDelay(time.Second)
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := r.backend.MintKey(ctx, tailnet.KeyRequest{Tags: testTags, Expiry: time.Hour})
		require.Error(t, err)
		require.Less(t, time.Since(start), 900*time.Millisecond)
	})
}

func TestBadCredentialsAreFatalAndNotLeaked(t *testing.T) {
	t.Run("headscale", func(t *testing.T) {
		r := newHeadscaleRig(t, tailnet.Config{})
		b, err := tailnet.NewHeadscale(tailnet.HeadscaleConfig{
			Config: tailnet.Config{BaseURL: r.url},
			APIKey: func() (tailnet.Secret, error) { return "wrong-api-key-value", nil },
		})
		require.NoError(t, err)
		_, err = b.MintKey(t.Context(), tailnet.KeyRequest{Tags: testTags, Expiry: time.Hour})
		e := requireError(t, err, tailnet.Fatal)
		require.Equal(t, 401, e.Status)
		require.NotContains(t, err.Error(), "wrong-api-key-value")
	})
	t.Run("tailscale", func(t *testing.T) {
		r := newTailscaleRig(t, tailnet.Config{})
		b, err := tailnet.NewTailscale(tailnet.TailscaleConfig{
			Config: tailnet.Config{BaseURL: r.url},
			Credentials: func() (string, tailnet.Secret, error) {
				return testClientID, "wrong-client-secret", nil
			},
		})
		require.NoError(t, err)
		_, err = b.MintKey(t.Context(), tailnet.KeyRequest{Tags: testTags, Expiry: time.Hour})
		e := requireError(t, err, tailnet.Fatal)
		require.Equal(t, 401, e.Status)
		require.Contains(t, err.Error(), "client authentication failed")
		require.NotContains(t, err.Error(), "wrong-client-secret")
	})
}

func TestTailscaleTokenIsCachedAndRefreshedOn401(t *testing.T) {
	r := newTailscaleRig(t, tailnet.Config{})
	mint(t, r)
	mint(t, r)
	require.Equal(t, 1, r.count("POST", "/api/v2/oauth/token"), "one token serves both calls")

	// The server forgets the token (revoked, rotated): one 401, a new token, the call succeeds.
	r.tsFake.RevokeTokens()
	mint(t, r)
	require.Equal(t, 2, r.count("POST", "/api/v2/oauth/token"))
}

func TestTailscaleTokenExpiryRefreshes(t *testing.T) {
	r := newTailscaleRig(t, tailnet.Config{})
	mint(t, r)
	r.clock.Advance(59*time.Minute + 30*time.Second) // inside the refresh skew
	mint(t, r)
	require.Equal(t, 2, r.count("POST", "/api/v2/oauth/token"))
}

func TestTailscaleDescriptionIsShortened(t *testing.T) {
	r := newTailscaleRig(t, tailnet.Config{})
	_, err := r.backend.MintKey(t.Context(), tailnet.KeyRequest{
		Tags: testTags, Expiry: time.Hour, Description: strings.Repeat("d", 120),
	})
	require.NoError(t, err)
}

func TestHeadscaleOldServerHint(t *testing.T) {
	// A server before 0.28 refuses a key without a user.
	r := newHeadscaleRig(t, tailnet.Config{})
	r.failNext(1, 400, "")
	_, err := r.backend.MintKey(t.Context(), tailnet.KeyRequest{Tags: testTags, Expiry: time.Hour})
	e := requireError(t, err, tailnet.Fatal)
	require.Contains(t, e.Error(), "0.28")
}

func TestMalformedResponseQuotesNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"preAuthKey":{"id":"1","key":"hskey-auth-LEAKED-xyz",`))
	}))
	defer srv.Close()
	b, err := tailnet.NewHeadscale(tailnet.HeadscaleConfig{
		Config: tailnet.Config{BaseURL: srv.URL},
		APIKey: func() (tailnet.Secret, error) { return testAPIKey, nil },
	})
	require.NoError(t, err)
	_, err = b.MintKey(t.Context(), tailnet.KeyRequest{Tags: testTags, Expiry: time.Hour})
	requireClass(t, err, tailnet.Fatal)
	require.NotContains(t, err.Error(), "LEAKED")
}

func TestUnknownErrorBodyIsNotCopied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "echo: Bearer "+testAPIKey, http.StatusForbidden)
	}))
	defer srv.Close()
	b, err := tailnet.NewHeadscale(tailnet.HeadscaleConfig{
		Config: tailnet.Config{BaseURL: srv.URL},
		APIKey: func() (tailnet.Secret, error) { return testAPIKey, nil },
	})
	require.NoError(t, err)
	err = b.DeleteDevice(t.Context(), "5")
	e := requireError(t, err, tailnet.Fatal)
	require.Equal(t, 403, e.Status)
	require.NotContains(t, err.Error(), testAPIKey)
}

func TestInvalidConfiguration(t *testing.T) {
	_, err := tailnet.NewHeadscale(tailnet.HeadscaleConfig{Config: tailnet.Config{BaseURL: "headscale.example.com"}, APIKey: nil})
	require.Error(t, err)
	_, err = tailnet.NewHeadscale(tailnet.HeadscaleConfig{
		Config: tailnet.Config{BaseURL: "headscale.example.com"},
		APIKey: func() (tailnet.Secret, error) { return "k", nil },
	})
	require.Error(t, err)
	_, err = tailnet.NewTailscale(tailnet.TailscaleConfig{})
	require.Error(t, err)
}

func TestDirCredentials(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "client-id"), []byte("id\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "client-secret"), []byte("s3cret\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "api-key"), []byte("\n"), 0o600))
	id, secret, err := tailnet.DirTailscaleCredentials(dir)()
	require.NoError(t, err)
	require.Equal(t, "id", id)
	require.Equal(t, "s3cret", secret.Reveal())
	_, err = tailnet.DirHeadscaleAPIKey(dir)()
	require.Error(t, err, "an empty file is an error")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "api-key"), []byte("k"), 0o600))
	key, err := tailnet.DirHeadscaleAPIKey(dir)()
	require.NoError(t, err)
	require.Equal(t, "k", key.Reveal())
	_, _, err = tailnet.DirTailscaleCredentials(t.TempDir())()
	require.Error(t, err)
}

func TestSecretIsRedactedEverywhere(t *testing.T) {
	const value = "tskey-auth-kSECRETVALUE-abcdef"
	s := tailnet.Secret(value)
	k := tailnet.Key{ID: "k1", Secret: s}
	var logs strings.Builder
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("minted", "secret", s, "key", k, "grp", slog.GroupValue(slog.Any("s", s)))
	slog.New(slog.NewTextHandler(&logs, nil)).Info("minted", "secret", s, "key", k)

	out := []string{
		fmt.Sprint(s), fmt.Sprintf("%s %v %+v %#v %q %x %d", s, s, s, s, s, s, s),
		fmt.Sprintf("%v %+v %#v %s", k, k, k, k), fmt.Sprintf("%v", []tailnet.Secret{s}),
		fmt.Sprintf("%v", map[string]tailnet.Secret{"a": s}), fmt.Sprintf("%v", &k), logs.String(),
	}
	for _, o := range out {
		require.NotContains(t, o, "SECRETVALUE")
	}
	require.Contains(t, fmt.Sprint(s), "redacted")
	require.Equal(t, value, s.Reveal())
}

func TestNoKeyInErrorsOrLogs(t *testing.T) {
	// Every error an operation can return, with a key in flight, is free of the secrets.
	eachBackend(t, tailnet.Config{MaxAttempts: 2}, func(t *testing.T, r *rig) {
		k := mint(t, r)
		errs := make([]error, 0, 24)
		for _, status := range []int{400, 401, 403, 404, 409, 429, 500, 503} {
			r.failNext(2, status, "30")
			_, e1 := r.backend.MintKey(t.Context(), tailnet.KeyRequest{Tags: testTags, Expiry: time.Hour})
			r.clock.Advance(time.Minute)
			errs = append(errs, e1, r.backend.RevokeKey(t.Context(), k.ID))
			r.clock.Advance(time.Minute)
			_, e2 := r.backend.FindDevice(t.Context(), tailnet.DeviceQuery{Hostname: "x"})
			errs = append(errs, e2)
			r.clock.Advance(time.Minute)
		}
		for _, err := range errs {
			if err != nil {
				require.NotContains(t, err.Error(), k.Secret.Reveal())
				require.NotContains(t, err.Error(), testAPIKey)
				require.NotContains(t, err.Error(), testClientSecret)
			}
		}
	})
}
