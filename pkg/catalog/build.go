/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog

import (
	"cmp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog/labels"
	"github.com/SpareCores/scarpenter/pkg/catalog/model"
	"github.com/SpareCores/scarpenter/pkg/nvidia"
)

// builtType is an instance type of a vendor for a NodeClass, with what the
// per-query steps (pre-filter, availability, pricing) need about each of its
// offerings. The InstanceType itself is never modified after the build: queries
// that change availability or prices work on copies.
type builtType struct {
	it     *cloudprovider.InstanceType
	server *model.Server
	// offerings parallels it.Offerings.
	offerings []offeringInfo
	// removed are the offerings of the type the build left out, with the reason.
	removed []offeringDrop
}

type offeringInfo struct {
	region       string
	zone         string
	capacityType string
	// usd is the hourly price: instance price plus the settings' price delta.
	usd float64
	// base is the instance price, delta the default settings' price delta and
	// complete says whether the price has every component (00a section 10).
	base, delta float64
	complete    bool
}

type vendorBuild struct {
	types  []*builtType
	byName map[string]*builtType
	// drops are the servers of the vendor left out of the catalog, with the
	// reason, in server order.
	drops []typeDrop
}

// vendorBuild returns the instance types of a vendor for a NodeClass, built
// once per snapshot, class and facts epoch.
func (c *Catalog) vendorBuild(snap *snapshot, class *Class, vendor string) *vendorBuild {
	epoch := c.opts.Facts.Epoch()
	snap.mu.Lock()
	if snap.buildEpoch != epoch {
		snap.builds = map[buildKey]*vendorBuild{}
		snap.buildEpoch = epoch
	}
	if vb, ok := snap.builds[buildKey{class.Hash, vendor}]; ok {
		snap.mu.Unlock()
		return vb
	}
	snap.mu.Unlock()

	vb := c.buildVendor(snap, class, vendor)

	snap.mu.Lock()
	defer snap.mu.Unlock()
	if existing, ok := snap.builds[buildKey{class.Hash, vendor}]; ok {
		return existing
	}
	snap.builds[buildKey{class.Hash, vendor}] = vb
	return vb
}

// regionOfferings are the shared requirements of every offering in a region.
type regionInfo struct {
	region   *model.Region
	location []*scheduling.Requirement
}

