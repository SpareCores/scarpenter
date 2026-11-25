/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/labels"
	"github.com/SpareCores/scarpenter/pkg/scope"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// listedInstance is an instance of the cluster with the vendor that owns it.
type listedInstance struct {
	vendor   string
	instance *vendors.Instance
}

// instanceCache is the List cache (overview section 4): every instance of the
// cluster, read from the drivers at most every TTL. Create and Delete update it
// in place so that a Get right after them is right; a deleted instance stays
// hidden until the next listing has had time to forget it.
type instanceCache struct {
	mu       sync.Mutex
	listedAt time.Time
	byID     map[string]listedInstance // by provider ID
	dropped  map[string]time.Time
	// seen are the vendor/region pairs that were listed once; failedAt is when
	// the listing of a region last failed.
	seen     map[string]bool
	failedAt map[string]time.Time
}

func (c *CloudProvider) putInstance(vendor string, inst *vendors.Instance) {
	id := scope.ProviderID(vendor, inst.Region, inst.Tags[vendors.TagNodeClaim])
	c.cache.mu.Lock()
	defer c.cache.mu.Unlock()
	if c.cache.byID == nil {
		return
	}
	delete(c.cache.dropped, id)
	c.cache.byID[id] = listedInstance{vendor: vendor, instance: inst}
}

func (c *CloudProvider) dropInstance(providerID string) {
	c.cache.mu.Lock()
	defer c.cache.mu.Unlock()
	if c.cache.dropped == nil {
		c.cache.dropped = map[string]time.Time{}
	}
	c.cache.dropped[providerID] = c.opts.Clock.Now()
	delete(c.cache.byID, providerID)
}

// instances returns the cached instances by provider ID, listing them first when
// the cache is older than its TTL or force is set. Callers must not modify the
// result.
func (c *CloudProvider) instances(ctx context.Context, force bool) (map[string]listedInstance, error) {
	c.listMu.Lock()
	defer c.listMu.Unlock() // one listing at a time; the others wait for its result

	c.cache.mu.Lock()
	fresh := c.cache.byID != nil && c.opts.Clock.Since(c.cache.listedAt) < c.opts.InstanceCacheTTL
	if fresh && !force {
		out := c.snapshotLocked()
		c.cache.mu.Unlock()
		return out, nil
	}
	c.cache.mu.Unlock()

	regions, err := c.opts.Scope.InUse(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolving the regions in use: %w", err)
	}
	listed := map[string]listedInstance{}
	started := c.opts.Clock.Now()
	for _, vendor := range c.opts.Registry.Vendors() {
		if len(regions[vendor]) == 0 {
			continue
		}
		drv, _ := c.opts.Registry.Driver(vendor)
		results := c.listVendor(ctx, drv, regions[vendor], started)
		if err := ctx.Err(); err != nil {
			return nil, err // what was listed so far is partial: do not cache it
		}
		for _, res := range results {
			if res.err != nil {
				if err := c.keepListed(ctx, listed, vendor, res, started); err != nil {
					return nil, err
				}
				continue
			}
			c.markListed(vendor, res.regions)
			for _, inst := range res.insts {
				name := inst.Tags[vendors.TagNodeClaim]
				if inst.Gone() || name == "" {
					continue
				}
				listed[scope.ProviderID(vendor, inst.Region, name)] = listedInstance{vendor: vendor, instance: inst}
			}
		}
	}

	c.cache.mu.Lock()
	defer c.cache.mu.Unlock()
	for id, at := range c.cache.dropped {
		if c.opts.Clock.Since(at) > 2*c.opts.InstanceCacheTTL {
			delete(c.cache.dropped, id)
		}
	}
	// What was dropped while this listing ran, or less than a TTL ago, may still
	// be in the vendor's answer.
	for id, at := range c.cache.dropped {
		if at.After(started.Add(-c.opts.InstanceCacheTTL)) {
			delete(listed, id)
		}
	}
	c.cache.byID, c.cache.listedAt = listed, c.opts.Clock.Now()
	return c.snapshotLocked(), nil
}

// Listing limits: how many regions are listed at once, and how long a region
// that failed is left alone (its last-known instances are kept meanwhile).
const (
	maxParallelListings = 8
	listRetryAfter      = 3 * time.Minute
)

var errListedRecently = errors.New("the listing of the region failed recently, not asked again yet")

// regionListing is the answer of a driver for a group of regions: one region
// for a driver that lists per region, else all of them in one call.
type regionListing struct {
	regions []string
	insts   []*vendors.Instance
	err     error
}

