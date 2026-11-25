/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package v1alpha1

import (
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// Labels (overview section 5.5, 00a sections 6 and 7). They are registered as
// well-known with Karpenter core, so NodePools may use them and the scheduler
// treats them as known. Every selectable key is defined on every instance
// type, with DoesNotExist where unknown (00a section 2).
const (
	// LabelVendor is the vendor of an instance type or node, for example aws.
	LabelVendor = Group + "/vendor"
	// LabelAPIReference is the vendor's name of the instance type, for example m7i.large.
	LabelAPIReference = Group + "/api-reference"
	// LabelFamily is Keeper's family, for example m7i.
	LabelFamily = Group + "/family"
	// LabelZoneID is Keeper's zone ID, portable across accounts (ADR-009).
	LabelZoneID = Group + "/zone-id"

	// LabelCPUManufacturer, LabelCPUFamily and LabelCPUAllocation describe the CPU.
	LabelCPUManufacturer = Group + "/cpu-manufacturer"
	LabelCPUFamily       = Group + "/cpu-family"
	LabelCPUAllocation   = Group + "/cpu-allocation"

	// LabelAcceleratorCount and the following describe accelerators;
	// DoesNotExist on types without.
	LabelAcceleratorCount        = Group + "/accelerator-count"
	LabelAcceleratorMemoryMiB    = Group + "/accelerator-memory-mib"
	LabelAcceleratorManufacturer = Group + "/accelerator-manufacturer"
	LabelAcceleratorModel        = Group + "/accelerator-model"
	LabelAcceleratorType         = Group + "/accelerator-type"

	// LabelNVIDIADriverBranch, LabelNVIDIADriverVersion and LabelNVIDIADriverFlavor
	// say which driver an NVIDIA GPU type boots with (plan 12): the branch
	// (570), the exact version (570.211.01) and the kernel modules (open or
	// proprietary). DoesNotExist on every other type.
	LabelNVIDIADriverBranch  = Group + "/nvidia-driver-branch"
	LabelNVIDIADriverVersion = Group + "/nvidia-driver-version"
	LabelNVIDIADriverFlavor  = Group + "/nvidia-driver-flavor"

	// Attribute labels with integer or boolean values, for range selection (Gt, Lt).
	LabelVCPUs               = Group + "/vcpus"
	LabelCPUCores            = Group + "/cpu-cores"
	LabelMemoryMiB           = Group + "/memory-mib"
	LabelMemoryPerVCPUMiB    = Group + "/memory-per-vcpu-mib"
	LabelCPUClockMHz         = Group + "/cpu-clock-mhz"
	LabelCPUL2PerCoreKiB     = Group + "/cpu-l2-per-core-kib"
	LabelCPUL3MiB            = Group + "/cpu-l3-mib"
	LabelHyperthreading      = Group + "/hyperthreading"
	LabelNetworkBaselineMbps = Group + "/network-baseline-mbps"
	LabelNetworkPeakMbps     = Group + "/network-peak-mbps"
	LabelLocalStorageGB      = Group + "/local-storage-gb"
	LabelLocalStorageCount   = Group + "/local-storage-count"
	LabelCPUFlagPrefix       = Group + "/cpu-flag-" // + flag, for the flags of catalog.cpuFlagLabels
	LabelMetricPrefix        = Group + "/metric-"   // + metric name, see MetricLabel
	LabelSelection           = Group + "/selection"
	LabelStorageProfile      = Group + "/storage-profile"
	LabelNetworkProfile      = Group + "/network-profile"
	LabelPlacementProfile    = Group + "/placement-profile"
	LabelPlacementGroup      = Group + "/placement-group" // run key: free-form, undefined on types (00a section 6.3)
	LabelCountry             = Group + "/country"
	LabelContinent           = Group + "/continent"
	LabelCity                = Group + "/city"
	LabelState               = Group + "/state"
	LabelGreenEnergy         = Group + "/green-energy"
	DefaultProfile           = "default" // value of the profile labels when no profile applies
)

const (
	metricPercentileSuffix = "-pct"
	metricPerUSDSuffix     = "-per-usd"
)

// AnnotationPublishOffered on an SCNodeClass set to "true" makes the controller
// publish the full list of offered instance types of the class as one ConfigMap
// per vendor in its namespace (plan 28). It is not part of the spec and not of
// the drift hash.
const AnnotationPublishOffered = Group + "/publish-offered"

// Annotations SCarpenter puts on NodeClaims (overview section 4.1).
const (
	// AnnotationInstanceRef is the driver-opaque instance reference, for
	// example us-east-1/i-0abc.
	AnnotationInstanceRef = Group + "/instance-ref"
	// AnnotationNodeClassHash is the vendor's drift hash at launch (section 6.3).
	AnnotationNodeClassHash = Group + "/nodeclass-hash"
	// AnnotationNodeClassHashVersion is the version of the hash computation.
	AnnotationNodeClassHashVersion = Group + "/nodeclass-hash-version"
	// AnnotationPrice is the hourly price at launch in USD.
	AnnotationPrice = Group + "/price"
	// AnnotationProfileHashes records the hashes of the profiles a node was
	// launched with, as JSON {"storage/<name>": "<hash>"}: changing one
	// profile drifts only the nodes that use it.
	AnnotationProfileHashes = Group + "/profile-hashes"
	// AnnotationResources records the vendor resources the node was launched
	// with (the IDs its `use` selectors resolved to, and the managed ones), as
	// JSON {"subnets": ["subnet-0abc"]}: a later re-resolution that finds other
	// resources changes new launches only, never a running node.
	AnnotationResources = Group + "/resources"
	// AnnotationPriceIncomplete is "true" when the price misses components the
	// data does not have.
	AnnotationPriceIncomplete = Group + "/price-incomplete"
	// AnnotationPublicIPv4 and AnnotationPublicIPv6 are the instance's public
	// addresses, kept by the garbage collection controller on the Node and its
	// NodeClaim once the Node exists. Absent while the instance has no address
	// of the family; never empty.
	AnnotationPublicIPv4 = Group + "/public-ipv4"
	AnnotationPublicIPv6 = Group + "/public-ipv6"
)

// Interruption annotations (ADR-027). The node agent or the interruption poller
// writes them on the Node; the interruption controller copies them to the
// NodeClaim.
const (
	// AnnotationInterruption is the reason of an interruption notice (a
	// Reason* value below).
	AnnotationInterruption = Group + "/interruption"
	// AnnotationInterruptionDeadline is when the vendor takes the instance away,
	// RFC 3339.
	AnnotationInterruptionDeadline = Group + "/interruption-deadline"
	// AnnotationInterruptionSource says who found the notice (a Source* value
	// below).
	AnnotationInterruptionSource = Group + "/interruption-source"
)

// The reasons of an interruption, on the Interrupted condition of the NodeClaim
// (as reason and as the DisruptionReason condition's reason) and in the
// annotation of the Node.
const (
	// ConditionInterrupted is the NodeClaim condition that records an
	// interruption. It is SCarpenter's own: core clears DisruptionReason on
	// nodes it does not consider disrupting.
	ConditionInterrupted = "Interrupted"
	// ReasonSpotInterrupted: the vendor announced the reclaim of a spot or
	// preemptible instance.
	ReasonSpotInterrupted = "SpotInterrupted"
	// ReasonReclaimedWithoutNotice: a spot or preemptible instance was gone with
	// no notice seen and no vendor word on why (a silent reclaim, or a crash the
	// vendor does not tell apart).
	ReasonReclaimedWithoutNotice = "ReclaimedWithoutNotice"
	// ReasonInstanceVanished: an on-demand instance was gone without SCarpenter
	// deleting it. It is an Event reason only; the Interrupted condition is for
	// spot.
	ReasonInstanceVanished = "InstanceVanished"
)

// The sources of an interruption notice.
const (
	// SourceInstanceMetadata: the node agent read the vendor's in-VM notice.
	SourceInstanceMetadata = "instance-metadata"
	// SourceVendorAPI: the controller found the notice through the vendor's API.
	SourceVendorAPI = "vendor-api"
	// SourceGC: no notice was seen; the instance was found gone.
	SourceGC = "gc"
)

// MetricLabel is the label of a metric's score on an instance type.
func MetricLabel(metric string) string { return LabelMetricPrefix + metric }

// MetricPercentileLabel is the label of a metric's percentile (0 to 100,
// 100 best) among the types of the NodeClass's vendors.
func MetricPercentileLabel(metric string) string {
	return LabelMetricPrefix + metric + metricPercentileSuffix
}

// MetricPerUSDLabel is the label of a metric's score per USD/hour on an offering.
func MetricPerUSDLabel(metric string) string {
	return LabelMetricPrefix + metric + metricPerUSDSuffix
}

// CPUFlagLabelValues are the CPU flags that can get a cpu-flag-<flag> label: the
// closed enum of spec.catalog.cpuFlagLabels (a test keeps the two in step).
// All of them are registered with core at start-up.
var CPUFlagLabelValues = []string{
	"avx", "avx2", "avx512f", "avx512bw", "avx512vl", "avx512dq", "avx512cd",
	"avx512_vnni", "avx512_bf16", "avx512_fp16", "avx512vbmi", "avx512ifma", "avx_vnni",
	"amx_tile", "amx_bf16", "amx_int8", "sha_ni", "aes", "vaes", "gfni", "pclmulqdq",
	"bmi2", "fma", "sse4_2", "asimd", "sve", "sve2", "sha2", "sha3",
}

// DefaultCPUFlagLabels are the flags that get a label when a SCNodeClass does
// not set spec.catalog.cpuFlagLabels.
var DefaultCPUFlagLabels = []string{"avx2", "avx512f", "amx_tile", "sha_ni"}

// CPUFlagLabel is the label of a CPU flag.
func CPUFlagLabel(flag string) string { return LabelCPUFlagPrefix + flag }

// WellKnownLabels are the statically known labels registered with core in
// init(). The per-NodeClass keys (metric-<m>, cpu-flag-<f>) are not in this
// list, but core rejects unregistered keys of a restricted domain in NodePool
// requirements and in Pod requirements alike, so they must be registered too:
// plan 03 registers the metric keys of the SCNodeClasses that exist at
// start-up, before core's controllers are built, and the cpu-flag keys from the
// closed set of the CRD enum (spec.catalog.cpuFlagLabels). A metric added later
// surfaces as a RestartRequired condition. karpv1.WellKnownLabels must never be
// mutated after WithCoreControllers: core builds label sets from it when its
// controllers are constructed, and the set is not synchronised.
var WellKnownLabels = []string{
	LabelVendor, LabelAPIReference, LabelFamily, LabelZoneID,
	LabelCPUManufacturer, LabelCPUFamily, LabelCPUAllocation,
	LabelAcceleratorCount, LabelAcceleratorMemoryMiB, LabelAcceleratorManufacturer,
	LabelAcceleratorModel, LabelAcceleratorType,
	LabelNVIDIADriverBranch, LabelNVIDIADriverVersion, LabelNVIDIADriverFlavor,
	LabelVCPUs, LabelCPUCores, LabelMemoryMiB, LabelMemoryPerVCPUMiB, LabelCPUClockMHz,
	LabelCPUL2PerCoreKiB, LabelCPUL3MiB, LabelHyperthreading,
	LabelNetworkBaselineMbps, LabelNetworkPeakMbps, LabelLocalStorageGB, LabelLocalStorageCount,
	LabelSelection, LabelStorageProfile, LabelNetworkProfile, LabelPlacementProfile, LabelPlacementGroup,
	LabelCountry, LabelContinent, LabelCity, LabelState, LabelGreenEnergy,
}

func init() {
	// The domain is restricted (ADR-015): NodePool templates may not set
	// arbitrary keys in it, only the well-known ones.
	karpv1.RestrictedLabelDomains = karpv1.RestrictedLabelDomains.Insert(Group)
	karpv1.WellKnownLabels = karpv1.WellKnownLabels.Insert(WellKnownLabels...)
}