// buildVendor maps the data of one vendor to instance types (overview sections
// 5.3 to 5.5).
func (c *Catalog) buildVendor(snap *snapshot, class *Class, vendor string) *vendorBuild {
	vd := snap.vendors[vendor]
	vb := &vendorBuild{byName: map[string]*builtType{}}
	if vd == nil {
		return vb
	}
	facts := c.opts.Facts
	launchable, _ := facts.(LaunchableFacts)
	virtualGPU, _ := facts.(VirtualGPUFacts)
	regionPriced := false
	if rp, ok := facts.(RegionPricedFacts); ok {
		regionPriced = rp.RegionPriced(vendor)
	}
	zonesByRegion, hasZones := facts.Zones(vendor)
	view := snap.view(class)
	allowed := class.Vendors[vendor]

	// Core checks offerings with Requirements.IsCompatible, which rejects an
	// offering that defines a key core does not know as well-known ("label does
	// not have known values"): an unregistered metric-<m>-per-usd key would
	// exclude every benchmarked type. Core only intersects type requirements,
	// ignoring unknown keys, so an unregistered type-level key would be
	// unselectable. Either way only registered keys are stamped. The metrics of a class that were added
	// after start-up wait for a restart (RestartRequired, 00a section 7.2).
	flags := labels.RegisteredFlags(class.CPUFlags)
	metricNames := labels.RegisteredMetrics(class.MetricNames())

	regions := map[string]*regionInfo{}
	for id, r := range vd.regions {
		if !r.Active || !regionAllowed(r, allowed) {
			continue
		}
		regions[id] = &regionInfo{region: r, location: labels.Location(r)}
	}

	sharedReqs := map[string]scheduling.Requirements{}
	zoneReq := map[Zone]*scheduling.Requirement{}
	zoneIDReq := map[string]*scheduling.Requirement{}
	regionReq := map[string]*scheduling.Requirement{}
	capReq := map[string]*scheduling.Requirement{
		model.CapacityOnDemand: labels.In(karpv1.CapacityTypeLabelKey, model.CapacityOnDemand),
		model.CapacitySpot:     labels.In(karpv1.CapacityTypeLabelKey, model.CapacitySpot),
	}

	for _, srv := range vd.servers {
		drop := func(code Code, detail string) {
			vb.drops = append(vb.drops, typeDrop{vendor, srv.APIReference, Reason{Code: code, Detail: detail}})
		}
		prices := vd.prices[srv.ID]
		if code, detail := typeRefusal(facts, vendor, srv, prices); code != "" {
			drop(code, detail)
			continue
		}
		gpu, reason := gpuSupport(srv, class, vendor, virtualGPU)
		if reason != nvidia.Supported {
			drop(Code(reason), "")
			continue
		}
		if !c.opts.NVIDIAGPUs {
			gpu = nil
		}
		name := InstanceTypeName(vendor, srv.APIReference)
		set := c.offeringsOf(vd, srv, prices, regions, zonesByRegion, hasZones, regionPriced, class, vendor)
		offs, removed := withoutUnlaunchable(vendor, srv, set, launchable)
		if len(offs) == 0 {
			code, detail := emptyReason(set, removed)
			drop(code, detail)
			continue
		}

		// Offerings, their requirements and the type-level unions.
		bt := &builtType{server: srv, removed: removed}
		it := &cloudprovider.InstanceType{Name: name}
		var regionNames, zoneNames, zoneIDs, capTypes []string
		// What the driver supports per zone; the type's labels are the union.
		perZone := map[string]map[labels.ProfileKind][]string{}
		union := map[labels.ProfileKind][]string{}
		for _, o := range offs {
			if _, seen := perZone[o.zone.Name]; seen {
				continue
			}
			sup := facts.Supported(vendor, class, srv, o.zone.Name)
			perZone[o.zone.Name] = sup
			for k, names := range sup {
				union[k] = append(union[k], names...)
			}
		}
		for k := range union {
			union[k] = uniq(union[k])
		}
		// A type that cannot host the default settings is offered with a
		// storage profile that makes it work, else not at all.
		defaultOK := defaultRouteSupported(facts, vendor, class, srv)
		if !defaultOK && len(union[labels.ProfileStorage]) == 0 {
			drop(StorageIncompatible, "")
			continue
		}
		for _, o := range offs {
			regionNames = append(regionNames, o.region.region.Name)
			zoneNames = append(zoneNames, o.zone.Name)
			zoneIDs = append(zoneIDs, o.zone.ID)
			capTypes = append(capTypes, o.capacityType)

			rr, ok := regionReq[o.region.region.Name]
			if !ok {
				rr = labels.In(corev1.LabelTopologyRegion, o.region.region.Name)
				regionReq[o.region.region.Name] = rr
			}
			zr, ok := zoneReq[o.zone]
			if !ok {
				zr = labels.In(corev1.LabelTopologyZone, o.zone.Name)
				zoneReq[o.zone] = zr
			}
			zir, ok := zoneIDReq[o.zone.ID]
			if !ok {
				zir = labels.In(v1alpha1.LabelZoneID, o.zone.ID)
				zoneIDReq[o.zone.ID] = zir
			}
			perZoneProfiles := !sameProfiles(perZone[o.zone.Name], union)
			// Without metrics an offering's requirements depend only on where
			// it is, so offerings share one map (nothing mutates them).
			shareKey := ""
			if len(metricNames) == 0 {
				shareKey = o.capacityType + "|" + o.region.region.Name + "|" + o.zone.ID + "|" + o.zone.Name
				if perZoneProfiles {
					shareKey += "|" + profilesKey(perZone[o.zone.Name])
				}
				if shared, ok := sharedReqs[shareKey]; ok {
					it.Offerings = append(it.Offerings, &cloudprovider.Offering{Requirements: shared, Price: o.usd, Available: true})
					bt.offerings = append(bt.offerings, o.info())
					continue
				}
			}
			reqs := make([]*scheduling.Requirement, 0, 12+len(metricNames))
			reqs = append(reqs, capReq[o.capacityType], zr, rr, zir)
			reqs = append(reqs, o.region.location...)
			for _, mname := range metricNames {
				if mv := view.metrics[mname]; mv != nil {
					reqs = append(reqs, mv.PerUSDRequirement(srv.Key(), o.usd))
				} else {
					reqs = append(reqs, labels.Absent(v1alpha1.MetricPerUSDLabel(mname)))
				}
			}
			// Per-zone profile labels only where they differ from the type's
			// union, so that a profile a zone cannot host never matches there;
			// with the default only they never do.
			if perZoneProfiles {
				reqs = append(reqs, labels.ProfileRequirements(perZone[o.zone.Name], defaultOK)...)
			}
			offReqs := scheduling.NewRequirements(reqs...)
			if shareKey != "" {
				sharedReqs[shareKey] = offReqs
			}
			it.Offerings = append(it.Offerings, &cloudprovider.Offering{
				Requirements: offReqs, Price: o.usd, Available: true,
			})
			bt.offerings = append(bt.offerings, o.info())
		}

		reqs := []*scheduling.Requirement{
			labels.In(corev1.LabelInstanceTypeStable, name),
			labels.In(corev1.LabelArchStable, srv.Arch),
			labels.In(corev1.LabelOSStable, string(corev1.Linux)),
			labels.In(corev1.LabelTopologyRegion, uniq(regionNames)...),
			labels.In(corev1.LabelTopologyZone, uniq(zoneNames)...),
			labels.In(v1alpha1.LabelZoneID, uniq(zoneIDs)...),
			labels.In(karpv1.CapacityTypeLabelKey, uniq(capTypes)...),
		}
		reqs = append(reqs, labels.Attributes(srv, flags)...)
		reqs = append(reqs, labels.NVIDIADriver(gpu)...)
		for _, mname := range metricNames {
			if mv := view.metrics[mname]; mv != nil {
				reqs = append(reqs, mv.TypeRequirements(srv.Key())...)
			} else {
				reqs = append(reqs, labels.AbsentMetric(mname)...)
			}
		}
		reqs = append(reqs, labels.Selection(view.members[srv.Key()]))
		reqs = append(reqs, labels.ProfileRequirements(union, defaultOK)...)
		it.Requirements = scheduling.NewRequirements(reqs...)

		rootDisk, rootKnown := facts.RootDiskBytes(vendor, class, srv)
		it.Capacity = capacity(srv.VCPUs, srv.MemoryMiB, class, rootDisk, rootKnown, gpu)
		it.Overhead = overhead(srv.VCPUs, class, it.Capacity)
		bt.it = it
		vb.types = append(vb.types, bt)
		vb.byName[name] = bt
	}
	slices.SortFunc(vb.types, func(a, b *builtType) int { return strings.Compare(a.it.Name, b.it.Name) })
	return vb
}

