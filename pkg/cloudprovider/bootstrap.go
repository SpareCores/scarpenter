/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider

import (
	"context"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/bootstrap"
)

// TokenMinter creates and removes the bootstrap token of a NodeClaim (overview
// section 8.1); bootstrap.Minter implements it. Without one, Create uses
// StubBootstrap.
type TokenMinter interface {
	// Mint creates the token Secret of the NodeClaim and returns the token
	// ("<id>.<secret>"). It is idempotent per NodeClaim.
	Mint(ctx context.Context, nodeClaim *karpv1.NodeClaim) (string, error)
	// Delete removes the NodeClaim's token; a missing token is not an error.
	Delete(ctx context.Context, nodeClaimName string) error
}

// RenderRequest is what the user-data of a node depends on (overview section
// 8.2); see bootstrap.RenderRequest.
type RenderRequest = bootstrap.RenderRequest

// UserDataRenderer renders the user-data (Ignition) of a node;
// bootstrap.Renderer implements it.
type UserDataRenderer interface {
	Render(ctx context.Context, req RenderRequest) ([]byte, error)
}

// StubBootstrap is the default of the tests and tools that need neither: empty
// user-data and no tokens.
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
