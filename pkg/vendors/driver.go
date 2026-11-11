/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package vendors is the driver contract (overview section 7, ADR-004): the
// Driver interface every vendor package implements, the optional capability
// interfaces a driver may add (discovered by type assertion), the error
// classes, the logical tag keys and the registry that instantiates the enabled
// drivers. The core never imports a vendor SDK: everything vendor-specific sits
// behind this package.
package vendors

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
)

// Capacity types, as Karpenter spells them.
const (
	CapacityOnDemand = "on-demand"
	CapacitySpot     = "spot"
)

// Instance states. A terminating or terminated instance counts as gone for
// Karpenter (Get returns NodeClaimNotFound). Terminating covers an instance the
// vendor is deleting and one that is stopped, powered off, suspended or
// deallocated by itself: it is not coming back, so the core deletes it after the
// terminating grace (DefaultTerminatingGrace).
const (
	StatePending     = "pending"
	StateRunning     = "running"
	StateTerminating = "terminating"
	StateTerminated  = "terminated"
)

// TagKey is a logical tag key. Drivers map it to the vendor's charset with
// Driver.TagKey (overview section 7.1).
type TagKey string

// The logical tag keys every instance carries.
const (
	TagCluster   TagKey = "cluster"
	TagNodeClaim TagKey = "nodeclaim"
	TagNodePool  TagKey = "nodepool"
	TagNodeClass TagKey = "nodeclass"
	TagManagedBy TagKey = "managed-by"
	// TagIdempotencyKey holds the NodeClaim UID on the instances of vendors that
	// have no create token (IdempotencyTag); drivers with a token or a
	// deterministic name do not write it.
	TagIdempotencyKey TagKey = "idempotency-key"
)

// ManagedByValue is the value of the managed-by tag.
const ManagedByValue = "scarpenter"

// TagUse is the logical key of the opt-in tag of the discovery step (ADR-022):
// a resource that carries it with the value OptInValue is one the operator
// offers to SCarpenter. It is reserved, encoded per vendor by Driver.TagKey
// like the other keys (scarpenter-use where "/" is not allowed), and is not one
// of TagKeys: instances do not carry it.
const TagUse TagKey = "use"

// OptInValue is the value of the opt-in tag.
const OptInValue = "true"

// TagKeys lists the logical tag keys in a fixed order.
var TagKeys = []TagKey{TagCluster, TagNodeClaim, TagNodePool, TagNodeClass, TagManagedBy, TagIdempotencyKey}

// DefaultTagKey maps a logical key the way AWS does:
// scarpenter.sparecores.com/<key>. Vendors with a restrictive label charset
// map to scarpenter-<key> instead.
func DefaultTagKey(k TagKey) string { return v1alpha1.Group + "/" + string(k) }

// IdempotencyTag returns tags with the idempotency key added (the input is not
// modified), for the driver of a vendor without a create token to write on the
// instance. An empty key adds nothing. The NodeClaim UID is 36 lower-case
// characters and fits every vendor's value rules.
func IdempotencyTag(tags map[TagKey]string, key string) map[TagKey]string {
	out := make(map[TagKey]string, len(tags)+1)
	maps.Copy(out, tags)
	if key != "" {
		out[TagIdempotencyKey] = key
	}
	return out
}

// IdempotencyKeyOf reads the idempotency key back from an instance's tags; empty
// when the instance has none.
func IdempotencyKeyOf(tags map[TagKey]string) string { return tags[TagIdempotencyKey] }

// FindByIdempotencyKey is the lookup of such a driver before every create: the
// instance of the key in the region, in whatever zone, type or capacity type it
// was launched, or nil when there is none (a failed create leaves the key
// unused). Terminated instances do not count.
func FindByIdempotencyKey(ctx context.Context, d Driver, region, key string) (*Instance, error) {
	if key == "" {
		return nil, nil
	}
	inst, err := d.FindInstance(ctx, region, map[TagKey]string{TagIdempotencyKey: key})
	if err != nil {
		if IsClass(err, NotFound) {
			return nil, nil
		}
		return nil, err
	}
	if inst.State == StateTerminated {
		return nil, nil
	}
	return inst, nil
}

// InstanceRef is the driver-opaque reference of an instance: the driver's
// region and its instance ID, "<region>/<id>" (for example us-east-1/i-0abc).
// Only drivers interpret the ID part; the core stores the reference in the
// NodeClaim annotation scarpenter.sparecores.com/instance-ref.
type InstanceRef string

// NewInstanceRef builds a reference from the region and the vendor's ID.
func NewInstanceRef(region, id string) InstanceRef {
	return InstanceRef(region + "/" + id)
}

