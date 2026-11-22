/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
	"github.com/SpareCores/scarpenter/pkg/nvidia"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws"
	"github.com/SpareCores/scarpenter/pkg/vendors/azure"
)

// virtualFacts reports the listed types as having a virtual GPU.
type virtualFacts struct {
	catalog.StaticFacts
	virtual map[string]bool
}

func (f virtualFacts) VirtualGPU(vendor, apiReference string) bool {
	return f.virtual[vendor+"."+apiReference]
}

// gpuCatalog is a catalog of the vendors over the snapshot for a class pinning
// the Flatcar version, with the gauges registered in reg and the NVIDIA GPU
// resources advertised (as once nodes boot the driver).
func gpuCatalog(t *testing.T, flatcar string, facts catalog.Facts, reg prometheus.Registerer,
	vendors ...string) (*catalog.Catalog, []*cloudprovider.InstanceType) {
	t.Helper()
	return gpuCatalogWith(t, true, flatcar, facts, reg, vendors...)
}

func gpuCatalogWith(t *testing.T, nvidiaGPUs bool, flatcar string, facts catalog.Facts, reg prometheus.Registerer,
	vendors ...string) (*catalog.Catalog, []*cloudprovider.InstanceType) {
	t.Helper()
	scope := catalog.Scope{}
	for _, v := range vendors {
		scope[v] = nil
	}
	cl := catalog.NewClass("c", scope)
	cl.FlatcarVersion = flatcar
	cl.Finalize()
	c := catalog.New(catalog.Options{Source: catalogtest.Source(t), Facts: facts, Registerer: reg, NVIDIAGPUs: nvidiaGPUs})
	c.SetClasses([]catalog.Class{cl})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	return c, all(t, c, catalog.Filter{})
}

func gpuCapacity(it *cloudprovider.InstanceType) int64 {
	q := it.Capacity[catalog.ResourceNVIDIAGPU]
	return q.Value()
}

