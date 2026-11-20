/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package tailnet

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	opToken = "token"
	// Tailscale limits a key's description; longer text is shortened.
	tailscaleDescriptionMax = 50
	// A token is refreshed this long before it expires.
	tokenSkew = time.Minute
)

// TailscaleConfig configures the Tailscale API backend.
type TailscaleConfig struct {
	Config
	// Tailnet is the tailnet path segment; "-" (the default) is the OAuth client's own.
	Tailnet string
	// Credentials returns the OAuth client's ID and secret. It is called at every token
	// fetch, so a rotated Secret takes effect without a restart.
	Credentials func() (clientID string, clientSecret Secret, err error)
}

// Tailscale is the Tailscale API v2 backend. The OAuth client needs the scopes auth_keys
// (write) and devices:core (write) and the NodeClasses' tags.
type Tailscale struct {
	c       *client
	tailnet string
	creds   func() (string, Secret, error)

	mu       sync.Mutex
	token    Secret
	tokenExp time.Time
}

var _ Backend = (*Tailscale)(nil)

// NewTailscale returns the Tailscale backend. BaseURL defaults to https://api.tailscale.com.
func NewTailscale(cfg TailscaleConfig) (*Tailscale, error) {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.tailscale.com"
	}
	if cfg.Credentials == nil {
		return nil, fmt.Errorf("tailscale backend needs credentials")
	}
	c, err := newClient(cfg.Config)
	if err != nil {
		return nil, err
	}
	if cfg.Tailnet == "" {
		cfg.Tailnet = "-"
	}
	return &Tailscale{c: c, tailnet: url.PathEscape(cfg.Tailnet), creds: cfg.Credentials}, nil
}

// accessToken returns a cached OAuth token or fetches one (client credentials grant).
func (t *Tailscale) accessToken(ctx context.Context) (Secret, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && t.c.now().Add(tokenSkew).Before(t.tokenExp) {
		return t.token, nil
	}
	id, secret, err := t.creds()
	if err != nil {
		return "", &Error{Class: Fatal, Op: opToken, Message: "reading the OAuth client credentials failed: " + err.Error()}
	}
	var resp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	err = t.c.do(ctx, call{
		op: opToken, method: http.MethodPost, path: "/api/v2/oauth/token",
		form: url.Values{
			"grant_type":    {"client_credentials"},
			"client_id":     {id},
			"client_secret": {secret.Reveal()},
		},
	}, &resp)
	if err != nil {
		return "", err
	}
	if resp.AccessToken == "" {
		return "", &Error{Class: Fatal, Op: opToken, Message: "the token response has no access token"}
	}
	t.token = Secret(resp.AccessToken)
	t.tokenExp = t.c.now().Add(time.Duration(resp.ExpiresIn) * time.Second)
	return t.token, nil
}

func (t *Tailscale) forgetToken() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.token = ""
}

func (t *Tailscale) authorize(ctx context.Context, r *http.Request) error {
	tok, err := t.accessToken(ctx)
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", "Bearer "+tok.Reveal())
	return nil
}

func (t *Tailscale) call(op, method, path string, body any) call {
	return call{op: op, method: method, path: path, body: body, authorize: t.authorize, reauth: t.forgetToken}
}

type tailscaleCreate struct {
	Reusable      bool     `json:"reusable"`
	Ephemeral     bool     `json:"ephemeral"`
	Preauthorized bool     `json:"preauthorized"`
	Tags          []string `json:"tags"`
}

type tailscaleKeyRequest struct {
	Capabilities struct {
		Devices struct {
			Create tailscaleCreate `json:"create"`
		} `json:"devices"`
	} `json:"capabilities"`
	ExpirySeconds int64  `json:"expirySeconds"`
	Description   string `json:"description,omitempty"`
}