// Region returns the region part.
func (r InstanceRef) Region() string {
	region, _, _ := strings.Cut(string(r), "/")
	return region
}

// ID returns the vendor's instance ID.
func (r InstanceRef) ID() string {
	_, id, _ := strings.Cut(string(r), "/")
	return id
}

// String implements fmt.Stringer.
func (r InstanceRef) String() string { return string(r) }

// Region is a region the driver's credentials can use.
type Region struct {
	// Name is the vendor's region ID, as Keeper's region api_reference
	// (us-east-1).
	Name string
}

// Zone is a zone of a region as the driver's account sees it (overview section
// 5.8, ADR-009).
type Zone struct {
	// ID is Keeper's zone ID, the portable identity (use1-az1).
	ID string
	// Name is the account's zone name (us-east-1a); the value of
	// topology.kubernetes.io/zone.
	Name string
}

// ImageRequest asks for the image of a Flatcar release.
type ImageRequest struct {
	Region  string
	Arch    string // amd64 or arm64
	Channel string
	Version string
	// Owner overrides the official image owner where the vendor has one.
	Owner string
	// Use is the NodeClass's `images` selector for the region and architecture,
	// with the placeholders expanded and the IDs of the region gathered (A1). When
	// it IsDefault the NodeClass gave none and the driver finds the pinned release
	// by its own convention (rungs 1 and 2 of ADR-021) or has none (rung 3: the
	// image is then not found). Whatever the lookup, the driver finishes with
	// CheckImageVersion: an image that does not say it is the pinned release is
	// never returned.
	Use UseRef
}

// Image is a boot image the driver resolved.
type Image struct {
	// Ref is the vendor's image reference (an AMI ID).
	Ref string
	// RootDiskBytes is the size of the image's root disk, 0 when unknown.
	RootDiskBytes int64
	// Channel and Version are the Flatcar release the image says it is, read
	// from its own labels, tags, properties or name; empty when it does not say.
	Channel string
	Version string
}

// NodeInfo describes the Node the instance will become. Real drivers ignore it
// (the kubelet registers the Node itself, configured by the user-data); the fake
// driver needs it to create a Node object for KWOK.
type NodeInfo struct {
	// Name is the node name (the NodeClaim name).
	Name       string
	ProviderID string
	Labels     map[string]string
	// Capacity and Allocatable are the instance type's resources.
	Capacity    corev1.ResourceList
	Allocatable corev1.ResourceList
}

// CreateRequest is one launch attempt (overview section 7.1).
type CreateRequest struct {
	Region string
	// Zone is the account's zone name.
	Zone string
	// InstanceType is the vendor's API reference (m7i.large), not the catalog
	// name (aws.m7i.large).
	InstanceType string
	CapacityType string
	Arch         string
	Image        Image
	// UserData is the rendered user-data, not encoded.
	UserData []byte
	// Settings are the resolved vendor settings (defaults, vendor block,
	// profiles).
	Settings VendorSettings
	// Prepared are the resources Prepare made ready.
	Prepared *Prepared
	// Tags holds the logical tags; ExtraTags the NodeClass's own tags.
	Tags      map[TagKey]string
	ExtraTags map[string]string
	// IdempotencyKey is the NodeClaim UID: a second call with the same key
	// returns the instance the first one created (AWS ClientToken).
	IdempotencyKey string
	Node           NodeInfo
}

// Instance is an instance as the driver reports it.
type Instance struct {
	Ref          InstanceRef
	Region       string
	Zone         string
	Type         string // the vendor's API reference
	CapacityType string
	State        string
	CreatedAt    time.Time
	// BillingStart is the vendor's own creation or billing-start stamp, which
	// the billing periods of the instance count from (ADR-024). Zero when the
	// vendor has none and the driver could not record one (billing retention
	// then does not hold the node, with an Event). It is not CreatedAt, which
	// is not always the billing start. The driver sets the earliest plausible
	// stamp: an early stamp only makes the computed period boundaries early,
	// the safe side. It is never after the instance's first GetInstance, is
	// stable across calls, and survives FindInstance and ListInstances.
	BillingStart time.Time
	Tags         map[TagKey]string
	// Addresses are the instance's IP addresses, if known.
	Addresses []string
}

// Gone reports whether Karpenter must treat the instance as not existing.
func (i *Instance) Gone() bool {
	return i.State == StateTerminating || i.State == StateTerminated
}