func TestSupportedGPUTypesHaveCapacityAndDriverLabels(t *testing.T) {
	_, its := gpuCatalog(t, "4757.2.3", catalogtestFacts(t), nil, "aws")

	g5 := byName(its, "aws.g5.24xlarge")
	if g5 == nil {
		t.Fatal("aws.g5.24xlarge is missing")
	}
	if got := gpuCapacity(g5); got != 4 {
		t.Errorf("nvidia.com/gpu = %d, want 4", got)
	}
	if got := g5.Overhead.KubeReserved[catalog.ResourceNVIDIAGPU]; !got.IsZero() {
		t.Errorf("the overhead reserves GPUs: %v", got)
	}
	for key, want := range map[string]string{
		v1alpha1.LabelNVIDIADriverBranch:  "570",
		v1alpha1.LabelNVIDIADriverVersion: "570.211.01",
		v1alpha1.LabelNVIDIADriverFlavor:  "open",
		v1alpha1.LabelAcceleratorCount:    "4",
	} {
		if got := g5.Requirements.Get(key).Any(); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	// Volta is proprietary.
	p3dn := byName(its, "aws.p3dn.24xlarge")
	if p3dn == nil {
		t.Fatal("aws.p3dn.24xlarge (8 V100, no NVSwitch) is missing")
	}
	if got := p3dn.Requirements.Get(v1alpha1.LabelNVIDIADriverFlavor).Any(); got != "proprietary" {
		t.Errorf("V100 flavour = %q", got)
	}
	if gpuCapacity(p3dn) != 8 {
		t.Errorf("p3dn capacity = %d", gpuCapacity(p3dn))
	}
}

// TestWithoutNodeBootstrapGPUTypesArePlainMachines is the release guard of
// plan 12: until nodes boot the driver, a supported type is offered with no
// nvidia.com/gpu (a Pod requesting GPUs gets no node that never initializes)
// and no driver labels, while unsupported types are still dropped.
func TestWithoutNodeBootstrapGPUTypesArePlainMachines(t *testing.T) {
	reg := prometheus.NewRegistry()
	_, its := gpuCatalogWith(t, false, "4757.2.3", catalogtestFacts(t), reg, "aws")
	g5 := byName(its, "aws.g5.24xlarge")
	if g5 == nil {
		t.Fatal("aws.g5.24xlarge is missing")
	}
	if _, ok := g5.Capacity[catalog.ResourceNVIDIAGPU]; ok {
		t.Error("nvidia.com/gpu is advertised without the node bootstrap")
	}
	for _, key := range labelsNVIDIADriver {
		if g5.Requirements.Get(key).Operator() != corev1.NodeSelectorOpDoesNotExist {
			t.Errorf("%s is defined without the node bootstrap", key)
		}
	}
	if got := g5.Requirements.Get(v1alpha1.LabelAcceleratorCount).Any(); got != "4" {
		t.Errorf("accelerator-count = %q, want 4", got)
	}
	if byName(its, "aws.g6f.large") != nil {
		t.Error("a fractional type is offered")
	}
	if got := droppedGauge(t, reg)["c/aws/GPUFractional"]; got < 1 {
		t.Errorf("GPUFractional = %v", got)
	}
}

var labelsNVIDIADriver = []string{
	v1alpha1.LabelNVIDIADriverBranch, v1alpha1.LabelNVIDIADriverVersion, v1alpha1.LabelNVIDIADriverFlavor,
}

func TestNonGPUTypesHaveNoGPUResourceAndAbsentLabels(t *testing.T) {
	_, its := gpuCatalog(t, "4757.2.3", catalogtestFacts(t), nil, "aws")
	m7i := byName(its, "aws.m7i.large")
	if _, ok := m7i.Capacity[catalog.ResourceNVIDIAGPU]; ok {
		t.Error("a CPU type has nvidia.com/gpu")
	}
	for _, key := range []string{
		v1alpha1.LabelNVIDIADriverBranch, v1alpha1.LabelNVIDIADriverVersion, v1alpha1.LabelNVIDIADriverFlavor,
	} {
		if m7i.Requirements.Get(key).Operator() != corev1.NodeSelectorOpDoesNotExist {
			t.Errorf("%s is defined on a CPU type", key)
		}
	}
	// The key is defined on every type, so a Pod without it is not matched by accident.
	for _, it := range its {
		if !it.Requirements.Has(v1alpha1.LabelNVIDIADriverBranch) {
			t.Fatalf("%s lacks the driver branch key", it.Name)
		}
	}
	// NPUs, FPGAs and VPUs keep today's behaviour: offered, with no GPU resource.
	for _, name := range []string{"aws.inf1.xlarge", "aws.trn1.2xlarge"} {
		it := byName(its, name)
		if it == nil {
			t.Errorf("%s was dropped", name)
			continue
		}
		if _, ok := it.Capacity[catalog.ResourceNVIDIAGPU]; ok {
			t.Errorf("%s has nvidia.com/gpu", name)
		}
	}
}

// The 8-GPU HGX types of A100, H100 and H200 are offered with the Fabric Manager pin of their
// driver; the NVLink5 B200 needs more than Fabric Manager and stays dropped.
func TestNVSwitchTypesAreOfferedWithFabricManager(t *testing.T) {
	_, its := gpuCatalog(t, "4757.2.3", catalogtestFacts(t), nil, "aws")
	for _, name := range []string{"aws.p4d.24xlarge", "aws.p5.48xlarge"} {
		it := byName(its, name)
		if it == nil {
			t.Errorf("%s is dropped", name)
			continue
		}
		if got := gpuCapacity(it); got != 8 {
			t.Errorf("%s: nvidia.com/gpu = %d, want 8", name, got)
		}
	}
	reg := prometheus.NewRegistry()
	_, its = gpuCatalog(t, "4865.0.0", catalogtestFacts(t), reg, "aws")
	if byName(its, "aws.p6-b200.48xlarge") != nil {
		t.Error("the 8-GPU B200 is offered without nvlsm")
	}
	if got := droppedGauge(t, reg)["c/aws/GPUNVSwitch"]; got < 1 {
		t.Errorf("GPUNVSwitch = %v, want at least 1", got)
	}
}

// awsVirtualFacts answers VirtualGPU as the AWS driver does.
type awsVirtualFacts struct{ catalog.StaticFacts }

func (awsVirtualFacts) VirtualGPU(_, apiReference string) bool {
	return (&aws.Driver{}).Virtual(apiReference)
}

// TestAWSOffersTheNVIDIAFamiliesOfThePlan is the AWS list of plan 12 on 4757.2.3 over the
// snapshot: the families the pinned release can run are offered (the NVSwitch ones with
// Fabric Manager), the arm64, fractional and Blackwell ones are not.
func TestAWSOffersTheNVIDIAFamiliesOfThePlan(t *testing.T) {
	_, its := gpuCatalog(t, "4757.2.3", awsVirtualFacts{catalogtestFacts(t)}, nil, "aws")
	for _, name := range []string{
		"aws.g4dn.xlarge", "aws.g5.xlarge", "aws.g6.xlarge", "aws.gr6.4xlarge", "aws.g6e.xlarge",
		"aws.p3dn.24xlarge", "aws.p5.4xlarge", "aws.p4d.24xlarge", "aws.p4de.24xlarge", "aws.p5.48xlarge", "aws.p5e.48xlarge", "aws.p5en.48xlarge",
	} {
		if byName(its, name) == nil {
			t.Errorf("%s is not offered", name)
		}
	}
	for _, name := range []string{"aws.g5g.xlarge", "aws.g6f.large", "aws.gr6f.4xlarge", "aws.g7e.2xlarge", "aws.p6-b200.48xlarge", "aws.p6-b300.48xlarge"} {
		if byName(its, name) != nil {
			t.Errorf("%s is offered", name)
		}
	}
}

// azureVirtualFacts answers VirtualGPU as the Azure driver does.
type azureVirtualFacts struct{ catalog.StaticFacts }

func (azureVirtualFacts) VirtualGPU(_, apiReference string) bool {
	return (&azure.Driver{}).Virtual(apiReference)
}

// TestAzureOffersTheNVIDIASizesOfThePlan is the Azure list of plan 12 on 4757.2.3 over the
// snapshot: the NC and ND sizes (passthrough GPUs, the 8-GPU A100, H100 and H200 with Fabric
// Manager) are offered, the NV sizes (GRID vGPUs) are not. (The confidential NCC H100 size is dropped
// by the driver's launchable sizes, SKU.Launchable, not by the NVIDIA rules.)
func TestAzureOffersTheNVIDIASizesOfThePlan(t *testing.T) {
	reg := prometheus.NewRegistry()
	_, its := gpuCatalog(t, "4757.2.3", azureVirtualFacts{catalogtest.Facts(t, "azure")}, reg, "azure")
	for _, name := range []string{
		"azure.Standard_NC4as_T4_v3", "azure.Standard_NC64as_T4_v3", "azure.Standard_NC16ads_A10_v4",
		"azure.Standard_NC24ads_A100_v4", "azure.Standard_NC96ads_A100_v4", "azure.Standard_NC40ads_H100_v5",
		"azure.Standard_NC80adis_H100_v5", "azure.Standard_ND40rs_v2", "azure.Standard_ND96asr_v4",
		"azure.Standard_ND96amsr_A100_v4", "azure.Standard_ND96isr_H100_v5", "azure.Standard_ND96isr_H200_v5",
	} {
		if byName(its, name) == nil {
			t.Errorf("%s is not offered", name)
		}
	}
	for _, name := range []string{
		"azure.Standard_NV6s_v2", "azure.Standard_NV24s_v2", "azure.Standard_NV36ads_A10_v5", "azure.Standard_NV72ads_A10_v5",
	} {
		if byName(its, name) != nil {
			t.Errorf("%s is offered", name)
		}
	}
	// The six NV sizes of the pinned snapshot.
	if got := droppedGauge(t, reg)["c/azure/GPUVirtual"]; got != 6 {
		t.Errorf("GPUVirtual = %v, want 6", got)
	}
}

func TestUnsupportedGPUTypesAreDroppedWithTheirReason(t *testing.T) {
	reg := prometheus.NewRegistry()
	facts := virtualFacts{StaticFacts: catalogtestFacts(t), virtual: map[string]bool{"aws.g5.xlarge": true}}
	_, its := gpuCatalog(t, "4757.2.3", facts, reg, "aws")
	for name, why := range map[string]nvidia.Reason{
		"aws.g6f.large":        nvidia.ReasonFractional,
		"aws.gr6f.4xlarge":     nvidia.ReasonFractional,
		"aws.g5g.xlarge":       nvidia.ReasonArch,
		"aws.p6-b200.48xlarge": nvidia.ReasonNoDriverBranch,
		"aws.g5.xlarge":        nvidia.ReasonVirtual,
	} {
		if byName(its, name) != nil {
			t.Errorf("%s is offered, want it dropped for %s", name, why)
		}
	}
	got := droppedGauge(t, reg)
	for key, min := range map[string]float64{
		"c/aws/GPUFractional": 1, "c/aws/GPUArch": 1, "c/aws/GPUNoDriverBranch": 1,
		"c/aws/GPUVirtual": 1,
	} {
		if got[key] < min {
			t.Errorf("scarpenter_catalog_dropped_types{%s} = %v, want at least %v (all: %v)", key, got[key], min, got)
		}
	}
	if got["c/aws/GPUVirtual"] != 1 {
		t.Errorf("GPUVirtual = %v, want 1", got["c/aws/GPUVirtual"])
	}
	if _, ok := got["c/aws/GPUManufacturer"]; ok {
		t.Error("AWS has no non-NVIDIA GPU, yet GPUManufacturer is exported")
	}
}

// droppedGauge returns the drop counts as "class/vendor/reason" to value.
func droppedGauge(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() != "scarpenter_catalog_dropped_types" {
			continue
		}
		for _, m := range mf.GetMetric() {
			l := map[string]string{}
			for _, p := range m.GetLabel() {
				l[p.GetName()] = p.GetValue()
			}
			out[l["nodeclass"]+"/"+l["vendor"]+"/"+l["reason"]] = m.GetGauge().GetValue()
		}
	}
	return out
}

