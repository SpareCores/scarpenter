/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package fake is an in-memory OpenStack cloud shaped like OVHcloud Public
// Cloud: a Keystone catalog of regions (some without endpoints), and per region
// a Nova (flavors with their own IDs, servers with a BUILD to ACTIVE or ERROR
// state machine and the scheduling task state, name and tag filters, metadata,
// server groups, absolute limits with OverQuota, NoValidHost injection, foreign
// servers, guest poweroff), a Neutron (the Ext-Net network, security groups with
// creation defaults, a group in use refusing delete), a Glance and a Cinder. It
// implements osapi.Provider and is what the driver's tests run against.
//
// Error texts and statuses are those Nova, Neutron and Glance use upstream; the
// plan marks them to be confirmed on the real service (spike S7).
package fake

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi"
)

// MaxUserDataBase64 is Nova's limit of base64-encoded user data.
const MaxUserDataBase64 = 65535

// RegionConfig describes one region of the cloud.
type RegionConfig struct {
	// NoEndpoint leaves the region out of the catalog's compute endpoints.
	NoEndpoint bool
	// Flavors are the region's flavors; their IDs are generated per region when
	// empty.
	Flavors []osapi.Flavor
	// AZs are the availability zones; empty means Nova's single "nova" zone,
	// which the create call must not name.
	AZs []string
	// Limits are the absolute limits; zero fields are generous defaults.
	Limits osapi.Limits
	// VolumeTypes are the Cinder volume types; empty means classic, high-speed
	// and high-speed-gen2.
	VolumeTypes []string
	// Hosts bounds the members of an anti-affinity server group (0: unlimited).
	Hosts int
}

// Cloud is the in-memory cloud. Methods are safe for concurrent use.
type Cloud struct {
	mu sync.Mutex
	// ProjectID owns the project's own images.
	ProjectID string
	// BuildReads is the number of reads (get or list) after the scheduling one
	// that a server stays in BUILD before it turns ACTIVE; default 1.
	BuildReads int
	// DeleteReads is the number of calls into the region after a delete that
	// the server stays deleting (its status kept, task state deleting, still counted by the
	// limits, its volumes and groups still held) before it is gone; default 1.
	// 0 removes it at once.
	DeleteReads int
	// Now is the clock of created timestamps.
	Now func() time.Time

	order   []string
	regions map[string]*region
	seq     int
	inject  []*injection
}

type injection struct {
	region, op string
	err        error
	times      int
}

type serverRec struct {
	osapi.Server
	reads int
	// deleteCalls counts the calls since the delete; -1 when not deleting.
	deleteCalls int
	groupID     string
	secGroups   []string
	volumes     []*volumeRec
	foreign     bool
}

type volumeRec struct {
	id, volType        string
	sizeGiB            int
	deleteOnTerminate  bool
	attachedTo, source string
}

type region struct {
	name       string
	cfg        RegionConfig
	flavors    []osapi.Flavor
	servers    map[string]*serverRec
	order      []string
	groups     map[string]*osapi.ServerGroup
	secGroups  []*osapi.SecurityGroup
	images     []osapi.Image
	extNet     osapi.Network
	privNets   []osapi.Network
	volTypes   []osapi.VolumeType
	volumes    map[string]*volumeRec
	noValid    map[string]bool // flavor name
	defaultSGs string
}

// New returns an empty cloud with the project ID "proj-0".
func New() *Cloud {
	return &Cloud{ProjectID: "proj-0", BuildReads: 1, DeleteReads: 1, Now: time.Now, regions: map[string]*region{}}
}