// Config is what a driver gets at Init.
type Config struct {
	ClusterName string
	// Client and Reader reach the Kubernetes API. Real drivers do not need
	// them; the fake driver keeps its Nodes and its configuration there.
	Client client.Client
	Reader client.Reader
	Clock  clock.Clock
	Log    logr.Logger
	// KeeperURL is the Keeper API base URL, for drivers that impersonate a
	// vendor's catalog (fake).
	KeeperURL string
}

// Driver is implemented by every vendor package and registered by name.
type Driver interface {
	// Vendor is the Keeper vendor ID ("aws").
	Vendor() string
	Init(ctx context.Context, cfg Config) error
	// Regions returns the regions the credentials can use.
	Regions(ctx context.Context) ([]Region, error)
	// Zones returns the account's zones of a region, with Keeper zone IDs.
	Zones(ctx context.Context, region string) ([]Zone, error)
	// ResolveImage resolves the official image.
	ResolveImage(ctx context.Context, req ImageRequest) (*Image, error)
	// CreateInstance launches one instance. Idempotency key rules, which the
	// attempt loop of the CloudProvider relies on: replaying a key returns the
	// original instance, whatever zone, type or capacity type the replay
	// asks for (callers build the NodeClaim from the returned instance); and
	// a create that failed leaves the key unused, so the next attempt with
	// another offering launches. Errors are *Error values (see ClassOf).
	CreateInstance(ctx context.Context, req CreateRequest) (*Instance, error)
	// GetInstance returns the instance, also when terminated; NotFound when
	// the vendor does not know it, and for a reference the driver cannot parse.
	GetInstance(ctx context.Context, ref InstanceRef) (*Instance, error)
	// FindInstance looks an instance up by tags (the fallback when the
	// reference is unknown); NotFound when there is none.
	FindInstance(ctx context.Context, region string, tags map[TagKey]string) (*Instance, error)
	// ListInstances returns the instances in the regions that carry all the
	// tags, terminated ones excluded; terminating ones are returned (the
	// garbage collector deletes a stopped instance from this list).
	ListInstances(ctx context.Context, regions []string, tags map[TagKey]string) ([]*Instance, error)
	// DeleteInstance terminates the instance, a terminating (stopped) one
	// included; idempotent: deleting an instance that is gone is not an error.
	// A reference the driver cannot parse is an instance that is gone.
	DeleteInstance(ctx context.Context, ref InstanceRef) error
	// TagKey maps a logical key to the vendor's charset.
	TagKey(k TagKey) string
}

// NodeClassValidator is the optional capability to validate a NodeClass's
// blocks for the vendor.
type NodeClassValidator interface {
	ValidateNodeClass(ctx context.Context, nc *v1alpha1.SCNodeClass) error
}

// NodeClassWarner is the optional capability to report settings of the NodeClass
// that are valid but have no effect for the vendor (for example an isolation a
// brought resource overrides). It looks at the NodeClass only (no vendor call)
// and is asked on every reconcile of a vendor that is ready; a stable text per
// setting keeps the status quiet. The NodeClass controller shows the warnings in
// the informational vendor condition ConfigurationWarning (True while there is
// one, absent otherwise) and publishes one Warning event when they appear or
// change.
type NodeClassWarner interface {
	NodeClassWarnings(nc *v1alpha1.SCNodeClass) []string
}

// ImageReporter is the optional capability to report the boot images of the
// NodeClass's Flatcar release per region and architecture, for the NodeClass
// controller (status.vendors.<vendor>.images and the condition ImagesReady). It
// answers for the regions the NodeClass's vendor block allows. A driver without
// it is treated as having its images ready: it resolves them at launch with
// ResolveImage.
type ImageReporter interface {
	ReportImages(ctx context.Context, nc *v1alpha1.SCNodeClass) ([]v1alpha1.ImageStatus, error)
}

// Profile kinds, the keys of Profiles and of the maps the NodeOptions hooks
// return (00a section 6): one label each.
const (
	ProfileStorage   = "storage"
	ProfileNetwork   = "network"
	ProfilePlacement = "placement"
)

// DefaultProfile is the name that stands for "no profile": the vendor defaults
// and the NodeClass's vendor block.
const DefaultProfile = v1alpha1.DefaultProfile

