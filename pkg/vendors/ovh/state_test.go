/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func TestInstanceState(t *testing.T) {
	for status, want := range map[string]string{
		"BUILD": vendors.StatePending, "REBUILD": vendors.StatePending, "RESIZE": vendors.StatePending,
		"VERIFY_RESIZE": vendors.StatePending, "REVERT_RESIZE": vendors.StatePending, "MIGRATING": vendors.StatePending,
		"UNKNOWN": vendors.StatePending, "SOMETHING_NEW": vendors.StatePending,
		"ACTIVE": vendors.StateRunning, "REBOOT": vendors.StateRunning, "HARD_REBOOT": vendors.StateRunning, "PASSWORD": vendors.StateRunning,
		// Stopped nodes are terminating (plan 14, A4): OVH bills a SHUTOFF server.
		"SHUTOFF": vendors.StateTerminating, "STOPPED": vendors.StateTerminating, "PAUSED": vendors.StateTerminating,
		"SUSPENDED": vendors.StateTerminating, "SHELVED": vendors.StateTerminating, "SHELVED_OFFLOADED": vendors.StateTerminating,
		"RESCUE": vendors.StateTerminating, "DELETING": vendors.StateTerminating, "ERROR": vendors.StateTerminating,
		"DELETED": vendors.StateTerminated, "SOFT_DELETED": vendors.StateTerminated,
		"active": vendors.StateRunning, "shutoff": vendors.StateTerminating,
	} {
		if got := InstanceState(status); got != want {
			t.Errorf("%s: %s, want %s", status, got, want)
		}
	}
}

func TestProviderIDAndRef(t *testing.T) {
	id := ProviderID("EU-WEST-PAR", "default-abc12")
	if id != "scarpenter://ovh/EU-WEST-PAR/default-abc12" {
		t.Fatal(id)
	}
	r, n, err := ParseProviderID(id)
	if err != nil || r != "EU-WEST-PAR" || n != "default-abc12" {
		t.Fatalf("%s %s %v", r, n, err)
	}
	for _, bad := range []string{"", "scarpenter://aws/us-east-1/x", "scarpenter://ovh/GRA11", "ovh://GRA11/x", "scarpenter://ovh//x"} {
		if _, _, err := ParseProviderID(bad); err == nil {
			t.Errorf("%q must not parse", bad)
		}
	}
	uuid := "9c1f0000-0000-4000-8000-0000000000aa"
	ref := InstanceRef("GRA11", uuid)
	if region, sid, err := ParseInstanceRef(ref); err != nil || region != "GRA11" || sid != uuid {
		t.Fatalf("%s %s %v", region, sid, err)
	}
	// Malformed references are NotFound, never Fatal (plan 14, A9).
	for _, bad := range []vendors.InstanceRef{"GRA11/i-never-existed", "GRA11/", "/" + vendors.InstanceRef(uuid), "noslash", ""} {
		if _, _, err := ParseInstanceRef(bad); !vendors.IsClass(err, vendors.NotFound) {
			t.Errorf("%q: want NotFound, got %v", bad, err)
		}
	}
}
