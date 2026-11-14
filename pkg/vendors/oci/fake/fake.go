/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package fake is the in-memory OCI behind oci.ComputeAPI, NetworkAPI, IdentityAPI
// and LimitsAPI. It is faithful to the behaviours plan 25 relies on: region
// subscriptions with a region that is not READY, availability domains named with
// a tenancy prefix (and a region with one AD), shapes per AD with isFlexible,
// OCPU and memory bounds, per-NUMA limits and limit names, instances with states
// and freeform tags, the display-name and lifecycle-state filters, paged lists,
// opc-retry-token semantics (a replay of a successful launch returns the
// original, another body is a 409, a failed launch records nothing), launches
// that fail with the errors OCI documents (capacity as HTTP 500, LimitExceeded,
// an incompatible shape), a TERMINATED instance that stays listed, boot volumes
// that go with the instance only when asked, GetResourceAvailability computed from
// the fake's own instances, and per-call error injection.
//
// It does not import pkg/vendors/oci (the interfaces are satisfied
// structurally; oci's tests assert it), so the driver's tests can import it.
//
// Assumptions that the plan marks UNVERIFIED are the fake's choices and are
// marked here: the code of a retry token conflict, that preemptible launches
// consume the same limits as on-demand ones, and that capacity errors are
// synchronous on the launch call.
package fake

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/oracle/oci-go-sdk/v65/identity"
	"github.com/oracle/oci-go-sdk/v65/limits"
)

// ServiceError is the error the SDK returns for an HTTP error response: it
// satisfies common.ServiceError.
type ServiceError struct {
	Status  int
	Code    string
	Message string
}

// Error implements error with the SDK's own phrasing.
func (e *ServiceError) Error() string {
	return fmt.Sprintf("Error returned by service. Http Status Code: %d. Error Code: %s. Message: %s",
		e.Status, e.Code, e.Message)
}

// GetHTTPStatusCode implements common.ServiceError.
func (e *ServiceError) GetHTTPStatusCode() int { return e.Status }

// GetMessage implements common.ServiceError.
func (e *ServiceError) GetMessage() string { return e.Message }

// GetCode implements common.ServiceError.
func (e *ServiceError) GetCode() string { return e.Code }

// GetOpcRequestID implements common.ServiceError.
func (e *ServiceError) GetOpcRequestID() string { return "fake-request-id" }

// Error builds the error the SDK returns for the status, code and message.
func Error(status int, code, msg string) error {
	return &ServiceError{Status: status, Code: code, Message: msg}
}

func notFound(what string) error {
	return Error(http.StatusNotFound, "NotAuthorizedOrNotFound",
		what+" does not exist or you are not authorized to access it")
}

func invalid(format string, args ...any) error {
	return Error(http.StatusBadRequest, "InvalidParameter", fmt.Sprintf(format, args...))
}

type region struct {
	name    string
	status  identity.RegionSubscriptionStatusEnum
	domains []string
	shapes  map[string]map[string]core.Shape // AD -> shape name -> shape
}

type tokenRec struct {
	fingerprint string
	instanceID  string
}

type bootVolume struct {
	ID     string
	SizeGB int64
	VPUs   int64
	KMSKey string
}

// Cloud is the fake. The zero value is not usable: use New.
type Cloud struct {
	mu          sync.Mutex
	now         func() time.Time
	seq         uint64
	tenancy     string
	compartment string
	pageSize    int

	regions   map[string]*region
	instances map[string]*core.Instance
	attach    map[string][]core.VnicAttachment // instance ID -> attachments
	vnics     map[string]core.Vnic
	boot      map[string]bootVolume // instance ID -> boot volume
	tokens    map[string]tokenRec   // region|token
	net       netStore
	seedVCN   string
	images    map[string]*imageRec
	// imageComps are the compartments besides the controller's that list images (AddImageCompartment).
	imageComps map[string]bool
	launches   []core.LaunchInstanceRequest

	limitValues map[string]int64 // region|ad|name, ad "" for the region's
	scopes      map[string]limits.LimitDefinitionSummaryScopeTypeEnum
	unsupported map[string]bool // limit names whose availability is not readable
	stockouts   map[string]bool // region|ad|shape

	injected map[string][]error
	lost     int // launches that succeed but whose response is lost
	calls    map[string]int
}