type resolvedOffering struct {
	region       *regionInfo
	zone         Zone
	capacityType string
	usd          float64
	base, delta  float64
	complete     bool
}

func (o resolvedOffering) info() offeringInfo {
	return offeringInfo{
		region: o.region.region.Name, zone: o.zone.Name, capacityType: o.capacityType,
		usd: o.usd, base: o.base, delta: o.delta, complete: o.complete,
	}
}

// offeringSet is what the price rows of a server resolve to.
type offeringSet struct {
	offerings []resolvedOffering
	// removed are the offerings the rows named but the account, the zone list or
	// the price settings do not allow.
	removed []offeringDrop
	// otherRegions are the regions of the rows the class does not use.
	otherRegions []string
}

// offeringsOf resolves the price rows of a server to offerings: one per zone
// and capacity type, in a deterministic order.
func (c *Catalog) offeringsOf(vd *vendorData, srv *model.Server, prices []model.Price, regions map[string]*regionInfo,
	zonesByRegion map[string][]Zone, hasZones, regionPriced bool, class *Class, vendor string) offeringSet {
	type key struct {
		region, zone, capacityType string
	}
	best := map[key]resolvedOffering{}
	// lost is the reason of a row that gave nothing; a key some other row
	// resolved is not lost.
	lost := map[key]offeringDrop{}
	var otherRegions []string
	for _, p := range prices {
		ri := regions[p.RegionID]
		if ri == nil {
			otherRegions = append(otherRegions, p.RegionID)
			continue
		}
		zones, why := resolveZones(vd, ri.region, p, zonesByRegion, hasZones, regionPriced)
		if len(zones) == 0 {
			k := key{ri.region.ID, why, p.CapacityType}
			lost[k] = offeringDrop{ri.region.Name, why, p.CapacityType, Reason{Code: ZoneNotAvailable, Detail: why}}
		}
		for _, z := range zones {
			delta, complete := c.opts.Facts.PriceDelta(vendor, class, srv, z.Name, p.CapacityType)
			k := key{ri.region.ID, z.ID, p.CapacityType}
			if !complete && !class.AllowIncompletePrice {
				lost[k] = offeringDrop{ri.region.Name, z.ID, p.CapacityType,
					Reason{Code: IncompletePrice, Detail: z.ID + " " + p.CapacityType}}
				continue
			}
			usd := p.USDPerHour + delta
			if old, ok := best[k]; !ok || usd < old.usd {
				best[k] = resolvedOffering{region: ri, zone: z, capacityType: p.CapacityType, usd: usd,
					base: p.USDPerHour, delta: delta, complete: complete}
			}
		}
	}
	out := offeringSet{offerings: make([]resolvedOffering, 0, len(best)), otherRegions: uniq(otherRegions)}
	for _, o := range best {
		out.offerings = append(out.offerings, o)
	}
	slices.SortFunc(out.offerings, func(a, b resolvedOffering) int {
		if d := strings.Compare(a.region.region.Name, b.region.region.Name); d != 0 {
			return d
		}
		if d := strings.Compare(a.zone.Name, b.zone.Name); d != 0 {
			return d
		}
		return strings.Compare(a.capacityType, b.capacityType)
	})
	for k, d := range lost {
		if _, ok := best[k]; !ok {
			out.removed = append(out.removed, d)
		}
	}
	slices.SortFunc(out.removed, func(a, b offeringDrop) int {
		return cmp.Or(strings.Compare(a.region, b.region), strings.Compare(a.zone, b.zone),
			strings.Compare(a.capacityType, b.capacityType))
	})
	return out
}

