/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	selapi "github.com/SpareCores/sc-keeper-go/selection/api"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
)

// SCNodeClassSpec is the node definition shared by every vendor, plus a typed
// block per vendor (ADR-014). Which vendor a NodeClaim uses is chosen by the
// NodePool's scarpenter.sparecores.com/vendor requirement.
type SCNodeClassSpec struct {
	// flatcar selects the Flatcar Container Linux release nodes boot.
	// +required
	Flatcar FlatcarSpec `json:"flatcar"`

	// kubernetes selects the Kubernetes release (a sysext-bakery release)
	// nodes run.
	// +required
	Kubernetes KubernetesSpec `json:"kubernetes"`

	// network is the node bootstrap network template (how nodes reach the
	// control plane).
	// +required
	Network NetworkSpec `json:"network"`

	// networking is the generic network setting, identical in meaning on every
	// vendor; vendor blocks may override it.
	// +kubebuilder:default={}
	// +optional
	Networking common.Networking `json:"networking,omitempty"`

	// kubelet configures the kubelet and the node capacity the scheduler
	// assumes.
	// +kubebuilder:default={}
	// +optional
	Kubelet KubeletSpec `json:"kubelet,omitempty"`

	// butane references extra Butane configuration merged into every node's
	// Ignition.
	// +optional
	Butane *ButaneSpec `json:"butane,omitempty"`

	// bootstrap holds the opt-in changes to the node's boot configuration that
	// a vendor's driver can contribute (plan 14 A7).
	// +optional
	Bootstrap *BootstrapSpec `json:"bootstrap,omitempty"`

	// sysexts lists extra systemd sysexts installed on every node.
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	// +listMapKey=arch
	// +optional
	Sysexts []Sysext `json:"sysexts,omitempty"`

	// poweroffAfter powers a node off after this long: a fail-safe against
	// instances that outlive their NodeClaim.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(s|m|h))+$`
	// +optional
	PoweroffAfter *metav1.Duration `json:"poweroffAfter,omitempty"`

	// tags are added to every vendor resource SCarpenter creates (tags or
	// labels, as the vendor calls them).
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="self.all(k, !k.startsWith('scarpenter.sparecores.com/'))",message="tag keys in the scarpenter.sparecores.com domain are reserved"
	// +optional
	Tags map[string]string `json:"tags,omitempty"`

	// pricing configures how offerings are priced.
	// +kubebuilder:default={}
	// +optional
	Pricing PricingSpec `json:"pricing,omitempty"`

	// billing opts in to keeping nodes until the end of the billing period
	// they have already paid for (ADR-024). Changing it is not drift.
	// +optional
	Billing *BillingSpec `json:"billing,omitempty"`

	// catalog configures the instance type catalog built from Keeper.
	// +optional
	Catalog *CatalogSpec `json:"catalog,omitempty"`

	// metrics define benchmark metric labels (metric-<name>, -pct and -per-usd)
	// on instance types and offerings.
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$') && size(k) <= 40)",message="metric names are lower-case label-safe slugs of at most 40 characters"
	// +optional
	Metrics map[string]Metric `json:"metrics,omitempty"`

	// selections define selection profiles: sc-karpenter-nodepool's selector
	// and ranking, evaluated on every catalog refresh. Instance types carry the
	// label selection=<name> for each profile they belong to.
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$') && size(k) <= 63)",message="selection names are lower-case label-safe slugs"
	// +optional
	Selections map[string]Selection `json:"selections,omitempty"`

	// profiles are named, explicit, vendor-native setups that workloads pick
	// by name through labels.
	// +kubebuilder:default={}
	// +optional
	Profiles Profiles `json:"profiles,omitempty"`

	// vendors has one block per vendor; a vendor is enabled for this
	// SCNodeClass by having a block.
	// +required
	Vendors VendorBlocks `json:"vendors"`
}

// Hardening is a named, opt-in hardening of the node's boot configuration. A
// driver that offers it contributes a Butane fragment that the core merges into
// the base config.
// +kubebuilder:validation:Enum=metadata-from-pods
type Hardening string

