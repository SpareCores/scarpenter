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
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
)

// Capacity types, as Karpenter spells them.
const (
	CapacityOnDemand = "on-demand"
	CapacitySpot     = "spot"
)

// Instance states. A terminating or terminated instance counts as gone for
// Karpenter (Get returns NodeClaimNotFound).
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
)

// ManagedByValue is the value of the managed-by tag.
const ManagedByValue = "scarpenter"

// TagKeys lists the logical tag keys in a fixed order.
var TagKeys = []TagKey{TagCluster, TagNodeClaim, TagNodePool, TagNodeClass, TagManagedBy}

// DefaultTagKey maps a logical key the way AWS does:
// scarpenter.sparecores.com/<key>. Vendors with a restrictive label charset
// map to scarpenter-<key> instead.
func DefaultTagKey(k TagKey) string { return v1alpha1.Group + "/" + string(k) }

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

// ImageRequest asks for the official image of a Flatcar release.
type ImageRequest struct {
	Region  string
	Arch    string // amd64 or arm64
	Channel string
	Version string
	// Owner overrides the official image owner where the vendor has one.
	Owner string
}

// Image is a boot image the driver resolved.
type Image struct {
	// Ref is the vendor's image reference (an AMI ID).
	Ref string
	// RootDiskBytes is the size of the image's root disk, 0 when unknown.
	RootDiskBytes int64
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
	// the vendor does not know it.
	GetInstance(ctx context.Context, ref InstanceRef) (*Instance, error)
	// FindInstance looks an instance up by tags (the fallback when the
	// reference is unknown); NotFound when there is none.
	FindInstance(ctx context.Context, region string, tags map[TagKey]string) (*Instance, error)
	// ListInstances returns the instances in the regions that carry all the
	// tags, terminated ones excluded.
	ListInstances(ctx context.Context, regions []string, tags map[TagKey]string) ([]*Instance, error)
	// DeleteInstance terminates the instance; idempotent: deleting an instance
	// that is gone is not an error.
	DeleteInstance(ctx context.Context, ref InstanceRef) error
	// TagKey maps a logical key to the vendor's charset.
	TagKey(k TagKey) string
}

// NodeClassValidator is the optional capability to validate a NodeClass's
// blocks for the vendor.
type NodeClassValidator interface {
	ValidateNodeClass(ctx context.Context, nc *v1alpha1.SCNodeClass) error
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

// VendorSettings are the resolved vendor settings of a node (00a section 3:
// defaults, NodeClass vendor block, profiles). Plan 06 fills them; plan 04 only
// carries what the core itself needs.
type VendorSettings struct {
	// RootDiskBytes is the size of the root disk the scheduler assumes as
	// ephemeral storage; 0 when unknown.
	RootDiskBytes int64
	// Raw is the vendor block, decoded by the driver.
	Raw json.RawMessage
}

// CatalogType identifies an instance type for the profile hook.
type CatalogType struct {
	Vendor       string
	APIReference string
	Arch         string
	VCPUs        int
	MemoryMiB    float64
}

// Profiles are the profile names a NodeClass defines for the vendor, by kind
// (storage, network, placement).
type Profiles map[string][]string

// Offering identifies an offering for the price hook.
type Offering struct {
	Region       string
	Zone         string
	CapacityType string
	APIReference string
}

// PrepareRequest asks a driver to make the resources an instance needs first
// (vendor resources with create, placement groups, separate volumes).
type PrepareRequest struct {
	Region       string
	Zone         string
	InstanceType string
	CapacityType string
	Settings     VendorSettings
	Tags         map[TagKey]string
	// RunKey is the placement run key, if any.
	RunKey string
}

// Prepared is what Prepare resolved or ensured: resource references and device
// hints for the user-data (00a section 11).
type Prepared struct {
	Resources   []ResourceRef
	DeviceHints map[string]string
}

// NodeOptions is the optional capability for vendor-native node settings
// (00a section 5.3).
type NodeOptions interface {
	// Defaults are the built-in settings (layer 1).
	Defaults() VendorSettings
	// Supported returns the valid profile names per kind for the type in the
	// zone ("default" is always valid).
	Supported(it CatalogType, zone string, p Profiles) map[string][]string
	// PriceDelta is the price of the settings on top of the instance price, in
	// USD per hour, and whether it is complete.
	PriceDelta(s VendorSettings, o Offering) (usdPerHour float64, complete bool)
	// Prepare makes pre-instance resources ready.
	Prepare(ctx context.Context, req PrepareRequest) (*Prepared, error)
}

// ResourceKind is a kind of vendor resource (subnets, securityGroups, ...).
type ResourceKind string

// UseRef says which existing resources to use: IDs, a name pattern or tags.
type UseRef struct {
	IDs         []string
	NamePattern string
	Tags        map[string]string
}

// CreateSpec describes a resource to find or create.
type CreateSpec struct {
	Name   string
	Tags   map[string]string
	Params json.RawMessage
}

// ResourceRef is a vendor resource.
type ResourceRef struct {
	Kind    ResourceKind
	Region  string
	ID      string
	Name    string
	Managed bool
}

// ManagedResources is the optional capability for vendor resources that are
// brought by the user (use) or created by SCarpenter (create), 00a section 9.
type ManagedResources interface {
	Kinds() []ResourceKind
	Resolve(ctx context.Context, kind ResourceKind, region string, ref UseRef) ([]ResourceRef, error)
	Ensure(ctx context.Context, kind ResourceKind, region string, spec CreateSpec) (ResourceRef, error)
	InUse(ctx context.Context, ref ResourceRef) (bool, time.Time, error)
	Delete(ctx context.Context, ref ResourceRef) error
	List(ctx context.Context, tags map[TagKey]string) ([]ResourceRef, error)
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
