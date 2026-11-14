/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import (
	"context"

	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/oracle/oci-go-sdk/v65/identity"
	"github.com/oracle/oci-go-sdk/v65/limits"
)

// The narrow interfaces of the OCI services the driver uses (plan 25, section 5):
// the SDK's generated clients are concrete structs with one client per region,
// so each method takes the region first and the SDK's own request and response
// types after it. The SDK adapter (SDK) implements them, per-region clients and
// per-call deadlines included; the in-memory fake (pkg/vendors/oci/fake) too.
// Every method has a statement in docs/reference/oci-iam-policy.txt, which
// iam_test.go checks.

// ComputeAPI is the part of the Compute service the driver calls.
type ComputeAPI interface {
	// LaunchInstance starts an instance. It is never retried by a retry policy
	// (plan 25, finding 7): an HTTP 500 "Out of host capacity" must reach the
	// driver, which classifies it.
	LaunchInstance(ctx context.Context, region string, req core.LaunchInstanceRequest,
	) (core.LaunchInstanceResponse, error)
	GetInstance(ctx context.Context, region string, req core.GetInstanceRequest) (core.GetInstanceResponse, error)
	ListInstances(ctx context.Context, region string, req core.ListInstancesRequest,
	) (core.ListInstancesResponse, error)
	TerminateInstance(ctx context.Context, region string, req core.TerminateInstanceRequest,
	) (core.TerminateInstanceResponse, error)
	ListVnicAttachments(ctx context.Context, region string, req core.ListVnicAttachmentsRequest,
	) (core.ListVnicAttachmentsResponse, error)
	ListShapes(ctx context.Context, region string, req core.ListShapesRequest) (core.ListShapesResponse, error)
}

// NetworkAPI is the part of the Virtual Network service the driver calls. Step
// (c) adds the subnets, groups and the managed network.
type NetworkAPI interface {
	GetVnic(ctx context.Context, region string, req core.GetVnicRequest) (core.GetVnicResponse, error)
}

// IdentityAPI is the part of the Identity service the driver calls.
type IdentityAPI interface {
	// ListRegionSubscriptions is answered by the home region's endpoint.
	ListRegionSubscriptions(ctx context.Context, req identity.ListRegionSubscriptionsRequest,
	) (identity.ListRegionSubscriptionsResponse, error)
	ListAvailabilityDomains(ctx context.Context, region string, req identity.ListAvailabilityDomainsRequest,
	) (identity.ListAvailabilityDomainsResponse, error)
}

// LimitsAPI is the part of the Limits service the driver calls.
type LimitsAPI interface {
	GetResourceAvailability(ctx context.Context, region string, req limits.GetResourceAvailabilityRequest,
	) (limits.GetResourceAvailabilityResponse, error)
	ListLimitDefinitions(ctx context.Context, region string, req limits.ListLimitDefinitionsRequest,
	) (limits.ListLimitDefinitionsResponse, error)
}

// Clients bundles the four services; the SDK adapter and the fake each provide
// all of them.
type Clients struct {
	Compute  ComputeAPI
	Network  NetworkAPI
	Identity IdentityAPI
	Limits   LimitsAPI
}
