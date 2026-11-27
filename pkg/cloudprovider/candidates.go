/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	karpoptions "sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/utils/resources"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// candidate is one launchable offering of a NodeClaim.
type candidate struct {
	vendor       string
	apiReference string
	// compatibleStorage are the storage types the type can attach (empty: unknown).
	compatibleStorage []string
	typeName          string
	region            string
	zone              string
	capacityType      string
	price             float64
	// headroom is the quota headroom of the offering (the minimum over the
	// buckets it consumes), -1 when the quota is unknown.
	headroom int64
	it       *cloudprovider.InstanceType
	off      *cloudprovider.Offering

	// catalogPrice is what the catalog says about the offering (instance price
	// and the default settings' delta); priced is false when it knows nothing.
	catalogPrice catalog.OfferingPrice
	priced       bool
	// sel are the profiles this launch applies and settings the resolved
	// vendor settings; usd is the real price with them, complete whether it has
	// every component (00a section 10).
	sel      options.Selection
	settings vendors.VendorSettings
	usd      float64
	complete bool
	// capacity and allocatable are the resources the node launches with: the
	// instance type's, except that a NodeClass with rootVolume sizes the root
	// volume at launch, and the ephemeral storage follows it.
	capacity, allocatable corev1.ResourceList
}

// priceEpsilon is the difference below which two prices are a tie.
const priceEpsilon = 1e-9

// candidates computes the offerings a NodeClaim may launch into (overview
// section 4.1 step 2): the instance types of its requirements crossed with their
// available offerings that satisfy its zone, region and capacity-type
// requirements, for the vendors that are ready and whose instance fits the
// resource requests, ordered by price. Ties are broken by the larger quota
// headroom, then by instance type name, zone, region and capacity type, so the
// order is deterministic.
func (c *CloudProvider) candidates(
	ctx context.Context, nc *v1alpha1.SCNodeClass, nodeClaim *karpv1.NodeClaim, res *settingsResolver,
) ([]candidate, error) {
	reqs := scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...)
	its, err := c.opts.Catalog.InstanceTypes(ctx, catalog.Query{Class: nc.Name, Filter: catalog.NewFilter(reqs)})
	if err != nil {
		return nil, fmt.Errorf("resolving instance types: %w", err)
	}
	// NodeOverlays apply to the types of a NodePool, like core does for
	// GetInstanceTypes (the AWS provider does the same in Create).
	if c.opts.InstanceTypeStore != nil && karpoptions.FromContext(ctx).FeatureGates.NodeOverlay {
		if pool := nodeClaim.Labels[karpv1.NodePoolLabelKey]; pool != "" {
			if its, err = c.opts.InstanceTypeStore.ApplyAll(pool, its); err != nil {
				return nil, fmt.Errorf("applying node overlays: %w", err)
			}
		}
	}
	ready := readyVendors(nc)
	var out []candidate
	for _, it := range its {
		ref, ok := c.opts.Catalog.Lookup(it.Name)
		if !ok || !ready[ref.Vendor] {
			continue
		}
		if _, enabled := c.opts.Registry.Driver(ref.Vendor); !enabled {
			continue
		}
		// Core only intersects type requirements (a key the type does not
		// define passes); offerings are checked with IsCompatible below.
		if it.Requirements.Intersects(reqs) != nil || !resources.Fits(nodeClaim.Spec.Resources.Requests, it.Allocatable()) {
			continue
		}
		for _, off := range it.Offerings {
			if !off.Available || !reqs.IsCompatible(off.Requirements, scheduling.AllowUndefinedWellKnownLabels) {
				continue
			}
			cand := candidate{
				vendor: ref.Vendor, apiReference: ref.APIReference, compatibleStorage: ref.CompatibleStorage,
				typeName: it.Name,
				region:   off.Requirements.Get(corev1.LabelTopologyRegion).Any(), zone: off.Zone(),
				capacityType: off.CapacityType(), price: off.Price, headroom: -1, it: it, off: off,
				capacity: it.Capacity, allocatable: it.Allocatable(),
			}
			if !c.settle(ctx, nc, res, reqs, nodeClaim.Spec.Resources.Requests, &cand) {
				continue
			}
			if c.opts.Quota != nil {
				if h, known := c.opts.Quota.Headroom(cand.vendor, cand.region, cand.apiReference, cand.capacityType); known {
					cand.headroom = h
				}
			}
			out = append(out, cand)
		}
	}
	slices.SortFunc(out, compareCandidates)
	return out, nil
}

