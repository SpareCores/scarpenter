/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci_test

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	driver "github.com/SpareCores/scarpenter/pkg/vendors/oci"
)

// policyFile is the policy of the controller's identity, one of the reference
// documents.
const policyFile = "../../../docs/reference/oci-iam-policy.txt"

// need is what one call requires: a verb on a resource type in a scope.
type need struct{ verb, resource, scope string }

// methodNeeds maps every method of the driver's API interfaces to the policy
// statements its call needs. UNVERIFIED (spike S12), as the policy file is.
var methodNeeds = map[string][]need{
	"ComputeAPI.LaunchInstance": {
		{"manage", "instances", "compartment"}, {"use", "boot-volumes", "compartment"},
		{"use", "subnets", "compartment"}, {"use", "network-security-groups", "compartment"},
	},
	"ComputeAPI.GetInstance":              {{"read", "instances", "compartment"}},
	"ComputeAPI.ListInstances":            {{"inspect", "instances", "compartment"}},
	"ComputeAPI.TerminateInstance":        {{"manage", "instances", "compartment"}, {"use", "boot-volumes", "compartment"}},
	"ComputeAPI.ListVnicAttachments":      {{"read", "vnic-attachments", "compartment"}},
	"ComputeAPI.ListShapes":               {{"inspect", "instances", "compartment"}},
	"NetworkAPI.GetVnic":                  {{"read", "vnics", "compartment"}},
	"IdentityAPI.ListRegionSubscriptions": {{"inspect", "tenancies", "tenancy"}},
	"IdentityAPI.ListAvailabilityDomains": {{"inspect", "compartments", "tenancy"}},
	"LimitsAPI.GetResourceAvailability":   {{"inspect", "resource-availability", "tenancy"}},
	"LimitsAPI.ListLimitDefinitions":      {{"inspect", "limits", "tenancy"}},
}

// families are the resource types the aggregate resource types of the policy
// language contain (the part the driver uses).
var families = map[string][]string{
	"instance-family":        {"instances", "vnic-attachments", "instance-images", "console-histories"},
	"volume-family":          {"volumes", "boot-volumes", "volume-attachments", "boot-volume-attachments"},
	"virtual-network-family": {"vnics", "subnets", "vcns", "network-security-groups", "route-tables", "security-lists", "internet-gateways", "private-ips", "vnic-attachments"},
}

var verbRank = map[string]int{"inspect": 1, "read": 2, "use": 3, "manage": 4}

var statementRe = regexp.MustCompile(`^Allow group <group> to (inspect|read|use|manage) ([a-z-]+) in (compartment <compartment>|tenancy)$`)

type statement struct {
	need
	optional bool
}

func readPolicy(t *testing.T) []statement {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(policyFile))
	if err != nil {
		t.Fatal(err)
	}
	var out []statement
	optional := false
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case strings.HasPrefix(line, "# optional"):
			optional = true
		case strings.HasPrefix(line, "#"):
		default:
			m := statementRe.FindStringSubmatch(line)
			if m == nil {
				t.Fatalf("%s: %q is not a statement of the form Allow group <group> to <verb> <resource> in <scope>", policyFile, line)
			}
			scope := "tenancy"
			if strings.HasPrefix(m[3], "compartment") {
				scope = "compartment"
			}
			out = append(out, statement{need{m[1], m[2], scope}, optional})
		}
	}
	return out
}

func (s statement) covers(n need) bool {
	if s.scope != n.scope || verbRank[s.verb] < verbRank[n.verb] {
		return false
	}
	return s.resource == n.resource || slices.Contains(families[s.resource], n.resource)
}

// TestPolicyMatchesTheDriver: every method of the API interfaces the driver calls is
// covered by a statement, every required statement is needed by a method and no
// stronger than the strongest need it covers, and the optional block adds only the
// managed network.
func TestPolicyMatchesTheDriver(t *testing.T) {
	var methods []string
	for _, typ := range []reflect.Type{reflect.TypeFor[driver.ComputeAPI](), reflect.TypeFor[driver.NetworkAPI](),
		reflect.TypeFor[driver.IdentityAPI](), reflect.TypeFor[driver.LimitsAPI]()} {
		for m := range typ.Methods() {
			methods = append(methods, typ.Name()+"."+m.Name)
		}
	}
	for _, m := range methods {
		if _, ok := methodNeeds[m]; !ok {
			t.Errorf("the API method %s has no entry in this test's table", m)
		}
	}
	for m := range methodNeeds {
		if !slices.Contains(methods, m) {
			t.Errorf("the table names %s, which is not an API method", m)
		}
	}

	stmts := readPolicy(t)
	maxNeed := map[int]int{}
	for m, needs := range methodNeeds {
		for _, n := range needs {
			first := slices.IndexFunc(stmts, func(s statement) bool { return !s.optional && s.covers(n) })
			if first < 0 {
				t.Errorf("%s needs %s %s in %s: no statement covers it", m, n.verb, n.resource, n.scope)
				continue
			}
			maxNeed[first] = max(maxNeed[first], verbRank[n.verb])
		}
	}
	for i, s := range stmts {
		switch {
		case s.optional:
			if s.resource != "virtual-network-family" || s.scope != "compartment" {
				t.Errorf("the optional statement %+v is not the managed network", s)
			}
		case maxNeed[i] == 0:
			t.Errorf("the statement %+v is needed by no method", s)
		case verbRank[s.verb] > maxNeed[i]:
			t.Errorf("the statement %+v is stronger than the strongest need (%d) it covers", s, maxNeed[i])
		}
	}
	for _, s := range stmts {
		if s.verb == "manage" && !slices.Contains([]string{"instance-family", "virtual-network-family"}, s.resource) {
			t.Errorf("the policy manages %s", s.resource)
		}
	}
}
