/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi"
)

// The OpenStack role the policy grants for each narrow API interface of the driver:
// a new interface, or one that moves to another service, makes this table (and the
// policy and the how-to) answer to it.
var roleOfInterface = map[string][]string{
	"Compute":    {"computeOperator"},
	"NetworkAPI": {"networkOperator", "networkSecurityOperator"},
	"Images":     {"imageOperator"},
	"Volumes":    {"volumeOperator"},
}

func TestIAMPolicyGrantsTheRolesOfTheDriver(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(testutil.RepoRoot(), "docs", "reference", "ovh-iam-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Name       string `json:"name"`
		Identities []string
		Resources  []struct {
			URN string `json:"urn"`
		}
		Allow []struct {
			Action string `json:"action"`
		}
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Name == "" || len(doc.Identities) != 1 || len(doc.Resources) != 1 ||
		!strings.HasPrefix(doc.Resources[0].URN, "urn:v1:eu:resource:publicCloudProject:") {
		t.Errorf("the policy needs a name, one identity and one project: %+v", doc)
	}
	granted := map[string]bool{}
	for _, a := range doc.Allow {
		role, ok := strings.CutPrefix(a.Action, "publicCloudProject:openstack:")
		if !ok || strings.Contains(role, "*") || granted[role] {
			t.Errorf("the action %q is not a single OpenStack role of the project, or is repeated", a.Action)
		}
		granted[role] = true
	}
	want := map[string]bool{}
	for _, iface := range []reflect.Type{
		reflect.TypeFor[osapi.Compute](), reflect.TypeFor[osapi.NetworkAPI](), reflect.TypeFor[osapi.Images](),
		reflect.TypeFor[osapi.Volumes](),
	} {
		roles, ok := roleOfInterface[iface.Name()]
		if !ok {
			t.Errorf("no role for the API interface %s", iface.Name())
		}
		for _, r := range roles {
			want[r] = true
		}
	}
	for r := range want {
		if !granted[r] {
			t.Errorf("the policy does not grant %s, which the driver needs", r)
		}
	}
	for r := range granted {
		if !want[r] {
			t.Errorf("the policy grants %s, which the driver does not need", r)
		}
	}
	// The how-to names the same roles.
	how, err := os.ReadFile(filepath.Join(testutil.RepoRoot(), "docs", "how-to-guides", "ovh-credentials.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range slices.Sorted(func(yield func(string) bool) {
		for k := range want {
			if !yield(k) {
				return
			}
		}
	}) {
		if !strings.Contains(string(how), r) {
			t.Errorf("ovh-credentials.md does not mention %s", r)
		}
	}
}
