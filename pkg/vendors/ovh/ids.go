/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/SpareCores/scarpenter/pkg/scope"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Vendor is the Keeper vendor ID.
const Vendor = "ovh"

// ProviderID is scarpenter://ovh/<region>/<nodeclaim>, rendered before the
// instance exists. The region is the OpenStack (API) region name.
func ProviderID(region, nodeClaim string) string {
	return scope.ProviderID(Vendor, region, nodeClaim)
}

// ParseProviderID splits an OVH provider ID into the region and the NodeClaim
// name.
func ParseProviderID(id string) (region, nodeClaim string, err error) {
	v, region, nodeClaim, err := scope.ParseProviderID(id)
	if err != nil {
		return "", "", err
	}
	if v != Vendor {
		return "", "", fmt.Errorf("provider ID %q is of vendor %q, not %s", id, v, Vendor)
	}
	return region, nodeClaim, nil
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// InstanceRef is "<region>/<server UUID>" (the core stores it in the NodeClaim
// annotation).
func InstanceRef(region, serverID string) vendors.InstanceRef {
	return vendors.NewInstanceRef(region, serverID)
}

// ParseInstanceRef splits a reference. A reference the driver cannot parse (no
// region, an ID that is not a UUID, such as the conformance suite's
// <region>/i-never-existed) is an instance that cannot exist: the error is
// NotFound, never Fatal (plan 14, A9).
func ParseInstanceRef(ref vendors.InstanceRef) (region, id string, err error) {
	region, id = ref.Region(), ref.ID()
	if region == "" || !uuidRe.MatchString(id) || strings.Contains(region, "/") {
		return "", "", vendors.NewError(vendors.NotFound, "MalformedReference",
			errors.New("not an OVH instance reference: "+string(ref)))
	}
	return region, id, nil
}
