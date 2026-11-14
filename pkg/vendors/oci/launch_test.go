/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci_test

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	driver "github.com/SpareCores/scarpenter/pkg/vendors/oci"
	"github.com/SpareCores/scarpenter/pkg/vendors/oci/fake"
)

func baseSpec() driver.LaunchSpec {
	return driver.LaunchSpec{
		CompartmentID: fake.SeedCompartment, AvailabilityDomain: "aBcD:EU-FRANKFURT-1-AD-1",
		Type:        driver.InstanceType{Shape: "VM.Standard.E5.Flex", Sized: true, OCPUs: 2, GB: 16},
		DisplayName: "claim-1", ImageID: fake.SeedImage,
		Root:     driver.RootVolume{SizeGb: 64, Performance: driver.PerformanceBalanced},
		SubnetID: fake.SeedSubnet, NSGIDs: []string{"nsg-a", "nsg-b"}, PublicIPv4: true,
		Tags:     map[string]string{"scarpenter-cluster": "c", "team": "infra"},
		UserData: []byte(`{"ignition":{"version":"3.4.0"}}`), ExtraMetadata: map[string]string{"owner": "me"},
		RetryToken: "scarpenter-token", IdempotencyKey: "uid-1",
	}
}

// The launch request of plan 25, section 5, field by field.
func TestBuildLaunchSized(t *testing.T) {
	req, err := driver.BuildLaunch(baseSpec())
	if err != nil {
		t.Fatal(err)
	}
	d := req.LaunchInstanceDetails
	src, ok := d.SourceDetails.(core.InstanceSourceViaImageDetails)
	if !ok {
		t.Fatalf("source details are %T", d.SourceDetails)
	}
	check := func(what string, got, want any) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", what, got, want)
		}
	}
	check("compartmentId", *d.CompartmentId, fake.SeedCompartment)
	check("availabilityDomain", *d.AvailabilityDomain, "aBcD:EU-FRANKFURT-1-AD-1")
	check("faultDomain", d.FaultDomain, (*string)(nil))
	check("shape", *d.Shape, "VM.Standard.E5.Flex")
	check("ocpus", *d.ShapeConfig.Ocpus, float32(2))
	check("memoryInGBs", *d.ShapeConfig.MemoryInGBs, float32(16))
	check("vcpus is never sent", d.ShapeConfig.Vcpus, (*int)(nil))
	check("baselineOcpuUtilization is never sent", d.ShapeConfig.BaselineOcpuUtilization,
		core.LaunchInstanceShapeConfigDetailsBaselineOcpuUtilizationEnum(""))
	check("displayName", *d.DisplayName, "claim-1")
	check("imageId", *src.ImageId, fake.SeedImage)
	check("bootVolumeSizeInGBs", *src.BootVolumeSizeInGBs, int64(64))
	check("bootVolumeVpusPerGB", *src.BootVolumeVpusPerGB, int64(10))
	check("kmsKeyId", src.KmsKeyId, (*string)(nil))
	v := d.CreateVnicDetails
	check("subnetId", *v.SubnetId, fake.SeedSubnet)
	check("assignPublicIp", *v.AssignPublicIp, true)
	check("assignIpv6Ip", v.AssignIpv6Ip, (*bool)(nil))
	check("nsgIds", v.NsgIds, []string{"nsg-a", "nsg-b"})
	check("vnic displayName", *v.DisplayName, "claim-1")
	check("hostnameLabel", v.HostnameLabel, (*string)(nil))
	check("skipSourceDestCheck", *v.SkipSourceDestCheck, false)
	wantTags := map[string]string{"scarpenter-cluster": "c", "team": "infra", "scarpenter-idempotency-key": "uid-1"}
	check("freeformTags", d.FreeformTags, wantTags)
	check("vnic freeformTags", v.FreeformTags, wantTags)
	check("definedTags", d.DefinedTags, map[string]map[string]any(nil))
	check("metadata keys", len(d.Metadata), 2)
	check("owner", d.Metadata["owner"], "me")
	raw, err := base64.StdEncoding.DecodeString(d.Metadata["user_data"])
	if err != nil || string(raw) != `{"ignition":{"version":"3.4.0"}}` {
		t.Errorf("user_data = %q, %v; want the base64 of the Ignition", d.Metadata["user_data"], err)
	}
	if _, ok := d.Metadata["ssh_authorized_keys"]; ok {
		t.Error("the launch sets ssh_authorized_keys")
	}
	check("legacy IMDS endpoints disabled", *d.InstanceOptions.AreLegacyImdsEndpointsDisabled, true)
	check("launchOptions for amd64", d.LaunchOptions, (*core.LaunchOptions)(nil))
	check("preemptibleInstanceConfig", d.PreemptibleInstanceConfig, (*core.PreemptibleInstanceConfigDetails)(nil))
	check("availabilityConfig", d.AvailabilityConfig, (*core.LaunchInstanceAvailabilityConfigDetails)(nil))
	check("opc-retry-token", *req.OpcRetryToken, "scarpenter-token")
}

