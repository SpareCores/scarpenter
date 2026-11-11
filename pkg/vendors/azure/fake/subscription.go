/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

//nolint:lll,goconst,dupl // the fake repeats the SDK's long signatures and ARM texts
package fake

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armsubscriptions"
)

// Subscription is the in-memory state of one Azure subscription.
type Subscription struct {
	mu sync.Mutex
	// ID is the subscription ID that appears in resource IDs.
	ID string
	// Now is the clock of timestamps (TimeCreated); time.Now by default.
	Now func() time.Time

	locations map[string]*location
	groups    map[string]bool // lower-cased names
	images    map[string]*image
	agreement map[string]bool
	vms       map[string]*vmState
	nics      map[string]*armnetwork.Interface
	pips      map[string]*armnetwork.PublicIPAddress
	disks     map[string]string // lower-cased rg/name -> VM name that owns it, "" when detached
	nextIP    int
	nextPub   int

	failures []*failure
	rules    []*capacityRule
	calls    map[string]int
	// hold makes VM creates block until the context ends (the VM stays
	// Creating): the 4 minute wait of the driver.
	hold bool
	// leaveFailed leaves a VM in provisioningState Failed after an allocation
	// failure (plan 17 section 3, step 5: unverified on real Azure).
	leaveFailed bool
}

// Usage names of the compute API.
const (
	usageCores = "cores"
	usageSpot  = "lowPriorityCores"
)

type location struct {
	name   string
	zonal  bool
	skus   []*armcompute.ResourceSKU
	limits map[string]int64 // usage name -> limit
	// foreign is the usage of resources outside the fake's VMs (usage name ->
	// value), standing for the rest of the subscription.
	foreign map[string]int64
}

type image struct {
	plan *armcompute.PurchasePlan
	// missingIn lists the locations that do not have this version.
	missingIn map[string]bool
}

type vmState struct {
	vm    armcompute.VirtualMachine
	rg    string
	power string
}

type failure struct {
	op   string
	err  error
	left int
}

type capacityRule struct {
	location, size, zone string
	spotOnly             bool
	code                 string
}

// New creates an empty subscription with the given ID.
func New(id string) *Subscription {
	return &Subscription{
		ID: id, Now: time.Now,
		locations: map[string]*location{}, groups: map[string]bool{}, images: map[string]*image{},
		agreement: map[string]bool{}, vms: map[string]*vmState{}, nics: map[string]*armnetwork.Interface{},
		pips: map[string]*armnetwork.PublicIPAddress{}, disks: map[string]string{}, calls: map[string]int{},
		nextIP: 4, nextPub: 10,
	}
}

func key(rg, name string) string { return strings.ToLower(rg + "/" + name) }

func val[T any](p *T) T {
	if p == nil {
		var z T
		return z
	}
	return *p
}

// begin counts a call and returns an injected failure of it, if any. The
// caller holds the lock.
func (s *Subscription) begin(ctx context.Context, op string) error {
	s.calls[op]++
	if err := ctx.Err(); err != nil {
		return err
	}
	for i, f := range s.failures {
		if f.op == op && f.left != 0 {
			if f.left > 0 {
				f.left--
			}
			if f.left == 0 {
				s.failures = slices.Delete(s.failures, i, i+1)
			}
			return f.err
		}
	}
	return nil
}

// Calls returns how often an operation was called ("vm.get", "vm.put",
// "vm.delete", "vm.list", "nic.get", "nic.put", "nic.delete", "pip.get",
// "pip.put", "pip.delete", "resources.list", "skus.list", "usages.compute",
// "usages.network", "image.get", "image.skus", "agreement.get",
// "locations.list").
func (s *Subscription) Calls(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[op]
}

// FailNext makes the next n calls of the operation (see Calls) fail with err; a
// negative n fails every call until ClearFailures.
func (s *Subscription) FailNext(op string, err error, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = append(s.failures, &failure{op: op, err: err, left: n})
}

// ClearFailures removes every injected failure and capacity rule.
func (s *Subscription) ClearFailures() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures, s.rules = nil, nil
}

// NoCapacity makes launches of the size in the location (and the zone, "" for
// any) fail with the allocation failure of Azure: ZonalAllocationFailed in a
// zone, AllocationFailed without one. spotOnly restricts it to Spot VMs.
func (s *Subscription) NoCapacity(location, size, zone string, spotOnly bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = append(s.rules, &capacityRule{location: location, size: size, zone: zone, spotOnly: spotOnly})
}

// FailAllocation makes launches of the size fail with an arbitrary ARM code
// (OverconstrainedAllocationRequest, SkuNotAvailable ...).
func (s *Subscription) FailAllocation(location, size, zone, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = append(s.rules, &capacityRule{location: location, size: size, zone: zone, code: code})
}

