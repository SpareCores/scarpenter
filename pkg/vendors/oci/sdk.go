/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

//nolint:lll // the adapters repeat the SDK method signatures
package oci

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/oracle/oci-go-sdk/v65/identity"
	"github.com/oracle/oci-go-sdk/v65/limits"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// DefaultCallTimeout bounds one API call (one page of a list), the retries of
// the adapter included: every SDK call the driver makes runs under it.
const DefaultCallTimeout = 45 * time.Second

// SDK implements ComputeAPI, NetworkAPI, IdentityAPI and LimitsAPI over the
// OCI Go SDK (plan 25, section 5): one client of each service per region, made on
// first use, with the user agent scarpenter/<version> and a deadline on every
// call.
//
// The SDK's own retry policy is off on every client (NoRetryPolicy), because
// its retry loop cannot run under a deadline without a data race (see within).
// invoke retries the idempotent calls itself: reads and deletes retry 429, 5xx
// (but the capacity answer) and the conflicts of a resource in use with backoff,
// bounded by the call's deadline; a 404 is never retried, so NotFound stays
// visible to Get and Delete. LaunchInstance is never retried: the HTTP 500 "Out of
// host capacity" is the driver's to classify.
type SDK struct {
	provider common.ConfigurationProvider
	agent    string
	timeout  time.Duration
	// host overrides the endpoint of a service in a region (the tests point it
	// at a local server); nil uses the SDK's endpoints.
	host func(service, region string) string
	// backoff overrides the wait between retries (the tests); nil is retryBackoff.
	backoff func(attempt int) time.Duration

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

// maxCallAttempts bounds the driver-side retries of an idempotent call.
const maxCallAttempts = 4

// retryBackoff is the wait before the retry after attempt n (0-based): 0.5 s,
// 1 s, 2 s, with up to a quarter of jitter. The call's deadline bounds the sum.
func retryBackoff(n int) time.Duration {
	d := 500 * time.Millisecond << n
	return d - time.Duration(rand.Int64N(int64(d/4)+1))
}

// within runs one call under a deadline; a shorter deadline of the caller wins.
// The call runs on its own goroutine under a context that never ends, and the
// deadline is enforced here: the SDK's common.Retry selects on its context and,
// when the context ends first, returns a response variable its own goroutine
// still writes, a data race on every timed-out call (oci-go-sdk v65.130.0,
// common/retry.go). Without a Done channel in the SDK's context that select
// cannot fire. The abandoned request ends by the HTTP client's own timeout; its
// result is dropped (the channel is buffered).
func within[Q, R any](ctx context.Context, d time.Duration, f func(context.Context, Q) (R, error), req Q) (R, error) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	type result struct {
		r   R
		err error
	}
	done := make(chan result, 1)
	sdkCtx := context.WithoutCancel(ctx)
	go func() {
		r, err := f(sdkCtx, req)
		done <- result{r, err}
	}()
	select {
	case res := <-done:
		return res.r, res.err
	case <-ctx.Done():
		var zero R
		return zero, ctx.Err()
	}
}

// invoke runs one SDK call under the adapter's deadline. The SDK's own retry
// policy is off on every client (see within for why it cannot run under a
// deadline safely); an idempotent call (retry true) is retried here, up to
// maxCallAttempts times and within the one deadline, when its error is
// Retryable: 429, 5xx but the capacity answer, the conflicts of a resource still
// in use and transport errors. A 404 is never retried, so NotFound stays visible.
// LaunchInstance passes false: the capacity answer must reach the driver at once.
func invoke[Q, R any](ctx context.Context, s *SDK, retry bool, f func(context.Context, Q) (R, error), req Q,
) (R, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	for attempt := 0; ; attempt++ {
		r, err := within(ctx, s.timeout, f, req)
		if err == nil || !retry || attempt+1 >= maxCallAttempts || ctx.Err() != nil ||
			!vendors.IsClass(classify(err, kindRead), vendors.Retryable) {
			return r, err
		}
		wait := s.backoff
		if wait == nil {
			wait = retryBackoff
		}
		t := time.NewTimer(wait(attempt))
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return r, err
		}
	}
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
	// The SDK's retry loop is off (invoke retries); see within.
	policy := common.NoRetryPolicy()
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
	return invoke(ctx, s, false, c.LaunchInstance, req)
}

// GetInstance implements ComputeAPI.
func (s *SDK) GetInstance(ctx context.Context, region string, req core.GetInstanceRequest,
) (core.GetInstanceResponse, error) {
	c, err := s.computeClient(region)
	if err != nil {
		return core.GetInstanceResponse{}, err
	}
	return invoke(ctx, s, true, c.GetInstance, req)
}