func TestBuildLaunchVariants(t *testing.T) {
	t.Run("fixed shape has no shapeConfig", func(t *testing.T) {
		s := baseSpec()
		s.Type = driver.InstanceType{Shape: "VM.Standard2.4"}
		req, err := driver.BuildLaunch(s)
		if err != nil {
			t.Fatal(err)
		}
		if req.ShapeConfig != nil || *req.Shape != "VM.Standard2.4" {
			t.Errorf("shape %q, config %+v", *req.Shape, req.ShapeConfig)
		}
	})
	t.Run("spot terminates and deletes the boot volume", func(t *testing.T) {
		s := baseSpec()
		s.Spot = true
		req, err := driver.BuildLaunch(s)
		if err != nil {
			t.Fatal(err)
		}
		act, ok := req.PreemptibleInstanceConfig.PreemptionAction.(core.TerminatePreemptionAction)
		if !ok || act.PreserveBootVolume == nil || *act.PreserveBootVolume {
			t.Errorf("preemption action %+v", req.PreemptibleInstanceConfig.PreemptionAction)
		}
	})
	t.Run("arm64 has the five launch options of Flatcar's mantle", func(t *testing.T) {
		s := baseSpec()
		s.Arm64 = true
		req, err := driver.BuildLaunch(s)
		if err != nil {
			t.Fatal(err)
		}
		lo := req.LaunchOptions
		if lo == nil || lo.Firmware != core.LaunchOptionsFirmwareUefi64 ||
			lo.BootVolumeType != core.LaunchOptionsBootVolumeTypeParavirtualized ||
			lo.NetworkType != core.LaunchOptionsNetworkTypeParavirtualized ||
			lo.RemoteDataVolumeType != core.LaunchOptionsRemoteDataVolumeTypeParavirtualized ||
			lo.IsConsistentVolumeNamingEnabled == nil || !*lo.IsConsistentVolumeNamingEnabled {
			t.Errorf("launch options %+v", lo)
		}
	})
	t.Run("IPv6, a key and the ultra-high-performance level", func(t *testing.T) {
		s := baseSpec()
		s.PublicIPv4, s.PublicIPv6 = false, true
		s.Root = driver.RootVolume{SizeGb: 100, Performance: driver.PerformanceUltraHigh, VpusPerGb: 60, KmsKeyID: "ocid1.key.oc1..k"}
		req, err := driver.BuildLaunch(s)
		if err != nil {
			t.Fatal(err)
		}
		src := req.SourceDetails.(core.InstanceSourceViaImageDetails)
		if *req.CreateVnicDetails.AssignPublicIp || !*req.CreateVnicDetails.AssignIpv6Ip ||
			*src.BootVolumeVpusPerGB != 60 || *src.KmsKeyId != "ocid1.key.oc1..k" || *src.BootVolumeSizeInGBs != 100 {
			t.Errorf("vnic %+v, source %+v", req.CreateVnicDetails, src)
		}
	})
	t.Run("no idempotency key writes no tag", func(t *testing.T) {
		s := baseSpec()
		s.IdempotencyKey, s.RetryToken = "", ""
		req, err := driver.BuildLaunch(s)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := req.FreeformTags["scarpenter-idempotency-key"]; ok || req.OpcRetryToken != nil {
			t.Errorf("tags %v, token %v", req.FreeformTags, req.OpcRetryToken)
		}
	})
	t.Run("the spec is not modified", func(t *testing.T) {
		s := baseSpec()
		if _, err := driver.BuildLaunch(s); err != nil {
			t.Fatal(err)
		}
		if len(s.Tags) != 2 || len(s.ExtraMetadata) != 1 {
			t.Errorf("BuildLaunch changed its input: tags %v, metadata %v", s.Tags, s.ExtraMetadata)
		}
	})
}