// Hardenings.
const (
	// HardeningMetadataFromPods drops Pod traffic that is forwarded to the
	// vendor's metadata address (the user-data holds a bootstrap token and, with
	// the tailscale template, the Tailscale key), on vendors with nothing like
	// AWS's hop limit. The node's own units and host-network Pods are unaffected.
	HardeningMetadataFromPods Hardening = "metadata-from-pods"
)

// BootstrapSpec is the optional bootstrap hardening of the node.
type BootstrapSpec struct {
	// hardening lists the opt-in hardenings of the boot configuration. Off by
	// default: they change what Pods on the node can reach. A vendor whose driver
	// offers none of them (AWS) ignores the list and says so in an Event.
	// +kubebuilder:validation:MaxItems=8
	// +listType=set
	// +optional
	Hardening []Hardening `json:"hardening,omitempty"`
}

// HardeningNames returns the enabled hardenings as strings (nil when none).
func (s *SCNodeClassSpec) HardeningNames() []string {
	if s.Bootstrap == nil {
		return nil
	}
	out := make([]string, len(s.Bootstrap.Hardening))
	for i, h := range s.Bootstrap.Hardening {
		out[i] = string(h)
	}
	return out
}

// ButaneSpec references extra Butane configuration.
type ButaneSpec struct {
	// configMapRef references the ConfigMap key holding the Butane config.
	// +required
	ConfigMapRef ConfigMapKeyRef `json:"configMapRef"`
}

// FlatcarChannel is a Flatcar release channel.
// +kubebuilder:validation:Enum=stable;beta;alpha;lts
type FlatcarChannel string

// FlatcarSpec pins the Flatcar release (ADR-012).
type FlatcarSpec struct {
	// channel of the release.
	// +kubebuilder:default=stable
	// +optional
	Channel FlatcarChannel `json:"channel,omitempty"`

	// version is the pinned release, e.g. 4757.2.3. Changing it drifts nodes.
	// +kubebuilder:validation:Pattern=`^[0-9]+\.[0-9]+\.[0-9]+$`
	// +required
	Version string `json:"version"`
}

// KubernetesSpec pins the Kubernetes release.
type KubernetesSpec struct {
	// version of a sysext-bakery Kubernetes release, e.g. v1.37.1.
	// +kubebuilder:validation:Pattern=`^v[0-9]+\.[0-9]+\.[0-9]+$`
	// +required
	Version string `json:"version"`
}

// NetworkTemplateName is a node bootstrap network template.
// +kubebuilder:validation:Enum=tailscale;direct;custom
type NetworkTemplateName string

// Network templates.
const (
	NetworkTemplateTailscale NetworkTemplateName = "tailscale"
	NetworkTemplateDirect    NetworkTemplateName = "direct"
	NetworkTemplateCustom    NetworkTemplateName = "custom"
)

// NetworkSpec selects the bootstrap network template and holds its settings.
// +kubebuilder:validation:XValidation:rule="self.template == 'tailscale' ? has(self.tailscale) : !has(self.tailscale)",message="network.tailscale is required with template tailscale and invalid otherwise"
// +kubebuilder:validation:XValidation:rule="self.template == 'custom' ? has(self.custom) : !has(self.custom)",message="network.custom is required with template custom and invalid otherwise"
type NetworkSpec struct {
	// template: tailscale joins nodes through a tailnet, direct has nodes
	// reach the API server directly, custom renders your own Butane template.
	// +required
	Template NetworkTemplateName `json:"template"`

	// tailscale configures the tailscale template.
	// +optional
	Tailscale *TailscaleSpec `json:"tailscale,omitempty"`

	// custom configures the custom template.
	// +optional
	Custom *CustomNetworkSpec `json:"custom,omitempty"`
}

