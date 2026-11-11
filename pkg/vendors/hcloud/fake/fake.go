/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/
// Package fake is the in-memory Hetzner Cloud API of the driver's tests (plan
// 15, "In-memory fake"): locations with network zones, server types with
// per-location availability and deprecation, labelled snapshot images of both
// architectures, servers with the status machine and unique names, label
// selectors, Primary IPs, networks with subnets by zone, firewalls with
// applied_to, spread placement groups with the 10-server limit, volumes that
// outlive servers, project limits, RateLimit counters with a 429 mode, per-call
// error injection and servers of other owners. It implements hcloud.API.
package fake

import (
	"context"
	"fmt"
	"maps"
	"net"
	"slices"
	"strconv"
	"sync"
	"time"

	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"
	"k8s.io/utils/clock"

	"github.com/SpareCores/scarpenter/pkg/vendors/hcloud"
)

// PageSize is the API's page size; a list costs one request per started page.
const PageSize = 50

// RateLimitPerHour is the project's rate limit.
const RateLimitPerHour = 3600

// Limits are the project's resource limits (0 is unlimited); a create beyond
// one fails with resource_limit_exceeded. Servers count every server of the
// project, other owners' included.
type Limits struct {
	Servers, Volumes, Firewalls, PlacementGroups, PrimaryIPs, Networks int
}

type action struct {
	a      *hc.Action
	apply  func()
	failed *hc.Action
}

// Fake is the in-memory API.
type Fake struct {
	mu    sync.Mutex
	clock clock.Clock
	next  int64

	locations []*hc.Location
	types     []*hc.ServerType
	images    map[int64]*hc.Image
	servers   map[int64]*hc.Server
	networks  map[int64]*hc.Network
	firewalls map[int64]*hc.Firewall
	groups    map[int64]*hc.PlacementGroup
	volumes   map[int64]*hc.Volume
	ips       map[int64]*hc.PrimaryIP
	sshKeys   map[int64]*hc.SSHKey
	actions   map[int64]*action

	// Limits are the project limits.
	limits Limits
	// ipAutoDelete is the auto_delete of the Primary IPs created with a server.
	ipAutoDelete bool
	// requests counts API requests (pages included); rateLimited forces 429.
	requests    int
	rateLimited bool
	// inject holds the errors to return by operation name.
	inject map[string][]injected
	calls  map[string]int
	// failCreate makes the create actions fail with the error.
	failCreate *hc.Action
}

type injected struct {
	err   error
	times int // -1: forever
}

var _ hcloud.API = (*Fake)(nil)

// Option configures New.
type Option func(*Fake)

// WithClock sets the clock of created timestamps.
func WithClock(c clock.Clock) Option { return func(f *Fake) { f.clock = c } }

// WithLimits sets the project limits.
func WithLimits(l Limits) Option { return func(f *Fake) { f.limits = l } }

// WithLocations replaces the default locations.
func WithLocations(l []*hc.Location) Option { return func(f *Fake) { f.locations = l } }

// WithServerTypes replaces the default server types.
func WithServerTypes(t []*hc.ServerType) Option { return func(f *Fake) { f.types = t } }

// WithPrimaryIPAutoDelete sets auto_delete of the Primary IPs the fake creates
// with a server (default true; false exercises the driver's cleanup).
func WithPrimaryIPAutoDelete(b bool) Option { return func(f *Fake) { f.ipAutoDelete = b } }

// New returns a fake with the default locations (fsn1, nbg1, hel1, ash, hil,
// sin) and server types (see DefaultServerTypes).
func New(opts ...Option) *Fake {
	f := &Fake{
		clock: clock.RealClock{}, next: 1000, ipAutoDelete: true,
		locations: DefaultLocations(), types: DefaultServerTypes(),
		images: map[int64]*hc.Image{}, servers: map[int64]*hc.Server{}, networks: map[int64]*hc.Network{},
		firewalls: map[int64]*hc.Firewall{}, groups: map[int64]*hc.PlacementGroup{}, volumes: map[int64]*hc.Volume{},
		ips: map[int64]*hc.PrimaryIP{}, sshKeys: map[int64]*hc.SSHKey{}, actions: map[int64]*action{},
		inject: map[string][]injected{}, calls: map[string]int{},
	}
	for _, o := range opts {
		o(f)
	}
	return f
}

