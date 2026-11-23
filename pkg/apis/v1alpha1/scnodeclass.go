/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package v1alpha1

import (
	"fmt"
	"maps"

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

	// bootstrap is deprecated: no effect; instance metadata is restricted by
	// spec.instanceMetadata; will be removed in a later API version.
	// +optional
	Bootstrap *BootstrapSpec `json:"bootstrap,omitempty"`

	// instanceMetadata controls which processes on a node can reach the
	// vendor's instance metadata service. Unset means Restricted for new nodes
	// (ADR-025); nodes launched before that keep what they have until the
	// field is set.
	// +optional
	InstanceMetadata *InstanceMetadataSpec `json:"instanceMetadata,omitempty"`

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

	// launch is the launch retry policy of the class.
	// +optional
	Launch *LaunchSpec `json:"launch,omitempty"`

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

// Hardening is a name of the deprecated spec.bootstrap.hardening.
// +kubebuilder:validation:Enum=metadata-from-pods
type Hardening string

// HardeningMetadataFromPods is the only value of the deprecated hardening list.
const HardeningMetadataFromPods Hardening = "metadata-from-pods"

// BootstrapSpec is the deprecated bootstrap hardening of the node. It stays in
// the API, and in the drift hash, so that a stored NodeClass that sets it does
// not drift (ADR-025).
type BootstrapSpec struct {
	// hardening is deprecated: no effect; instance metadata is restricted by
	// spec.instanceMetadata; will be removed in a later API version.
	// +kubebuilder:validation:MaxItems=8
	// +listType=set
	// +optional
	Hardening []Hardening `json:"hardening,omitempty"`
}

// InstanceMetadataAccess is who on a node can reach the vendor's instance
// metadata service.
// +kubebuilder:validation:Enum=Restricted;Open
type InstanceMetadataAccess string

// Instance metadata access modes.
const (
	// InstanceMetadataRestricted lets only the node components the driver lists
	// (Afterburn's units and the vendor's guest agents) reach the metadata
	// service; every other host process and every Pod, in the Pod network or the
	// host network, is dropped. The user-data it serves holds the bootstrap token
	// and, with the tailscale template, the Tailscale key.
	InstanceMetadataRestricted InstanceMetadataAccess = "Restricted"
	// InstanceMetadataOpen leaves the metadata service open to every process and
	// Pod on the node, for workloads that need it (a vendor's CSI driver or cloud
	// controller, a benchmark that reads the instance's identity).
	InstanceMetadataOpen InstanceMetadataAccess = "Open"
)

// InstanceMetadataSpec is the access to the vendor's instance metadata service.
type InstanceMetadataSpec struct {
	// access is Restricted (the default when instanceMetadata is unset) or Open.
	// +required
	Access InstanceMetadataAccess `json:"access"`
}

// RestrictsInstanceMetadata reports whether nodes of the NodeClass are launched
// with the metadata service closed to everything but the node's own components:
// the default, unless the NodeClass says Open.
func (s *SCNodeClassSpec) RestrictsInstanceMetadata() bool {
	return s.InstanceMetadata == nil || s.InstanceMetadata.Access != InstanceMetadataOpen
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

// TailnetKeySource says where the auth key of a node comes from.
// +kubebuilder:validation:Enum=Static;PerNode
type TailnetKeySource string

// Tailnet key sources.
const (
	// TailnetKeyStatic: every node joins with the key of authKeySecretRef (the default).
	TailnetKeyStatic TailnetKeySource = "Static"
	// TailnetKeyPerNode: the controller mints one single-use, ephemeral key per
	// node when it launches it, through the tailnet backend it is configured with
	// (--tailnet-backend).
	TailnetKeyPerNode TailnetKeySource = "PerNode"
)

// TailscaleSpec configures the tailscale network template.
// +kubebuilder:validation:XValidation:rule="has(self.keySource) && self.keySource == 'PerNode' ? !has(self.authKeySecretRef) : has(self.authKeySecretRef)",message="authKeySecretRef is required unless keySource is PerNode, and invalid with it"
// +kubebuilder:validation:XValidation:rule="!(has(self.keySource) && self.keySource == 'PerNode') || (has(self.tags) && size(self.tags) > 0)",message="tags must not be empty with keySource PerNode"
type TailscaleSpec struct {
	// version of the Tailscale sysext, e.g. v1.98.10.
	// +kubebuilder:validation:Pattern=`^v[0-9]+\.[0-9]+\.[0-9]+$`
	// +required
	Version string `json:"version"`

	// loginServer is the coordination server URL; empty means Tailscale SaaS,
	// set it for Headscale. With keySource PerNode it must match the controller's
	// tailnet backend (empty for tailscale, --tailnet-api-url for headscale).
	// +kubebuilder:validation:Pattern=`^(https?://[^\s]+)?$`
	// +optional
	LoginServer string `json:"loginServer,omitempty"`

	// keySource is Static (the default when unset: every node joins with the key
	// of authKeySecretRef) or PerNode (the controller mints a single-use,
	// pre-authorised, ephemeral key per node, tagged with tags). PerNode needs the
	// controller's --tailnet-backend; switching to it drifts the nodes once.
	// +optional
	KeySource TailnetKeySource `json:"keySource,omitempty"`

	// authKeySecretRef references the Secret key holding the auth key nodes
	// join the tailnet with. Required with keySource Static, invalid with PerNode.
	// +optional
	AuthKeySecretRef *SecretKeyRef `json:"authKeySecretRef,omitempty"`

	// tags are the ACL tags nodes advertise (and, with keySource PerNode, the tags
	// of their keys; required then).
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:Pattern=`^tag:[a-zA-Z0-9][a-zA-Z0-9-]*$`
	// +listType=set
	// +optional
	Tags []string `json:"tags,omitempty"`

	// acceptRoutes makes nodes accept the subnet routes other tailnet members
	// advertise (tailscale up --accept-routes), so workloads can reach
	// addresses behind a subnet router, e.g. a control plane's private
	// addresses. The flag runs on every boot. Changing it drifts nodes.
	// +kubebuilder:default=false
	// +optional
	AcceptRoutes bool `json:"acceptRoutes,omitempty"`
}

// PerNode reports whether the nodes get an auth key each (keySource PerNode).
func (t *TailscaleSpec) PerNode() bool {
	return t != nil && t.KeySource == TailnetKeyPerNode
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
	// maxPods is the maximum number of Pods per node. Unset means 110, or 10
	// with footprint Lean.
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

	// footprint selects how much of the node's memory the node itself uses.
	// Standard (the default when unset) keeps the kubelet and the OS as they
	// are. Lean is for single-purpose nodes, such as benchmark runners, and
	// makes 1 GiB instance types usable: it presets maxPods, kubeReserved,
	// systemReserved, evictionHard and some more kubelet settings, and
	// limits the memory of the node's daemons (see the node settings
	// reference). Fields set in this block always override the preset.
	// +kubebuilder:validation:Enum=Standard;Lean
	// +optional
	Footprint Footprint `json:"footprint,omitempty"`
}

// Footprint is the memory footprint preset of a node.
type Footprint string

// Footprints.
const (
	// FootprintStandard leaves the kubelet and the OS at their defaults.
	FootprintStandard Footprint = "Standard"
	// FootprintLean is the preset for single-purpose nodes with little memory.
	FootprintLean Footprint = "Lean"
)

// LeanMaxPods is maxPods with the Lean footprint.
const LeanMaxPods = 10

// The memory the Lean footprint reserves for a node of 768 MiB to 2 GiB (a
// 1 GiB or 2 GiB instance type), in MiB; LeanMemory scales it with the node's
// memory. The measured allocatable is 581 MiB on a 951 MiB node and 1587 MiB on
// a 1957 MiB one.
const (
	// LeanKubeReservedMiB is for the kubelet and containerd, with their memory
	// limits of the Lean bootstrap settings.
	LeanKubeReservedMiB = 192
	// LeanSystemReservedMiB is for systemd, journald, resolved, networkd, udevd
	// and tailscaled.
	LeanSystemReservedMiB = 128
	// LeanEvictionMiB is the memory.available eviction threshold.
	LeanEvictionMiB = 50
)

// Where LeanMemory changes its reservations, in MiB of node memory.
const (
	// leanSmallBelowMiB: a node below it (an instance type below 1 GiB) is
	// small. Between a 0.5 GiB node (448 MiB) and a 1 GiB one (951 MiB).
	leanSmallBelowMiB = 768
	// leanGrowAboveMiB: above it the system reservation grows by 1/16 of the
	// node's memory beyond it, up to leanGrowMaxMiB.
	leanGrowAboveMiB = 2048
	leanGrowMaxMiB   = 256
)

// LeanMemory returns the Lean footprint's memory reservations for a node with
// nodeMiB of memory (the catalog's estimate of the node's memory, the instance
// type's less the VM overhead; 0 or less means unknown and gives the 1-2 GiB
// values), in MiB: kubeReserved, systemReserved
// and the memory.available eviction threshold. The catalog and the kubelet
// configuration both call it (KubeletSpec.EffectiveFor), so the node's
// allocatable equals what the scheduler assumed.
//
//   - Below 768 MiB (a 0.5 GiB instance type, estimated at 448 MiB): 176 + 128 MiB
//     reserved and 16 MiB eviction. The kernel, kubelet, containerd and a
//     working page cache need about 300 MiB of a 448 MiB node, so a Pod that
//     requests nothing gets 128 MiB of allocatable and the kubepods cgroup
//     limit is 144 MiB (the eviction threshold is not subtracted from it). With
//     less reserved, a Pod near the limit thrashed the page cache, with no
//     swap, and the node went NotReady without an OOM kill or an eviction.
//   - 768 MiB to 2 GiB: 192 + 128 MiB and 50 MiB, as measured on 1 and 2 GiB.
//   - Above 2 GiB: the system reservation grows by 1/16 of the memory beyond
//     2 GiB, at most 256 MiB (reached at 6 GiB). The system, with its slab and
//     page cache, uses about 400 MiB on a 4 GiB node, more than the 320 MiB
//     the 2 GiB values reserve: 4 GiB gets 192 + 244 MiB, so a runaway Pod
//     reaches the kubepods limit before a global OOM.
func LeanMemory(nodeMiB int64) (kube, system, eviction int64) {
	switch {
	case nodeMiB > 0 && nodeMiB < leanSmallBelowMiB:
		return 176, 128, 16
	case nodeMiB > leanGrowAboveMiB:
		grow := min((nodeMiB-leanGrowAboveMiB)/16, leanGrowMaxMiB)
		return LeanKubeReservedMiB, LeanSystemReservedMiB + grow, LeanEvictionMiB
	}
	return LeanKubeReservedMiB, LeanSystemReservedMiB, LeanEvictionMiB
}

// leanEvictionHard is evictionHard with the Lean footprint besides the memory
// threshold: the kubelet's defaults (DefaultEvictionHard of
// pkg/kubelet/eviction/defaults_linux.go). They are spelled out because the
// kubelet applies its defaults only to an evictionHard that is not set at all
// (unless mergeDefaultEvictionSettings is set, which SCarpenter does not set).
var leanEvictionHard = map[string]string{
	"nodefs.available":   "10%",
	"nodefs.inodesFree":  "5%",
	"imagefs.available":  "15%",
	"imagefs.inodesFree": "5%",
}

// IsLean says whether the Lean footprint is selected.
func (k KubeletSpec) IsLean() bool { return k.Footprint == FootprintLean }

// Effective returns the kubelet settings with the footprint preset applied
// under the fields the NodeClass sets, key by key, except the memory
// reservations and eviction threshold, which depend on the node's size
// (EffectiveFor). With the Standard footprint it returns the spec unchanged.
func (k KubeletSpec) Effective() KubeletSpec {
	if !k.IsLean() {
		return k
	}
	if k.MaxPods == nil {
		maxPods := int32(LeanMaxPods)
		k.MaxPods = &maxPods
	}
	merged := maps.Clone(leanEvictionHard)
	maps.Copy(merged, k.EvictionHard)
	k.EvictionHard = merged
	return k
}

// EffectiveFor is Effective with the memory reservations and the memory.available
// threshold of LeanMemory for a node with nodeMiB of memory, under the fields
// the NodeClass sets. The kubelet configuration renders it.
func (k KubeletSpec) EffectiveFor(nodeMiB int64) KubeletSpec {
	k = k.Effective()
	if !k.IsLean() {
		return k
	}
	kube, system, eviction := LeanMemory(nodeMiB)
	k.KubeReserved = withDefaults(k.KubeReserved, "memory", fmt.Sprintf("%dMi", kube))
	k.SystemReserved = withDefaults(k.SystemReserved, "memory", fmt.Sprintf("%dMi", system))
	k.EvictionHard = withDefaults(k.EvictionHard, "memory.available", fmt.Sprintf("%dMi", eviction))
	return k
}

// withDefaults returns a copy of m that has key, set to value when m lacks it.
func withDefaults(m map[string]string, key, value string) map[string]string {
	out := maps.Clone(m)
	if out == nil {
		out = map[string]string{}
	}
	if _, ok := out[key]; !ok {
		out[key] = value
	}
	return out
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