func (c *Cloud) id(kind string) string {
	c.seq++
	sum := sha256.Sum256(fmt.Appendf(nil, "%s-%d", kind, c.seq))
	h := hex.EncodeToString(sum[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// AddRegion adds a region (API name, upper case as Keystone has it).
func (c *Cloud) AddRegion(name string, cfg RegionConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := &region{
		name: name, cfg: cfg, servers: map[string]*serverRec{}, groups: map[string]*osapi.ServerGroup{},
		volumes: map[string]*volumeRec{}, noValid: map[string]bool{},
	}
	for _, f := range cfg.Flavors {
		if f.ID == "" {
			sum := sha256.Sum256([]byte(name + "/" + f.Name))
			f.ID = hex.EncodeToString(sum[:8])
		}
		r.flavors = append(r.flavors, f)
	}
	r.extNet = osapi.Network{ID: c.id("net"), Name: osapi.ExternalNetwork, External: true}
	vts := cfg.VolumeTypes
	if len(vts) == 0 {
		vts = []string{"classic", "high-speed", "high-speed-gen2"}
	}
	for _, v := range vts {
		r.volTypes = append(r.volTypes, osapi.VolumeType{ID: c.id("vt"), Name: v})
	}
	def := &osapi.SecurityGroup{ID: c.id("sg"), Name: "default", Description: "Default security group"}
	def.Rules = defaultRules(def.ID)
	r.secGroups = append(r.secGroups, def)
	r.defaultSGs = def.ID
	if _, ok := c.regions[name]; !ok {
		c.order = append(c.order, name)
	}
	c.regions[name] = r
}

func defaultRules(groupID string) []osapi.SecurityRule {
	return []osapi.SecurityRule{
		{ID: groupID + "-e4", Direction: "egress", EtherType: "IPv4"},
		{ID: groupID + "-e6", Direction: "egress", EtherType: "IPv6"},
		{ID: groupID + "-i4", Direction: "ingress", EtherType: "IPv4", RemoteGroupID: groupID},
		{ID: groupID + "-i6", Direction: "ingress", EtherType: "IPv6", RemoteGroupID: groupID},
	}
}

func (c *Cloud) region(name string) *region { return c.regions[name] }

// Regions implements osapi.Provider: the regions with a compute endpoint.
func (c *Cloud) Regions(ctx context.Context) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.injected("", "Regions"); err != nil {
		return nil, err
	}
	var out []string
	for _, n := range c.order {
		if !c.regions[n].cfg.NoEndpoint {
			out = append(out, n)
		}
	}
	return out, nil
}

// Clients implements osapi.Provider.
func (c *Cloud) Clients(ctx context.Context, name string) (osapi.Clients, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.injected(name, "Clients"); err != nil {
		return osapi.Clients{}, err
	}
	r := c.regions[name]
	if r == nil || r.cfg.NoEndpoint {
		return osapi.Clients{}, osapi.ErrNoEndpoint
	}
	cl := &client{c: c, r: r}
	return osapi.Clients{Compute: cl, Network: cl, Images: cl, Volumes: cl}, nil
}

// InjectError makes the next times calls of op in the region (region "" is every
// region, op "*" every call, "Clients" the catalog lookup) fail with err.
// times 0 means until ClearErrors.
func (c *Cloud) InjectError(region, op string, err error, times int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inject = append(c.inject, &injection{region: region, op: op, err: err, times: times})
}

// ClearErrors removes every injected error.
func (c *Cloud) ClearErrors() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inject = nil
}

func (c *Cloud) injected(region, op string) error {
	for i, in := range c.inject {
		if (in.region == "" || in.region == region) && (in.op == "*" || in.op == op) {
			if in.times > 0 {
				in.times--
				if in.times == 0 {
					c.inject = slices.Delete(c.inject, i, i+1)
				}
			}
			return in.err
		}
	}
	return nil
}

// SetNoValidHost makes the scheduler fail the servers of the flavor in the
// region: they turn ERROR with the NoValidHost fault when first read.
func (c *Cloud) SetNoValidHost(region, flavor string, on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.region(region).noValid[flavor] = on
}

// SetLimits replaces the absolute limits of the region (the used numbers are
// computed).
func (c *Cloud) SetLimits(region string, l osapi.Limits) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.region(region).cfg.Limits = l
}

// AddImage adds an image to the region and returns its ID. Empty Status is
// active, empty Visibility private, empty Owner the project.
func (c *Cloud) AddImage(region string, im osapi.Image) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if im.ID == "" {
		im.ID = c.id("img")
	}
	if im.Status == "" {
		im.Status = osapi.ImageActive
	}
	if im.Visibility == "" {
		im.Visibility = osapi.VisibilityPriv
	}
	if im.Owner == "" {
		im.Owner = c.ProjectID
	}
	if im.Created.IsZero() {
		im.Created = c.Now()
	}
	r := c.region(region)
	r.images = append(r.images, im)
	return im.ID
}