// VendorSettings are the resolved vendor settings of a node (00a section 3):
// the driver's built-in defaults, overridden by the NodeClass's vendor block,
// overridden by the profiles the workload selected. The merge is generic (JSON
// merge: objects merge key by key, lists and scalars replace); the driver
// decodes the result into its own types.
type VendorSettings struct {
	// RootDiskBytes is the size of the root disk, which the scheduler assumes
	// as ephemeral storage; 0 when unknown. It is what the driver's
	// RootDiskBytes says about the resolved settings.
	RootDiskBytes int64
	// Raw is the merged vendor block (regions and resources removed): its
	// storage member already holds the selected storage profile.
	Raw json.RawMessage
	// Network and Placement are the blocks of the selected network and
	// placement profiles; nil with the default.
	Network   json.RawMessage
	Placement json.RawMessage
	// Networking is the generic networking: spec.networking with the vendor
	// block's override applied.
	Networking common.Networking
	// Profiles are the applied profile names by kind (storage, network,
	// placement): the selected name or "default".
	Profiles map[string]string
}

// CatalogType identifies an instance type for the profile and price hooks.
type CatalogType struct {
	Vendor       string
	APIReference string
	Family       string
	Arch         string
	VCPUs        int
	MemoryMiB    float64
	// LocalStorageGB and LocalStorageCount describe the bundled local disks
	// (0: none or unknown).
	LocalStorageGB    float64
	LocalStorageCount int
}

// Profiles are the profiles a NodeClass defines for the vendor: kind
// (storage, network, placement) to profile name to the profile's block for
// this vendor (JSON of the vendor package's type). A profile that has no block
// for the vendor is not in it: it does not exist on the vendor (ADR-017).
type Profiles map[string]map[string]json.RawMessage

// Offering identifies an offering for the price hook.
type Offering struct {
	Region       string
	Zone         string
	CapacityType string
	APIReference string
	Type         CatalogType
}

// ResourceProvider gives a driver's Prepare the vendor resources the instance
// needs, as the NodeClass configures them (00a section 9): brought by the user
// (use), the vendor's own default, or managed by SCarpenter (find or create
// with ownership marker, serialised and tracked for garbage collection). The
// driver says what it needs; the core decides where it comes from.
type ResourceProvider interface {
	// Require returns the resources of the kind for the launch. A resource
	// that is missing, or that the NodeClass forbids to create and the vendor
	// has no default for, is an *Error of class Unsupported with code
	// ResourceMissing or ResourceForbidden.
	Require(ctx context.Context, req ResourceRequest) ([]ResourceRef, error)
}

// Codes of the Unsupported errors of a ResourceProvider.
const (
	CodeResourceMissing   = "ResourceMissing"
	CodeResourceForbidden = "ResourceForbidden"
	// CodeResourceMissingInZone: a zone pattern of a `use` reference matches
	// nothing in the launch's zone; the other zones of the region are fine.
	CodeResourceMissingInZone = "ResourceMissingInZone"
)

// ResourceRequest asks a ResourceProvider for a resource kind.
type ResourceRequest struct {
	Kind ResourceKind
	// Zone is set for zonal kinds.
	Zone string
	// Key discriminates managed resources of a per-run or per-node kind (the
	// profile and run key of a placement group, the node and volume name of a
	// volume); the engine derives the deterministic name from it. Empty for
	// shared regional kinds.
	Key string
	// Params are the kind's create parameters, passed to Ensure as they are.
	Params json.RawMessage
	// Use overrides the NodeClass's configuration of the kind: the resource is
	// referenced, never created and never deleted (a placement profile that
	// names an existing group).
	Use *UseRef
	// Network is the network the launch is placed in (the ID the driver
	// chose for the kind's InNetwork provider, such as the VPC of the chosen
	// subnet): the kind is resolved only within it. Empty without one.
	Network string
	// Settings are the resolved settings of the launch; the vendor's default of
	// the kind is used only if it honours them (DefaultChecker). nil means the
	// NodeClass's default settings.
	Settings *VendorSettings
}

// PrepareRequest asks a driver to make the resources an instance needs first
// (vendor resources with create, placement groups, separate volumes).
type PrepareRequest struct {
	Region       string
	Zone         string
	InstanceType string
	CapacityType string
	// Type describes the instance type (its local disks, for example).
	Type     CatalogType
	Settings VendorSettings
	Tags     map[TagKey]string
	// RunKey is the placement run key, if any.
	RunKey string
	// Resources provides the vendor resources.
	Resources ResourceProvider
}

// DeviceHint says how the guest finds an extra volume and what to put on it
// (00a section 11). Exactly one of ByID and SerialPattern identifies it.
type DeviceHint struct {
	// Name is the volume's name in the profile.
	Name       string
	Filesystem string
	MountPath  string
	// ByID is the stable device path (/dev/disk/by-id/...) when the vendor
	// knows it before the instance exists.
	ByID string
	// SerialPattern is a shell pattern for the device serial, with SizeBytes,
	// when the ID exists only after the create.
	SerialPattern string
	SizeBytes     int64
}

