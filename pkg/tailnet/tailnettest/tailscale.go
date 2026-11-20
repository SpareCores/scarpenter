/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package tailnettest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TailscaleKey is a key of the fake Tailscale tailnet.
type TailscaleKey struct {
	ID            string
	Secret        string
	Tags          []string
	Reusable      bool
	Ephemeral     bool
	Preauthorized bool
	Description   string
	Expires       time.Time
}

// TailscaleDevice is a device of the fake Tailscale tailnet.
type TailscaleDevice struct {
	NodeID    string
	Hostname  string
	Addresses []string
	Tags      []string
	Ephemeral bool
	Created   time.Time
}

// Tailscale fakes https://api.tailscale.com: the OAuth token endpoint, auth keys and
// devices of the tailnet "-". A single-use key is revoked (removed) when a device joins
// with it, as Tailscale does.
type Tailscale struct {
	server
	clientID, clientSecret string
	tokenTTL               time.Duration
	tokens                 map[string]time.Time
	keys                   []*TailscaleKey
	devices                []*TailscaleDevice
}

// NewTailscale starts the fake with the OAuth client it accepts.
func NewTailscale(t testing.TB, clientID, clientSecret string) *Tailscale {
	f := &Tailscale{clientID: clientID, clientSecret: clientSecret, tokenTTL: time.Hour, tokens: map[string]time.Time{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v2/oauth/token", f.handleToken)
	mux.HandleFunc("POST /api/v2/tailnet/{tailnet}/keys", f.authed(f.handleCreateKey))
	mux.HandleFunc("DELETE /api/v2/tailnet/{tailnet}/keys/{id}", f.authed(f.handleDeleteKey))
	mux.HandleFunc("GET /api/v2/tailnet/{tailnet}/devices", f.authed(f.handleListDevices))
	mux.HandleFunc("DELETE /api/v2/device/{id}", f.authed(f.handleDeleteDevice))
	newServer(t, mux, &f.server)
	return f
}

// SetTokenTTL sets the lifetime of the tokens handed out from now on.
func (f *Tailscale) SetTokenTTL(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenTTL = d
}

// RevokeTokens makes every token handed out so far invalid.
func (f *Tailscale) RevokeTokens() {
	f.mu.Lock()
	defer f.mu.Unlock()
	clear(f.tokens)
}

// Keys returns the keys that exist (not revoked, not used up).
func (f *Tailscale) Keys() []TailscaleKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]TailscaleKey, len(f.keys))
	for i, k := range f.keys {
		out[i] = *k
	}
	return out
}

// Devices returns the devices of the tailnet.
func (f *Tailscale) Devices() []TailscaleDevice {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]TailscaleDevice, len(f.devices))
	for i, d := range f.devices {
		out[i] = *d
	}
	return out
}

// Join registers a device with the key. A key that does not exist (revoked, used up or
// never minted) or has expired is refused; a single-use key is consumed.
func (f *Tailscale) Join(secret, hostname string) (TailscaleDevice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.IndexFunc(f.keys, func(k *TailscaleKey) bool { return k.Secret == secret })
	if i < 0 {
		return TailscaleDevice{}, &JoinError{Reason: "invalid key"}
	}
	k := f.keys[i]
	if !f.now().Before(k.Expires) {
		return TailscaleDevice{}, &JoinError{Reason: "key expired"}
	}
	n := f.next()
	v4, v6 := addr(n)
	d := &TailscaleDevice{
		NodeID: "n" + strconv.Itoa(n) + "CNTRL", Hostname: hostname, Addresses: []string{v4, v6},
		Tags: k.Tags, Ephemeral: k.Ephemeral, Created: f.now().UTC(),
	}
	f.devices = append(f.devices, d)
	if !k.Reusable {
		f.keys = slices.Delete(f.keys, i, i+1)
	}
	return *d, nil
}

func apiError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg})
}

func (f *Tailscale) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.ParseForm() != nil || r.PostForm.Get("grant_type") != "client_credentials" ||
		r.PostForm.Get("client_id") != f.clientID || r.PostForm.Get("client_secret") != f.clientSecret {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "invalid_client", "error_description": "client authentication failed",
		})
		return
	}
	tok := fmt.Sprintf("tskey-api-ktest%d-token", f.next())
	f.tokens[tok] = f.now().Add(f.tokenTTL)
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": tok, "token_type": "Bearer", "expires_in": int(f.tokenTTL / time.Second),
		"scope": "auth_keys devices:core",
	})
}

