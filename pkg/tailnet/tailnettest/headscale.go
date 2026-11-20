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

// HeadscaleKey is a pre-auth key of the fake Headscale.
type HeadscaleKey struct {
	ID         uint64
	Secret     string
	Tags       []string
	Reusable   bool
	Ephemeral  bool
	Used       bool
	Expiration time.Time
}

// HeadscaleNode is a node of the fake Headscale.
type HeadscaleNode struct {
	ID         uint64
	Name       string
	IPs        []string
	Tags       []string
	PreAuthKey uint64
	Created    time.Time
}

// Headscale fakes a Headscale 0.28+ server's /api/v1 (grpc-gateway JSON: camelCase names,
// uint64 IDs as strings, unpopulated fields present). Keys are never deleted: expiring sets
// the expiration to now, and using a single-use key marks it used.
type Headscale struct {
	server
	apiKey string
	keys   []*HeadscaleKey
	nodes  []*HeadscaleNode
}

// NewHeadscale starts the fake with the API key it accepts.
func NewHeadscale(t testing.TB, apiKey string) *Headscale {
	f := &Headscale{apiKey: apiKey}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/preauthkey", f.handleCreateKey)
	mux.HandleFunc("POST /api/v1/preauthkey/expire", f.handleExpireKey)
	mux.HandleFunc("GET /api/v1/node", f.handleListNodes)
	mux.HandleFunc("DELETE /api/v1/node/{id}", f.handleDeleteNode)
	newServer(t, f.auth(mux), &f.server)
	return f
}

// Keys returns every pre-auth key, valid or not.
func (f *Headscale) Keys() []HeadscaleKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]HeadscaleKey, len(f.keys))
	for i, k := range f.keys {
		out[i] = *k
	}
	return out
}

// ValidKeys returns the keys a node could still register with.
func (f *Headscale) ValidKeys() []HeadscaleKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []HeadscaleKey
	for _, k := range f.keys {
		if f.usable(k) == nil {
			out = append(out, *k)
		}
	}
	return out
}

// Nodes returns the registered nodes.
func (f *Headscale) Nodes() []HeadscaleNode {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]HeadscaleNode, len(f.nodes))
	for i, n := range f.nodes {
		out[i] = *n
	}
	return out
}

// usable is Headscale's key validation: expiry first, then single use.
func (f *Headscale) usable(k *HeadscaleKey) error {
	switch {
	case !f.now().Before(k.Expiration):
		return &JoinError{Reason: "auth-key expired"}
	case k.Used && !k.Reusable:
		return &JoinError{Reason: "auth-key has already been used"}
	}
	return nil
}

// Join registers a node with the key; a tagged key makes a tagged node.
func (f *Headscale) Join(secret, hostname string) (HeadscaleNode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.IndexFunc(f.keys, func(k *HeadscaleKey) bool { return k.Secret == secret })
	if i < 0 {
		return HeadscaleNode{}, &JoinError{Reason: "auth-key not found"}
	}
	k := f.keys[i]
	if err := f.usable(k); err != nil {
		return HeadscaleNode{}, err
	}
	k.Used = true
	n := f.next()
	v4, v6 := addr(n)
	node := &HeadscaleNode{
		ID: uint64(n), Name: hostname, IPs: []string{v4, v6}, Tags: k.Tags, PreAuthKey: k.ID, Created: f.now().UTC(),
	}
	f.nodes = append(f.nodes, node)
	return *node, nil
}

func (f *Headscale) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bearer(r) != f.apiKey {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// grpcError is grpc-gateway's error document (code is the gRPC code).
func grpcError(w http.ResponseWriter, status, code int, msg string) {
	writeJSON(w, status, map[string]any{"code": code, "message": msg, "details": []any{}})
}

func (f *Headscale) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User       json.Number `json:"user"`
		Reusable   bool        `json:"reusable"`
		Ephemeral  bool        `json:"ephemeral"`
		Expiration *time.Time  `json:"expiration"`
		ACLTags    []string    `json:"aclTags"`
	}
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		grpcError(w, http.StatusBadRequest, 3, "invalid request body")
		return
	}
	if u := strings.Trim(req.User.String(), `"`); u != "" && u != "0" {
		grpcError(w, http.StatusInternalServerError, 2, "user not found")
		return
	}
	for _, tag := range req.ACLTags {
		if !validTag(tag) {
			grpcError(w, http.StatusBadRequest, 3, "invalid tag")
			return
		}
	}
	if len(req.ACLTags) == 0 {
		grpcError(w, http.StatusInternalServerError, 2, "auth-key must be either tagged or owned by user")
		return
	}
	k := &HeadscaleKey{ID: uint64(f.next()), Tags: req.ACLTags, Reusable: req.Reusable, Ephemeral: req.Ephemeral}
	k.Secret = fmt.Sprintf("hskey-auth-%012x-%064x", k.ID, k.ID)
	if req.Expiration != nil {
		k.Expiration = req.Expiration.UTC()
	}
	f.keys = append(f.keys, k)
	writeJSON(w, http.StatusOK, map[string]any{"preAuthKey": map[string]any{
		"user": nil, "id": strconv.FormatUint(k.ID, 10), "key": k.Secret,
		"reusable": k.Reusable, "ephemeral": k.Ephemeral, "used": false,
		"expiration": k.Expiration, "createdAt": f.now().UTC(), "aclTags": k.Tags,
	}})
}

func (f *Headscale) handleExpireKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID json.Number `json:"id"`
	}
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	if dec.Decode(&req) != nil {
		grpcError(w, http.StatusBadRequest, 3, "invalid request body")
		return
	}
	id, err := strconv.ParseUint(strings.Trim(req.ID.String(), `"`), 10, 64)
	if err != nil {
		grpcError(w, http.StatusBadRequest, 3, "invalid id")
		return
	}
	// An unknown ID is not an error: the update matches no row.
	for _, k := range f.keys {
		if k.ID == id {
			k.Expiration = f.now().UTC()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (f *Headscale) handleListNodes(w http.ResponseWriter, _ *http.Request) {
	nodes := make([]map[string]any, 0, len(f.nodes))
	for _, n := range f.nodes {
		var pak any
		if i := slices.IndexFunc(f.keys, func(k *HeadscaleKey) bool { return k.ID == n.PreAuthKey }); i >= 0 {
			k := f.keys[i]
			pak = map[string]any{
				"user": nil, "id": strconv.FormatUint(k.ID, 10), "key": "", "reusable": k.Reusable,
				"ephemeral": k.Ephemeral, "used": k.Used, "expiration": k.Expiration, "aclTags": k.Tags,
			}
		}
		nodes = append(nodes, map[string]any{
			"id": strconv.FormatUint(n.ID, 10), "ipAddresses": n.IPs, "name": n.Name, "givenName": n.Name,
			"user": map[string]any{"name": "tagged-devices"}, "preAuthKey": pak, "createdAt": n.Created,
			"registerMethod": "REGISTER_METHOD_AUTH_KEY", "online": true, "tags": n.Tags,
			"approvedRoutes": []string{}, "availableRoutes": []string{}, "subnetRoutes": []string{},
			"lastSeen": nil, "expiry": nil,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
}

func (f *Headscale) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	i := slices.IndexFunc(f.nodes, func(n *HeadscaleNode) bool { return n.ID == id })
	if err != nil || i < 0 {
		grpcError(w, http.StatusNotFound, 5, "node not found")
		return
	}
	f.nodes = slices.Delete(f.nodes, i, i+1)
	writeJSON(w, http.StatusOK, map[string]any{})
}