// LocalHint configures the instance-local disks (00a section 6.1).
type LocalHint struct {
	// Policy is mount or raid0 (ignore needs no hint).
	Policy    string
	MountPath string
	// DevicePattern is a shell pattern over /dev/disk/by-id matching the local
	// disks, Count their number.
	DevicePattern string
	Count         int
}

// Prepared is what Prepare resolved or ensured: resource references and the
// device hints for the user-data.
type Prepared struct {
	Resources []ResourceRef
	Devices   []DeviceHint
	Local     *LocalHint
}

// NodeOptions is the optional capability for vendor-native node settings
// (00a section 5.3).
type NodeOptions interface {
	// Defaults are the built-in settings (layer 1): Raw is in the shape of the
	// vendor block.
	Defaults() VendorSettings
	// RootDiskBytes is the root disk of the resolved settings.
	RootDiskBytes(s VendorSettings) int64
	// RootDiskForType is the root disk of an instance type whose root disk
	// the type defines (hcloud, Vultr and UpCloud: the plan's disk; OVH
	// flavors with a local root disk), whatever the settings say. ok is false
	// when the type does not define it; the catalog then uses RootDiskBytes
	// of the settings, and for 0 the catalog's own storage size of the type.
	RootDiskForType(s VendorSettings, it CatalogType) (bytes int64, ok bool)
	// Supported returns the valid profile names per kind for the type in the
	// zone ("default" is always valid).
	Supported(it CatalogType, zone string, p Profiles) map[string][]string
	// PriceDelta is the price of the settings on top of the instance price, in
	// USD per hour, and whether it is complete.
	PriceDelta(s VendorSettings, o Offering) (usdPerHour float64, complete bool)
	// Prepare makes pre-instance resources ready.
	Prepare(ctx context.Context, req PrepareRequest) (*Prepared, error)
}

// NetworkAwareOptions is the optional capability of a driver whose supported
// profiles depend on the NodeClass's resolved networking (the isolation): the
// catalog asks SupportedFor, with the networking of the NodeClass for the
// vendor (the vendor block's override and the API defaults applied, so the
// isolation is never empty), instead of Supported. SupportedFor only narrows:
// it returns a subset of what Supported returns for the same arguments. A
// profile the networking cannot satisfy is not offered, so that Karpenter
// never picks it; the driver's ValidateNodeClass says why in the NodeClass's
// status. The catalog caches by the NodeClass's content, networking included.
// A driver without it is asked Supported, as before.
type NetworkAwareOptions interface {
	SupportedFor(it CatalogType, zone string, net common.Networking, p Profiles) map[string][]string
}

// EpochReporter is the optional capability of a driver whose answers to
// Supported and PriceDelta can change at runtime (the fake driver's steering):
// the epoch changes whenever they may have, so the catalog rebuilds.
type EpochReporter interface {
	NodeOptionsEpoch() uint64
}

// ResourceKind is a kind of vendor resource (subnets, securityGroups, ...);
// kinds are the keys of spec.vendors.<vendor>.resources.
type ResourceKind string

// ResourceScope says how widely a managed resource is shared.
type ResourceScope string

// The scopes of a resource kind.
const (
	// ScopeRegion: one per region (or zone), shared by every node: networks,
	// subnets, firewalls.
	ScopeRegion ResourceScope = "region"
	// ScopeRun: one per placement run key: placement groups.
	ScopeRun ResourceScope = "run"
	// ScopeNode: one per instance: separate volumes.
	ScopeNode ResourceScope = "node"
)

// KindSpec describes a resource kind of a driver.
type KindSpec struct {
	Kind  ResourceKind
	Scope ResourceScope
	// DefaultPolicy is the lifecycle of a managed resource of the kind when
	// the NodeClass says nothing (00a section 9.3).
	DefaultPolicy common.LifecyclePolicy
	// DeleteOrder orders deletions within a region: lower is deleted first
	// (instances, volumes, placement groups, security groups, subnets,
	// networks, resource groups).
	DeleteOrder int
	// InNetwork names the kind that provides the network this kind lives in
	// (AWS: security groups live in the network of the subnets). The kind is
	// resolved after it and only within the networks it resolved to, whichever
	// step of the resolution order found them (an implicit match.network, set
	// where the kind's selector supports it). Empty: the kind is not
	// network-scoped.
	InNetwork ResourceKind
	// Optional marks a kind a launch may go without and that SCarpenter never
	// creates (a private network the operator may bring): when no `use`, no
	// opt-in tag and no vendor default provides it, Require returns no
	// resource and no error, and the status check reports no problem. A
	// `create` of such a kind is the driver's validation to refuse.
	Optional bool
}

