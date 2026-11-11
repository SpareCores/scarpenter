/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/internal/testutil"
)

// TestRAMPolicyAllowsEveryCallOfTheDriver ties the documented policy to the driver's
// narrow API interfaces: every method of ECSAPI and VPCAPI is allowed by a statement
// (ecs:<Method>, vpc:<Method>), and the policy allows no action the driver does not call.
// Quota Center is not called (the quota reader uses DescribeAccountAttributes). The
// mutating statements carry the cluster tag condition.
func TestRAMPolicyAllowsEveryCallOfTheDriver(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(testutil.RepoRoot(), "docs", "reference", "alicloud-ram-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Version   string `json:"Version"`
		Statement []struct {
			Effect    string                    `json:"Effect"`
			Action    []string                  `json:"Action"`
			Resource  any                       `json:"Resource"`
			Condition map[string]map[string]any `json:"Condition"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != "1" {
		t.Errorf("version %q", doc.Version)
	}
	allowed := map[string]bool{}
	conditioned := map[string]bool{}
	for _, s := range doc.Statement {
		if s.Effect != "Allow" {
			t.Errorf("effect %q", s.Effect)
		}
		for _, a := range s.Action {
			allowed[a] = true
			if len(s.Condition) > 0 {
				conditioned[a] = true
			}
		}
	}
	called := map[string]bool{}
	for prefix, typ := range map[string]reflect.Type{
		"ecs": reflect.TypeFor[ECSAPI](), "vpc": reflect.TypeFor[VPCAPI](),
	} {
		for m := range typ.Methods() {
			action := prefix + ":" + m.Name
			called[action] = true
			if !allowed[action] {
				t.Errorf("%s is called by the driver and not allowed", action)
			}
			name := m.Name
			mutates := strings.HasPrefix(name, "Create") || strings.HasPrefix(name, "Delete") || strings.HasPrefix(name, "Authorize") ||
				strings.HasPrefix(name, "Revoke") || name == "RunInstances" || name == "TagResources"
			if mutates && !conditioned[action] {
				t.Errorf("%s mutates and no statement allows it under a condition", action)
			}
			if !mutates && conditioned[action] && !strings.HasPrefix(name, "Describe") {
				t.Errorf("%s", action)
			}
		}
	}
	for a := range allowed {
		if !called[a] {
			t.Errorf("%s is allowed and the driver does not call it", a)
		}
	}
	if !slices.Contains(doc.Statement[0].Action, "ecs:DescribeAccountAttributes") {
		t.Error("the quota read")
	}
}
