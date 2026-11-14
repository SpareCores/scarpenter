/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/oracle/oci-go-sdk/v65/identity"
	"github.com/oracle/oci-go-sdk/v65/limits"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// DefaultCallTimeout bounds one API call (one page of a list), the retries of
// the client's policy included: every SDK call the driver makes runs under it.
const DefaultCallTimeout = 45 * time.Second

// SDK implements ComputeAPI, NetworkAPI, IdentityAPI and LimitsAPI over the
// OCI Go SDK (plan 25, section 5): one client of each service per region, made on
// first use, with the user agent scarpenter/<version> and a deadline on every
// call.
//
// The retry policy of every client is common.DefaultRetryPolicyWithoutEventualConsistency:
// reads and the idempotent deletes retry 429, 5xx (but 501) and the conflicts
// IncorrectState and LockConflict with backoff, bounded by the call's deadline.
// It is not DefaultRetryPolicy, whose eventual-consistency part would retry a 404
// for minutes and hide NotFound from Get and Delete. LaunchInstance overrides it
// per request with NoRetryPolicy: a retry policy would retry the HTTP 500 "Out of
// host capacity" up to eight times, and the capacity answer is for the driver
// to classify.
type SDK struct {
	provider common.ConfigurationProvider
	agent    string
	timeout  time.Duration
	// host overrides the endpoint of a service in a region (the tests point it
	// at a local server); nil uses the SDK's endpoints.
	host func(service, region string) string

	mu       sync.Mutex
	compute  map[string]*core.ComputeClient
	network  map[string]*core.VirtualNetworkClient
	identity map[string]*identity.IdentityClient
	limits   map[string]*limits.LimitsClient
}

var (
	_ ComputeAPI  = (*SDK)(nil)
	_ NetworkAPI  = (*SDK)(nil)
	_ IdentityAPI = (*SDK)(nil)
	_ LimitsAPI   = (*SDK)(nil)
)

// SDKOption configures NewSDK.
type SDKOption func(*SDK)

// WithSDKTimeout sets the deadline of one call; zero or less keeps the default.
func WithSDKTimeout(d time.Duration) SDKOption {
	return func(s *SDK) {
		if d > 0 {
			s.timeout = d
		}
	}
}