// typeRefusal is why the type is dropped before any offering is looked at: no
// price, or the driver cannot launch the type at all (facts that cannot say
// launch it). The code is empty when the type stays.
func typeRefusal(facts Facts, vendor string, srv *model.Server, prices []model.Price) (Code, string) {
	if len(prices) == 0 {
		return NoPrice, ""
	}
	if tf, ok := facts.(TypeFacts); ok {
		if launchable, why := tf.TypeLaunchable(vendor, srv); !launchable {
			return NotLaunchable, why
		}
	}
	return "", ""
}

// defaultRouteSupported asks the facts whether the type works with the class's
// default settings; facts that cannot say support it.
func defaultRouteSupported(facts Facts, vendor string, class *Class, srv *model.Server) bool {
	df, ok := facts.(DefaultRouteFacts)
	return !ok || df.DefaultSupported(vendor, class, srv)
}

// withoutUnlaunchable removes the offerings the driver says cannot launch, and
// returns what is left with everything removed so far.
func withoutUnlaunchable(vendor string, srv *model.Server, set offeringSet, launchable LaunchableFacts,
) ([]resolvedOffering, []offeringDrop) {
	offs, removed := set.offerings, set.removed
	if launchable == nil {
		return offs, removed
	}
	offs = slices.DeleteFunc(offs, func(o resolvedOffering) bool {
		ok, known := launchable.Launchable(vendor, srv.APIReference, o.zone.ID)
		if known && !ok {
			removed = append(removed, offeringDrop{o.region.region.Name, o.zone.ID, o.capacityType,
				Reason{Code: NotLaunchable, Detail: o.zone.ID}})
		}
		return known && !ok
	})
	return offs, removed
}