// DefaultLocations are Hetzner's six active locations.
func DefaultLocations() []*hc.Location {
	mk := func(id int64, name, city, country string, z hc.NetworkZone) *hc.Location {
		return &hc.Location{ID: id, Name: name, City: city, Country: country, NetworkZone: z}
	}
	return []*hc.Location{
		mk(1, "fsn1", "Falkenstein", "DE", hc.NetworkZoneEUCentral),
		mk(2, "nbg1", "Nuremberg", "DE", hc.NetworkZoneEUCentral),
		mk(3, "hel1", "Helsinki", "FI", hc.NetworkZoneEUCentral),
		mk(4, "ash", "Ashburn", "US", hc.NetworkZoneUSEast),
		mk(5, "hil", "Hillsboro", "US", hc.NetworkZoneUSWest),
		mk(6, "sin", "Singapore", "SG", hc.NetworkZoneAPSouthEast),
	}
}

// DefaultServerTypes are a small table: x86 shared (cx23, cpx22), x86 dedicated
// (ccx13) and arm (cax11, in the three EU locations only), all available.
func DefaultServerTypes() []*hc.ServerType {
	locs := DefaultLocations()
	mk := func(
		id int64, name string, cores int, mem float32, disk int, arch hc.Architecture, in ...string,
	) *hc.ServerType {
		t := &hc.ServerType{
			ID: id, Name: name, Cores: cores, Memory: mem, Disk: disk, Architecture: arch, StorageType: hc.StorageTypeLocal,
		}
		for _, l := range locs {
			if len(in) == 0 || slices.Contains(in, l.Name) {
				t.Locations = append(t.Locations, hc.ServerTypeLocation{Location: l, Available: true})
			}
		}
		return t
	}
	return []*hc.ServerType{
		mk(1, "cx23", 2, 4, 40, hc.ArchitectureX86),
		mk(2, "cpx22", 2, 4, 80, hc.ArchitectureX86),
		mk(3, "ccx13", 2, 8, 80, hc.ArchitectureX86),
		mk(4, "cax11", 2, 4, 40, hc.ArchitectureARM, "fsn1", "nbg1", "hel1"),
	}
}

// APIError builds the error the API returns for a code; the field names fill
// the details of an invalid_input.
func APIError(code hc.ErrorCode, msg string, fields ...string) error {
	e := hc.Error{Code: code, Message: msg}
	if len(fields) > 0 {
		d := hc.ErrorDetailsInvalidInput{}
		for _, f := range fields {
			d.Fields = append(d.Fields, hc.ErrorDetailsInvalidInputField{Name: f, Messages: []string{msg}})
		}
		e.Details = d
	}
	return e
}

func (f *Fake) id() int64 { f.next++; return f.next }

// call is the start of every API method: it counts the request(s), applies the
// rate limit and the injected errors.
func (f *Fake) call(op string, pages int) error {
	f.calls[op]++
	if pages < 1 {
		pages = 1
	}
	if f.rateLimited || f.requests+pages > RateLimitPerHour {
		f.requests++
		return APIError(hc.ErrorCodeRateLimitExceeded, "limit of 3600 requests per hour reached")
	}
	f.requests += pages
	if l := f.inject[op]; len(l) > 0 {
		in := &l[0]
		err := in.err
		if in.times > 0 {
			in.times--
			if in.times == 0 {
				f.inject[op] = l[1:]
			}
		}
		return err
	}
	return nil
}

func pagesFor(n int) int { return max(1, (n+PageSize-1)/PageSize) }

// Inject makes the next times calls of an API method fail with err (-1: every
// call); the method is named as in hcloud.API ("CreateServer").
func (f *Fake) Inject(op string, err error, times int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inject[op] = append(f.inject[op], injected{err: err, times: times})
}

// ClearInjected removes the injected errors.
func (f *Fake) ClearInjected() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inject = map[string][]injected{}
}

// Calls returns how often an API method was called.
func (f *Fake) Calls(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[op]
}

// Requests returns the request count (a list counts a request per page).
func (f *Fake) Requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

// SetRateLimited makes every call fail with rate_limit_exceeded (the 429 mode).
func (f *Fake) SetRateLimited(b bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rateLimited = b
}

// ResetRateLimit refills the bucket.
func (f *Fake) ResetRateLimit() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = 0
}

// RateLimit implements hcloud.API with the RateLimit-* counters.
func (f *Fake) RateLimit() hcloud.RateLimit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return hcloud.RateLimit{
		Limit: RateLimitPerHour, Remaining: max(0, RateLimitPerHour-f.requests), Reset: f.clock.Now().Add(time.Hour).Unix(),
	}
}