// AddForeignServer adds a server of the project that SCarpenter did not create
// (it counts against the limits and shows in lists).
func (c *Cloud) AddForeignServer(region, name, flavor string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.region(region)
	f := r.flavorByName(flavor)
	s := &serverRec{deleteCalls: -1, Server: osapi.Server{
		ID: c.id("srv"), Name: name, Status: osapi.StatusActive, Created: c.Now(), FlavorID: f.ID,
		Metadata: map[string]string{},
	}, foreign: true}
	r.servers[s.ID] = s
	r.order = append(r.order, s.ID)
	return s.ID
}

// PowerOff simulates a guest poweroff: an ACTIVE server turns SHUTOFF and stays
// (OpenStack has no shutdown behaviour terminate; the cloud keeps billing it).
func (c *Cloud) PowerOff(region, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.region(region).servers[id]; s != nil && s.Status == osapi.StatusActive {
		s.Status = osapi.StatusShutoff
	}
}

// ServerIDs returns the IDs of the region's servers, oldest first.
func (c *Cloud) ServerIDs(region string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.region(region).order)
}

// VolumeCount returns the number of Cinder volumes in the region.
func (c *Cloud) VolumeCount(region string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.region(region).volumes)
}

func (r *region) flavorByName(name string) osapi.Flavor {
	for _, f := range r.flavors {
		if f.Name == name {
			return f
		}
	}
	return osapi.Flavor{}
}

func (r *region) flavorByID(id string) (osapi.Flavor, bool) {
	for _, f := range r.flavors {
		if f.ID == id {
			return f, true
		}
	}
	return osapi.Flavor{}, false
}

func apiErr(service string, status int, format string, args ...any) *osapi.APIError {
	return &osapi.APIError{Service: service, Status: status, Message: fmt.Sprintf(format, args...)}
}

type client struct {
	c *Cloud
	r *region
}

// enter takes the lock and checks the injected errors.
func (cl *client) enter(op string) error {
	cl.c.mu.Lock()
	cl.reap()
	return cl.c.injected(cl.r.name, op)
}

// reap advances the servers in DELETING: after DeleteReads calls into the
// region they are gone, with their volumes and group membership.
func (cl *client) reap() {
	for _, id := range slices.Clone(cl.r.order) {
		s := cl.r.servers[id]
		if s.deleteCalls < 0 {
			continue
		}
		if s.deleteCalls >= cl.c.DeleteReads {
			cl.remove(s)
			continue
		}
		s.deleteCalls++
	}
}

func (cl *client) remove(s *serverRec) {
	for _, v := range s.volumes {
		if v.deleteOnTerminate {
			delete(cl.r.volumes, v.id)
		} else {
			v.attachedTo = ""
		}
	}
	if g := cl.r.groups[s.groupID]; g != nil {
		g.Members = slices.DeleteFunc(g.Members, func(m string) bool { return m == s.ID })
	}
	delete(cl.r.servers, s.ID)
	cl.r.order = slices.DeleteFunc(cl.r.order, func(o string) bool { return o == s.ID })
}

func (cl *client) leave() { cl.c.mu.Unlock() }

// advance moves a server along its state machine on a read. A server being
// deleted stays as it is (Nova does not finish the build of a server it deletes).
func (cl *client) advance(s *serverRec) {
	if s.Status != osapi.StatusBuild || s.deleteCalls >= 0 {
		return
	}
	s.reads++
	if s.reads == 1 {
		f, _ := cl.r.flavorByID(s.FlavorID)
		if cl.r.noValid[f.Name] || cl.antiAffinityFull(s) {
			s.Status, s.TaskState = osapi.StatusError, ""
			s.Fault = &osapi.Fault{Code: 500, Message: "No valid host was found. There are not enough hosts available."}
			return
		}
		s.TaskState = osapi.TaskSpawning
		return
	}
	if s.reads > cl.c.BuildReads {
		s.Status, s.TaskState = osapi.StatusActive, ""
		s.Addresses = []osapi.Address{
			{Network: osapi.ExternalNetwork, Addr: fmt.Sprintf("198.51.100.%d", 1+len(cl.r.order)%250), Version: 4},
			{Network: osapi.ExternalNetwork, Addr: fmt.Sprintf("2001:db8::%x", 1+len(cl.r.order)), Version: 6},
		}
	}
}

