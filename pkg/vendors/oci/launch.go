/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import (
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// LaunchSpec is everything the launch request depends on, already resolved: the
// driver fills it from the CreateRequest, the settings and the prepared
// resources. BuildLaunch is pure.
type LaunchSpec struct {
	CompartmentID string
	// AvailabilityDomain is the full name of the zone's availability domain
	// ("Xxxx:EU-FRANKFURT-1-AD-1").
	AvailabilityDomain string
	Type               InstanceType
	// DisplayName is the NodeClaim name.
	DisplayName string
	// Spot selects a preemptible instance; false is on-demand.
	Spot bool
	// Arm64 selects the launch options a Flatcar arm64 image needs.
	Arm64   bool
	ImageID string
	// Root is the boot volume.
	Root           RootVolume
	SubnetID       string
	NSGIDs         []string
	PublicIPv4     bool
	PublicIPv6     bool
	Tags           map[string]string
	UserData       []byte
	ExtraMetadata  map[string]string
	RetryToken     string
	IdempotencyKey string
}

// BuildLaunch builds the LaunchInstance request: every field of the plan's
// launch table (section 5). It does not call the API.
func BuildLaunch(s LaunchSpec) (core.LaunchInstanceRequest, error) {
	if err := s.validate(); err != nil {
		return core.LaunchInstanceRequest{}, err
	}
	vpus, err := s.Root.vpus()
	if err != nil {
		return core.LaunchInstanceRequest{}, err
	}
	meta, err := s.metadata()
	if err != nil {
		return core.LaunchInstanceRequest{}, err
	}
	tags := maps.Clone(s.Tags)
	if tags == nil {
		tags = map[string]string{}
	}
	if s.IdempotencyKey != "" {
		tags[TagKey(vendors.TagIdempotencyKey)] = s.IdempotencyKey
	}
	source := core.InstanceSourceViaImageDetails{
		ImageId: new(s.ImageID), BootVolumeSizeInGBs: new(s.Root.SizeGb),
		BootVolumeVpusPerGB: new(vpus),
	}
	if s.Root.KmsKeyID != "" {
		source.KmsKeyId = new(s.Root.KmsKeyID)
	}
	vnic := &core.CreateVnicDetails{
		SubnetId: new(s.SubnetID), AssignPublicIp: new(s.PublicIPv4),
		NsgIds: slices.Clone(s.NSGIDs), DisplayName: new(s.DisplayName),
		FreeformTags: maps.Clone(tags), SkipSourceDestCheck: new(false),
	}
	if s.PublicIPv6 {
		vnic.AssignIpv6Ip = new(true)
	}
	details := core.LaunchInstanceDetails{
		CompartmentId:      new(s.CompartmentID),
		AvailabilityDomain: new(s.AvailabilityDomain),
		Shape:              new(s.Type.Shape),
		DisplayName:        new(s.DisplayName),
		SourceDetails:      source,
		CreateVnicDetails:  vnic,
		Metadata:           meta,
		FreeformTags:       tags,
		// Ignition reads /opc/v2/ (verified live) and SCarpenter uses no Afterburn,
		// so the legacy endpoints are off; the lane re-checks that a node boots.
		InstanceOptions: &core.InstanceOptions{AreLegacyImdsEndpointsDisabled: new(true)},
	}
	if s.Type.Sized {
		details.ShapeConfig = &core.LaunchInstanceShapeConfigDetails{
			Ocpus: new(float32(s.Type.OCPUs)), MemoryInGBs: new(float32(s.Type.GB)),
		}
	}
	if s.Arm64 {
		// The five options of Flatcar's mantle (launchOptionsForBoard), verified live
		// on A1.Flex.
		details.LaunchOptions = &core.LaunchOptions{
			Firmware:                        core.LaunchOptionsFirmwareUefi64,
			BootVolumeType:                  core.LaunchOptionsBootVolumeTypeParavirtualized,
			NetworkType:                     core.LaunchOptionsNetworkTypeParavirtualized,
			RemoteDataVolumeType:            core.LaunchOptionsRemoteDataVolumeTypeParavirtualized,
			IsConsistentVolumeNamingEnabled: new(true),
		}
	}
	if s.Spot {
		// Terminate on reclaim and delete the boot volume: a preemptible instance
		// cannot be stopped.
		details.PreemptibleInstanceConfig = &core.PreemptibleInstanceConfigDetails{
			PreemptionAction: core.TerminatePreemptionAction{PreserveBootVolume: new(false)},
		}
	}
	// The launch must never run under a retry policy: the SDK's default one
	// retries every 5xx, and the capacity error is an HTTP 500. The SDK adapter
	// enforces it too; setting it here makes the request carry the rule.
	none := common.NoRetryPolicy()
	req := core.LaunchInstanceRequest{
		LaunchInstanceDetails: details, RequestMetadata: common.RequestMetadata{RetryPolicy: &none},
	}
	if s.RetryToken != "" {
		req.OpcRetryToken = new(s.RetryToken)
	}
	return req, nil
}

func (s LaunchSpec) validate() error {
	if s.CompartmentID == "" || s.AvailabilityDomain == "" || s.Type.Shape == "" || s.ImageID == "" ||
		s.SubnetID == "" || s.DisplayName == "" {
		return errors.New("launch: compartment, availability domain, shape, image, subnet and display name are required")
	}
	if s.Root.SizeGb <= 0 {
		return errors.New("launch: the root volume needs a size")
	}
	if len(s.NSGIDs) > maxNSGs {
		return fmt.Errorf("launch: %d network security groups; OCI allows %d on a VNIC", len(s.NSGIDs), maxNSGs)
	}
	return nil
}

// metadata builds the instance metadata: the block's keys and user_data, the
// base64 of the Ignition. The total of every value stays under OCI's limit.
func (s LaunchSpec) metadata() (map[string]string, error) {
	meta := maps.Clone(s.ExtraMetadata)
	if meta == nil {
		meta = map[string]string{}
	}
	for _, k := range ReservedMetadataKeys {
		if _, ok := meta[k]; ok {
			return nil, fmt.Errorf("launch: metadata key %q is reserved", k)
		}
	}
	meta[MetaUserData] = base64.StdEncoding.EncodeToString(s.UserData)
	total := 0
	for k, v := range meta {
		total += len(k) + len(v)
	}
	if total > MaxMetadataBytes {
		return nil, fmt.Errorf("launch: the metadata is %d bytes (user_data %d as base64); OCI allows %d",
			total, len(meta[MetaUserData]), MaxMetadataBytes)
	}
	return meta, nil
}