// FailCreateAction makes the create action of the next servers end in error
// (code, message); nil restores success. The server stays, as at Hetzner.
func (f *Fake) FailCreateAction(code, msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if code == "" {
		f.failCreate = nil
		return
	}
	f.failCreate = &hc.Action{Status: hc.ActionStatusError, ErrorCode: code, ErrorMessage: msg}
}

// SetAvailable sets available of a type's entry for a location (false: out of
// stock). The type must have the entry.
func (f *Fake) SetAvailable(typ, location string, available bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.types {
		if t.Name != typ {
			continue
		}
		for i := range t.Locations {
			if t.Locations[i].Location.Name == location {
				t.Locations[i].Available = available
			}
		}
	}
}

// SetDeprecation sets unavailable_after of a type's entry for a location.
func (f *Fake) SetDeprecation(typ, location string, announced, unavailableAfter time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.types {
		if t.Name != typ {
			continue
		}
		for i := range t.Locations {
			if t.Locations[i].Location.Name == location {
				t.Locations[i].Deprecation = &hc.DeprecationInfo{Announced: announced, UnavailableAfter: unavailableAfter}
			}
		}
	}
}

// AddImage adds a snapshot (type snapshot, status available) and returns it.
func (f *Fake) AddImage(arch hc.Architecture, labels map[string]string) *hc.Image {
	f.mu.Lock()
	defer f.mu.Unlock()
	im := &hc.Image{
		ID: f.id(), Type: hc.ImageTypeSnapshot, Status: hc.ImageStatusAvailable, Architecture: arch,
		Labels: cloneMap(labels), Created: f.clock.Now(), DiskSize: 20, ImageSize: 0.5,
	}
	f.images[im.ID] = im
	return im
}

// AddFlatcarImages adds the two snapshots of a Flatcar release with the label
// convention (os, flatcar-channel, flatcar-version) and returns x86 and arm.
func (f *Fake) AddFlatcarImages(channel, version string) (x86, arm *hc.Image) {
	l := map[string]string{"os": "flatcar", "flatcar-channel": channel, "flatcar-version": version}
	return f.AddImage(hc.ArchitectureX86, l), f.AddImage(hc.ArchitectureARM, l)
}

// AddSSHKey adds a key.
func (f *Fake) AddSSHKey(name string) *hc.SSHKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := &hc.SSHKey{ID: f.id(), Name: name}
	f.sshKeys[k.ID] = k
	return k
}

// AddNetwork adds a network with a cloud subnet in each of the network zones.
func (f *Fake) AddNetwork(name, cidr string, labels map[string]string, zones ...hc.NetworkZone) *hc.Network {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(err)
	}
	nw := &hc.Network{ID: f.id(), Name: name, IPRange: n, Labels: cloneMap(labels), Created: f.clock.Now()}
	for _, z := range zones {
		nw.Subnets = append(nw.Subnets, hc.NetworkSubnet{Type: hc.NetworkSubnetTypeCloud, IPRange: n, NetworkZone: z})
	}
	f.networks[nw.ID] = nw
	return nw
}

// AddForeignServer adds a server of another owner (no SCarpenter labels), to
// exercise account-wide counting.
func (f *Fake) AddForeignServer(location, typ string, labels map[string]string) *hc.Server {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, loc := f.findType(typ), f.findLocation(location)
	s := &hc.Server{
		ID: f.id(), Name: fmt.Sprintf("foreign-%d", f.next), Status: hc.ServerStatusRunning, Created: f.clock.Now(),
		ServerType: copyType(st), Location: loc, Labels: cloneMap(labels), PrimaryDiskSize: st.Disk,
	}
	s.PublicNet.IPv4 = hc.ServerPublicNetIPv4{ID: 0, IP: ipv4For(s.ID)}
	f.servers[s.ID] = s
	return s
}

// PowerOff powers a server off (the guest's poweroff): status off, still billed
// and listed.
func (f *Fake) PowerOff(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.servers[id]; s != nil {
		s.Status = hc.ServerStatusOff
	}
}

// SetStatus sets a server's status directly (migrating, rebuilding, unknown...).
func (f *Fake) SetStatus(id int64, st hc.ServerStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.servers[id]; s != nil {
		s.Status = st
	}
}