func (cl *client) antiAffinityFull(s *serverRec) bool {
	g := cl.r.groups[s.groupID]
	return g != nil && g.Policy == "anti-affinity" && cl.r.cfg.Hosts > 0 && slices.Index(g.Members, s.ID) >= cl.r.cfg.Hosts
}

// view returns a copy of the server as the API shows it. From microversion 2.47
// a server's flavor has a name and no ID.
func (r *region) view(s *serverRec) osapi.Server {
	out := s.Server
	f, _ := r.flavorByID(s.FlavorID)
	out.FlavorID, out.FlavorName = "", f.Name
	out.Metadata = cloneMap(s.Metadata)
	out.Tags = slices.Clone(s.Tags)
	out.Addresses = slices.Clone(s.Addresses)
	out.SecurityGroups = slices.Clone(s.secGroups)
	for _, v := range s.volumes {
		out.Volumes = append(out.Volumes, v.id)
	}
	return out
}

func cloneMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	return maps.Clone(m)
}

func (cl *client) ListFlavors(ctx context.Context) ([]osapi.Flavor, error) {
	if err := cl.enter("ListFlavors"); err != nil {
		cl.leave()
		return nil, err
	}
	defer cl.leave()
	return slices.Clone(cl.r.flavors), nil
}

func (cl *client) ListServers(ctx context.Context, f osapi.ServerFilter) ([]osapi.Server, error) {
	if err := cl.enter("ListServers"); err != nil {
		cl.leave()
		return nil, err
	}
	defer cl.leave()
	var re *regexp.Regexp
	if f.Name != "" {
		var err error
		if re, err = regexp.Compile(f.Name); err != nil {
			return nil, apiErr("nova", 400, "Invalid filter name: %v", err)
		}
	}
	var out []osapi.Server
	for _, id := range cl.r.order {
		s := cl.r.servers[id]
		cl.advance(s)
		if re != nil && !re.MatchString(s.Name) {
			continue
		}
		if f.Status != "" && s.Status != f.Status {
			continue
		}
		if !containsAll(s.Tags, f.Tags) {
			continue
		}
		out = append(out, cl.r.view(s))
	}
	return out, nil
}

