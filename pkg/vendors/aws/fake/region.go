/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// InstanceType is an instance type of the account's catalog.
type InstanceType struct {
	Name  string
	VCPUs int32
	// Arch is x86_64 (default) or arm64.
	Arch string
	// ENAExpress, Burstable and Nitro describe the type; a burstable type has
	// no cluster placement groups.
	ENAExpress bool
	Burstable  bool
	NonNitro   bool
	// LocalDisks of NVMe instance store, DiskGB each.
	LocalDisks int32
	DiskGB     int64
}

type vpc struct {
	id, cidr  string
	isDefault bool
	tags      map[string]string
}

type subnet struct {
	id, vpc, az, cidr string
	defaultForAZ      bool
	free              int32
	tags              map[string]string
	mapPublic         bool
}

type sgRule struct {
	protocol         string
	from, to         int32
	cidr, cidr6, grp string
}

type sg struct {
	id, name, vpc, desc string
	tags                map[string]string
	ingress             []sgRule
}

type placementGroup struct {
	id, name, strategy string
	partitions         int32
	tags               map[string]string
}

type igw struct {
	id, vpc string
	tags    map[string]string
}

type routeTable struct {
	id, vpc string
	main    bool
	subnets map[string]string // association ID -> subnet
	routes  []string
	tags    map[string]string
}

type volume struct {
	id   string
	tags map[string]string
}

type instance struct {
	id, typ, az, subnet, vpc, ami, token, tokenHash, userData string
	lifecycle                                                 string // "" or spot
	state                                                     types.InstanceStateName
	launched, terminatedAt                                    time.Time
	tags                                                      map[string]string
	groups                                                    []string
	pg, tenancy, privateIP, publicIP, root                    string
	volumes                                                   []string
	metadata                                                  *types.InstanceMetadataOptionsResponse
	owner                                                     string
	spotRequest                                               string
}

type spotRequest struct {
	id, instance, typ string
	state             string
	created           time.Time
	closeAt           time.Time
}

// Image is an AMI.
type Image struct {
	ID, Name, Owner string
	// Arch is x86_64 (default) or arm64.
	Arch string
	// State is available (default) or pending.
	State      string
	RootDevice string
	RootGiB    int32
}

// CapacityReservation is an On-Demand Capacity Reservation.
type CapacityReservation struct {
	ID, Type, AZ string
	Total, Free  int32
	// State is active (default), expired, cancelled.
	State string
	// Kind is default (an ODCR) or capacity-block.
	Kind string
}

type region struct {
	name     string
	disabled bool
	azs      []AZ
	vpcs     map[string]*vpc
	subnets  map[string]*subnet
	sgs      map[string]*sg
	pgs      map[string]*placementGroup
	igws     map[string]*igw
	attached map[string]string // igw -> vpc
	rts      map[string]*routeTable
	insts    []*instance
	spot     []*spotRequest
	volumes  map[string]*volume
	images   map[string]*Image
	crs      []*CapacityReservation
	offered  map[string]map[string]bool // type -> zone ID -> offered
	quotas   map[string]float64
	tokens   map[string]*instance
	rtTokens map[string]*routeTable
	cidrNext int
}

func (a *Account) addRegionLocked(s RegionSpec) {
	r := &region{
		name: s.Name, disabled: s.Disabled, azs: s.AZs, vpcs: map[string]*vpc{}, subnets: map[string]*subnet{},
		sgs: map[string]*sg{}, pgs: map[string]*placementGroup{}, igws: map[string]*igw{}, attached: map[string]string{},
		rts: map[string]*routeTable{}, volumes: map[string]*volume{}, images: map[string]*Image{},
		offered: map[string]map[string]bool{}, quotas: map[string]float64{}, tokens: map[string]*instance{}, rtTokens: map[string]*routeTable{},
	}
	for i := range r.azs {
		if r.azs[i].Type == "" {
			r.azs[i].Type = "availability-zone"
		}
		if r.azs[i].OptIn == "" {
			r.azs[i].OptIn = "opt-in-not-required"
		}
	}
	maps.Copy(r.quotas, defaultQuotas)
	a.regions[s.Name] = r
	a.order = append(a.order, s.Name)
	if !s.NoDefaultVPC {
		v := a.newVPC(r, "172.31.0.0/16", map[string]string{})
		v.isDefault = true
		g := &igw{id: a.id("igw"), vpc: v.id, tags: map[string]string{}}
		r.igws[g.id] = g
		r.attached[g.id] = v.id
		for i, az := range r.azs {
			if az.Type != "availability-zone" {
				continue
			}
			sn := a.newSubnet(r, v, az.Name, fmt.Sprintf("172.31.%d.0/20", i*16), map[string]string{})
			sn.defaultForAZ, sn.mapPublic = true, true
		}
	}
}