// ListInstances implements ComputeAPI.
func (s *SDK) ListInstances(ctx context.Context, region string, req core.ListInstancesRequest,
) (core.ListInstancesResponse, error) {
	c, err := s.computeClient(region)
	if err != nil {
		return core.ListInstancesResponse{}, err
	}
	return invoke(ctx, s, true, c.ListInstances, req)
}

// TerminateInstance implements ComputeAPI.
func (s *SDK) TerminateInstance(ctx context.Context, region string, req core.TerminateInstanceRequest,
) (core.TerminateInstanceResponse, error) {
	c, err := s.computeClient(region)
	if err != nil {
		return core.TerminateInstanceResponse{}, err
	}
	return invoke(ctx, s, true, c.TerminateInstance, req)
}

// ListVnicAttachments implements ComputeAPI.
func (s *SDK) ListVnicAttachments(ctx context.Context, region string, req core.ListVnicAttachmentsRequest,
) (core.ListVnicAttachmentsResponse, error) {
	c, err := s.computeClient(region)
	if err != nil {
		return core.ListVnicAttachmentsResponse{}, err
	}
	return invoke(ctx, s, true, c.ListVnicAttachments, req)
}

// ListShapes implements ComputeAPI.
func (s *SDK) ListShapes(ctx context.Context, region string, req core.ListShapesRequest,
) (core.ListShapesResponse, error) {
	c, err := s.computeClient(region)
	if err != nil {
		return core.ListShapesResponse{}, err
	}
	return invoke(ctx, s, true, c.ListShapes, req)
}

// GetVnic implements NetworkAPI.
func (s *SDK) GetVnic(ctx context.Context, region string, req core.GetVnicRequest) (core.GetVnicResponse, error) {
	c, err := s.networkClient(region)
	if err != nil {
		return core.GetVnicResponse{}, err
	}
	return invoke(ctx, s, true, c.GetVnic, req)
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
	return invoke(ctx, s, true, c.ListRegionSubscriptions, req)
}

// ListAvailabilityDomains implements IdentityAPI.
func (s *SDK) ListAvailabilityDomains(ctx context.Context, region string, req identity.ListAvailabilityDomainsRequest,
) (identity.ListAvailabilityDomainsResponse, error) {
	c, err := s.identityClient(region)
	if err != nil {
		return identity.ListAvailabilityDomainsResponse{}, err
	}
	return invoke(ctx, s, true, c.ListAvailabilityDomains, req)
}

// GetResourceAvailability implements LimitsAPI.
func (s *SDK) GetResourceAvailability(ctx context.Context, region string, req limits.GetResourceAvailabilityRequest,
) (limits.GetResourceAvailabilityResponse, error) {
	c, err := s.limitsClient(region)
	if err != nil {
		return limits.GetResourceAvailabilityResponse{}, err
	}
	return invoke(ctx, s, true, c.GetResourceAvailability, req)
}

// ListLimitDefinitions implements LimitsAPI.
func (s *SDK) ListLimitDefinitions(ctx context.Context, region string, req limits.ListLimitDefinitionsRequest,
) (limits.ListLimitDefinitionsResponse, error) {
	c, err := s.limitsClient(region)
	if err != nil {
		return limits.ListLimitDefinitionsResponse{}, err
	}
	return invoke(ctx, s, true, c.ListLimitDefinitions, req)
}

// ListImages implements ComputeAPI.
func (s *SDK) ListImages(ctx context.Context, region string, req core.ListImagesRequest) (core.ListImagesResponse, error) {
	c, err := s.computeClient(region)
	if err != nil {
		return core.ListImagesResponse{}, err
	}
	return invoke(ctx, s, true, c.ListImages, req)
}

// GetImage implements ComputeAPI.
func (s *SDK) GetImage(ctx context.Context, region string, req core.GetImageRequest) (core.GetImageResponse, error) {
	c, err := s.computeClient(region)
	if err != nil {
		return core.GetImageResponse{}, err
	}
	return invoke(ctx, s, true, c.GetImage, req)
}

// ListImageShapeCompatibilityEntries implements ComputeAPI.
func (s *SDK) ListImageShapeCompatibilityEntries(ctx context.Context, region string, req core.ListImageShapeCompatibilityEntriesRequest) (core.ListImageShapeCompatibilityEntriesResponse, error) {
	c, err := s.computeClient(region)
	if err != nil {
		return core.ListImageShapeCompatibilityEntriesResponse{}, err
	}
	return invoke(ctx, s, true, c.ListImageShapeCompatibilityEntries, req)
}

// GetSubnet implements NetworkAPI.
func (s *SDK) GetSubnet(ctx context.Context, region string, req core.GetSubnetRequest) (core.GetSubnetResponse, error) {
	c, err := s.networkClient(region)
	if err != nil {
		return core.GetSubnetResponse{}, err
	}
	return invoke(ctx, s, true, c.GetSubnet, req)
}