// listVendor asks the driver for the instances of the regions. A driver that
// lists per region (vendors.RegionLister) is asked one region at a time, in
// parallel, so that a slow or failing region does not hide the others; the
// others list the whole account in one call, which is not repeated per region.
func (c *CloudProvider) listVendor(
	ctx context.Context, drv vendors.Driver, regions []string, now time.Time,
) []regionListing {
	groups := [][]string{regions}
	if _, ok := drv.(vendors.RegionLister); ok {
		groups = groups[:0]
		for _, r := range regions {
			groups = append(groups, []string{r})
		}
	}
	tags := map[vendors.TagKey]string{vendors.TagCluster: c.opts.ClusterName}
	results := make([]regionListing, len(groups))
	sem := make(chan struct{}, maxParallelListings)
	var wg sync.WaitGroup
	for i, group := range groups {
		results[i].regions = group
		if c.failedRecently(drv.Vendor(), group, now) {
			results[i].err = errListedRecently
			continue
		}
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i].insts, results[i].err = drv.ListInstances(ctx, group, tags)
		})
	}
	wg.Wait()
	return results
}

func regionKey(vendor, region string) string { return vendor + "/" + region }

func (c *CloudProvider) failedRecently(vendor string, regions []string, now time.Time) bool {
	c.cache.mu.Lock()
	defer c.cache.mu.Unlock()
	for _, r := range regions {
		if at, ok := c.cache.failedAt[regionKey(vendor, r)]; ok && now.Sub(at) < listRetryAfter {
			return true
		}
	}
	return false
}

// markListed records that the regions were listed, so that they are known (an
// empty region is empty, not unknown).
func (c *CloudProvider) markListed(vendor string, regions []string) {
	c.cache.mu.Lock()
	defer c.cache.mu.Unlock()
	if c.cache.seen == nil {
		c.cache.seen = map[string]bool{}
	}
	for _, r := range regions {
		c.cache.seen[regionKey(vendor, r)] = true
		delete(c.cache.failedAt, regionKey(vendor, r))
	}
}

// keepListed handles regions that could not be listed: their instances of the
// previous listing stay in listed, so that core's garbage collector (which
// deletes the registered NodeClaims missing from List) does not take them for
// gone. A region that was never listed has nothing to keep: it counts as empty
// unless a NodeClaim of the vendor lives in it, then the listing fails.
func (c *CloudProvider) keepListed(
	ctx context.Context, listed map[string]listedInstance, vendor string, res regionListing, now time.Time,
) error {
	logger := log.FromContext(ctx)
	for _, region := range res.regions {
		key := regionKey(vendor, region)
		c.cache.mu.Lock()
		seen := c.cache.seen[key]
		c.cache.mu.Unlock()
		if !seen {
			has, err := c.hasNodeClaimIn(ctx, vendor, region)
			if err != nil {
				return fmt.Errorf("checking the NodeClaims of %s in %s: %w", vendor, region, err)
			}
			if has {
				// Not "failed recently" any more: the next listing asks again.
				c.cache.mu.Lock()
				delete(c.cache.failedAt, key)
				c.cache.mu.Unlock()
				return fmt.Errorf("listing the instances of vendor %s in %s: %w", vendor, region, res.err)
			}
		}
		c.cache.mu.Lock()
		kept := 0
		for id, li := range c.cache.byID {
			if li.vendor == vendor && li.instance.Region == region {
				listed[id] = li
				kept++
			}
		}
		if c.cache.failedAt == nil {
			c.cache.failedAt = map[string]time.Time{}
		}
		_, failing := c.cache.failedAt[key]
		if !errors.Is(res.err, errListedRecently) {
			c.cache.failedAt[key] = now
		}
		c.cache.mu.Unlock()
		if failing {
			logger.V(1).Info("a region still cannot be listed, keeping the instances of the last listing",
				"vendor", vendor, "region", region, "kept", kept, "error", res.err.Error())
		} else {
			logger.Info("listing the instances of a region failed, keeping the ones of the last listing",
				"vendor", vendor, "region", region, "kept", kept, "error", res.err.Error())
		}
	}
	return nil
}

// hasNodeClaimIn says whether a NodeClaim of the vendor lives in the region.
func (c *CloudProvider) hasNodeClaimIn(ctx context.Context, vendor, region string) (bool, error) {
	var claims karpv1.NodeClaimList
	if err := c.opts.Client.List(ctx, &claims); err != nil {
		return false, err
	}
	for i := range claims.Items {
		v, r, _, err := scope.ParseProviderID(claims.Items[i].Status.ProviderID)
		if err == nil && v == vendor && r == region {
			return true, nil
		}
	}
	return false, nil
}