func (a *Account) newVPC(r *region, cidr string, tags map[string]string) *vpc {
	v := &vpc{id: a.id("vpc"), cidr: cidr, tags: tags}
	r.vpcs[v.id] = v
	// A VPC comes with a main route table and a default security group.
	rt := &routeTable{id: a.id("rtb"), vpc: v.id, main: true, subnets: map[string]string{}, tags: map[string]string{}}
	r.rts[rt.id] = rt
	d := &sg{id: a.id("sg"), name: "default", vpc: v.id, desc: "default VPC security group", tags: map[string]string{}}
	d.ingress = []sgRule{{protocol: "-1", grp: d.id}}
	r.sgs[d.id] = d
	return v
}

func (a *Account) newSubnet(r *region, v *vpc, az, cidr string, tags map[string]string) *subnet {
	s := &subnet{id: a.id("subnet"), vpc: v.id, az: az, cidr: cidr, free: 4091, tags: tags}
	r.subnets[s.id] = s
	return s
}

// SetInstanceTypes sets the account's instance types (every region offers them
// in every zone until SetOffering says otherwise).
func (a *Account) SetInstanceTypes(ts []InstanceType) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.types = slices.Clone(ts)
}

func (a *Account) typeOf(name string) (InstanceType, bool) {
	for _, t := range a.types {
		if t.Name == name {
			return t, true
		}
	}
	return InstanceType{}, false
}

// SetOffering says in which zones (zone IDs) of the region the type is
// offered; a type never mentioned is offered in every zone.
func (a *Account) SetOffering(regionName, typ string, zoneIDs ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.regions[regionName]
	m := map[string]bool{}
	for _, z := range zoneIDs {
		m[z] = true
	}
	r.offered[typ] = m
}

func (r *region) zoneByName(name string) (AZ, bool) {
	for _, z := range r.azs {
		if z.Name == name {
			return z, true
		}
	}
	return AZ{}, false
}

// AddImage registers an image in the region.
func (a *Account) AddImage(regionName string, img Image) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if img.Arch == "" {
		img.Arch = "x86_64"
	}
	if img.State == "" {
		img.State = "available"
	}
	if img.RootDevice == "" {
		img.RootDevice = "/dev/xvda"
	}
	if img.RootGiB == 0 {
		img.RootGiB = 8
	}
	a.regions[regionName].images[img.ID] = &img
}

// AddCapacityReservation registers a capacity reservation.
func (a *Account) AddCapacityReservation(regionName string, cr CapacityReservation) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if cr.State == "" {
		cr.State = "active"
	}
	if cr.Kind == "" {
		cr.Kind = "default"
	}
	a.regions[regionName].crs = append(a.regions[regionName].crs, &cr)
}

// SetQuota sets the applied value of an EC2 quota in the region.
func (a *Account) SetQuota(regionName, code string, value float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.regions[regionName].quotas[code] = value
}

// RemoveQuota makes the quota unknown to Service Quotas in the region.
func (a *Account) RemoveQuota(regionName, code string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.regions[regionName].quotas, code)
}

// SetFreeAddresses sets the free addresses of a subnet.
func (a *Account) SetFreeAddresses(regionName, subnetID string, n int32) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.regions[regionName].subnets[subnetID].free = n
}

// DefaultSubnets returns the IDs of the default VPC's default subnets by zone
// name.
func (a *Account) DefaultSubnets(regionName string) map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]string{}
	for id, s := range a.regions[regionName].subnets {
		if s.defaultForAZ {
			out[s.az] = id
		}
	}
	return out
}

// Regions returns the names of the account's regions, in creation order.
func (a *Account) Regions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.order)
}
