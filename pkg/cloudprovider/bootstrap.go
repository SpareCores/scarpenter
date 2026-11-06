/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider

import (
	"context"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
)

// TokenMinter creates and removes the bootstrap token of a NodeClaim (overview
// section 8.1). Plan 05 implements it; until then Create uses StubBootstrap.
type TokenMinter interface {
	// Mint creates the token Secret of the NodeClaim and returns the token
	// ("<id>.<secret>"). It is idempotent per NodeClaim.
	Mint(ctx context.Context, nodeClaim *karpv1.NodeClaim) (string, error)
	// Delete removes the NodeClaim's token; a missing token is not an error.
	Delete(ctx context.Context, nodeClaimName string) error
}

// RenderRequest is what the user-data of a node depends on (overview section
// 8.2). It depends on the architecture (sysext URLs) and the region, not on the
// instance type, so one rendering serves every launch attempt with the same
// architecture and region.
type RenderRequest struct {
	NodeClass *v1alpha1.SCNodeClass
	NodeClaim *karpv1.NodeClaim
	Vendor    string
	Region    string
	Arch      string
	// Token is the bootstrap token of the NodeClaim.
	Token string
	// NodeName and ProviderID are the node name (the NodeClaim name) and the
	// provider ID scarpenter://<vendor>/<region>/<nodeclaim>.
	NodeName   string
	ProviderID string
}

// UserDataRenderer renders the user-data (Ignition) of a node. Plan 05
// implements it.
type UserDataRenderer interface {
	Render(ctx context.Context, req RenderRequest) ([]byte, error)
}

// StubBootstrap is the placeholder of plan 04: empty user-data and no tokens.
type StubBootstrap struct{}

var (
	_ TokenMinter      = StubBootstrap{}
	_ UserDataRenderer = StubBootstrap{}
)

// Mint returns an empty token.
func (StubBootstrap) Mint(context.Context, *karpv1.NodeClaim) (string, error) { return "", nil }

// Delete does nothing.
func (StubBootstrap) Delete(context.Context, string) error { return nil }

// Render returns empty user-data.
func (StubBootstrap) Render(context.Context, RenderRequest) ([]byte, error) { return nil, nil }