func (c *CloudProvider) snapshotLocked() map[string]listedInstance {
	out := make(map[string]listedInstance, len(c.cache.byID))
	maps.Copy(out, c.cache.byID)
	return out
}

// instanceToNodeClaim converts an instance to the NodeClaim core expects from
// Get and List: provider ID, capacity and allocatable of its type, the labels of
// the instance type and offering, and the annotations.
func (c *CloudProvider) instanceToNodeClaim(vendor string, inst *vendors.Instance) *karpv1.NodeClaim {
	name := inst.Tags[vendors.TagNodeClaim]
	nc := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: metav1.NewTime(inst.CreatedAt),
			Labels:            map[string]string{},
			Annotations:       map[string]string{v1alpha1.AnnotationInstanceRef: string(inst.Ref)},
		},
		Status: karpv1.NodeClaimStatus{ProviderID: scope.ProviderID(vendor, inst.Region, name)},
	}
	setBillingStart(nc.Annotations, inst)
	typeName := catalog.InstanceTypeName(vendor, inst.Type)
	it, ok := c.opts.Catalog.Type(inst.Tags[vendors.TagNodeClass], typeName)
	if !ok {
		return nc
	}
	nc.Status.Capacity = it.Capacity
	nc.Status.Allocatable = it.Allocatable()
	var off *cloudprovider.Offering
	for _, o := range it.Offerings {
		if o.Zone() == inst.Zone && o.CapacityType() == inst.CapacityType {
			off = o
			break
		}
	}
	nc.Labels = nodeLabels(name, it, off, nil, "")
	if inst.Zone != "" {
		nc.Labels[corev1.LabelTopologyZone] = inst.Zone
	}
	nc.Labels[corev1.LabelTopologyRegion] = inst.Region
	nc.Labels[karpv1.CapacityTypeLabelKey] = inst.CapacityType
	return nc
}

// nodeLabels are the labels Create returns for the chosen type and offering
// (overview section 4.1 step 6): every requirement of the type and the offering
// that has exactly one value, the node name, the profiles applied (profile
// kind to name, "default" where none; nil stamps none, for the NodeClaims of
// Get and List, which do not know them) and the placement run key. Core copies
// them to the Node after registration, and it must find a concrete label for
// every key a pod selects on (00a section 2).
func nodeLabels(
	nodeName string, it *cloudprovider.InstanceType, off *cloudprovider.Offering, applied map[string]string, runKey string,
) map[string]string {
	out := map[string]string{
		corev1.LabelInstanceTypeStable: it.Name,
		corev1.LabelHostname:           nodeName,
	}
	add := func(reqs scheduling.Requirements) {
		for key, r := range reqs {
			if r.Len() == 1 && r.Operator() == corev1.NodeSelectorOpIn {
				out[key] = r.Values()[0]
			}
		}
	}
	add(it.Requirements)
	if off != nil {
		add(off.Requirements)
	}
	for _, kind := range labels.ProfileKinds {
		if name, ok := applied[string(kind)]; ok {
			out[labels.ProfileLabel(kind)] = name
		}
	}
	if runKey != "" {
		out[v1alpha1.LabelPlacementGroup] = runKey
	}
	return out
}

// setBillingStart records the vendor's billing-start stamp as the annotation
// the billing retention controller reads (RFC 3339, to the second: truncating
// only makes the stamp earlier, the safe side). An instance without a stamp
// gets none.
func setBillingStart(annotations map[string]string, inst *vendors.Instance) {
	if !inst.BillingStart.IsZero() {
		annotations[v1alpha1.AnnotationBillingStart] = inst.BillingStart.UTC().Format(time.RFC3339)
	}
}

// setPublicIPs puts the public addresses a created instance already has on the
// NodeClaim; the garbage collector keeps them up to date and fills in what is
// assigned later.
func setPublicIPs(annotations map[string]string, inst *vendors.Instance) {
	if inst.PublicUnknown {
		return
	}
	if inst.PublicIPv4 != "" {
		annotations[v1alpha1.AnnotationPublicIPv4] = inst.PublicIPv4
	}
	if inst.PublicIPv6 != "" {
		annotations[v1alpha1.AnnotationPublicIPv6] = inst.PublicIPv6
	}
}

func formatPrice(p float64) string { return strconv.FormatFloat(p, 'f', -1, 64) }