// HoldCreates makes VM creates wait until their context ends, leaving the VM in
// provisioningState Creating (a long-running operation that does not finish).
func (s *Subscription) HoldCreates(hold bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hold = hold
}

// LeaveFailedVMs makes an allocation failure leave the VM behind in
// provisioningState Failed.
func (s *Subscription) LeaveFailedVMs(leave bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leaveFailed = leave
}

// CompleteCreating finishes the VMs left Creating: they become Succeeded and
// running.
func (s *Subscription) CompleteCreating() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.vms {
		if val(v.vm.Properties.ProvisioningState) == "Creating" {
			v.vm.Properties.ProvisioningState = new("Succeeded")
			v.power = "running"
		}
	}
}

// PowerOff powers a VM off by itself (not through the API): stopped, or
// deallocated.
func (s *Subscription) PowerOff(rg, name string, deallocate bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.vms[key(rg, name)]
	if !ok {
		return fmt.Errorf("no VM %s/%s", rg, name)
	}
	v.power = "stopped"
	if deallocate {
		v.power = "deallocated"
	}
	return nil
}

// SetProvisioningState forces the provisioning state of a VM (Failed,
// Deleting ...) and returns an error for an unknown VM.
func (s *Subscription) SetProvisioningState(rg, name, state string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.vms[key(rg, name)]
	if !ok {
		return fmt.Errorf("no VM %s/%s", rg, name)
	}
	v.vm.Properties.ProvisioningState = new(state)
	return nil
}

// AddLocation registers a location; zonal says whether it has availability
// zones. Registered locations are the ones the subscription lists.
func (s *Subscription) AddLocation(name string, zonal bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.locations[name] = &location{name: name, zonal: zonal, limits: map[string]int64{}, foreign: map[string]int64{}}
}

// AddResourceGroup creates a resource group (the fake does not create one on
// demand: a call into a missing group is ResourceGroupNotFound).
func (s *Subscription) AddResourceGroup(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.groups[strings.ToLower(name)] = true
}

// SKU describes a virtual machine size of a location for AddSKU.
type SKU struct {
	Name, Family string
	VCPUs        int
	Arm          bool
	// Zones the size exists in; empty means every zone of a zonal location.
	Zones []string
	// NoGen2 makes the size Gen1-only.
	NoGen2             bool
	NoPremium          bool
	NoSpot             bool
	NoAcceleratedNet   bool
	Ephemeral          bool
	NVMeOnly           bool
	MaxDataDisks       int
	RestrictedLocation bool
	// RestrictedZones are zones the subscription cannot use for the size.
	RestrictedZones []string
}

// AddSKU adds a size to the location's SKU table.
func (s *Subscription) AddSKU(loc string, k SKU) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.locations[loc]
	if !ok {
		return fmt.Errorf("unknown location %s", loc)
	}
	zones := k.Zones
	if len(zones) == 0 && l.zonal {
		zones = []string{"1", "2", "3"}
	}
	capab := func(n, v string) *armcompute.ResourceSKUCapabilities {
		return &armcompute.ResourceSKUCapabilities{Name: new(n), Value: new(v)}
	}
	boolStr := func(b bool) string {
		if b {
			return "True"
		}
		return "False"
	}
	gens, ctrl, arch := "V1,V2", "SCSI, NVMe", "x64"
	if k.NoGen2 {
		gens = "V1"
	}
	if k.NVMeOnly {
		ctrl = "NVMe"
	}
	if k.Arm {
		arch = "Arm64"
	}
	maxDisks := k.MaxDataDisks
	if maxDisks == 0 {
		maxDisks = 8
	}
	r := &armcompute.ResourceSKU{
		Name: new(k.Name), Family: new(k.Family), ResourceType: new("virtualMachines"),
		Locations: []*string{new(loc)},
		Capabilities: []*armcompute.ResourceSKUCapabilities{
			capab("vCPUs", fmt.Sprint(k.VCPUs)), capab("HyperVGenerations", gens),
			capab("PremiumIO", boolStr(!k.NoPremium)), capab("LowPriorityCapable", boolStr(!k.NoSpot)),
			capab("AcceleratedNetworkingEnabled", boolStr(!k.NoAcceleratedNet)),
			capab("EphemeralOSDiskSupported", boolStr(k.Ephemeral)), capab("DiskControllerTypes", ctrl),
			capab("CpuArchitectureType", arch), capab("MaxDataDiskCount", fmt.Sprint(maxDisks)),
		},
	}
	info := &armcompute.ResourceSKULocationInfo{Location: new(loc)}
	for _, z := range zones {
		info.Zones = append(info.Zones, new(z))
	}
	r.LocationInfo = []*armcompute.ResourceSKULocationInfo{info}
	reason := armcompute.ResourceSKURestrictionsReasonCodeNotAvailableForSubscription
	if k.RestrictedLocation {
		r.Restrictions = append(r.Restrictions, &armcompute.ResourceSKURestrictions{
			Type: new(armcompute.ResourceSKURestrictionsTypeLocation), ReasonCode: &reason, Values: []*string{new(loc)},
		})
	}
	if len(k.RestrictedZones) > 0 {
		rs := &armcompute.ResourceSKURestrictions{Type: new(armcompute.ResourceSKURestrictionsTypeZone), ReasonCode: &reason}
		for _, z := range k.RestrictedZones {
			rs.Values = append(rs.Values, new(z))
		}
		r.Restrictions = append(r.Restrictions, rs)
	}
	l.skus = append(l.skus, r)
	return nil
}

