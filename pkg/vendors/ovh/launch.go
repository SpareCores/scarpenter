/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi"
)

// Metadata and tag conventions. Nova metadata keys are restricted in practice, so
// every logical tag goes through TagKey as scarpenter-<key>; the NodeClass's own
// tags are prefixed scarpenter-tag-.
const (
	MetadataPrefix = "scarpenter-"
	ExtraTagPrefix = "scarpenter-tag-"
	// MaxUserData is the driver's budget of user data (overview section 8.2);
	// Nova's own limit is 65,535 bytes base64-encoded.
	MaxUserData = 16 * 1024
	// MaxMetadataItems, MaxMetadataLen are Nova's limits of server metadata.
	MaxMetadataItems = 128
	MaxMetadataLen   = 255
	// MaxServerTagLen is Nova's limit of a server tag (microversion 2.26).
	MaxServerTagLen = 60
)

// TagKey maps a logical tag key to Nova's metadata charset: scarpenter-<key>.
func TagKey(k vendors.TagKey) string { return MetadataPrefix + string(k) }

// Metadata renders the logical tags (the idempotency key included when the caller
// added it with vendors.IdempotencyTag) and the NodeClass's own tags into server
// metadata, enforcing Nova's limits.
func Metadata(tags map[vendors.TagKey]string, extra map[string]string) (map[string]string, error) {
	md := make(map[string]string, len(tags)+len(extra))
	for k, v := range tags {
		md[TagKey(k)] = v
	}
	for k, v := range extra {
		md[ExtraTagPrefix+k] = v
	}
	if len(md) > MaxMetadataItems {
		return nil, invalidSettings("%d metadata items, Nova allows %d", len(md), MaxMetadataItems)
	}
	for k, v := range md {
		if len(k) > MaxMetadataLen || len(v) > MaxMetadataLen {
			return nil, invalidSettings("metadata %q: key and value are at most %d characters", k, MaxMetadataLen)
		}
	}
	return md, nil
}

// TagsFromMetadata reads the logical tags back from a server's metadata; keys that
// are not ours (or are the NodeClass's own tags) are ignored.
func TagsFromMetadata(md map[string]string) map[vendors.TagKey]string {
	out := map[vendors.TagKey]string{}
	for _, k := range vendors.TagKeys {
		if v, ok := md[TagKey(k)]; ok {
			out[k] = v
		}
	}
	return out
}

// ClusterTag is the Nova server tag that lists a cluster's servers server side
// (microversion 2.26): scarpenter-cluster=<cluster>, at most 60 characters. A
// longer one is shortened and ends in a hash of the full name, so two long names
// do not collide. Characters Nova rejects in a tag ("/" and ",") become "_".
func ClusterTag(cluster string) string {
	tag := "scarpenter-cluster=" + strings.NewReplacer("/", "_", ",", "_").Replace(cluster)
	if len(tag) <= MaxServerTagLen {
		return tag
	}
	sum := sha256.Sum256([]byte(cluster))
	return tag[:MaxServerTagLen-9] + "-" + hex.EncodeToString(sum[:4])
}

// LaunchPlan is everything BuildCreateRequest needs besides the core's
// CreateRequest: the per-region ids the driver resolved before the call.
type LaunchPlan struct {
	Request  vendors.CreateRequest
	Settings Settings
	// FlavorID is the region's ID of Request.InstanceType (IDs differ per
	// region; the name is Keeper's api_reference).
	FlavorID string
	// ExternalNetworkID is the region's Ext-Net network.
	ExternalNetworkID string
	// SecurityGroups are the group names to attach (Nova takes names); empty is
	// the project's default group.
	SecurityGroups []string
	// ServerGroupID is the server group of a placement profile, "".
	ServerGroupID string
	// MultiAZ is true for the 3-AZ regions: the create call names the zone. In a
	// 1-AZ region it is omitted (Nova's own zone name there is unverified, S4).
	MultiAZ bool
}

