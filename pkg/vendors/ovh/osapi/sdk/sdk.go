/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package sdk implements the osapi interfaces on gophercloud v2. It is the only
// place of the driver that touches the SDK. The calls are the OpenStack APIs of
// one OVHcloud Public Cloud project: Keystone's catalog, Nova, Neutron, Glance
// and Cinder, per region.
package sdk

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumetypes"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/availabilityzones"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/limits"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servergroups"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/groups"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/rules"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/v2/pagination"

	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi"
)

// DefaultComputeMicroversion is the Nova microversion the driver asks for: 2.67
// gives server tags (2.26), the block device mapping volume_type, and, from 2.64,
// the server group policy field. Unverified on OVH (plan spike S4).
//
// From 2.47 a server's flavor has no id, only the embedded flavor (the name is
// original_name), which the adapter reads into Server.FlavorName.
const DefaultComputeMicroversion = "2.67"

// Options configure the provider.
type Options struct {
	// ComputeMicroversion overrides DefaultComputeMicroversion.
	ComputeMicroversion string
	// UserAgent is prepended to the SDK's (scarpenter/<version>).
	UserAgent string
	// MaxBackoffRetries is how often gophercloud retries a 429 (default 5).
	MaxBackoffRetries uint
}

// Provider implements osapi.Provider on an authenticated gophercloud provider
// client.
type Provider struct {
	pc   *gophercloud.ProviderClient
	opts Options
}

// New wraps an authenticated provider client (any auth method: the standard
// environment, application credentials, the OIDC flow of the service-account
// mode: TODO(plan 14) wire ProviderClient.ReauthFunc for it).
func New(pc *gophercloud.ProviderClient, opts Options) *Provider {
	if opts.ComputeMicroversion == "" {
		opts.ComputeMicroversion = DefaultComputeMicroversion
	}
	if opts.MaxBackoffRetries == 0 {
		opts.MaxBackoffRetries = 5
	}
	if opts.UserAgent != "" {
		pc.UserAgent.Prepend(opts.UserAgent)
	}
	pc.MaxBackoffRetries = opts.MaxBackoffRetries
	return &Provider{pc: pc, opts: opts}
}

// FromEnv authenticates with the standard OS_* environment (OS_AUTH_URL,
// OS_USERNAME, OS_PASSWORD, OS_PROJECT_ID, OS_USER_DOMAIN_NAME): the
// `openstackUser` credentials mode. Re-authentication on expiry is on.
func FromEnv(ctx context.Context, opts Options) (*Provider, error) {
	ao, err := openstack.AuthOptionsFromEnv()
	if err != nil {
		return nil, err
	}
	ao.AllowReauth = true
	pc, err := openstack.AuthenticatedClient(ctx, ao)
	if err != nil {
		return nil, err
	}
	return New(pc, opts), nil
}

// Regions implements osapi.Provider: the regions of the project's token catalog
// that have a public compute endpoint.
func (p *Provider) Regions(ctx context.Context) ([]string, error) {
	ident, err := openstack.NewIdentityV3(p.pc, gophercloud.EndpointOpts{})
	if err != nil {
		return nil, wrap("keystone", err)
	}
	cat, err := tokens.Get(ctx, ident, p.pc.Token()).ExtractServiceCatalog()
	if err != nil {
		return nil, wrap("keystone", err)
	}
	return computeRegions(cat), nil
}