// Tick advances every transitional server one step: initializing to starting,
// starting to running, stopping to off, deleting to gone.
func (f *Fake) Tick() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range slices.Collect(mapValues(f.servers)) {
		switch s.Status {
		case hc.ServerStatusInitializing:
			s.Status = hc.ServerStatusStarting
		case hc.ServerStatusStarting:
			s.Status = hc.ServerStatusRunning
		case hc.ServerStatusStopping:
			s.Status = hc.ServerStatusOff
		case hc.ServerStatusDeleting:
			f.removeServer(s)
		}
	}
}

// ServerCount returns the number of servers of the project.
func (f *Fake) ServerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.servers)
}

// PrimaryIPs returns the Primary IPs that exist.
func (f *Fake) PrimaryIPs() []*hc.PrimaryIP {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*hc.PrimaryIP, 0, len(f.ips))
	for _, ip := range f.ips {
		c := *ip
		out = append(out, &c)
	}
	slices.SortFunc(out, func(a, b *hc.PrimaryIP) int { return int(a.ID - b.ID) })
	return out
}

func mapValues[K comparable, V any](m map[K]V) func(func(V) bool) {
	return func(yield func(V) bool) {
		for _, v := range m {
			if !yield(v) {
				return
			}
		}
	}
}

func cloneMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	maps.Copy(out, m)
	return out
}

func copyType(t *hc.ServerType) *hc.ServerType {
	c := *t
	c.Locations = nil
	return &c
}

func (f *Fake) findType(name string) *hc.ServerType {
	for _, t := range f.types {
		if t.Name == name {
			return t
		}
	}
	return nil
}

func (f *Fake) findLocation(name string) *hc.Location {
	for _, l := range f.locations {
		if l.Name == name {
			return l
		}
	}
	return nil
}

func ipv4For(id int64) net.IP { return net.IPv4(203, 0, byte(id/250%250), byte(id%250+1)) }

func ipv6NetFor(id int64) *net.IPNet {
	ip := net.ParseIP(fmt.Sprintf("2001:db8:%x::", id))
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(64, 128)}
}

// ---- reads ----

// Locations implements hcloud.API.
func (f *Fake) Locations(context.Context) ([]*hc.Location, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("Locations", pagesFor(len(f.locations))); err != nil {
		return nil, err
	}
	out := make([]*hc.Location, len(f.locations))
	for i, l := range f.locations {
		c := *l
		out[i] = &c
	}
	return out, nil
}

// ServerTypes implements hcloud.API.
func (f *Fake) ServerTypes(context.Context) ([]*hc.ServerType, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("ServerTypes", pagesFor(len(f.types))); err != nil {
		return nil, err
	}
	out := make([]*hc.ServerType, len(f.types))
	for i, t := range f.types {
		c := *t
		c.Locations = slices.Clone(t.Locations)
		out[i] = &c
	}
	return out, nil
}

// Images implements hcloud.API: type, status, architecture, label selector and
// the created:desc sort are honoured.
func (f *Fake) Images(_ context.Context, o hc.ImageListOpts) ([]*hc.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sel, err := ParseSelector(o.LabelSelector)
	if err != nil {
		f.calls["Images"]++
		return nil, APIError(hc.ErrorCodeInvalidInput, err.Error(), "label_selector")
	}
	var out []*hc.Image
	for _, im := range f.images {
		if len(o.Type) > 0 && !slices.Contains(o.Type, im.Type) {
			continue
		}
		if len(o.Status) > 0 && !slices.Contains(o.Status, im.Status) {
			continue
		}
		if len(o.Architecture) > 0 && !slices.Contains(o.Architecture, im.Architecture) {
			continue
		}
		if o.Name != "" && im.Name != o.Name {
			continue
		}
		if !sel.Matches(im.Labels) {
			continue
		}
		c := *im
		c.Labels = cloneMap(im.Labels)
		out = append(out, &c)
	}
	slices.SortFunc(out, func(a, b *hc.Image) int {
		if !a.Created.Equal(b.Created) {
			if a.Created.After(b.Created) {
				return -1
			}
			return 1
		}
		return int(b.ID - a.ID)
	})
	if err := f.call("Images", pagesFor(len(out))); err != nil {
		return nil, err
	}
	return out, nil
}

// SSHKey implements hcloud.API.
func (f *Fake) SSHKey(_ context.Context, idOrName string) (*hc.SSHKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("SSHKey", 1); err != nil {
		return nil, err
	}
	for _, k := range f.sshKeys {
		if k.Name == idOrName || strconv.FormatInt(k.ID, 10) == idOrName {
			c := *k
			return &c, nil
		}
	}
	return nil, nil
}
