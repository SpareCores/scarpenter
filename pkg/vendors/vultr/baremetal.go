/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr

import (
	"fmt"
	"strings"
	"time"

	"github.com/vultr/govultr/v3"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Bare-metal servers (plan family vbm) are another API (/v2/bare-metals) with
// IDs of their own, so their instance references say so: "<region>/bm:<uuid>".
// An instance reference is "<region>/<uuid>".
const (
	bareMetalFamily = "vbm"
	bareMetalPrefix = "bm:"
	// planTypeBareMetal is the type of the availability call that lists the
	// bare-metal plans in stock; "all" has none.
	planTypeBareMetal = "vbm"
)

// DefaultRegistrationTimeout is Karpenter's registration timeout when nothing
// sets it.
const DefaultRegistrationTimeout = 15 * time.Minute

// MinBareMetalRegistrationTimeout is the registration window a bare-metal plan
// needs: a server takes about 15 minutes from the create to a registered Node
// (13 to be active, 2 more to join), so the default of 15 minutes of Karpenter's
// liveness check would delete the NodeClaim just as the node comes up.
const MinBareMetalRegistrationTimeout = 25 * time.Minute

// isBareMetalPlan says whether the plan ID is a bare-metal plan.
func isBareMetalPlan(id string) bool {
	family, _, _ := strings.Cut(id, "-")
	return family == bareMetalFamily
}

// bareMetalRef is the reference of a bare-metal server.
func bareMetalRef(region, id string) vendors.InstanceRef {
	return vendors.NewInstanceRef(region, bareMetalPrefix+id)
}

// splitRef reads a reference: the Vultr ID and whether it is a bare-metal
// server's. The ID is not checked.
func splitRef(ref vendors.InstanceRef) (id string, bareMetal bool) {
	id, bareMetal = strings.CutPrefix(ref.ID(), bareMetalPrefix)
	return id, bareMetal
}

// bareMetalWindowOK says whether the registration timeout is long enough for
// bare-metal plans.
func bareMetalWindowOK(timeout time.Duration) bool {
	if timeout == 0 {
		timeout = DefaultRegistrationTimeout
	}
	return timeout >= MinBareMetalRegistrationTimeout
}

// checkBareMetalWindow is the registration-window guard: a bare-metal plan is
// refused unless the registration timeout (vendors.Config.RegistrationTimeout)
// is long enough. It is the one place that decides; the type check, Prepare and the
// create all call it.
func checkBareMetalWindow(plan string, timeout time.Duration) error {
	if !isBareMetalPlan(plan) || bareMetalWindowOK(timeout) {
		return nil
	}
	return vendors.NewError(vendors.Unsupported, codeIncompatible, fmt.Errorf(
		"bare metal plan %s needs a registration timeout of at least %s (Karpenter's --registration-timeout)",
		plan, MinBareMetalRegistrationTimeout))
}

// BuildBareMetalCreateRequest builds the bare-metal create request: the fields
// of the instance request that the API has. It has no firewall group and no VPC
// (a VPC is attached after the create). The response's default_password is never
// read.
func BuildBareMetalCreateRequest(req vendors.CreateRequest, o LaunchOptions) (*govultr.BareMetalCreate, error) {
	c, err := validateCreate(req, o)
	if err != nil {
		return nil, err
	}
	return &govultr.BareMetalCreate{
		Region:          req.Region,
		Plan:            req.InstanceType,
		SnapshotID:      o.SnapshotID,
		Label:           c.name,
		Hostname:        c.name,
		Tags:            c.tags,
		UserData:        c.userData,
		EnableIPv6:      new(o.IPv6),
		ActivationEmail: new(false),
	}, nil
}

// bareMetalStateOf maps a server's status fields as StateOf does an instance's;
// a server has no server_status.
func bareMetalStateOf(s govultr.BareMetalServer) string {
	switch {
	case s.Status == "pending":
		return vendors.StatePending
	case s.Status == "suspended" || s.PowerStatus == "stopped":
		return vendors.StateTerminating
	case s.Status == "active" && s.PowerStatus == "running":
		return vendors.StateRunning
	}
	return vendors.StatePending
}

// ToBareMetalInstance converts a bare-metal server.
func ToBareMetalInstance(s govultr.BareMetalServer, style TagStyle) *vendors.Instance {
	inst := &vendors.Instance{
		Ref:          bareMetalRef(s.Region, s.ID),
		Region:       s.Region,
		Zone:         s.Region,
		Type:         s.Plan,
		CapacityType: vendors.CapacityOnDemand,
		State:        bareMetalStateOf(s),
		Tags:         style.ParseTags(s.Tags),
	}
	if t, err := time.Parse(time.RFC3339, s.DateCreated); err == nil {
		inst.CreatedAt, inst.BillingStart = t, t
	}
	for _, a := range []string{s.MainIP, s.V6MainIP} {
		if a != "" && a != noIP && a != "::" {
			inst.Addresses = append(inst.Addresses, a)
		}
	}
	return inst
}