// TailscaleSpec configures the tailscale network template.
type TailscaleSpec struct {
	// version of the Tailscale sysext, e.g. v1.98.10.
	// +kubebuilder:validation:Pattern=`^v[0-9]+\.[0-9]+\.[0-9]+$`
	// +required
	Version string `json:"version"`

	// loginServer is the coordination server URL; empty means Tailscale SaaS,
	// set it for Headscale.
	// +kubebuilder:validation:Pattern=`^(https?://[^\s]+)?$`
	// +optional
	LoginServer string `json:"loginServer,omitempty"`

	// authKeySecretRef references the Secret key holding the auth key nodes
	// join the tailnet with.
	// +required
	AuthKeySecretRef SecretKeyRef `json:"authKeySecretRef"`

	// tags are the ACL tags nodes advertise.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:Pattern=`^tag:[a-zA-Z0-9][a-zA-Z0-9-]*$`
	// +listType=set
	// +optional
	Tags []string `json:"tags,omitempty"`
}

// CustomNetworkSpec configures the custom network template.
type CustomNetworkSpec struct {
	// configMapRef references the ConfigMap key holding the Butane template.
	// +required
	ConfigMapRef ConfigMapKeyRef `json:"configMapRef"`
}

// SecretKeyRef references a key of a Secret.
type SecretKeyRef struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +required
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +required
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +required
	Key string `json:"key"`
}

// ConfigMapKeyRef references a key of a ConfigMap.
type ConfigMapKeyRef struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +required
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +required
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +required
	Key string `json:"key"`
}

// KubeletSpec configures the kubelet and the capacity the scheduler assumes.
// +kubebuilder:validation:XValidation:rule="!has(self.systemReserved) || self.systemReserved.all(k, k in ['cpu', 'memory', 'ephemeral-storage', 'pid'])",message="systemReserved keys must be cpu, memory, ephemeral-storage or pid"
// +kubebuilder:validation:XValidation:rule="!has(self.kubeReserved) || self.kubeReserved.all(k, k in ['cpu', 'memory', 'ephemeral-storage', 'pid'])",message="kubeReserved keys must be cpu, memory, ephemeral-storage or pid"
// +kubebuilder:validation:XValidation:rule="!has(self.evictionHard) || self.evictionHard.all(k, k in ['memory.available', 'nodefs.available', 'nodefs.inodesFree', 'imagefs.available', 'imagefs.inodesFree', 'pid.available'])",message="evictionHard keys must be eviction signals"
type KubeletSpec struct {
	// maxPods is the maximum number of Pods per node.
	// +kubebuilder:default=110
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	// +optional
	MaxPods *int32 `json:"maxPods,omitempty"`

	// systemReserved resources for system daemons.
	// +kubebuilder:validation:MaxProperties=4
	// +optional
	SystemReserved map[string]string `json:"systemReserved,omitempty"`

	// kubeReserved overrides the computed resources reserved for Kubernetes
	// daemons.
	// +kubebuilder:validation:MaxProperties=4
	// +optional
	KubeReserved map[string]string `json:"kubeReserved,omitempty"`

	// evictionHard thresholds, e.g. memory.available: 100Mi.
	// +kubebuilder:validation:MaxProperties=6
	// +optional
	EvictionHard map[string]string `json:"evictionHard,omitempty"`

	// vmMemoryOverheadPercent is the share of the catalog memory a VM does
	// not offer to the guest, subtracted from the instance type's capacity.
	// +kubebuilder:default=7.5
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=50
	// +optional
	VMMemoryOverheadPercent *float64 `json:"vmMemoryOverheadPercent,omitempty"`
}

// Sysext is an extra systemd sysext installed on every node.
type Sysext struct {
	// name of the sysext.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9_.]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	// +required
	Name string `json:"name"`

	// arch the image is for.
	// +kubebuilder:validation:Enum=amd64;arm64
	// +required
	Arch string `json:"arch"`

	// url the image is downloaded from.
	// +kubebuilder:validation:Pattern=`^https://[^\s]+$`
	// +kubebuilder:validation:MaxLength=2048
	// +required
	URL string `json:"url"`

	// sha256 of the image, lower-case hex.
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	// +required
	SHA256 string `json:"sha256"`
}