// settle selects the profiles of the NodeClaim for the offering, resolves the
// settings and prices them: the candidate is ordered by the price with the
// selected profiles (overview section 4.1 step 2). It reports false for an
// offering the NodeClaim cannot use: a profile that cannot be resolved, or an
// incomplete price where the NodeClass does not allow it. With the NodeClass's
// rootVolume the settings carry the root disk the requests need (sizeRoot).
func (c *CloudProvider) settle(
	ctx context.Context, nc *v1alpha1.SCNodeClass, res *settingsResolver, reqs scheduling.Requirements,
	requests corev1.ResourceList, cand *candidate,
) bool {
	logger := log.FromContext(ctx)
	if p, ok := c.opts.Catalog.Price(nc.Name, cand.typeName, cand.region, cand.zone, cand.capacityType); ok {
		cand.catalogPrice, cand.priced = p, true
	} else {
		cand.catalogPrice = catalog.OfferingPrice{USD: cand.price, Base: cand.price, Complete: true}
	}
	sel, ok := options.Select(reqs, cand.it.Requirements, cand.off.Requirements)
	if !ok {
		logger.V(1).Info("the profile requirements match no profile of the offering",
			"type", cand.typeName, "zone", cand.zone)
		return false
	}
	settings, err := res.settings(cand.vendor, sel)
	if err != nil {
		logger.Error(err, "resolving the settings of an offering", "type", cand.typeName, "vendor", cand.vendor)
		return false
	}
	settings = c.sizeRoot(nc, res, requests, cand, settings)
	usd, delta, complete := res.priceOf(cand, settings, sel)
	cand.sel, cand.settings, cand.usd, cand.complete = sel, settings, usd, complete
	if !complete && !allowsIncomplete(nc) {
		return false
	}
	// Ordering uses the price the scheduler saw (the catalog's, which may be a
	// pinned objective's) adjusted by what the selected profiles cost on top of
	// the NodeClass defaults.
	cand.price += delta - cand.catalogPrice.Delta
	return true
}

func allowsIncomplete(nc *v1alpha1.SCNodeClass) bool {
	return nc.Spec.Pricing.AllowIncomplete == nil || *nc.Spec.Pricing.AllowIncomplete
}

// compareCandidates orders candidates by price, then by larger quota headroom
// (an unknown headroom sorts after a known one), then by type name, zone, region
// and capacity type.
func compareCandidates(a, b candidate) int {
	if math.Abs(a.price-b.price) > priceEpsilon {
		return cmp.Compare(a.price, b.price)
	}
	if a.headroom != b.headroom {
		return cmp.Compare(b.headroom, a.headroom)
	}
	return cmp.Or(
		cmp.Compare(a.typeName, b.typeName),
		cmp.Compare(a.zone, b.zone),
		cmp.Compare(a.region, b.region),
		cmp.Compare(a.capacityType, b.capacityType),
	)
}

// sizeRoot applies the NodeClass's rootVolume to the settings of a candidate:
// the root disk is the size its vendor block sets, grown to what the ephemeral
// storage of requests needs (the catalog advertised the type with the disk it
// can grow to, so the requests may exceed the configured size); the driver
// limits it to the vendor's maximum, and a root disk the type fixes stays. The
// candidate's capacity and allocatable then carry the ephemeral storage of
// the disk it launches with, not the advertised one. Without rootVolume, or for
// a driver that does not say its root disk, nothing changes.
func (c *CloudProvider) sizeRoot(
	nc *v1alpha1.SCNodeClass, res *settingsResolver, requests corev1.ResourceList, cand *candidate,
	s vendors.VendorSettings,
) vendors.VendorSettings {
	no, ok := res.nodeOptions(cand.vendor)
	if nc.Spec.RootVolume == nil || !ok {
		return s
	}
	if need, ok := c.opts.Catalog.RootDiskFor(nc.Name, cand.typeName, requests); ok {
		s = options.WithMinRoot(no, s, need)
	}
	root := s.RootDiskBytes
	if b, ok := no.RootDiskForType(s, catalogTypeOf(cand)); ok && b > 0 {
		root = b
	}
	if root <= 0 {
		return s
	}
	if capacity, allocatable, ok := c.opts.Catalog.ResourcesWithRoot(nc.Name, cand.typeName, root); ok {
		cand.capacity, cand.allocatable = capacity, allocatable
	}
	return s
}
