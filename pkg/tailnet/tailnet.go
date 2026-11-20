/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package tailnet mints and revokes per-node tailnet auth keys and deletes tailnet devices
// (plan 27). Two backends sit behind one small interface: the Tailscale API (SaaS) and the
// Headscale 0.28+ REST API. The package has no Kubernetes client and uses plain net/http.
//
// Every key is single use, pre-authorised, tagged and ephemeral. A key's secret is only
// ever held in a Secret, which prints "[redacted]" in every formatter; errors carry the
// status and the API's error text, never a request or response body.
package tailnet

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"time"
)

// Backend is the tailnet control plane as SCarpenter uses it.
type Backend interface {
	// MintKey creates a single-use, pre-authorised, ephemeral key with the request's tags.
	MintKey(ctx context.Context, r KeyRequest) (Key, error)
	// RevokeKey invalidates a key by ID. A key that is gone or already used is success.
	RevokeKey(ctx context.Context, id string) error
	// FindDevice returns the one device matching the query: ErrNotFound-class errors when
	// there is none and Ambiguous-class errors when several match.
	FindDevice(ctx context.Context, q DeviceQuery) (Device, error)
	// DeleteDevice removes a device by ID. A device that is gone is success.
	DeleteDevice(ctx context.Context, id string) error
}

// KeyRequest describes the key to mint.
type KeyRequest struct {
	// Tags are the ACL tags of the key and so of the device it admits ("tag:name"); required.
	Tags []string
	// Expiry is how long the key stays valid; required.
	Expiry time.Duration
	// Description is a label for the vendor's key list (Tailscale only; shortened to its limit).
	Description string
}

// Key is a minted key. Only ID may be logged or stored.
type Key struct {
	ID      string
	Secret  Secret
	Expires time.Time
}

// DeviceQuery selects a device. At least one of KeyIDs, IP and Hostname is required.
//
// KeyIDs match exactly where the backend knows which key admitted a device (Headscale).
// Otherwise IP and Hostname are cross-checked, and Tags (all of them) and CreatedAfter
// narrow the match further.
type DeviceQuery struct {
	KeyIDs       []string
	IP           netip.Addr
	Hostname     string
	Tags         []string
	CreatedAfter time.Time
}

// Device is a tailnet device as far as SCarpenter needs to know.
type Device struct {
	ID        string
	Hostname  string
	Addresses []netip.Addr
	Tags      []string
	Created   time.Time
}

// Secret is a credential that must never reach a log, an Event, a condition or an error.
type Secret string

const redacted = "[redacted]"

// Reveal returns the secret's value, for the one place that needs it: the node's user-data.
func (s Secret) Reveal() string { return string(s) }

// String implements fmt.Stringer.
func (Secret) String() string { return redacted }

// GoString implements fmt.GoStringer (%#v).
func (Secret) GoString() string { return redacted }

// Format implements fmt.Formatter, so no verb prints the value.
func (Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redacted)) }

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalText implements encoding.TextMarshaler (JSON and YAML encoders).
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }
