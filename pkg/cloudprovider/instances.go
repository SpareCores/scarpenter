/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
		clusterTag := map[vendors.TagKey]string{vendors.TagCluster: c.opts.ClusterName}
		insts, err := drv.ListInstances(ctx, regions[vendor], clusterTag)
		if err != nil {
			return nil, fmt.Errorf("listing the instances of vendor %s: %w", vendor, err)
		}
		for _, inst := range insts {
			name := inst.Tags[vendors.TagNodeClaim]
			if inst.Gone() || name == "" {
				continue
			}
			listed[scope.ProviderID(vendor, inst.Region, name)] = listedInstance{vendor: vendor, instance: inst}
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
	c.cache.byID, c.cache.listedAt = listed, started
	return c.snapshotLocked(), nil
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

func formatPrice(p float64) string { return strconv.FormatFloat(p, 'f', -1, 64) }
