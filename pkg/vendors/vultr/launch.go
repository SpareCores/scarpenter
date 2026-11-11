/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/vultr/govultr/v3"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// MaxUserDataBytes is the budget for the rendered Ignition: the plan's 16 KiB.
// Vultr documents no limit (spike S3 measures it), so this is our own ceiling.
const MaxUserDataBytes = 16 << 10

// LaunchOptions are what the driver resolved for one launch beyond the generic
// CreateRequest.
// TODO(plan 14): fill from vendors.CreateRequest.Settings (the resolved
// `vultr` block and networking) and Prepared (the firewall group and the VPC
// resource refs) once the NodeClass types exist.
type LaunchOptions struct {
	// SnapshotID is the Flatcar snapshot from ResolveImage (CreateRequest.Image.Ref).
	SnapshotID string
	// FirewallGroupID is the group from Prepare; empty leaves inbound open, which
	// the driver never does (the managed group is the default).
	FirewallGroupID string
	// VPCIDs are the VPCs to attach, none without a VPC resource.
	VPCIDs []string
	// IPv6 sets enable_ipv6 (a /64 on the public interface).
	IPv6 bool
	// Tags is the spelling of the tag strings.
	Tags TagStyle
}

// BuildCreateRequest builds the Instance.Create request (plan 20, section 4.1).
// The response's default_password is never read. The golden file
// testdata/create_request.golden.json pins every field.
func BuildCreateRequest(req vendors.CreateRequest, o LaunchOptions) (*govultr.InstanceCreateReq, error) {
	name := req.Node.Name
	if name == "" {
		name = req.Tags[vendors.TagNodeClaim]
	}
	switch {
	case req.Region == "":
		return nil, invalid("no region")
	case req.InstanceType == "":
		return nil, invalid("no plan")
	case o.SnapshotID == "":
		return nil, invalid("no snapshot")
	case name == "":
		return nil, invalid("no NodeClaim name for the label and hostname")
	case len(req.UserData) == 0:
		return nil, invalid("no user data: the node cannot configure itself without Ignition")
	case req.CapacityType != "" && req.CapacityType != vendors.CapacityOnDemand:
		return nil, noInstancesOf(req.CapacityType)
	case len(req.UserData) > MaxUserDataBytes:
		return nil, vendors.NewError(vendors.Fatal, codeUserDataTooBig,
			fmt.Errorf("user data is %d bytes, the budget is %d", len(req.UserData), MaxUserDataBytes))
	}
	tags := o.Tags.Tags(vendors.IdempotencyTag(req.Tags, req.IdempotencyKey), req.ExtraTags)
	out := &govultr.InstanceCreateReq{
		Region:          req.Region,
		Plan:            req.InstanceType,
		SnapshotID:      o.SnapshotID,
		Label:           name,
		Hostname:        name,
		Tags:            tags,
		UserData:        base64.StdEncoding.EncodeToString(req.UserData),
		FirewallGroupID: o.FirewallGroupID,
		EnableIPv6:      new(o.IPv6),
		Backups:         "disabled",
		ActivationEmail: new(false),
		DDOSProtection:  new(false),
	}
	if len(o.VPCIDs) > 0 {
		out.AttachVPC = slices.Clone(o.VPCIDs)
	}
	return out, nil
}

// noInstancesOf is the Unsupported error of a capacity type or an architecture
// Vultr does not have.
func noInstancesOf(what string) error {
	return vendors.NewError(vendors.Unsupported, codeUnsupportedArch, fmt.Errorf("%s has no %s instances", Vendor, what))
}

func invalid(msg string) error {
	return vendors.NewError(vendors.Fatal, codeInvalid, fmt.Errorf("%s", msg))
}

// MarshalCreateRequest is the JSON govultr sends for the request, for the
// golden test.
func MarshalCreateRequest(r *govultr.InstanceCreateReq) ([]byte, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// StateOf maps an instance's status fields (plan 20, section 4.1). An
// instance whose status is pending is pending whatever its power_status says
// (whether Vultr reports stopped while installing is unverified, S4, and a
// terminating answer would let the garbage collector delete a booting node).
// Otherwise a suspended or powered-off instance is terminating (plan 14, A4):
// it still bills for the started hour, so the garbage collector deletes it.
// Anything not yet fully up is pending.
func StateOf(in govultr.Instance) string {
	switch {
	case in.Status == "pending":
		return vendors.StatePending
	case in.Status == "suspended" || in.PowerStatus == "stopped":
		return vendors.StateTerminating
	case in.Status == "active" && in.PowerStatus == "running" && in.ServerStatus == "ok":
		return vendors.StateRunning
	}
	return vendors.StatePending
}

// noIP is main_ip while the instance is pending.
const noIP = "0.0.0.0"

// ToInstance converts a Vultr instance. The addresses feed the serving
// certificate approver: main_ip (none while pending), v6_main_ip and the VPC's
// internal_ip. The zone is the region.
func ToInstance(in govultr.Instance, style TagStyle) *vendors.Instance {
	inst := &vendors.Instance{
		Ref:          vendors.NewInstanceRef(in.Region, in.ID),
		Region:       in.Region,
		Zone:         in.Region,
		Type:         in.Plan,
		CapacityType: vendors.CapacityOnDemand,
		State:        StateOf(in),
		Tags:         style.ParseTags(in.Tags),
	}
	// date_created is the billing start (ADR-024); it may be a little earlier
	// than the real start, which is the safe side, except possibly on Vultr (S7).
	if t, err := time.Parse(time.RFC3339, in.DateCreated); err == nil {
		inst.CreatedAt = t
	}
	for _, a := range []string{in.MainIP, in.V6MainIP, in.InternalIP} {
		if a != "" && a != noIP && a != "::" {
			inst.Addresses = append(inst.Addresses, a)
		}
	}
	return inst
}

// hasTags reports whether the instance carries every requested logical tag.
func hasTags(inst *vendors.Instance, tags map[vendors.TagKey]string) bool {
	for k, v := range tags {
		if got, ok := inst.Tags[k]; !ok || got != v {
			return false
		}
	}
	return true
}
