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
	"net/url"
	"strconv"
	"strings"
	"time"
)

// HeadscaleConfig configures the Headscale backend.
type HeadscaleConfig struct {
	Config
	// APIKey returns the Headscale API key. It is called for every request, so a rotated
	// Secret takes effect without a restart.
	APIKey func() (Secret, error)
}

// Headscale is the Headscale 0.28+ REST backend (grpc-gateway under /api/v1). Pre-auth keys
// are expired and nodes deleted by ID, and keys need no user because tags are the identity.
// Older servers want a user and the key itself to expire a key, and are not supported.
type Headscale struct {
	c      *client
	apiKey func() (Secret, error)
}

var _ Backend = (*Headscale)(nil)

// NewHeadscale returns the Headscale backend. BaseURL is the server URL.
func NewHeadscale(cfg HeadscaleConfig) (*Headscale, error) {
	if cfg.APIKey == nil {
		return nil, fmt.Errorf("headscale backend needs an API key")
	}
	c, err := newClient(cfg.Config)
	if err != nil {
		return nil, err
	}
	return &Headscale{c: c, apiKey: cfg.APIKey}, nil
}

func (h *Headscale) call(op, method, path string, body any) call {
	return call{
		op: op, method: method, path: path, body: body,
		authorize: func(_ context.Context, r *http.Request) error {
			key, err := h.apiKey()
			if err != nil {
				return &Error{Class: Fatal, Op: op, Message: "reading the API key failed: " + err.Error()}
			}
			r.Header.Set("Authorization", "Bearer "+key.Reveal())
			return nil
		},
	}
}

// protoID is a protobuf uint64 as grpc-gateway writes it (a JSON string; a number is
// accepted too).
type protoID string

func (i *protoID) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "null" {
		s = ""
	}
	if s != "" {
		if _, err := strconv.ParseUint(s, 10, 64); err != nil {
			return fmt.Errorf("invalid ID")
		}
	}
	*i = protoID(s)
	return nil
}

type headscaleCreateRequest struct {
	Reusable   bool     `json:"reusable"`
	Ephemeral  bool     `json:"ephemeral"`
	Expiration string   `json:"expiration"`
	ACLTags    []string `json:"aclTags"`
}

// MintKey creates a single-use, ephemeral key with ACL tags and no user.
func (h *Headscale) MintKey(ctx context.Context, r KeyRequest) (Key, error) {
	const op = "mint"
	if err := validateKeyRequest(op, r); err != nil {
		return Key{}, err
	}
	req := headscaleCreateRequest{
		Ephemeral:  true,
		Expiration: h.c.now().Add(r.Expiry).UTC().Format(time.RFC3339),
		ACLTags:    r.Tags,
	}
	var resp struct {
		PreAuthKey struct {
			ID         protoID   `json:"id"`
			Key        string    `json:"key"`
			Expiration time.Time `json:"expiration"`
		} `json:"preAuthKey"`
	}
	if err := h.c.do(ctx, h.call(op, http.MethodPost, "/api/v1/preauthkey", req), &resp); err != nil {
		return Key{}, h.mintError(err)
	}
	if resp.PreAuthKey.ID == "" || resp.PreAuthKey.Key == "" {
		return Key{}, &Error{Class: Fatal, Op: op, Message: "the response has no key ID or key"}
	}
	pak := resp.PreAuthKey
	return Key{ID: string(pak.ID), Secret: Secret(pak.Key), Expires: pak.Expiration}, nil
}

// mintError adds the version hint. Servers before 0.28 look the key's user up even when
// none is given and answer the missing user with gRPC Unknown, which the gateway sends as
// HTTP 500 "record not found" (0.26, 0.27) or "user not found" (0.23 to 0.25); retrying
// cannot help, so that answer is Fatal.
func (h *Headscale) mintError(err error) error {
	e, ok := errors.AsType[*Error](err)
	if !ok {
		return err
	}
	switch {
	case e.Status == http.StatusInternalServerError && strings.Contains(e.Message, "not found"):
		e.Class = Fatal
		e.Message += " (per-node keys need Headscale 0.28 or newer, which mints keys without a user)"
	case e.Class == NotFound:
		e.Class = Fatal
		e.Message += " (is the URL a Headscale 0.28 or newer?)"
	}
	return err
}

// RevokeKey expires the key by ID. Headscale answers success for an unknown ID, and an
// expired key stays in the list until Headscale's own cleanup.
func (h *Headscale) RevokeKey(ctx context.Context, id string) error {
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return &Error{Class: Fatal, Op: "revoke", Message: "the key ID is not a number"}
	}
	cl := h.call("revoke", http.MethodPost, "/api/v1/preauthkey/expire", map[string]string{"id": id})
	cl.notFoundOK = true
	return h.c.do(ctx, cl, nil)
}

// FindDevice lists the nodes and picks the match; KeyIDs match the key that admitted a
// node exactly.
func (h *Headscale) FindDevice(ctx context.Context, q DeviceQuery) (Device, error) {
	const op = "find"
	if err := validateDeviceQuery(op, q); err != nil {
		return Device{}, err
	}
	var resp struct {
		Nodes []struct {
			ID          protoID   `json:"id"`
			Name        string    `json:"name"`
			IPAddresses []string  `json:"ipAddresses"`
			Tags        []string  `json:"tags"`
			CreatedAt   time.Time `json:"createdAt"`
			PreAuthKey  *struct {
				ID protoID `json:"id"`
			} `json:"preAuthKey"`
		} `json:"nodes"`
	}
	if err := h.c.do(ctx, h.call(op, http.MethodGet, "/api/v1/node", nil), &resp); err != nil {
		return Device{}, err
	}
	devs := make([]device, 0, len(resp.Nodes))
	for _, n := range resp.Nodes {
		d := device{Device: Device{
			ID: string(n.ID), Hostname: n.Name, Addresses: parseAddrs(n.IPAddresses), Tags: n.Tags, Created: n.CreatedAt,
		}}
		if n.PreAuthKey != nil {
			d.keyID = string(n.PreAuthKey.ID)
		}
		devs = append(devs, d)
	}
	return pickDevice(op, devs, q)
}

// DeleteDevice deletes the node by ID; a node that is gone is success.
func (h *Headscale) DeleteDevice(ctx context.Context, id string) error {
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return &Error{Class: Fatal, Op: "delete", Message: "the node ID is not a number"}
	}
	cl := h.call("delete", http.MethodDelete, "/api/v1/node/"+url.PathEscape(id), nil)
	cl.notFoundOK = true
	return h.c.do(ctx, cl, nil)
}