// New returns an empty fake for the tenancy and the compartment: no regions.
func New(tenancy, compartment string) *Cloud {
	return &Cloud{
		now: time.Now, tenancy: tenancy, compartment: compartment,
		regions: map[string]*region{}, instances: map[string]*core.Instance{},
		attach: map[string][]core.VnicAttachment{}, vnics: map[string]core.Vnic{}, boot: map[string]bootVolume{},
		tokens: map[string]tokenRec{}, net: newNetStore(), images: map[string]*imageRec{}, imageComps: map[string]bool{}, limitValues: map[string]int64{},
		scopes: map[string]limits.LimitDefinitionSummaryScopeTypeEnum{}, unsupported: map[string]bool{},
		stockouts: map[string]bool{}, injected: map[string][]error{}, calls: map[string]int{},
	}
}

// ----- test setup -----

// SetClock sets the clock that stamps timeCreated.
func (c *Cloud) SetClock(now func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// SetPageSize makes the lists return at most n items per page (0: everything).
func (c *Cloud) SetPageSize(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pageSize = n
}

// AddRegion adds a region with the full names of its availability domains. A
// region that is not READY is listed in the subscriptions but cannot be used.
func (c *Cloud) AddRegion(name string, status identity.RegionSubscriptionStatusEnum, domains ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.regions[name] = &region{name: name, status: status, domains: domains, shapes: map[string]map[string]core.Shape{}}
	for _, ad := range domains {
		c.regions[name].shapes[ad] = map[string]core.Shape{}
	}
}

// AddShape offers the shape in every availability domain of the region, or in the
// given ones.
func (c *Cloud) AddShape(regionName string, s core.Shape, ads ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.regions[regionName]
	if r == nil || s.Shape == nil {
		return
	}
	if len(ads) == 0 {
		ads = r.domains
	}
	for _, ad := range ads {
		if _, ok := r.shapes[ad]; ok {
			r.shapes[ad][*s.Shape] = s
		}
	}
}

// SetLimit sets a Compute limit; an empty ad sets the limit of every availability
// domain of the region (and the region's own). The default is 10000.
// RemoveShape removes a shape from the table of an availability domain.
func (c *Cloud) RemoveShape(regionName, shape, ad string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.regions[regionName]; r != nil {
		delete(r.shapes[ad], shape)
	}
}

func (c *Cloud) SetLimit(regionName, ad, name string, value int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.regions[regionName]
	if ad != "" || r == nil {
		c.limitValues[regionName+"|"+ad+"|"+name] = value
		return
	}
	c.limitValues[regionName+"||"+name] = value
	for _, d := range r.domains {
		c.limitValues[regionName+"|"+d+"|"+name] = value
	}
}

// SetScope sets the scope of a limit definition (default: AD).
func (c *Cloud) SetScope(name string, scope limits.LimitDefinitionSummaryScopeTypeEnum) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.scopes[name] = scope
}

// SetUnsupported makes the availability of the limit unreadable: its definition
// says isResourceAvailabilitySupported false.
func (c *Cloud) SetUnsupported(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.unsupported[name] = true
}

// SetStockout makes the shape out of host capacity in the availability domain: a
// launch fails with the HTTP 500 of OCI.
func (c *Cloud) SetStockout(regionName, ad, shape string, on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stockouts[regionName+"|"+ad+"|"+shape] = on
}

// SetState sets the lifecycle state of an instance, as a stop or a reclaim
// outside the driver would.
func (c *Cloud) SetState(id string, state core.InstanceLifecycleStateEnum) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	in, ok := c.instances[id]
	if ok {
		in.LifecycleState = state
	}
	return ok
}

// Inject makes the next n calls of the operation (the method's name) fail with
// err.
func (c *Cloud) Inject(op string, err error, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for range n {
		c.injected[op] = append(c.injected[op], err)
	}
}