// SetLimit sets a quota limit of the location by usage name (cores,
// lowPriorityCores, a family name, StandardPublicIPAddresses).
func (s *Subscription) SetLimit(loc, usage string, limit int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.locations[loc]; ok {
		l.limits[usage] = limit
	}
}

// SetForeignUsage sets the usage of the rest of the subscription (resources the
// fake does not hold) for a usage name.
func (s *Subscription) SetForeignUsage(loc, usage string, n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.locations[loc]; ok {
		l.foreign[usage] = n
	}
}

// AddImage publishes a marketplace image version; plan is nil for an image
// without a purchase plan.
func (s *Subscription) AddImage(publisher, offer, sku, version string, plan *armcompute.PurchasePlan) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.images[strings.ToLower(strings.Join([]string{publisher, offer, sku, version}, ":"))] = &image{plan: plan, missingIn: map[string]bool{}}
}

// RemoveImageFrom makes a published version missing in a location.
func (s *Subscription) RemoveImageFrom(loc, publisher, offer, sku, version string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if im, ok := s.images[strings.ToLower(strings.Join([]string{publisher, offer, sku, version}, ":"))]; ok {
		im.missingIn[loc] = true
	}
}

// AcceptTerms records the marketplace agreement of a plan as accepted.
func (s *Subscription) AcceptTerms(publisher, offer, plan string, accepted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agreement[strings.ToLower(publisher+"/"+offer+"/"+plan)] = accepted
}

// VMs, NICs and PIPs return the API objects; the others are below.
func (s *Subscription) VMs() *VMs   { return &VMs{s} }
func (s *Subscription) NICs() *NICs { return &NICs{s} }
func (s *Subscription) PIPs() *PIPs { return &PIPs{s} }

// Disks returns the names of the disks that exist.
func (s *Subscription) Disks() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.disks))
	for k := range s.disks {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// VMNames returns the names of the VMs that exist (rg/name, lower case).
func (s *Subscription) VMNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.vms))
	for k := range s.vms {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// NICNames and PIPNames return the names that exist (rg/name, lower case).
func (s *Subscription) NICNames() []string {
	return s.names(func() []string { return mapKeys(s.nics) })
}
func (s *Subscription) PIPNames() []string {
	return s.names(func() []string { return mapKeys(s.pips) })
}

func (s *Subscription) names(f func() []string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := f()
	slices.Sort(out)
	return out
}

func mapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func (s *Subscription) id(rg, provider, kind, name string) string {
	return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/%s/%s/%s", s.ID, rg, provider, kind, name)
}

func (s *Subscription) groupExists(rg string) error {
	if !s.groups[strings.ToLower(rg)] {
		return Error(http.StatusNotFound, "ResourceGroupNotFound", fmt.Sprintf("Resource group '%s' could not be found.", rg))
	}
	return nil
}

// Locations is the locations API.
type Locations struct{ s *Subscription }

// Locations returns the locations API.
func (s *Subscription) Locations() *Locations { return &Locations{s} }

// List lists the registered locations.
func (l *Locations) List(ctx context.Context) ([]*armsubscriptions.Location, error) {
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	if err := l.s.begin(ctx, "locations.list"); err != nil {
		return nil, err
	}
	var out []*armsubscriptions.Location
	for _, loc := range l.s.locations {
		out = append(out, &armsubscriptions.Location{
			Name: new(loc.name), DisplayName: new(loc.name),
			Metadata: &armsubscriptions.LocationMetadata{RegionType: new(armsubscriptions.RegionTypePhysical)},
		})
	}
	slices.SortFunc(out, func(a, b *armsubscriptions.Location) int { return strings.Compare(*a.Name, *b.Name) })
	return out, nil
}