// BuildCreateRequest turns a launch attempt into the Nova server create request:
// the name is the NodeClaim, the metadata carries every logical tag, the
// NodeClass's tags and the idempotency key, the server tag lists the cluster, the
// Ignition user data is passed as is, the node attaches to Ext-Net, and the disks
// are the settings' (the flavor's local disk by default).
func BuildCreateRequest(p LaunchPlan) (osapi.CreateServerRequest, error) {
	r := p.Request
	name := r.Node.Name
	if name == "" {
		name = r.Tags[vendors.TagNodeClaim]
	}
	if name == "" {
		return osapi.CreateServerRequest{}, invalidSettings("the launch has no NodeClaim name")
	}
	if len(r.UserData) > MaxUserData {
		return osapi.CreateServerRequest{}, vendors.NewError(vendors.Fatal, CodeUserDataTooLarge,
			fmt.Errorf("user data is %d bytes, the limit is %d", len(r.UserData), MaxUserData))
	}
	if p.FlavorID == "" || p.ExternalNetworkID == "" || r.Image.Ref == "" {
		return osapi.CreateServerRequest{}, invalidSettings(
			"flavor, Ext-Net and image must be resolved before the request is built")
	}
	md, err := Metadata(vendors.IdempotencyTag(r.Tags, r.IdempotencyKey), r.ExtraTags)
	if err != nil {
		return osapi.CreateServerRequest{}, err
	}
	req := osapi.CreateServerRequest{
		Name: name, FlavorID: p.FlavorID, ImageID: r.Image.Ref, NetworkIDs: []string{p.ExternalNetworkID},
		SecurityGroups: slices.Clone(p.SecurityGroups), UserData: r.UserData, ConfigDrive: p.Settings.ConfigDrive,
		Metadata: md, SchedulerGroup: p.ServerGroupID,
	}
	if req.ConfigDrive == nil {
		t := true
		req.ConfigDrive = &t
	}
	if c := r.Tags[vendors.TagCluster]; c != "" {
		req.Tags = []string{ClusterTag(c)}
	}
	if p.MultiAZ {
		req.AvailabilityZone = r.Zone
	}
	if b := p.Settings.Storage.Root.BootFromVolume; b != nil {
		// The root is a Cinder volume made from the image; the server's imageRef
		// is empty. The volume goes with the server.
		req.ImageID = ""
		req.BlockDevices = append(req.BlockDevices, osapi.BlockDevice{
			SourceType: "image", DestinationType: "volume", UUID: r.Image.Ref, BootIndex: 0,
			SizeGiB: int(gib(b.Size)), VolumeType: b.VolumeType, DeleteOnTermination: true,
		})
	}
	for _, d := range p.Settings.Storage.Data {
		req.BlockDevices = append(req.BlockDevices, osapi.BlockDevice{
			SourceType: "blank", DestinationType: "volume", BootIndex: -1,
			SizeGiB: int(gib(d.Size)), VolumeType: d.VolumeType, DeleteOnTermination: true,
		})
	}
	return req, nil
}

// InstanceFrom maps a server to the contract's Instance: region is the API region
// name, zone the Keeper-side zone name the caller derived (ZoneIDs), the type the
// flavor name (Nova's original_name, since the flavor has no ID from 2.47), the
// tags the logical ones from the metadata, and every address, IPv4 first.
func InstanceFrom(region, zone string, s osapi.Server) *vendors.Instance {
	var v4, v6 []string
	for _, a := range s.Addresses {
		if a.Version == 6 {
			v6 = append(v6, a.Addr)
		} else {
			v4 = append(v4, a.Addr)
		}
	}
	return &vendors.Instance{
		Ref: InstanceRef(region, s.ID), Region: region, Zone: zone, Type: s.FlavorName,
		CapacityType: vendors.CapacityOnDemand, State: InstanceState(s.Status), CreatedAt: s.Created,
		Tags: TagsFromMetadata(s.Metadata), Addresses: slices.Concat(v4, v6),
	}
}

// PrimaryAddress is the address a node is reached on under the direct join: the
// public IPv4, "" when it has none.
func PrimaryAddress(s osapi.Server) string {
	for _, a := range s.Addresses {
		if a.Version == 4 && a.Network == osapi.ExternalNetwork {
			return a.Addr
		}
	}
	return ""
}
