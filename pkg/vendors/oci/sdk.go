/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

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