// ListSubnets implements NetworkAPI.
func (s *SDK) ListSubnets(ctx context.Context, region string, req core.ListSubnetsRequest) (core.ListSubnetsResponse, error) {
	c, err := s.networkClient(region)
	if err != nil {
		return core.ListSubnetsResponse{}, err
	}
	return invoke(ctx, s, true, c.ListSubnets, req)
}

// CreateSubnet implements NetworkAPI.
func (s *SDK) CreateSubnet(ctx context.Context, region string, req core.CreateSubnetRequest) (core.CreateSubnetResponse, error) {
	c, err := s.networkClient(region)
	if err != nil {
		return core.CreateSubnetResponse{}, err
	}
	return invoke(ctx, s, false, c.CreateSubnet, req)
}

// DeleteSubnet implements NetworkAPI.
func (s *SDK) DeleteSubnet(ctx context.Context, region string, req core.DeleteSubnetRequest) (core.DeleteSubnetResponse, error) {
	c, err := s.networkClient(region)
	if err != nil {
		return core.DeleteSubnetResponse{}, err
	}
	return invoke(ctx, s, true, c.DeleteSubnet, req)
}

// ListVcns implements NetworkAPI.
func (s *SDK) ListVcns(ctx context.Context, region string, req core.ListVcnsRequest) (core.ListVcnsResponse, error) {
	c, err := s.networkClient(region)
	if err != nil {
		return core.ListVcnsResponse{}, err
	}
	return invoke(ctx, s, true, c.ListVcns, req)
}

// CreateVcn implements NetworkAPI.
func (s *SDK) CreateVcn(ctx context.Context, region string, req core.CreateVcnRequest) (core.CreateVcnResponse, error) {
	c, err := s.networkClient(region)
	if err != nil {
		return core.CreateVcnResponse{}, err
	}
	return invoke(ctx, s, false, c.CreateVcn, req)
}

// DeleteVcn implements NetworkAPI.
func (s *SDK) DeleteVcn(ctx context.Context, region string, req core.DeleteVcnRequest) (core.DeleteVcnResponse, error) {
	c, err := s.networkClient(region)
	if err != nil {
		return core.DeleteVcnResponse{}, err
	}
	return invoke(ctx, s, true, c.DeleteVcn, req)
}

// ListInternetGateways implements NetworkAPI.
func (s *SDK) ListInternetGateways(ctx context.Context, region string, req core.ListInternetGatewaysRequest) (core.ListInternetGatewaysResponse, error) {
	c, err := s.networkClient(region)
	if err != nil {
		return core.ListInternetGatewaysResponse{}, err
	}
	return invoke(ctx, s, true, c.ListInternetGateways, req)
}

// CreateInternetGateway implements NetworkAPI.
func (s *SDK) CreateInternetGateway(ctx context.Context, region string, req core.CreateInternetGatewayRequest) (core.CreateInternetGatewayResponse, error) {
	c, err := s.networkClient(region)
	if err != nil {
		return core.CreateInternetGatewayResponse{}, err
	}
	return invoke(ctx, s, false, c.CreateInternetGateway, req)
}

// DeleteInternetGateway implements NetworkAPI.
func (s *SDK) DeleteInternetGateway(ctx context.Context, region string, req core.DeleteInternetGatewayRequest) (core.DeleteInternetGatewayResponse, error) {
	c, err := s.networkClient(region)
	if err != nil {
		return core.DeleteInternetGatewayResponse{}, err
	}
	return invoke(ctx, s, true, c.DeleteInternetGateway, req)
}

// ListRouteTables implements NetworkAPI.
func (s *SDK) ListRouteTables(ctx context.Context, region string, req core.ListRouteTablesRequest) (core.ListRouteTablesResponse, error) {
	c, err := s.networkClient(region)
	if err != nil {
		return core.ListRouteTablesResponse{}, err
	}
	return invoke(ctx, s, true, c.ListRouteTables, req)
}

// CreateRouteTable implements NetworkAPI.
func (s *SDK) CreateRouteTable(ctx context.Context, region string, req core.CreateRouteTableRequest) (core.CreateRouteTableResponse, error) {
	c, err := s.networkClient(region)
	if err != nil {
		return core.CreateRouteTableResponse{}, err
	}
	return invoke(ctx, s, false, c.CreateRouteTable, req)
}

// DeleteRouteTable implements NetworkAPI.
func (s *SDK) DeleteRouteTable(ctx context.Context, region string, req core.DeleteRouteTableRequest) (core.DeleteRouteTableResponse, error) {
	c, err := s.networkClient(region)
	if err != nil {
		return core.DeleteRouteTableResponse{}, err
	}
	return invoke(ctx, s, true, c.DeleteRouteTable, req)
}

