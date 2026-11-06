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
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	karpoptions "sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/utils/resources"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog"
)

// candidate is one launchable offering of a NodeClaim.
type candidate struct {
	vendor       string
	apiReference string
	typeName     string
	region       string
	zone         string
	capacityType string
	price        float64
	// headroom is the quota headroom of the offering (the minimum over the
	// buckets it consumes), -1 when the quota is unknown.
	headroom int64
	it       *cloudprovider.InstanceType
	off      *cloudprovider.Offering
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
	ctx context.Context, nc *v1alpha1.SCNodeClass, nodeClaim *karpv1.NodeClaim,
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
				vendor: ref.Vendor, apiReference: ref.APIReference, typeName: it.Name,
				region: off.Requirements.Get(corev1.LabelTopologyRegion).Any(), zone: off.Zone(),
				capacityType: off.CapacityType(), price: off.Price, headroom: -1, it: it, off: off,
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