// UseRef is the selector of the resources to use (ADR-023, A10) for one region
// (and zone): the NodeClass's `use` with the region's IDs gathered ("*" and the
// region key) and the placeholders expanded. The fields are ANDed. A driver
// asks the vendor for what it can filter and finishes with
// selector.Apply; it never matches on its own. Nothing given means the vendor's
// own default resource of the kind, where it has one.
type UseRef struct {
	// IDs are the vendor IDs for the region.
	IDs []string
	// Name is a glob on the kind's name field.
	Name string
	// Tags are the tags (labels) that must be present; "*" means the key
	// exists.
	Tags map[string]string
	// Match holds the portable attributes.
	Match common.ResourceMatch
	// Filters are the vendor's own filters, passed through.
	Filters []common.ResourceFilter
	// Expression is a query in the vendor's own language.
	Expression string
	// Select and OrderBy say how many of the matches are used; the core fills
	// Select with the kind's default when the NodeClass leaves it out.
	Select  common.Select
	OrderBy string
}

// IsDefault reports whether the reference asks for the vendor's default:
// nothing that selects was given (Select and OrderBy only choose among
// matches).
func (u UseRef) IsDefault() bool {
	return len(u.IDs) == 0 && u.Name == "" && len(u.Tags) == 0 && len(u.Filters) == 0 &&
		u.Expression == "" && reflect.DeepEqual(u.Match, common.ResourceMatch{})
}

// TagResource is the logical tag that records the kind of a managed resource;
// with cluster, nodeclass and managed-by it is the ownership marker.
const TagResource TagKey = "resource-kind"

// ResourceTagKeys are the logical tags of a managed resource.
var ResourceTagKeys = []TagKey{TagCluster, TagNodeClass, TagManagedBy, TagResource}

// CreateSpec describes a resource to find or create.
type CreateSpec struct {
	// Name is the deterministic name (scarpenter-<cluster>-<kind>-<region>[-<hash>]).
	Name string
	Zone string
	// Tags are the ownership marker; ExtraTags the NodeClass's own tags.
	Tags      map[TagKey]string
	ExtraTags map[string]string
	// Params are the kind's create parameters (a placement strategy, a size).
	Params json.RawMessage
}

// ResourceRef is a vendor resource.
type ResourceRef struct {
	Kind   ResourceKind
	Region string
	ID     string
	Name   string
	// Managed is true for resources SCarpenter created (it carries the
	// ownership marker); resources of use and the vendor's defaults are not.
	Managed bool
	// Tags are the marker tags of a managed resource, as List reads them.
	Tags map[TagKey]string
	// CreatedAt is when the resource was created, zero when unknown.
	CreatedAt time.Time
	// Network is the ID of the network the resource belongs to, where the
	// driver knows it (a subnet's VPC, a security group's VPC; for a network
	// kind that is the network itself): what the kinds that live in a network
	// are constrained to.
	Network string
}

// ManagedResources is the optional capability for vendor resources that are
// brought by the user (use) or created by SCarpenter (create), 00a section 9.
type ManagedResources interface {
	// Kinds lists the kinds the driver knows.
	Kinds() []KindSpec
	// Resolve finds the existing resources of the kind in the region that the
	// selector selects (the vendor call, then selector.Apply: fields ANDed,
	// Select applied). A selector that selects nothing, or too many for its
	// Select, returns a *selector.NoMatchError or *selector.AmbiguousError (the
	// counts after each stage). An empty UseRef selects nothing: the vendor's
	// default resources are Default's (ResourceDefaults). Resolved resources are
	// never modified.
	Resolve(ctx context.Context, kind ResourceKind, region string, ref UseRef) ([]ResourceRef, error)
	// Selectors declares, per kind that accepts `use`, what the driver's
	// Resolve supports of the selector and on which side (SelectorCaps). A
	// kind that is absent accepts no `use`.
	Selectors() map[ResourceKind]SelectorCaps
	// Ensure finds the resource by name and ownership marker, or creates it;
	// idempotent. created is false when an existing resource was adopted.
	Ensure(ctx context.Context, kind ResourceKind, region string, spec CreateSpec) (
		ref ResourceRef, created bool, err error)
	// InUse says whether an instance uses the resource, and when it was last
	// used (or created, when never): the garbage collector's input.
	InUse(ctx context.Context, ref ResourceRef) (inUse bool, lastUsed time.Time, err error)
	// Delete removes a managed resource; idempotent. A resource the vendor
	// refuses to delete (still in use by something else) is an error of class
	// Retryable.
	Delete(ctx context.Context, ref ResourceRef) error
	// List returns the resources in the regions that carry all the tags (the
	// ownership marker), for adoption and garbage collection.
	List(ctx context.Context, regions []string, tags map[TagKey]string) ([]ResourceRef, error)
}