func computeRegions(cat *tokens.ServiceCatalog) []string {
	var out []string
	for _, e := range cat.Entries {
		if e.Type != "compute" {
			continue
		}
		for _, ep := range e.Endpoints {
			if ep.Interface == "public" && ep.Region != "" && !slices.Contains(out, ep.Region) {
				out = append(out, ep.Region)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Clients implements osapi.Provider. Compute, Neutron and Glance are required;
// a region without Cinder gets a Volumes that fails with ErrNoEndpoint.
func (p *Provider) Clients(ctx context.Context, region string) (osapi.Clients, error) {
	eo := gophercloud.EndpointOpts{Region: region, Availability: gophercloud.AvailabilityPublic}
	nova, err := openstack.NewComputeV2(p.pc, eo)
	if err != nil {
		return osapi.Clients{}, convertEndpoint(err)
	}
	nova.Microversion = p.opts.ComputeMicroversion
	neutron, err := openstack.NewNetworkV2(p.pc, eo)
	if err != nil {
		return osapi.Clients{}, convertEndpoint(err)
	}
	glance, err := openstack.NewImageV2(p.pc, eo)
	if err != nil {
		return osapi.Clients{}, convertEndpoint(err)
	}
	c := osapi.Clients{Compute: NewCompute(nova), Network: NewNetwork(neutron), Images: NewImages(glance), Volumes: noVolumes{}}
	if cinder, err := openstack.NewBlockStorageV3(p.pc, eo); err == nil {
		c.Volumes = NewVolumes(cinder)
	}
	return c, nil
}

func convertEndpoint(err error) error {
	if noEndpoint(err) {
		return fmt.Errorf("%w: %v", osapi.ErrNoEndpoint, err)
	}
	return wrap("keystone", err)
}

type noVolumes struct{}

func (noVolumes) ListVolumeTypes(context.Context) ([]osapi.VolumeType, error) {
	return nil, osapi.ErrNoEndpoint
}

// rawAll reads every page of a list and decodes the array under key into T. The
// raw JSON is used where gophercloud's structs lack fields the driver needs
// (Nova's OS-EXT-* and the 2.47 flavor, Neutron's router:external, the 2.64
// server group policy) and where their embedded-struct decoding would drop them.
func rawAll[T any](ctx context.Context, pager pagination.Pager, key string) ([]T, error) {
	page, err := pager.AllPages(ctx)
	if err != nil {
		return nil, err
	}
	return decodeKey[T](page.GetBody(), key)
}

func decodeKey[T any](body any, key string) ([]T, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var env map[string][]T
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, err
	}
	return env[key], nil
}

func decodeOne[T any](body any, key string) (*T, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var env map[string]*T
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, err
	}
	if env[key] == nil {
		return nil, fmt.Errorf("response has no %q", key)
	}
	return env[key], nil
}

// ---------------------------------------------------------------- Nova

// Compute implements osapi.Compute.
type Compute struct{ sc *gophercloud.ServiceClient }

// NewCompute wraps a compute service client (its Microversion is the caller's).
func NewCompute(sc *gophercloud.ServiceClient) *Compute { return &Compute{sc: sc} }

type wireServer struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Status  string    `json:"status"`
	Created time.Time `json:"created"`
	Flavor  struct {
		ID           string `json:"id"`
		OriginalName string `json:"original_name"`
	} `json:"flavor"`
	Image     json.RawMessage `json:"image"`
	Metadata  map[string]string
	Tags      []string
	Addresses map[string][]struct {
		Addr    string `json:"addr"`
		Version int    `json:"version"`
	} `json:"addresses"`
	AZ    string  `json:"OS-EXT-AZ:availability_zone"`
	Task  *string `json:"OS-EXT-STS:task_state"`
	Fault *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"fault"`
	Volumes []struct {
		ID string `json:"id"`
	} `json:"os-extended-volumes:volumes_attached"`
}

func (w wireServer) server() osapi.Server {
	s := osapi.Server{
		ID: w.ID, Name: w.Name, Status: w.Status, Created: w.Created, FlavorID: w.Flavor.ID,
		FlavorName: w.Flavor.OriginalName, AvailabilityZone: w.AZ, Metadata: w.Metadata, Tags: w.Tags,
	}
	if w.Task != nil {
		s.TaskState = *w.Task
	}
	var img struct {
		ID string `json:"id"`
	}
	if len(w.Image) > 0 && w.Image[0] == '{' && json.Unmarshal(w.Image, &img) == nil {
		s.ImageID = img.ID
	}
	nets := make([]string, 0, len(w.Addresses))
	for n := range w.Addresses {
		nets = append(nets, n)
	}
	sort.Strings(nets)
	for _, n := range nets {
		for _, a := range w.Addresses[n] {
			s.Addresses = append(s.Addresses, osapi.Address{Network: n, Addr: a.Addr, Version: a.Version})
		}
	}
	if w.Fault != nil {
		s.Fault = &osapi.Fault{Code: w.Fault.Code, Message: w.Fault.Message}
	}
	for _, v := range w.Volumes {
		s.Volumes = append(s.Volumes, v.ID)
	}
	return s
}

// ListFlavors implements osapi.Compute.
func (c *Compute) ListFlavors(ctx context.Context) ([]osapi.Flavor, error) {
	page, err := flavors.ListDetail(c.sc, flavors.ListOpts{}).AllPages(ctx)
	if err != nil {
		return nil, wrap("nova", err)
	}
	fl, err := flavors.ExtractFlavors(page)
	if err != nil {
		return nil, wrap("nova", err)
	}
	out := make([]osapi.Flavor, 0, len(fl))
	for _, f := range fl {
		out = append(out, osapi.Flavor{ID: f.ID, Name: f.Name, VCPUs: int64(f.VCPUs), RAMMiB: int64(f.RAM), DiskGB: int64(f.Disk)})
	}
	return out, nil
}

// ListServers implements osapi.Compute: the detailed list, every page.
func (c *Compute) ListServers(ctx context.Context, f osapi.ServerFilter) ([]osapi.Server, error) {
	opts := servers.ListOpts{Name: f.Name, Status: f.Status, Tags: strings.Join(f.Tags, ",")}
	raw, err := rawAll[wireServer](ctx, servers.List(c.sc, opts), "servers")
	if err != nil {
		return nil, wrap("nova", err)
	}
	out := make([]osapi.Server, 0, len(raw))
	for _, w := range raw {
		out = append(out, w.server())
	}
	return out, nil
}

// GetServer implements osapi.Compute.
func (c *Compute) GetServer(ctx context.Context, id string) (*osapi.Server, error) {
	r := servers.Get(ctx, c.sc, id)
	if r.Err != nil {
		return nil, wrap("nova", r.Err)
	}
	w, err := decodeOne[wireServer](r.Body, "server")
	if err != nil {
		return nil, err
	}
	s := w.server()
	return &s, nil
}

// createOpts builds the POST /servers body; exported to the tests through
// CreateBody.
func createOpts(req osapi.CreateServerRequest) (servers.CreateOpts, servers.SchedulerHintOptsBuilder) {
	o := servers.CreateOpts{
		Name: req.Name, FlavorRef: req.FlavorID, ImageRef: req.ImageID, SecurityGroups: req.SecurityGroups,
		ConfigDrive: req.ConfigDrive, AvailabilityZone: req.AvailabilityZone, Metadata: req.Metadata, Tags: req.Tags,
	}
	if len(req.UserData) > 0 {
		// Encode here: gophercloud sends data that already is valid base64 as is.
		o.UserData = []byte(base64.StdEncoding.EncodeToString(req.UserData))
	}
	if len(req.NetworkIDs) == 0 {
		o.Networks = "auto" // required from microversion 2.37
	} else {
		nets := make([]servers.Network, 0, len(req.NetworkIDs))
		for _, n := range req.NetworkIDs {
			nets = append(nets, servers.Network{UUID: n})
		}
		o.Networks = nets
	}
	for _, b := range req.BlockDevices {
		o.BlockDevice = append(o.BlockDevice, servers.BlockDevice{
			SourceType: servers.SourceType(b.SourceType), DestinationType: servers.DestinationType(b.DestinationType),
			UUID: b.UUID, BootIndex: b.BootIndex, VolumeSize: b.SizeGiB, VolumeType: b.VolumeType,
			DeleteOnTermination: b.DeleteOnTermination,
		})
	}
	var hints servers.SchedulerHintOptsBuilder
	if req.SchedulerGroup != "" {
		hints = servers.SchedulerHintOpts{Group: req.SchedulerGroup}
	}
	return o, hints
}

// CreateBody returns the JSON body CreateServer would send, for the tests and
// for debugging the request builder.
func CreateBody(req osapi.CreateServerRequest) (map[string]any, error) {
	o, hints := createOpts(req)
	body, err := o.ToServerCreateMap()
	if err != nil {
		return nil, err
	}
	if hints != nil {
		h, err := hints.ToSchedulerHintsMap()
		if err != nil {
			return nil, err
		}
		for k, v := range h {
			body[k] = v // {"os:scheduler_hints": {"group": id}}
		}
	}
	return body, nil
}

// CreateServer implements osapi.Compute. POST /servers answers with the new
// ID only, so the full server is read back; when that read fails the minimal
// server (ID, name, BUILD) is returned, so the caller never loses the ID.
func (c *Compute) CreateServer(ctx context.Context, req osapi.CreateServerRequest) (*osapi.Server, error) {
	o, hints := createOpts(req)
	r := servers.Create(ctx, c.sc, o, hints)
	if r.Err != nil {
		return nil, wrap("nova", r.Err)
	}
	w, err := decodeOne[struct {
		ID string `json:"id"`
	}](r.Body, "server")
	if err != nil {
		return nil, err
	}
	if s, err := c.GetServer(ctx, w.ID); err == nil {
		return s, nil
	}
	return &osapi.Server{ID: w.ID, Name: req.Name, Status: osapi.StatusBuild, Metadata: req.Metadata, Tags: req.Tags}, nil
}

// DeleteServer implements osapi.Compute.
func (c *Compute) DeleteServer(ctx context.Context, id string) error {
	return wrap("nova", servers.Delete(ctx, c.sc, id).Err)
}

// ListAvailabilityZones implements osapi.Compute.
func (c *Compute) ListAvailabilityZones(ctx context.Context) ([]osapi.AvailabilityZone, error) {
	page, err := availabilityzones.List(c.sc).AllPages(ctx)
	if err != nil {
		return nil, wrap("nova", err)
	}
	zs, err := availabilityzones.ExtractAvailabilityZones(page)
	if err != nil {
		return nil, wrap("nova", err)
	}
	out := make([]osapi.AvailabilityZone, 0, len(zs))
	for _, z := range zs {
		out = append(out, osapi.AvailabilityZone{Name: z.ZoneName, Available: z.ZoneState.Available})
	}
	return out, nil
}

// GetLimits implements osapi.Compute: the absolute limits of the project.
func (c *Compute) GetLimits(ctx context.Context) (*osapi.Limits, error) {
	l, err := limits.Get(ctx, c.sc, limits.GetOpts{}).Extract()
	if err != nil {
		return nil, wrap("nova", err)
	}
	a := l.Absolute
	return &osapi.Limits{
		MaxInstances: int64(a.MaxTotalInstances), UsedInstances: int64(a.TotalInstancesUsed),
		MaxCores: int64(a.MaxTotalCores), UsedCores: int64(a.TotalCoresUsed),
		MaxRAMMiB: int64(a.MaxTotalRAMSize), UsedRAMMiB: int64(a.TotalRAMUsed),
		MaxServerGroups: int64(a.MaxServerGroups), UsedServerGroups: int64(a.TotalServerGroupsUsed),
		MaxSecurityGroups: int64(a.MaxSecurityGroups), UsedSecurityGroups: int64(a.TotalSecurityGroupsUsed),
	}, nil
}

type wireServerGroup struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Policy   string   `json:"policy"`
	Policies []string `json:"policies"`
	Members  []string `json:"members"`
}

func (w wireServerGroup) group() osapi.ServerGroup {
	p := w.Policy
	if p == "" && len(w.Policies) > 0 {
		p = w.Policies[0] // before microversion 2.64
	}
	return osapi.ServerGroup{ID: w.ID, Name: w.Name, Policy: p, Members: w.Members}
}

// ListServerGroups implements osapi.Compute.
func (c *Compute) ListServerGroups(ctx context.Context) ([]osapi.ServerGroup, error) {
	raw, err := rawAll[wireServerGroup](ctx, servergroups.List(c.sc, servergroups.ListOpts{}), "server_groups")
	if err != nil {
		return nil, wrap("nova", err)
	}
	out := make([]osapi.ServerGroup, 0, len(raw))
	for _, w := range raw {
		out = append(out, w.group())
	}
	return out, nil
}

// CreateServerGroup implements osapi.Compute (microversion 2.64 policy field).
func (c *Compute) CreateServerGroup(ctx context.Context, name, policy string) (*osapi.ServerGroup, error) {
	r := servergroups.Create(ctx, c.sc, servergroups.CreateOpts{Name: name, Policy: policy})
	if r.Err != nil {
		return nil, wrap("nova", r.Err)
	}
	w, err := decodeOne[wireServerGroup](r.Body, "server_group")
	if err != nil {
		return nil, err
	}
	g := w.group()
	return &g, nil
}

// DeleteServerGroup implements osapi.Compute.
func (c *Compute) DeleteServerGroup(ctx context.Context, id string) error {
	return wrap("nova", servergroups.Delete(ctx, c.sc, id).Err)
}

// ---------------------------------------------------------------- Neutron

// Network implements osapi.NetworkAPI.
type Network struct{ sc *gophercloud.ServiceClient }

// NewNetwork wraps a Neutron service client.
func NewNetwork(sc *gophercloud.ServiceClient) *Network { return &Network{sc: sc} }

// ListNetworks implements osapi.NetworkAPI.
func (n *Network) ListNetworks(ctx context.Context) ([]osapi.Network, error) {
	type wireNet struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		External bool   `json:"router:external"`
	}
	raw, err := rawAll[wireNet](ctx, networks.List(n.sc, networks.ListOpts{}), "networks")
	if err != nil {
		return nil, wrap("neutron", err)
	}
	out := make([]osapi.Network, 0, len(raw))
	for _, w := range raw {
		out = append(out, osapi.Network{ID: w.ID, Name: w.Name, External: w.External})
	}
	return out, nil
}

func fromRule(r rules.SecGroupRule) osapi.SecurityRule {
	return osapi.SecurityRule{
		ID: r.ID, Direction: r.Direction, EtherType: r.EtherType, Protocol: r.Protocol,
		PortMin: r.PortRangeMin, PortMax: r.PortRangeMax, RemoteCIDR: r.RemoteIPPrefix, RemoteGroupID: r.RemoteGroupID,
	}
}

func fromGroup(g groups.SecGroup) osapi.SecurityGroup {
	out := osapi.SecurityGroup{ID: g.ID, Name: g.Name, Description: g.Description, Tags: g.Tags}
	for _, r := range g.Rules {
		out.Rules = append(out.Rules, fromRule(r))
	}
	return out
}

// ListSecurityGroups implements osapi.NetworkAPI.
func (n *Network) ListSecurityGroups(ctx context.Context) ([]osapi.SecurityGroup, error) {
	page, err := groups.List(n.sc, groups.ListOpts{}).AllPages(ctx)
	if err != nil {
		return nil, wrap("neutron", err)
	}
	gs, err := groups.ExtractGroups(page)
	if err != nil {
		return nil, wrap("neutron", err)
	}
	out := make([]osapi.SecurityGroup, 0, len(gs))
	for _, g := range gs {
		out = append(out, fromGroup(g))
	}
	return out, nil
}

// CreateSecurityGroup implements osapi.NetworkAPI.
func (n *Network) CreateSecurityGroup(ctx context.Context, name, description string) (*osapi.SecurityGroup, error) {
	g, err := groups.Create(ctx, n.sc, groups.CreateOpts{Name: name, Description: description}).Extract()
	if err != nil {
		return nil, wrap("neutron", err)
	}
	out := fromGroup(*g)
	return &out, nil
}

// CreateSecurityRule implements osapi.NetworkAPI.
func (n *Network) CreateSecurityRule(ctx context.Context, groupID string, r osapi.SecurityRule) (*osapi.SecurityRule, error) {
	o := rules.CreateOpts{
		Direction: rules.RuleDirection(r.Direction), EtherType: rules.RuleEtherType(r.EtherType), SecGroupID: groupID,
		Protocol: rules.RuleProtocol(r.Protocol), PortRangeMin: r.PortMin, PortRangeMax: r.PortMax,
		RemoteIPPrefix: r.RemoteCIDR, RemoteGroupID: r.RemoteGroupID,
	}
	created, err := rules.Create(ctx, n.sc, o).Extract()
	if err != nil {
		return nil, wrap("neutron", err)
	}
	out := fromRule(*created)
	return &out, nil
}

// DeleteSecurityGroup implements osapi.NetworkAPI.
func (n *Network) DeleteSecurityGroup(ctx context.Context, id string) error {
	return wrap("neutron", groups.Delete(ctx, n.sc, id).Err)
}

// ---------------------------------------------------------------- Glance

// Images implements osapi.Images.
type Images struct{ sc *gophercloud.ServiceClient }

// NewImages wraps a Glance service client.
func NewImages(sc *gophercloud.ServiceClient) *Images { return &Images{sc: sc} }

// ListImages implements osapi.Images; Glance filters server side on all the
// fields of the filter.
func (i *Images) ListImages(ctx context.Context, f osapi.ImageFilter) ([]osapi.Image, error) {
	opts := images.ListOpts{
		Name: f.Name, Status: images.ImageStatus(f.Status), Visibility: images.ImageVisibility(f.Visibility),
		Owner: f.Owner, Tags: f.Tags,
	}
	page, err := images.List(i.sc, opts).AllPages(ctx)
	if err != nil {
		return nil, wrap("glance", err)
	}
	ims, err := images.ExtractImages(page)
	if err != nil {
		return nil, wrap("glance", err)
	}
	out := make([]osapi.Image, 0, len(ims))
	for _, im := range ims {
		o := osapi.Image{
			ID: im.ID, Name: im.Name, Status: string(im.Status), Visibility: string(im.Visibility), Owner: im.Owner,
			Tags: im.Tags, Created: im.CreatedAt, SizeBytes: im.SizeBytes, VirtualSizeBytes: im.VirtualSize,
			Properties: map[string]string{},
		}
		for k, v := range im.Properties {
			switch x := v.(type) {
			case string:
				o.Properties[k] = x
			case float64:
				o.Properties[k] = fmt.Sprint(int64(x))
			}
		}
		out = append(out, o)
	}
	return out, nil
}

// ---------------------------------------------------------------- Cinder

// Volumes implements osapi.Volumes.
type Volumes struct{ sc *gophercloud.ServiceClient }

// NewVolumes wraps a Cinder service client.
func NewVolumes(sc *gophercloud.ServiceClient) *Volumes { return &Volumes{sc: sc} }

// ListVolumeTypes implements osapi.Volumes.
func (v *Volumes) ListVolumeTypes(ctx context.Context) ([]osapi.VolumeType, error) {
	page, err := volumetypes.List(v.sc, volumetypes.ListOpts{}).AllPages(ctx)
	if err != nil {
		return nil, wrap("cinder", err)
	}
	vts, err := volumetypes.ExtractVolumeTypes(page)
	if err != nil {
		return nil, wrap("cinder", err)
	}
	out := make([]osapi.VolumeType, 0, len(vts))
	for _, t := range vts {
		out = append(out, osapi.VolumeType{ID: t.ID, Name: t.Name})
	}
	return out, nil
}

var (
	_ osapi.Provider   = (*Provider)(nil)
	_ osapi.Compute    = (*Compute)(nil)
	_ osapi.NetworkAPI = (*Network)(nil)
	_ osapi.Images     = (*Images)(nil)
	_ osapi.Volumes    = (*Volumes)(nil)
)