func containsAll(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

func (cl *client) GetServer(ctx context.Context, id string) (*osapi.Server, error) {
	if err := cl.enter("GetServer"); err != nil {
		cl.leave()
		return nil, err
	}
	defer cl.leave()
	s := cl.r.servers[id]
	if s == nil {
		return nil, apiErr("nova", 404, "Instance %s could not be found.", id)
	}
	cl.advance(s)
	out := cl.r.view(s)
	return &out, nil
}

func (cl *client) used() (instances, cores, ram int64) {
	for _, s := range cl.r.servers {
		f, _ := cl.r.flavorByID(s.FlavorID)
		instances++
		cores += f.VCPUs
		ram += f.RAMMiB
	}
	return
}

func (cl *client) limits() osapi.Limits {
	l := cl.r.cfg.Limits
	def := func(v *int64, d int64) {
		if *v == 0 {
			*v = d
		}
	}
	def(&l.MaxInstances, 1000)
	def(&l.MaxCores, 100000)
	def(&l.MaxRAMMiB, 100000000)
	def(&l.MaxServerGroups, 10)
	def(&l.MaxSecurityGroups, 100)
	l.UsedInstances, l.UsedCores, l.UsedRAMMiB = cl.used()
	l.UsedServerGroups = int64(len(cl.r.groups))
	l.UsedSecurityGroups = int64(len(cl.r.secGroups))
	return l
}

func (cl *client) GetLimits(ctx context.Context) (*osapi.Limits, error) {
	if err := cl.enter("GetLimits"); err != nil {
		cl.leave()
		return nil, err
	}
	defer cl.leave()
	l := cl.limits()
	return &l, nil
}

func (cl *client) ListAvailabilityZones(ctx context.Context) ([]osapi.AvailabilityZone, error) {
	if err := cl.enter("ListAvailabilityZones"); err != nil {
		cl.leave()
		return nil, err
	}
	defer cl.leave()
	if len(cl.r.cfg.AZs) == 0 {
		return []osapi.AvailabilityZone{{Name: "nova", Available: true}}, nil
	}
	var out []osapi.AvailabilityZone
	for _, z := range cl.r.cfg.AZs {
		out = append(out, osapi.AvailabilityZone{Name: z, Available: true})
	}
	return out, nil
}

func (cl *client) CreateServer(ctx context.Context, req osapi.CreateServerRequest) (*osapi.Server, error) {
	if err := cl.enter("CreateServer"); err != nil {
		cl.leave()
		return nil, err
	}
	defer cl.leave()
	r := cl.r
	f, err := cl.checkParameters(req)
	if err != nil {
		return nil, err
	}
	vols, grp, err := cl.checkReferences(req, f)
	if err != nil {
		return nil, err
	}
	l := cl.limits()
	switch {
	case l.UsedInstances+1 > l.MaxInstances:
		return nil, apiErr(svcNova, 403,
			"Quota exceeded for instances: Requested 1, but already used %d of %d instances", l.UsedInstances, l.MaxInstances)
	case l.UsedCores+f.VCPUs > l.MaxCores:
		return nil, apiErr(svcNova, 403,
			"Quota exceeded for cores: Requested %d, but already used %d of %d cores", f.VCPUs, l.UsedCores, l.MaxCores)
	case l.UsedRAMMiB+f.RAMMiB > l.MaxRAMMiB:
		return nil, apiErr(svcNova, 403,
			"Quota exceeded for ram: Requested %d, but already used %d of %d ram", f.RAMMiB, l.UsedRAMMiB, l.MaxRAMMiB)
	}
	s := &serverRec{deleteCalls: -1, Server: osapi.Server{
		ID: cl.c.id("srv"), Name: req.Name, Status: osapi.StatusBuild, TaskState: osapi.TaskScheduling,
		Created: cl.c.Now(), FlavorID: f.ID, ImageID: req.ImageID, AvailabilityZone: req.AvailabilityZone,
		Metadata: cloneMap(req.Metadata), Tags: slices.Clone(req.Tags),
	}, secGroups: slices.Clone(req.SecurityGroups)}
	for _, v := range vols {
		v.id, v.attachedTo = cl.c.id("vol"), s.ID
		r.volumes[v.id] = v
		s.volumes = append(s.volumes, v)
	}
	if grp != nil {
		grp.Members = append(grp.Members, s.ID)
		s.groupID = grp.ID
	}
	r.servers[s.ID] = s
	r.order = append(r.order, s.ID)
	out := r.view(s)
	return &out, nil
}

const svcNova = "nova"

// checkParameters validates what the request says by itself: name, flavor, user
// data, metadata and availability zone.
func (cl *client) checkParameters(req osapi.CreateServerRequest) (osapi.Flavor, error) {
	r := cl.r
	if req.Name == "" || len(req.Name) > 255 {
		return osapi.Flavor{}, apiErr(svcNova, 400,
			"Invalid input for field/attribute name. Value: %q. is too short or too long", req.Name)
	}
	f, ok := r.flavorByID(req.FlavorID)
	if !ok {
		return osapi.Flavor{}, apiErr(svcNova, 400, "Invalid flavorRef provided.")
	}
	if n := base64.StdEncoding.EncodedLen(len(req.UserData)); n > MaxUserDataBase64 {
		return osapi.Flavor{}, apiErr(svcNova, 400,
			"User data too large. User data must be no larger than %d bytes once base64 encoded. "+
				"Your data is %d bytes", MaxUserDataBase64, n)
	}
	if len(req.Metadata) > 128 {
		return osapi.Flavor{}, apiErr(svcNova, 403,
			"Quota exceeded for metadata items: Requested %d, but already used 0 of 128 metadata items", len(req.Metadata))
	}
	for k, v := range req.Metadata {
		if k == "" || len(k) > 255 || len(v) > 255 {
			return osapi.Flavor{}, apiErr(svcNova, 400,
				"Invalid metadata: Metadata item key/value is not a string of 1 to 255 characters")
		}
	}
	if req.AvailabilityZone != "" {
		zones := r.cfg.AZs
		if len(zones) == 0 {
			zones = []string{"nova"}
		}
		if !slices.Contains(zones, req.AvailabilityZone) {
			return osapi.Flavor{}, apiErr(svcNova, 400, "The requested availability zone is not available")
		}
	}
	return f, nil
}

// checkReferences validates the objects the request points at (image, networks,
// security groups, volume types, server group) and returns the volumes the block
// device mappings will create and the server group.
func (cl *client) checkReferences(
	req osapi.CreateServerRequest, f osapi.Flavor,
) ([]*volumeRec, *osapi.ServerGroup, error) {
	r := cl.r
	if req.ImageID != "" {
		im, found := r.image(req.ImageID)
		if !found {
			return nil, nil, apiErr(svcNova, 400, "Invalid imageRef provided.")
		}
		if im.Status != osapi.ImageActive {
			return nil, nil, apiErr(svcNova, 400, "Image %s is not active.", im.ID)
		}
		if imageGiB := (im.VirtualSizeBytes + 1<<30 - 1) >> 30; imageGiB > f.DiskGB {
			return nil, nil, apiErr(svcNova, 400,
				"Flavor's disk is too small for requested image. "+
					"Flavor disk is %d bytes, image is %d bytes.", f.DiskGB<<30, im.VirtualSizeBytes)
		}
	}
	for _, n := range req.NetworkIDs {
		if n != r.extNet.ID && !r.hasNet(n) {
			return nil, nil, apiErr(svcNova, 400, "Network %s could not be found.", n)
		}
	}
	for _, name := range req.SecurityGroups {
		switch n := r.countGroups(name); {
		case n == 0:
			return nil, nil, apiErr(svcNova, 400, "Unable to find security group %s", name)
		case n > 1:
			return nil, nil, apiErr(svcNova, 409,
				"Multiple security groups found matching '%s'. Use an ID to be more specific.", name)
		}
	}
	var vols []*volumeRec
	for _, b := range req.BlockDevices {
		if b.DestinationType != "volume" {
			continue
		}
		if b.VolumeType != "" && !r.hasVolType(b.VolumeType) {
			return nil, nil, apiErr(svcNova, 400,
				"Invalid input received: Invalid input received: volume_type %q not found", b.VolumeType)
		}
		vols = append(vols, &volumeRec{
			volType: b.VolumeType, sizeGiB: b.SizeGiB, deleteOnTerminate: b.DeleteOnTermination, source: b.SourceType,
		})
	}
	var grp *osapi.ServerGroup
	if req.SchedulerGroup != "" {
		if grp = r.groups[req.SchedulerGroup]; grp == nil {
			return nil, nil, apiErr(svcNova, 400, "Server group %s could not be found.", req.SchedulerGroup)
		}
	}
	return vols, grp, nil
}

func (r *region) image(id string) (osapi.Image, bool) {
	for _, im := range r.images {
		if im.ID == id {
			return im, true
		}
	}
	return osapi.Image{}, false
}

func (r *region) hasNet(id string) bool {
	for _, n := range r.privNets {
		if n.ID == id {
			return true
		}
	}
	return false
}

func (r *region) countGroups(name string) int {
	n := 0
	for _, g := range r.secGroups {
		if g.Name == name || g.ID == name {
			n++
		}
	}
	return n
}

func (r *region) hasVolType(name string) bool {
	for _, v := range r.volTypes {
		if v.Name == name || v.ID == name {
			return true
		}
	}
	return false
}

func (cl *client) DeleteServer(ctx context.Context, id string) error {
	if err := cl.enter("DeleteServer"); err != nil {
		cl.leave()
		return err
	}
	defer cl.leave()
	s := cl.r.servers[id]
	if s == nil {
		return apiErr("nova", 404, "Instance %s could not be found.", id)
	}
	if s.deleteCalls >= 0 {
		return nil // Nova accepts a delete of a server already deleting
	}
	if cl.c.DeleteReads <= 0 {
		cl.remove(s)
		return nil
	}
	// Nova keeps the status (ACTIVE, ERROR, ...) and sets the task state.
	s.TaskState, s.deleteCalls = osapi.TaskDeleting, 0
	return nil
}

var serverGroupPolicies = []string{"affinity", "anti-affinity", "soft-affinity", "soft-anti-affinity"}

func (cl *client) ListServerGroups(ctx context.Context) ([]osapi.ServerGroup, error) {
	if err := cl.enter("ListServerGroups"); err != nil {
		cl.leave()
		return nil, err
	}
	defer cl.leave()
	var out []osapi.ServerGroup
	for _, g := range cl.r.groups {
		out = append(out, *g)
	}
	slices.SortFunc(out, func(a, b osapi.ServerGroup) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func (cl *client) CreateServerGroup(ctx context.Context, name, policy string) (*osapi.ServerGroup, error) {
	if err := cl.enter("CreateServerGroup"); err != nil {
		cl.leave()
		return nil, err
	}
	defer cl.leave()
	if !slices.Contains(serverGroupPolicies, policy) {
		return nil, apiErr("nova", 400,
			"Invalid input for field/attribute policy. Value: %q. is not one of the allowed values", policy)
	}
	if l := cl.limits(); l.UsedServerGroups+1 > l.MaxServerGroups {
		return nil, apiErr("nova", 403, "Quota exceeded, too many server groups.")
	}
	g := &osapi.ServerGroup{ID: cl.c.id("sgrp"), Name: name, Policy: policy}
	cl.r.groups[g.ID] = g
	out := *g
	return &out, nil
}

func (cl *client) DeleteServerGroup(ctx context.Context, id string) error {
	if err := cl.enter("DeleteServerGroup"); err != nil {
		cl.leave()
		return err
	}
	defer cl.leave()
	if cl.r.groups[id] == nil {
		return apiErr("nova", 404, "Instance group %s could not be found.", id)
	}
	delete(cl.r.groups, id)
	return nil
}

func (cl *client) ListNetworks(ctx context.Context) ([]osapi.Network, error) {
	if err := cl.enter("ListNetworks"); err != nil {
		cl.leave()
		return nil, err
	}
	defer cl.leave()
	return append([]osapi.Network{cl.r.extNet}, cl.r.privNets...), nil
}

func copyGroup(g *osapi.SecurityGroup) osapi.SecurityGroup {
	out := *g
	out.Rules = slices.Clone(g.Rules)
	out.Tags = slices.Clone(g.Tags)
	return out
}

func (cl *client) ListSecurityGroups(ctx context.Context) ([]osapi.SecurityGroup, error) {
	if err := cl.enter("ListSecurityGroups"); err != nil {
		cl.leave()
		return nil, err
	}
	defer cl.leave()
	out := make([]osapi.SecurityGroup, 0, len(cl.r.secGroups))
	for _, g := range cl.r.secGroups {
		out = append(out, copyGroup(g))
	}
	return out, nil
}

func (cl *client) CreateSecurityGroup(ctx context.Context, name, description string) (*osapi.SecurityGroup, error) {
	if err := cl.enter("CreateSecurityGroup"); err != nil {
		cl.leave()
		return nil, err
	}
	defer cl.leave()
	if l := cl.limits(); l.UsedSecurityGroups+1 > l.MaxSecurityGroups {
		return nil, apiErr("neutron", 409, "OverQuota: Quota exceeded for resources: ['security_group'].")
	}
	g := &osapi.SecurityGroup{ID: cl.c.id("sg"), Name: name, Description: description}
	// Neutron creates the two egress rules on every new group.
	g.Rules = defaultRules(g.ID)[:2]
	cl.r.secGroups = append(cl.r.secGroups, g)
	out := copyGroup(g)
	return &out, nil
}

func (cl *client) CreateSecurityRule(
	ctx context.Context, groupID string, rule osapi.SecurityRule,
) (*osapi.SecurityRule, error) {
	if err := cl.enter("CreateSecurityRule"); err != nil {
		cl.leave()
		return nil, err
	}
	defer cl.leave()
	for _, g := range cl.r.secGroups {
		if g.ID == groupID {
			for _, e := range g.Rules {
				e.ID = ""
				probe := rule
				probe.ID = ""
				if e == probe {
					return nil, apiErr("neutron", 409, "SecurityGroupRuleExists: Security group rule already exists.")
				}
			}
			rule.ID = cl.c.id("rule")
			g.Rules = append(g.Rules, rule)
			return &rule, nil
		}
	}
	return nil, apiErr("neutron", 404, "SecurityGroupNotFound: Security group %s does not exist", groupID)
}

func (cl *client) DeleteSecurityRule(ctx context.Context, ruleID string) error {
	if err := cl.enter("DeleteSecurityRule"); err != nil {
		cl.leave()
		return err
	}
	defer cl.leave()
	for _, g := range cl.r.secGroups {
		for i, r := range g.Rules {
			if r.ID == ruleID {
				g.Rules = slices.Delete(g.Rules, i, i+1)
				return nil
			}
		}
	}
	return apiErr("neutron", 404, "SecurityGroupRuleNotFound: Security group rule %s does not exist", ruleID)
}

func (cl *client) DeleteSecurityGroup(ctx context.Context, id string) error {
	if err := cl.enter("DeleteSecurityGroup"); err != nil {
		cl.leave()
		return err
	}
	defer cl.leave()
	for i, g := range cl.r.secGroups {
		if g.ID != id {
			continue
		}
		if id == cl.r.defaultSGs {
			return apiErr("neutron", 409,
				"SecurityGroupCannotRemoveDefault: Insufficient rights for removing default security group.")
		}
		for _, s := range cl.r.servers {
			if slices.Contains(s.secGroups, g.Name) || slices.Contains(s.secGroups, g.ID) {
				return apiErr("neutron", 409, "SecurityGroupInUse: Security Group %s in use.", id)
			}
		}
		cl.r.secGroups = slices.Delete(cl.r.secGroups, i, i+1)
		return nil
	}
	return apiErr("neutron", 404, "SecurityGroupNotFound: Security group %s does not exist", id)
}

func (cl *client) ListImages(ctx context.Context, f osapi.ImageFilter) ([]osapi.Image, error) {
	if err := cl.enter("ListImages"); err != nil {
		cl.leave()
		return nil, err
	}
	defer cl.leave()
	var out []osapi.Image
	for _, im := range cl.r.images {
		// Glance lists the project's own images, public ones, and community
		// images only when the visibility filter asks for them.
		visible := im.Owner == cl.c.ProjectID || im.Visibility == "public" ||
			(im.Visibility == osapi.VisibilityComm && f.Visibility == osapi.VisibilityComm)
		if !visible ||
			(f.Name != "" && im.Name != f.Name) || (f.Status != "" && im.Status != f.Status) ||
			(f.Visibility != "" && im.Visibility != f.Visibility) || (f.Owner != "" && im.Owner != f.Owner) ||
			!containsAll(im.Tags, f.Tags) {
			continue
		}
		im.Tags = slices.Clone(im.Tags)
		im.Properties = cloneMap(im.Properties)
		out = append(out, im)
	}
	return out, nil
}

func (cl *client) ListVolumeTypes(ctx context.Context) ([]osapi.VolumeType, error) {
	if err := cl.enter("ListVolumeTypes"); err != nil {
		cl.leave()
		return nil, err
	}
	defer cl.leave()
	return slices.Clone(cl.r.volTypes), nil
}

var _ osapi.Provider = (*Cloud)(nil)
var _ osapi.Compute = (*client)(nil)
var _ osapi.NetworkAPI = (*client)(nil)
var _ osapi.Images = (*client)(nil)
var _ osapi.Volumes = (*client)(nil)