// emptyReason is why a type is left with no offering: every price row was in a
// region the class does not use, or the dominant reason the offerings went.
func emptyReason(set offeringSet, removed []offeringDrop) (Code, string) {
	if len(removed) == 0 {
		return RegionNotAllowed, strings.Join(set.otherRegions, ",")
	}
	return NoOffering, string(dominant(removed))
}

// dominant is the reason code most of the dropped offerings share; a tie goes
// to the code that sorts first.
func dominant(drops []offeringDrop) Code {
	n := map[Code]int{}
	for _, d := range drops {
		n[d.reason.Code]++
	}
	var best Code
	for code, count := range n {
		if count > n[best] || (count == n[best] && code < best) {
			best = code
		}
	}
	return best
}

// resolveZones maps a price row to the zones the driver's account has (ADR-009):
// a zone-level price to its zone by Keeper zone ID, dropped when the account does
// not have it; a region-level price to every zone of the region. Vendors without
// zones are offered with zone = region. For a region-priced vendor (see
// RegionPricedFacts) a zone-level price of an active zone is a region-level one;
// where several rows price the same zone, the cheapest wins (offeringsOf). When
// no zone results, why names what was lost: the zone ID, or the region when the
// driver lists no zones for it.
func resolveZones(vd *vendorData, region *model.Region, p model.Price,
	zonesByRegion map[string][]Zone, hasZones, regionPriced bool) (zones []Zone, why string) {
	if !hasZones {
		id := p.ZoneID
		if id == "" {
			id = region.Name
		}
		return []Zone{{ID: id, Name: region.Name}}, ""
	}
	zones = zonesByRegion[region.Name]
	if zones == nil {
		zones = zonesByRegion[region.ID]
	}
	if p.ZoneID != "" {
		// Keeper marks retired zones inactive in its zone table.
		if active, known := vd.zones[zoneKey{p.RegionID, p.ZoneID}]; known && !active {
			return nil, p.ZoneID
		}
	}
	if len(zones) == 0 {
		return nil, region.Name
	}
	if p.ZoneID == "" || regionPriced {
		return zones, ""
	}
	for _, z := range zones {
		if z.ID == p.ZoneID {
			return []Zone{z}, ""
		}
	}
	return nil, p.ZoneID
}

func regionAllowed(r *model.Region, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	return slices.ContainsFunc(allowed, func(name string) bool { return matchesRegion(r, name) })
}

func uniq(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}

func sameProfiles(a, b map[labels.ProfileKind][]string) bool {
	for _, k := range labels.ProfileKinds {
		if !slices.Equal(uniq(a[k]), b[k]) {
			return false
		}
	}
	return true
}

func profilesKey(p map[labels.ProfileKind][]string) string {
	var b strings.Builder
	for _, k := range labels.ProfileKinds {
		b.WriteString(string(k) + "=" + strings.Join(uniq(p[k]), ",") + ";")
	}
	return b.String()
}

// gpuSupport applies the GPU rules of plan 12 (D2) to a server: for an NVIDIA
// GPU type the driver selection (nil for every other type), and for a GPU type
// the table cannot run the reason it is dropped. Accelerators that are not
// GPUs (NPUs, FPGAs, TPUs) keep today's behaviour.
func gpuSupport(srv *model.Server, class *Class, vendor string,
	virtual VirtualGPUFacts) (*nvidia.Selection, nvidia.Reason) {
	if srv.AcceleratorType != model.AcceleratorGPU || srv.AcceleratorCount <= 0 {
		return nil, nvidia.Supported
	}
	sel, reason := nvidia.Select(nvidia.GPU{
		Manufacturer: srv.AcceleratorManufacturer, Model: srv.AcceleratorModel, Count: srv.AcceleratorCount,
		AMD64:   srv.Arch == model.ArchAMD64,
		Virtual: virtual != nil && virtual.VirtualGPU(vendor, srv.APIReference),
	}, class.FlatcarVersion)
	if reason != nvidia.Supported {
		return nil, reason
	}
	return &sel, nvidia.Supported
}