// MintKey creates a single-use, ephemeral, pre-authorised, tagged key.
func (t *Tailscale) MintKey(ctx context.Context, r KeyRequest) (Key, error) {
	const op = "mint"
	if err := validateKeyRequest(op, r); err != nil {
		return Key{}, err
	}
	var req tailscaleKeyRequest
	req.Capabilities.Devices.Create = tailscaleCreate{Ephemeral: true, Preauthorized: true, Tags: r.Tags}
	req.ExpirySeconds = int64((r.Expiry + time.Second - 1) / time.Second)
	desc := []rune(strings.TrimSpace(r.Description))
	req.Description = string(desc[:min(len(desc), tailscaleDescriptionMax)])

	var resp struct {
		ID      string    `json:"id"`
		Key     string    `json:"key"`
		Expires time.Time `json:"expires"`
	}
	if err := t.c.do(ctx, t.call(op, http.MethodPost, "/api/v2/tailnet/"+t.tailnet+"/keys", req), &resp); err != nil {
		return Key{}, mintError(err)
	}
	if resp.ID == "" || resp.Key == "" {
		return Key{}, &Error{Class: Fatal, Op: op, Message: "the response has no key ID or key"}
	}
	return Key{ID: resp.ID, Secret: Secret(resp.Key), Expires: resp.Expires}, nil
}

// mintError makes a 404 on create fatal: the tailnet path or the API URL is wrong.
func mintError(err error) error {
	if e, ok := errors.AsType[*Error](err); ok && e.Class == NotFound {
		e.Class = Fatal
	}
	return err
}

// RevokeKey deletes the key. Tailscale revokes a single-use key itself when a device uses
// it, so a missing key is success; the call tidies the key list.
func (t *Tailscale) RevokeKey(ctx context.Context, id string) error {
	cl := t.call("revoke", http.MethodDelete, "/api/v2/tailnet/"+t.tailnet+"/keys/"+url.PathEscape(id), nil)
	cl.notFoundOK = true
	return t.c.do(ctx, cl, nil)
}

type tailscaleDevice struct {
	NodeID    string    `json:"nodeId"`
	ID        string    `json:"id"`
	Hostname  string    `json:"hostname"`
	Addresses []string  `json:"addresses"`
	Tags      []string  `json:"tags"`
	Created   time.Time `json:"created"`
}

// FindDevice lists the devices and picks the match. Tailscale does not say which key
// admitted a device, so KeyIDs are ignored: use IP (the Node's tailscale address) and
// Hostname, or Hostname with Tags and CreatedAfter.
func (t *Tailscale) FindDevice(ctx context.Context, q DeviceQuery) (Device, error) {
	const op = "find"
	if err := validateDeviceQuery(op, q); err != nil {
		return Device{}, err
	}
	if !q.IP.IsValid() && q.Hostname == "" {
		return Device{}, &Error{Class: Fatal, Op: op, Message: "Tailscale devices do not name their key: use IP or hostname"}
	}
	q.KeyIDs = nil
	var resp struct {
		Devices []tailscaleDevice `json:"devices"`
	}
	if err := t.c.do(ctx, t.call(op, http.MethodGet, "/api/v2/tailnet/"+t.tailnet+"/devices", nil), &resp); err != nil {
		return Device{}, err
	}
	devs := make([]device, 0, len(resp.Devices))
	for _, d := range resp.Devices {
		id := d.NodeID
		if id == "" {
			id = d.ID
		}
		devs = append(devs, device{Device: Device{
			ID: id, Hostname: d.Hostname, Addresses: parseAddrs(d.Addresses), Tags: d.Tags, Created: d.Created,
		}})
	}
	return pickDevice(op, devs, q)
}

// DeleteDevice deletes the device by node ID; a device that is gone is success.
func (t *Tailscale) DeleteDevice(ctx context.Context, id string) error {
	cl := t.call("delete", http.MethodDelete, "/api/v2/device/"+url.PathEscape(id), nil)
	cl.notFoundOK = true
	return t.c.do(ctx, cl, nil)
}

func parseAddrs(ss []string) []netip.Addr {
	var out []netip.Addr
	for _, s := range ss {
		if a, err := netip.ParseAddr(s); err == nil {
			out = append(out, a)
		}
	}
	return out
}