// NewSDK returns the adapter over the SDK for the credentials of provider. The
// clients are made on the first call of a region, so an unreachable Oracle does
// not keep the controller from starting.
func NewSDK(provider common.ConfigurationProvider, userAgent string, opts ...SDKOption) *SDK {
	s := &SDK{
		provider: provider, agent: userAgent, timeout: DefaultCallTimeout,
		compute: map[string]*core.ComputeClient{}, network: map[string]*core.VirtualNetworkClient{},
		identity: map[string]*identity.IdentityClient{}, limits: map[string]*limits.LimitsClient{},
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// within runs one call under a deadline; a shorter deadline of the caller wins.
func within[Q, R any](ctx context.Context, d time.Duration, f func(context.Context, Q) (R, error), req Q) (R, error) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return f(ctx, req)
}

// configure applies what every client gets: the retry policy, the user agent and
// the endpoint of the region. Only the retry policy of the client's configuration
// is set: the rest (the circuit breaker the SDK's environment variables ask for,
// the dual-stack choice) stays as the constructor made it.
func (s *SDK) configure(base *common.BaseClient, service, region string, setRegion func(string)) {
	setRegion(region)
	if s.host != nil {
		base.Host = s.host(service, region)
	}
	base.UserAgent = s.agent
	policy := common.DefaultRetryPolicyWithoutEventualConsistency()
	base.Configuration.RetryPolicy = &policy
}

func clientError(service, region string, err error) error {
	return vendors.NewError(vendors.Fatal, "ClientFactory",
		fmt.Errorf("creating the %s client of %s: %w", service, region, err))
}

func (s *SDK) computeClient(region string) (*core.ComputeClient, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.compute[region]; ok {
		return c, nil
	}
	c, err := core.NewComputeClientWithConfigurationProvider(s.provider)
	if err != nil {
		return nil, clientError("Compute", region, err)
	}
	s.configure(&c.BaseClient, "compute", region, c.SetRegion)
	s.compute[region] = &c
	return &c, nil
}

func (s *SDK) networkClient(region string) (*core.VirtualNetworkClient, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.network[region]; ok {
		return c, nil
	}
	c, err := core.NewVirtualNetworkClientWithConfigurationProvider(s.provider)
	if err != nil {
		return nil, clientError("Virtual Network", region, err)
	}
	s.configure(&c.BaseClient, "network", region, c.SetRegion)
	s.network[region] = &c
	return &c, nil
}

func (s *SDK) identityClient(region string) (*identity.IdentityClient, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.identity[region]; ok {
		return c, nil
	}
	c, err := identity.NewIdentityClientWithConfigurationProvider(s.provider)
	if err != nil {
		return nil, clientError("Identity", region, err)
	}
	s.configure(&c.BaseClient, "identity", region, c.SetRegion)
	s.identity[region] = &c
	return &c, nil
}

func (s *SDK) limitsClient(region string) (*limits.LimitsClient, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.limits[region]; ok {
		return c, nil
	}
	c, err := limits.NewLimitsClientWithConfigurationProvider(s.provider)
	if err != nil {
		return nil, clientError("Limits", region, err)
	}
	s.configure(&c.BaseClient, "limits", region, c.SetRegion)
	s.limits[region] = &c
	return &c, nil
}

// LaunchInstance implements ComputeAPI. The request runs without a retry policy
// whatever the caller set: a request that asks for none gets NoRetryPolicy.
func (s *SDK) LaunchInstance(ctx context.Context, region string, req core.LaunchInstanceRequest,
) (core.LaunchInstanceResponse, error) {
	c, err := s.computeClient(region)
	if err != nil {
		return core.LaunchInstanceResponse{}, err
	}
	none := common.NoRetryPolicy()
	req.RequestMetadata.RetryPolicy = &none
	return within(ctx, s.timeout, c.LaunchInstance, req)
}

// GetInstance implements ComputeAPI.
func (s *SDK) GetInstance(ctx context.Context, region string, req core.GetInstanceRequest,
) (core.GetInstanceResponse, error) {
	c, err := s.computeClient(region)
	if err != nil {
		return core.GetInstanceResponse{}, err
	}
	return within(ctx, s.timeout, c.GetInstance, req)
}

// ListInstances implements ComputeAPI.
func (s *SDK) ListInstances(ctx context.Context, region string, req core.ListInstancesRequest,
) (core.ListInstancesResponse, error) {
	c, err := s.computeClient(region)
	if err != nil {
		return core.ListInstancesResponse{}, err
	}
	return within(ctx, s.timeout, c.ListInstances, req)
}

// TerminateInstance implements ComputeAPI.
func (s *SDK) TerminateInstance(ctx context.Context, region string, req core.TerminateInstanceRequest,
) (core.TerminateInstanceResponse, error) {
	c, err := s.computeClient(region)
	if err != nil {
		return core.TerminateInstanceResponse{}, err
	}
	return within(ctx, s.timeout, c.TerminateInstance, req)
}

// ListVnicAttachments implements ComputeAPI.
func (s *SDK) ListVnicAttachments(ctx context.Context, region string, req core.ListVnicAttachmentsRequest,
) (core.ListVnicAttachmentsResponse, error) {
	c, err := s.computeClient(region)
	if err != nil {
		return core.ListVnicAttachmentsResponse{}, err
	}
	return within(ctx, s.timeout, c.ListVnicAttachments, req)
}

// ListShapes implements ComputeAPI.
func (s *SDK) ListShapes(ctx context.Context, region string, req core.ListShapesRequest,
) (core.ListShapesResponse, error) {
	c, err := s.computeClient(region)
	if err != nil {
		return core.ListShapesResponse{}, err
	}
	return within(ctx, s.timeout, c.ListShapes, req)
}

// GetVnic implements NetworkAPI.
func (s *SDK) GetVnic(ctx context.Context, region string, req core.GetVnicRequest) (core.GetVnicResponse, error) {
	c, err := s.networkClient(region)
	if err != nil {
		return core.GetVnicResponse{}, err
	}
	return within(ctx, s.timeout, c.GetVnic, req)
}

// ListRegionSubscriptions implements IdentityAPI, in the home region of the
// credentials.
func (s *SDK) ListRegionSubscriptions(ctx context.Context, req identity.ListRegionSubscriptionsRequest,
) (identity.ListRegionSubscriptionsResponse, error) {
	home, err := s.provider.Region()
	if err != nil {
		return identity.ListRegionSubscriptionsResponse{}, vendors.NewError(vendors.Fatal, "NoHomeRegion",
			fmt.Errorf("reading the region of the credentials: %w", err))
	}
	c, err := s.identityClient(home)
	if err != nil {
		return identity.ListRegionSubscriptionsResponse{}, err
	}
	return within(ctx, s.timeout, c.ListRegionSubscriptions, req)
}

// ListAvailabilityDomains implements IdentityAPI.
func (s *SDK) ListAvailabilityDomains(ctx context.Context, region string, req identity.ListAvailabilityDomainsRequest,
) (identity.ListAvailabilityDomainsResponse, error) {
	c, err := s.identityClient(region)
	if err != nil {
		return identity.ListAvailabilityDomainsResponse{}, err
	}
	return within(ctx, s.timeout, c.ListAvailabilityDomains, req)
}

// GetResourceAvailability implements LimitsAPI.
func (s *SDK) GetResourceAvailability(ctx context.Context, region string, req limits.GetResourceAvailabilityRequest,
) (limits.GetResourceAvailabilityResponse, error) {
	c, err := s.limitsClient(region)
	if err != nil {
		return limits.GetResourceAvailabilityResponse{}, err
	}
	return within(ctx, s.timeout, c.GetResourceAvailability, req)
}

// ListLimitDefinitions implements LimitsAPI.
func (s *SDK) ListLimitDefinitions(ctx context.Context, region string, req limits.ListLimitDefinitionsRequest,
) (limits.ListLimitDefinitionsResponse, error) {
	c, err := s.limitsClient(region)
	if err != nil {
		return limits.ListLimitDefinitionsResponse{}, err
	}
	return within(ctx, s.timeout, c.ListLimitDefinitions, req)
}