func TestBlackwellIsOfferedOnAReleaseWith580(t *testing.T) {
	_, its := gpuCatalog(t, "4865.0.0", catalogtestFacts(t), nil, "aws")
	it := byName(its, "aws.g7e.2xlarge")
	if it == nil {
		// The RTX PRO 6000 types are named after their family; take any Blackwell type.
		for _, c := range its {
			if c.Requirements.Get(v1alpha1.LabelAcceleratorModel).Has("RTX-PRO-6000") {
				it = c
				break
			}
		}
	}
	if it == nil {
		t.Fatal("no Blackwell type on a release with 580-open")
	}
	if got := it.Requirements.Get(v1alpha1.LabelNVIDIADriverBranch).Any(); got != "580" {
		t.Errorf("branch = %q", got)
	}
	if got := it.Requirements.Get(v1alpha1.LabelNVIDIADriverFlavor).Any(); got != "open" {
		t.Errorf("flavour = %q", got)
	}
}

func TestAClassWithoutAKnownReleaseOffersNoGPUType(t *testing.T) {
	reg := prometheus.NewRegistry()
	_, its := gpuCatalog(t, "1.0.0", catalogtestFacts(t), reg, "aws")
	for _, it := range its {
		if it.Requirements.Get(v1alpha1.LabelAcceleratorType).Has("GPU") {
			t.Fatalf("%s is offered on an unknown release", it.Name)
		}
	}
	if got := droppedGauge(t, reg)["c/aws/GPUNoDriverBranch"]; got < 10 {
		t.Errorf("GPUNoDriverBranch = %v", got)
	}
}