// The launch must not run under a retry policy that retries an HTTP 500: "Out of
// host capacity" is a 500 (plan 25, finding 7).
func TestBuildLaunchHasNoRetryPolicy(t *testing.T) {
	req, err := driver.BuildLaunch(baseSpec())
	if err != nil {
		t.Fatal(err)
	}
	p := req.RetryPolicy()
	if p == nil {
		t.Fatal("the launch request has no retry policy of its own: the client's default would retry a 500")
	}
	if p.MaximumNumberAttempts != 1 {
		t.Errorf("the launch retry policy allows %d attempts", p.MaximumNumberAttempts)
	}
	for _, status := range []int{500, 502, 503, 429} {
		resp := common.OCIOperationResponse{Error: fake.Error(status, "InternalError", "Out of host capacity.")}
		if p.ShouldRetryOperation(resp) {
			t.Errorf("the launch policy retries a %d", status)
		}
	}
	// The default policy the clients carry does retry them: the per-request policy is
	// what makes the difference.
	def := common.DefaultRetryPolicyWithoutEventualConsistency()
	if !def.ShouldRetryOperation(common.OCIOperationResponse{Error: fake.Error(500, "InternalError", "x")}) {
		t.Error("the client's default policy no longer retries a 500: re-read plan 25, section 5")
	}
}

func TestBuildLaunchRejects(t *testing.T) {
	big := strings.Repeat("x", 32000)
	for name, edit := range map[string]func(*driver.LaunchSpec){
		"no compartment":     func(s *driver.LaunchSpec) { s.CompartmentID = "" },
		"no availability":    func(s *driver.LaunchSpec) { s.AvailabilityDomain = "" },
		"no shape":           func(s *driver.LaunchSpec) { s.Type = driver.InstanceType{} },
		"no image":           func(s *driver.LaunchSpec) { s.ImageID = "" },
		"no subnet":          func(s *driver.LaunchSpec) { s.SubnetID = "" },
		"no name":            func(s *driver.LaunchSpec) { s.DisplayName = "" },
		"no root size":       func(s *driver.LaunchSpec) { s.Root.SizeGb = 0 },
		"six groups":         func(s *driver.LaunchSpec) { s.NSGIDs = []string{"1", "2", "3", "4", "5", "6"} },
		"reserved user_data": func(s *driver.LaunchSpec) { s.ExtraMetadata = map[string]string{"user_data": "x"} },
		"reserved ssh keys":  func(s *driver.LaunchSpec) { s.ExtraMetadata = map[string]string{"ssh_authorized_keys": "x"} },
		"metadata too large": func(s *driver.LaunchSpec) { s.ExtraMetadata = map[string]string{"big": big} },
		"unknown level":      func(s *driver.LaunchSpec) { s.Root.Performance = "turbo" },
		"vpus on balanced":   func(s *driver.LaunchSpec) { s.Root.VpusPerGb = 20 },
	} {
		t.Run(name, func(t *testing.T) {
			s := baseSpec()
			edit(&s)
			if _, err := driver.BuildLaunch(s); err == nil {
				t.Error("no error")
			}
		})
	}
}

// A spec without tags still gets the idempotency key's tag.
func TestBuildLaunchWithoutTags(t *testing.T) {
	s := baseSpec()
	s.Tags = nil
	req, err := driver.BuildLaunch(s)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.FreeformTags[driver.TagKey(vendors.TagIdempotencyKey)]; got == "" || got != s.IdempotencyKey {
		t.Errorf("idempotency tag %q, want %q", got, s.IdempotencyKey)
	}
}