// ListSecurityLists implements NetworkAPI.
func (s *SDK) ListSecurityLists(ctx context.Context, region string, req core.ListSecurityListsRequest) (core.ListSecurityListsResponse, error) {
	c, err := s.networkClient(region)
	if err != nil {
		return core.ListSecurityListsResponse{}, err
	}
	return invoke(ctx, s, true, c.ListSecurityLists, req)
}

// CreateSecurityList implements NetworkAPI.
func (s *SDK) CreateSecurityList(ctx context.Context, region string, req core.CreateSecurityListRequest) (core.CreateSecurityListResponse, error) {
	c, err := s.networkClient(region)
	if err != nil {
		return core.CreateSecurityListResponse{}, err
	}
	return invoke(ctx, s, false, c.CreateSecurityList, req)
}

// DeleteSecurityList implements NetworkAPI.
func (s *SDK) DeleteSecurityList(ctx context.Context, region string, req core.DeleteSecurityListRequest) (core.DeleteSecurityListResponse, error) {
	c, err := s.networkClient(region)
	if err != nil {
		return core.DeleteSecurityListResponse{}, err
	}
	return invoke(ctx, s, true, c.DeleteSecurityList, req)
}

// GetNetworkSecurityGroup implements NetworkAPI.
func (s *SDK) GetNetworkSecurityGroup(ctx context.Context, region string, req core.GetNetworkSecurityGroupRequest) (core.GetNetworkSecurityGroupResponse, error) {
	c, err := s.networkClient(region)
	if err != nil {
		return core.GetNetworkSecurityGroupResponse{}, err
	}
	return invoke(ctx, s, true, c.GetNetworkSecurityGroup, req)
}

// ListNetworkSecurityGroups implements NetworkAPI.
func (s *SDK) ListNetworkSecurityGroups(ctx context.Context, region string, req core.ListNetworkSecurityGroupsRequest) (core.ListNetworkSecurityGroupsResponse, error) {
	c, err := s.networkClient(region)
	if err != nil {
		return core.ListNetworkSecurityGroupsResponse{}, err
	}
	return invoke(ctx, s, true, c.ListNetworkSecurityGroups, req)
}

// CreateNetworkSecurityGroup implements NetworkAPI.
func (s *SDK) CreateNetworkSecurityGroup(ctx context.Context, region string, req core.CreateNetworkSecurityGroupRequest) (core.CreateNetworkSecurityGroupResponse, error) {
	c, err := s.networkClient(region)
	if err != nil {
		return core.CreateNetworkSecurityGroupResponse{}, err
	}
	return invoke(ctx, s, false, c.CreateNetworkSecurityGroup, req)
}

// DeleteNetworkSecurityGroup implements NetworkAPI.
func (s *SDK) DeleteNetworkSecurityGroup(ctx context.Context, region string, req core.DeleteNetworkSecurityGroupRequest) (core.DeleteNetworkSecurityGroupResponse, error) {
	c, err := s.networkClient(region)
	if err != nil {
		return core.DeleteNetworkSecurityGroupResponse{}, err
	}
	return invoke(ctx, s, true, c.DeleteNetworkSecurityGroup, req)
}

// AddNetworkSecurityGroupSecurityRules implements NetworkAPI.
func (s *SDK) AddNetworkSecurityGroupSecurityRules(ctx context.Context, region string, req core.AddNetworkSecurityGroupSecurityRulesRequest) (core.AddNetworkSecurityGroupSecurityRulesResponse, error) {
	c, err := s.networkClient(region)
	if err != nil {
		return core.AddNetworkSecurityGroupSecurityRulesResponse{}, err
	}
	return invoke(ctx, s, false, c.AddNetworkSecurityGroupSecurityRules, req)
}

// ListNetworkSecurityGroupVnics implements NetworkAPI.
func (s *SDK) ListNetworkSecurityGroupVnics(ctx context.Context, region string, req core.ListNetworkSecurityGroupVnicsRequest) (core.ListNetworkSecurityGroupVnicsResponse, error) {
	c, err := s.networkClient(region)
	if err != nil {
		return core.ListNetworkSecurityGroupVnicsResponse{}, err
	}
	return invoke(ctx, s, true, c.ListNetworkSecurityGroupVnics, req)
}

// ListNetworkSecurityGroupSecurityRules implements NetworkAPI.
func (s *SDK) ListNetworkSecurityGroupSecurityRules(ctx context.Context, region string, req core.ListNetworkSecurityGroupSecurityRulesRequest) (core.ListNetworkSecurityGroupSecurityRulesResponse, error) {
	c, err := s.networkClient(region)
	if err != nil {
		return core.ListNetworkSecurityGroupSecurityRulesResponse{}, err
	}
	return invoke(ctx, s, true, c.ListNetworkSecurityGroupSecurityRules, req)
}