func TestOtherVendorsDropWhatTheTableCannotRun(t *testing.T) {
	reg := prometheus.NewRegistry()
	_, its := gpuCatalog(t, "4757.2.3", catalogtestFacts(t), reg, "azure", "alicloud")
	got := droppedGauge(t, reg)
	for _, key := range []string{"c/azure/GPUManufacturer", "c/azure/GPUFractional", "c/alicloud/GPUFractional"} {
		if got[key] < 1 {
			t.Errorf("%s = %v (all: %v)", key, got[key], got)
		}
	}
	for _, it := range its {
		if it.Requirements.Get(v1alpha1.LabelAcceleratorType).Has("GPU") && gpuCapacity(it) == 0 {
			t.Errorf("%s: a GPU type is offered without nvidia.com/gpu", it.Name)
		}
	}
}

// TestNoFractionalGPUCountReachesTheLabel is the guard of labels.Int's
// rounding: a fraction of a GPU would become the label "0".
func TestNoFractionalGPUCountReachesTheLabel(t *testing.T) {
	_, its := gpuCatalog(t, "4757.2.3", catalogtestFacts(t), nil, "aws", "gcp", "azure", "alicloud", "ovh", "upcloud")
	gpus := 0
	for _, it := range its {
		if !it.Requirements.Get(v1alpha1.LabelAcceleratorType).Has("GPU") {
			continue
		}
		gpus++
		n, err := strconv.Atoi(it.Requirements.Get(v1alpha1.LabelAcceleratorCount).Any())
		if err != nil || n < 1 || int64(n) != gpuCapacity(it) {
			t.Errorf("%s: count label %q, capacity %d", it.Name, it.Requirements.Get(v1alpha1.LabelAcceleratorCount).Any(), gpuCapacity(it))
		}
	}
	if gpus < 50 {
		t.Errorf("only %d GPU types across the vendors", gpus)
	}
}