// LeftoverCollector is the optional capability of a driver whose launch makes
// vendor resources of its own besides the instance (a network interface, a
// public address) that are neither managed resources nor removed with the
// instance when the launch fails or is interrupted before the instance exists.
// The garbage collector calls it once per region after its instance sweep (not
// in dry-run, which deletes nothing). nodeClaimExists says whether a NodeClaim
// of the name exists when it is called (a live read, true when the read fails):
// the driver lists its candidates first and asks for each after that listing,
// never from an answer cached across calls, so that the resources of a launch
// still in flight (its NodeClaim exists before the launch starts) are kept. The
// driver deletes only what carries this cluster's tag, names a NodeClaim that
// does not exist and nothing uses, puts a deadline on every vendor call, and
// returns how many it deleted (also when it returns an error). The collector
// logs and counts them (scarpenter_gc_deletions_total, kind launch-leftover).
type LeftoverCollector interface {
	CollectLeftovers(ctx context.Context, region string, nodeClaimExists func(name string) bool) (int, error)
}

// ResourceDefaults is the optional capability of a driver whose vendor has a
// default resource of a kind (step 3 of the resolution order, ADR-022): the AWS
// default VPC and its default subnets, GCP's default network. A driver without it
// has no defaults and the order goes from discovery to creation.
type ResourceDefaults interface {
	// Default returns the vendor's own default resources of the kind in the
	// region, or none (an empty result, not an error). They are never
	// modified or deleted. A kind without a default returns none.
	Default(ctx context.Context, kind ResourceKind, region string) ([]ResourceRef, error)
}

// DefaultChecker is the optional refinement of ResourceDefaults: whether the
// vendor's default can honour the requested settings (ADR-017: a default firewall
// that cannot isolate runs or restrict inbound rules, a default network without
// IPv6, publicIPv4: false). false skips the default, with the reason for the
// failure message; the order continues with creation and never degrades the
// request. An error is the vendor's (not a verdict) and stops the order.
type DefaultChecker interface {
	DefaultSuits(
		ctx context.Context, kind ResourceKind, region string, defaults []ResourceRef, s VendorSettings,
	) (ok bool, reason string, err error)
}

// ResourceDiscoverer is the optional override of step 2 of the resolution order
// (ADR-022) for a driver whose opt-in resources cannot be found with the
// selector `tags: {<opt-in key>: "true"}`. Without it the core runs exactly that
// selector, with the kind's default select, through the driver's Resolve and
// the shared matcher; a kind whose capability declaration has no tags skips the
// step. The result is treated like `use`: a selector that selects nothing returns a
// *selector.NoMatchError (the order goes on), one that is ambiguous for the
// kind's select a *selector.AmbiguousError (the order fails).
type ResourceDiscoverer interface {
	Discover(ctx context.Context, kind ResourceKind, region string) ([]ResourceRef, error)
}

// ArchNamer is the optional capability of a driver whose vendor spells the CPU
// architectures of images its own way: the value of {vendorArch} in `use`
// (AWS x86_64, GCP X86_64, Azure x64, hcloud x86). ok is false for an
// architecture the vendor has no image for.
type ArchNamer interface {
	VendorArch(arch string) (name string, ok bool)
}

// ZoneChecker is the optional capability of a driver that can tell, before any
// launch, which zones of a region cannot host a node with the resolved default
// settings of a NodeClass: AWS with publicIPv6 on a subnet without an IPv6 CIDR
// block. found holds the resources the NodeClass's configuration resolved in the
// region, by kind. The result maps zone names to the reason; the resource check
// makes those zones' offerings unavailable and reports the reason (a region where
// every zone is a gap is not ready).
type ZoneChecker interface {
	ZoneGaps(
		ctx context.Context, region string, s VendorSettings, found map[ResourceKind][]ResourceRef,
	) (map[string]string, error)
}

// QuotaScope says whether a bucket is per region or account-wide.
type QuotaScope string

// The scopes of a quota bucket.
const (
	QuotaScopeRegion QuotaScope = "region"
	QuotaScopeGlobal QuotaScope = "global"
)

// Bucket is one quota bucket (overview section 5.9).
type Bucket struct {
	// ID is the vendor's quota identifier (L-1216C47A).
	ID    string
	Scope QuotaScope
	// Unit is vCPU, instances, GPUs or GiB.
	Unit  string
	Limit int64
	// Used is the consumption of the whole account, not only SCarpenter's
	// instances.
	Used int64
}