func (f *Tailscale) authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		exp, ok := f.tokens[bearer(r)]
		if !ok || !f.now().Before(exp) {
			apiError(w, http.StatusUnauthorized, "API token invalid")
			return
		}
		h(w, r)
	}
}

func (f *Tailscale) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Capabilities struct {
			Devices struct {
				Create struct {
					Reusable      bool     `json:"reusable"`
					Ephemeral     bool     `json:"ephemeral"`
					Preauthorized bool     `json:"preauthorized"`
					Tags          []string `json:"tags"`
				} `json:"create"`
			} `json:"devices"`
		} `json:"capabilities"`
		ExpirySeconds int64  `json:"expirySeconds"`
		Description   string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	c := req.Capabilities.Devices.Create
	if len(c.Tags) == 0 {
		apiError(w, http.StatusBadRequest, "requested tags are required for keys created by an OAuth client")
		return
	}
	for _, tag := range c.Tags {
		if !validTag(tag) {
			apiError(w, http.StatusBadRequest, fmt.Sprintf("tag %q is invalid", tag))
			return
		}
	}
	// "A maximum of 50 alphanumeric characters. Hyphens and spaces are also allowed."
	if len(req.Description) > 50 || strings.ContainsFunc(req.Description, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != ' '
	}) {
		apiError(w, http.StatusBadRequest, "invalid description")
		return
	}
	if req.ExpirySeconds <= 0 {
		req.ExpirySeconds = 90 * 24 * 3600
	}
	n := f.next()
	k := &TailscaleKey{
		ID: fmt.Sprintf("k%dCNTRL", n), Secret: fmt.Sprintf("tskey-auth-k%dCNTRL-%032x", n, n),
		Tags: c.Tags, Reusable: c.Reusable, Ephemeral: c.Ephemeral, Preauthorized: c.Preauthorized,
		Description: req.Description, Expires: f.now().Add(time.Duration(req.ExpirySeconds) * time.Second).UTC(),
	}
	f.keys = append(f.keys, k)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": k.ID, "key": k.Secret, "created": f.now().UTC(), "expires": k.Expires,
		"capabilities": map[string]any{"devices": map[string]any{"create": map[string]any{
			"reusable": k.Reusable, "ephemeral": k.Ephemeral, "preauthorized": k.Preauthorized, "tags": k.Tags,
		}}},
		"description": k.Description,
	})
}

func (f *Tailscale) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	i := slices.IndexFunc(f.keys, func(k *TailscaleKey) bool { return k.ID == r.PathValue("id") })
	if i < 0 {
		apiError(w, http.StatusNotFound, "key not found")
		return
	}
	f.keys = slices.Delete(f.keys, i, i+1)
	w.WriteHeader(http.StatusOK)
}

func (f *Tailscale) handleListDevices(w http.ResponseWriter, _ *http.Request) {
	// Every tailnet lists Tailscale's shared-in "hello" device: external, with an empty
	// created date.
	out := make([]map[string]any, 0, 1+len(f.devices))
	out = append(out, map[string]any{
		"id": "50052", "nodeId": "nHELLO0CNTRL", "hostname": "hello", "name": "hello.ts.net",
		"addresses": []string{"100.101.102.103"}, "tags": nil, "created": "", "authorized": true,
		"isExternal": true,
	})
	for _, d := range f.devices {
		out = append(out, map[string]any{
			"id": d.NodeID[1:len(d.NodeID)-5] + "0000", "nodeId": d.NodeID,
			"hostname": d.Hostname, "name": d.Hostname + ".tail-test.ts.net",
			"addresses": d.Addresses, "tags": d.Tags, "created": d.Created, "authorized": true,
			"isEphemeral": d.Ephemeral, "keyExpiryDisabled": len(d.Tags) > 0,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": out})
}

func (f *Tailscale) handleDeleteDevice(w http.ResponseWriter, r *http.Request) {
	i := slices.IndexFunc(f.devices, func(d *TailscaleDevice) bool { return d.NodeID == r.PathValue("id") })
	if i < 0 {
		apiError(w, http.StatusNotFound, "device not found")
		return
	}
	f.devices = slices.Delete(f.devices, i, i+1)
	w.WriteHeader(http.StatusOK)
}