// snapshotServer is the part of a server row the classification needs.
type snapshotServer struct {
	APIReference string  `json:"api_reference"`
	Type         *string `json:"accelerator_type"`
	Manufacturer *string `json:"accelerator_manufacturer"`
	Model        *string `json:"accelerator_model"`
	Status       string  `json:"status"`
}

// unclassified are NVIDIA models of the snapshots that are deliberately not in
// the table: a vendor's own label, with no architecture to read off it.
var unclassified = map[string]string{
	"G59": "an Alibaba Cloud label of an 8-GPU type, no public architecture",
}

// TestEveryNVIDIAModelOfTheSnapshotsIsClassified makes a new model in Keeper's
// data a visible failure, to fix with one line in pkg/nvidia, and never a
// silent drop.
func TestEveryNVIDIAModelOfTheSnapshotsIsClassified(t *testing.T) {
	rows := make([]snapshotServer, 0, 8192)
	files, err := filepath.Glob(filepath.Join(catalogtest.SnapshotDir(), "snapshot", "server", "*.json.gz"))
	if err != nil || len(files) < 8 {
		t.Fatalf("snapshot server files: %v %v", files, err)
	}
	for _, f := range files {
		vendor := strings.TrimSuffix(filepath.Base(f), ".json.gz")
		rows = append(rows, catalogtest.Rows[snapshotServer](t, "server/"+vendor)...)
	}
	rows = append(rows, catalogtest.OCIRows[snapshotServer](t, "server/oci")...)
	if _, err := os.Stat(catalogtest.OCIDir()); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if r.Status != "active" || r.Type == nil || !strings.EqualFold(*r.Type, "GPU") ||
			r.Manufacturer == nil || *r.Manufacturer != nvidia.Manufacturer || r.Model == nil {
			continue
		}
		model := *r.Model
		seen[model] = true
		if _, ok := nvidia.ModelArchitecture(model); !ok && unclassified[model] == "" {
			t.Errorf("%s: the NVIDIA model %q is not in the table of pkg/nvidia", r.APIReference, model)
		}
	}
	if len(seen) < 20 {
		t.Errorf("only %d NVIDIA models in the snapshots: %v", len(seen), seen)
	}
	for model := range unclassified {
		if _, ok := nvidia.ModelArchitecture(model); ok {
			t.Errorf("%s is classified now: remove it from the exceptions", model)
		}
	}
}