// PricingSpec configures how offerings are priced (00a section 10).
type PricingSpec struct {
	// allowIncomplete allows offerings whose price misses components the data
	// does not have (provisioned IOPS, dedicated tenancy, paid bandwidth).
	// Such nodes are annotated scarpenter.sparecores.com/price-incomplete.
	// +kubebuilder:default=true
	// +optional
	AllowIncomplete *bool `json:"allowIncomplete,omitempty"`
}

// CatalogSpec configures the catalog.
type CatalogSpec struct {
	// cpuFlagLabels are the CPU flags that get a cpu-flag-<flag> label on
	// instance types. The set is closed: the labels are registered with
	// Karpenter core at start-up, so only this curated list of flags useful
	// for workload selection (names as in Keeper's cpu_flags, Linux
	// /proc/cpuinfo spelling) is accepted. Defaults to avx2, avx512f,
	// amx_tile and sha_ni.
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:Enum=avx;avx2;avx512f;avx512bw;avx512vl;avx512dq;avx512cd;avx512_vnni;avx512_bf16;avx512_fp16;avx512vbmi;avx512ifma;avx_vnni;amx_tile;amx_bf16;amx_int8;sha_ni;aes;vaes;gfni;pclmulqdq;bmi2;fma;sse4_2;asimd;sve;sve2;sha2;sha3
	// +listType=set
	// +optional
	CPUFlagLabels []string `json:"cpuFlagLabels,omitempty"`
}

// Metric defines a benchmark metric: a benchmark or a workload profile,
// optionally normalised per vCPU. Exactly one of benchmark and workloadProfile
// is set.
// +kubebuilder:validation:XValidation:rule="has(self.benchmark) != has(self.workloadProfile)",message="set exactly one of benchmark and workloadProfile"
type Metric struct {
	// benchmark identifies a Spare Cores benchmark (id and config).
	// +optional
	Benchmark *selapi.BenchmarkRef `json:"benchmark,omitempty"`

	// workloadProfile is one of the Spare Cores compound workload benchmarks.
	// +optional
	WorkloadProfile selapi.WorkloadProfileName `json:"workloadProfile,omitempty"`

	// normalize is applied to the score before the labels are computed.
	// +kubebuilder:default=None
	// +optional
	Normalize selapi.RankingNormalize `json:"normalize,omitempty"`

	// scale multiplies the score before it is rounded to the integer label
	// value, to keep precision (e.g. 100).
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000000
	// +optional
	Scale *int32 `json:"scale,omitempty"`
}

// Selection is a selection profile: sc-karpenter-nodepool's selector and
// ranking, verbatim. At least one of them is set.
// +kubebuilder:validation:XValidation:rule="has(self.selector) || has(self.ranking)",message="set selector, ranking or both"
type Selection struct {
	// selector filters instance types by Spare Cores data.
	// +optional
	Selector *selapi.Selector `json:"selector,omitempty"`

	// ranking keeps the best families of the candidates.
	// +optional
	Ranking *selapi.Ranking `json:"ranking,omitempty"`
}

// Profiles are the named vendor-native setups workloads pick by name
// (storage-profile, network-profile and placement-profile labels). The name
// "default" is reserved for the vendor defaults.
type Profiles struct {
	// storage profiles: root and extra volumes, local storage.
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$') && size(k) <= 63)",message="profile names are lower-case label-safe slugs"
	// +kubebuilder:validation:XValidation:rule="!('default' in self)",message="the profile name 'default' is reserved for the vendor defaults"
	// +optional
	Storage map[string]StorageProfile `json:"storage,omitempty"`

	// network profiles: NIC features.
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$') && size(k) <= 63)",message="profile names are lower-case label-safe slugs"
	// +kubebuilder:validation:XValidation:rule="!('default' in self)",message="the profile name 'default' is reserved for the vendor defaults"
	// +optional
	Network map[string]NetworkProfile `json:"network,omitempty"`

	// placement profiles: placement constructs and tenancy.
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$') && size(k) <= 63)",message="profile names are lower-case label-safe slugs"
	// +kubebuilder:validation:XValidation:rule="!('default' in self)",message="the profile name 'default' is reserved for the vendor defaults"
	// +optional
	Placement map[string]PlacementProfile `json:"placement,omitempty"`
}