// LoseLaunchResponses makes the next n launches succeed in the fake while the
// caller sees a deadline error: the response was lost on the way back.
func (c *Cloud) LoseLaunchResponses(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lost = n
}

// Calls returns how many times the operation was called.
func (c *Cloud) Calls(op string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[op]
}

// Launches returns every launch request the fake received, accepted or not.
func (c *Cloud) Launches() []core.LaunchInstanceRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]core.LaunchInstanceRequest(nil), c.launches...)
}

// Instance returns a copy of the instance.
func (c *Cloud) Instance(id string) (core.Instance, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	in, ok := c.instances[id]
	if !ok {
		return core.Instance{}, false
	}
	return *in, true
}

// BootVolumes returns the IDs of the instances whose boot volume still exists.
func (c *Cloud) BootVolumes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.boot))
	for id := range c.boot {
		out = append(out, id)
	}
	return out
}

// ----- plumbing -----

// enter counts the call and returns the injected error of the operation, if any,
// or the context's.
func (c *Cloud) enter(ctx context.Context, op string) error {
	c.calls[op]++
	if q := c.injected[op]; len(q) > 0 {
		c.injected[op] = q[1:]
		return q[0]
	}
	return ctx.Err()
}

func (c *Cloud) nextID(kind, regionName string) string {
	c.seq++
	return "ocid1." + kind + ".oc1." + regionName + ".aaaa" + strconv.FormatUint(c.seq, 36)
}

// paged cuts a page out of items: the page token is the index of the first item.
func paged[T any](items []T, size int, token *string) (page []T, next *string) {
	start := 0
	if token != nil && *token != "" {
		if n, err := strconv.Atoi(*token); err == nil && n >= 0 && n <= len(items) {
			start = n
		}
	}
	items = items[start:]
	if size > 0 && len(items) > size {
		n := strconv.Itoa(start + size)
		return items[:size], &n
	}
	return items, nil
}

func (c *Cloud) usable(regionName string) (*region, error) {
	r := c.regions[regionName]
	if r == nil || r.status != identity.RegionSubscriptionStatusReady {
		return nil, notFound("region " + regionName)
	}
	return r, nil
}

// ----- identity -----

// ListRegionSubscriptions implements oci.IdentityAPI.
func (c *Cloud) ListRegionSubscriptions(ctx context.Context, req identity.ListRegionSubscriptionsRequest,
) (identity.ListRegionSubscriptionsResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.enter(ctx, "ListRegionSubscriptions"); err != nil {
		return identity.ListRegionSubscriptionsResponse{}, err
	}
	if req.TenancyId == nil || *req.TenancyId != c.tenancy {
		return identity.ListRegionSubscriptionsResponse{}, notFound("tenancy")
	}
	var out []identity.RegionSubscription
	for _, r := range sortedRegions(c.regions) {
		out = append(out, identity.RegionSubscription{
			RegionName: &r.name, RegionKey: &r.name, Status: r.status, IsHomeRegion: new(len(out) == 0),
		})
	}
	return identity.ListRegionSubscriptionsResponse{Items: out}, nil
}

// ListAvailabilityDomains implements oci.IdentityAPI.
func (c *Cloud) ListAvailabilityDomains(
	ctx context.Context, regionName string, req identity.ListAvailabilityDomainsRequest,
) (identity.ListAvailabilityDomainsResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.enter(ctx, "ListAvailabilityDomains"); err != nil {
		return identity.ListAvailabilityDomainsResponse{}, err
	}
	r, err := c.usable(regionName)
	if err != nil {
		return identity.ListAvailabilityDomainsResponse{}, err
	}
	if req.CompartmentId == nil || *req.CompartmentId != c.tenancy {
		return identity.ListAvailabilityDomainsResponse{}, notFound("compartment")
	}
	var out []identity.AvailabilityDomain
	for _, d := range r.domains {
		out = append(out, identity.AvailabilityDomain{Name: new(d), Id: new("ocid1.availabilitydomain.oc1.." + d)})
	}
	return identity.ListAvailabilityDomainsResponse{Items: out}, nil
}

//go:fix inline