// QuotaSnapshot is the quota state of a region (or of the account).
type QuotaSnapshot struct {
	Buckets []Bucket
	// Counted are the references of the instances included in Used, so the
	// in-flight ledger can release their reservations.
	Counted []InstanceRef
}

// Consumption is how much of a bucket one instance consumes.
type Consumption struct {
	Bucket string
	Amount int64
}

// QuotaReporter is the optional capability for vendors with quota APIs.
type QuotaReporter interface {
	// Quotas returns the region's buckets. The tracker passes WithQuotaRefresh
	// in the context: drivers may reuse limits cached from an earlier call
	// unless the refresh asks for limits.
	Quotas(ctx context.Context, region string) (*QuotaSnapshot, error)
	// GlobalQuotas returns the account-wide buckets; it may be empty.
	GlobalQuotas(ctx context.Context) (*QuotaSnapshot, error)
	// Consumption returns the buckets an instance of the type consumes; false
	// means the driver cannot map the type, whose quota is unknown (reactive).
	Consumption(instanceType, capacityType string) ([]Consumption, bool)
}

// BillingRule is a driver's own billing rule for a region and instance family,
// where it is finer than Keeper's vendor row or Keeper has none (OVH US versus
// EU, Alibaba's minimum by vCPU). Every field but Verified is optional; a set
// field overrides Keeper's value of the same name. A rule that is not Verified
// is a fact no live check has confirmed: billing retention stays off for it and
// the NodeClass status says so (ADR-017, ADR-024). A value lower than Keeper's
// row needs a code comment that links the vendor's own documentation.
type BillingRule struct {
	// Increment is the granularity compute is billed in (one hour: 3600 s).
	Increment *time.Duration
	// Minimum is the minimum billed duration of a run; zero or nil means none.
	Minimum *time.Duration
	// StoppedCharged says whether a stopped server is still charged.
	StoppedCharged *bool
	// DeleteLatency is how long a delete of this vendor takes to stop the
	// charge after DeleteInstance is called (UpCloud stops, then deletes). The
	// retention margin is at least DeleteLatency plus 90 s.
	DeleteLatency time.Duration
	// Verified says that the rule was checked against a live account.
	Verified bool
	// Source says where the rule comes from, for the NodeClass status (a
	// documentation page, "live check 2026-11-10").
	Source string
}

// BillingDescriber is the optional capability to describe the vendor's billing
// rule for a region and instance family (ADR-024). A driver without it uses
// Keeper's vendor row and is unverified: billing retention is off for it. The
// core never branches on a vendor name; everything vendor-specific is here.
type BillingDescriber interface {
	// BillingRule returns the rule for the region and family; false means the
	// driver has nothing to add to Keeper's row for them.
	BillingRule(region, family string) (BillingRule, bool)
}

// OfferingLister is the optional capability to report which instance types the
// account can launch in which zones.
type OfferingLister interface {
	// LaunchableTypes maps the API reference of each launchable type to the
	// Keeper IDs of the zones where it can be launched.
	LaunchableTypes(ctx context.Context, region string) (map[string][]string, error)
}

type quotaRefreshKey struct{}

// QuotaRefresh says what a Quotas call must refresh.
type QuotaRefresh struct {
	// Limits asks for fresh limits (every 15 minutes); without it a driver may
	// reuse cached limits and only read the usage.
	Limits bool
}

// WithQuotaRefresh annotates the context of a Quotas call.
func WithQuotaRefresh(ctx context.Context, r QuotaRefresh) context.Context {
	return context.WithValue(ctx, quotaRefreshKey{}, r)
}

// QuotaRefreshFrom returns what the caller asks for; without an annotation
// everything is refreshed.
func QuotaRefreshFrom(ctx context.Context) QuotaRefresh {
	if r, ok := ctx.Value(quotaRefreshKey{}).(QuotaRefresh); ok {
		return r
	}
	return QuotaRefresh{Limits: true}
}

// ImageImporter is reserved for the first vendor that imports images (hcloud).
type ImageImporter interface {
	ImportImage(ctx context.Context, req ImageRequest) (*Image, error)
	DeleteImage(ctx context.Context, image Image) error
	ListImages(ctx context.Context, region string) ([]Image, error)
}

// String implements fmt.Stringer for logs.
func (c CreateRequest) String() string {
	return fmt.Sprintf("%s/%s %s %s", c.Region, c.Zone, c.InstanceType, c.CapacityType)
}
