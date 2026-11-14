/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/oracle/oci-go-sdk/v65/limits"
)

// defaultLimit is the limit of a name nobody set.
const defaultLimit = 10000

func (c *Cloud) scope(name string) limits.LimitDefinitionSummaryScopeTypeEnum {
	if s, ok := c.scopes[name]; ok {
		return s
	}
	return limits.LimitDefinitionSummaryScopeTypeAd
}

// limit returns the limit of a name in an AD (or the region's, ad "").
func (c *Cloud) limit(regionName, ad, name string) int64 {
	if v, ok := c.limitValues[regionName+"|"+ad+"|"+name]; ok {
		return v
	}
	return defaultLimit
}

// consumes is what an instance of the shape and size consumes of a limit: the
// OCPUs for *-core-count, the GB for *-memory-count, one for any other name. The
// same for preemptible instances (UNVERIFIED, spike S9).
func consumes(name string, ocpus, mem float64) int64 {
	switch {
	case strings.HasSuffix(name, "-core-count"):
		return int64(ocpus)
	case strings.HasSuffix(name, "-memory-count"):
		return int64(mem)
	}
	return 1
}

// holdsLimit says whether an instance in the state counts against the limits.
func holdsLimit(s core.InstanceLifecycleStateEnum) bool {
	return s != core.InstanceLifecycleStateTerminated && s != core.InstanceLifecycleStateStopped
}

// usedIn is the usage of a limit in an AD (or the whole region, ad ""): the
// instances of the tenancy, whatever compartment they are in.
func (c *Cloud) usedIn(regionName, ad, name string) int64 {
	var used int64
	for _, in := range c.instances {
		if deref(in.Region) != regionName || (ad != "" && deref(in.AvailabilityDomain) != ad) ||
			!holdsLimit(in.LifecycleState) {
			continue
		}
		si, ok := c.regions[regionName].shapes[deref(in.AvailabilityDomain)][deref(in.Shape)]
		if !ok || !slices.Contains(si.QuotaNames, name) {
			continue
		}
		used += consumes(name, float64(deref(in.ShapeConfig.Ocpus)), float64(deref(in.ShapeConfig.MemoryInGBs)))
	}
	return used
}

// checkLimits fails the launch with LimitExceeded when a limit of the shape
// cannot take the instance.
func (c *Cloud) checkLimits(regionName, ad string, shape core.Shape, ocpus, mem float64) error {
	var over []string
	for _, name := range shape.QuotaNames {
		where := ad
		if c.scope(name) != limits.LimitDefinitionSummaryScopeTypeAd {
			where = ""
		}
		if c.usedIn(regionName, where, name)+consumes(name, ocpus, mem) > c.limit(regionName, where, name) {
			over = append(over, name)
		}
	}
	if len(over) == 0 {
		return nil
	}
	return Error(400, "LimitExceeded", "The following service limits were exceeded: "+strings.Join(over, ", ")+
		". Request a service limit increase from the service limits page in the console.")
}

// quotaNames are the limit names of the region's shapes.
func (r *region) quotaNames() []string {
	set := map[string]bool{}
	for _, shapes := range r.shapes {
		for _, s := range shapes {
			for _, n := range s.QuotaNames {
				set[n] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// GetResourceAvailability implements oci.LimitsAPI: used and available computed
// from the fake's instances, in an AD or (without one) in the region.
func (c *Cloud) GetResourceAvailability(ctx context.Context, regionName string,
	req limits.GetResourceAvailabilityRequest,
) (limits.GetResourceAvailabilityResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.enter(ctx, "GetResourceAvailability"); err != nil {
		return limits.GetResourceAvailabilityResponse{}, err
	}
	r, err := c.usable(regionName)
	if err != nil {
		return limits.GetResourceAvailabilityResponse{}, err
	}
	name := deref(req.LimitName)
	if deref(req.ServiceName) != "compute" || !slices.Contains(r.quotaNames(), name) {
		return limits.GetResourceAvailabilityResponse{}, notFound("limit " + name)
	}
	ad := deref(req.AvailabilityDomain)
	if (c.scope(name) == limits.LimitDefinitionSummaryScopeTypeAd) != (ad != "") {
		return limits.GetResourceAvailabilityResponse{}, invalid("The limit %s is %s scoped", name, c.scope(name))
	}
	used := c.usedIn(regionName, ad, name)
	avail := max(c.limit(regionName, ad, name)-used, 0)
	return limits.GetResourceAvailabilityResponse{
		ResourceAvailability: limits.ResourceAvailability{Used: &used, Available: &avail},
	}, nil
}

// ListLimitDefinitions implements oci.LimitsAPI: the limits of the region's shapes.
func (c *Cloud) ListLimitDefinitions(ctx context.Context, regionName string, req limits.ListLimitDefinitionsRequest,
) (limits.ListLimitDefinitionsResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.enter(ctx, "ListLimitDefinitions"); err != nil {
		return limits.ListLimitDefinitionsResponse{}, err
	}
	r, err := c.usable(regionName)
	if err != nil {
		return limits.ListLimitDefinitionsResponse{}, err
	}
	if deref(req.CompartmentId) != c.tenancy {
		return limits.ListLimitDefinitionsResponse{}, notFound("compartment")
	}
	var all []limits.LimitDefinitionSummary
	for _, n := range r.quotaNames() {
		all = append(all, limits.LimitDefinitionSummary{
			Name: new(n), ServiceName: new("compute"), ScopeType: c.scope(n),
			IsResourceAvailabilitySupported: new(!c.unsupported[n]),
		})
	}
	page, next := paged(all, c.pageSize, req.Page)
	return limits.ListLimitDefinitionsResponse{Items: page, OpcNextPage: next}, nil
}
