/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws"
)

// withoutClusterTag lists, by name, the statements that cannot carry the cluster tag
// condition: what a launch or a security group only uses (no tag to compare), the
// KMS key, and PassRole.
var withoutClusterTag = map[string]bool{
	"RunInstancesUsedResources": true, "CreateSecurityGroupsInAVpc": true,
	"OptionalPassInstanceProfileRole": true, "OptionalCustomerKmsKey": true, "OptionalCustomerKmsKeyGrant": true,
}

type policyStatement struct {
	Sid       string                    `json:"Sid"`
	Effect    string                    `json:"Effect"`
	Action    []string                  `json:"Action"`
	Resource  any                       `json:"Resource"`
	Condition map[string]map[string]any `json:"Condition"`
}

// TestIAMPolicyAllowsEveryCallOfTheDriver ties the documented policy to the
// driver's narrow API interfaces: every method of EC2API and ServiceQuotasAPI
// must be allowed by a statement, and the policy allows no action the driver
// does not call except iam:PassRole (the optional instance profile) and the
// tagging of what it creates.
func TestIAMPolicyAllowsEveryCallOfTheDriver(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(testutil.RepoRoot(), "docs", "reference", "aws-iam-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Version   string            `json:"Version"`
		Statement []policyStatement `json:"Statement"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != "2012-10-17" {
		t.Errorf("policy version %q", doc.Version)
	}
	allowed := map[string]bool{}
	sids := map[string]bool{}
	for _, s := range doc.Statement {
		if s.Effect != "Allow" || s.Sid == "" || sids[s.Sid] {
			t.Errorf("statement %q: effect %q or a missing or repeated Sid", s.Sid, s.Effect)
		}
		sids[s.Sid] = true
		for _, a := range s.Action {
			if a == "*" || strings.HasSuffix(a, ":*") {
				t.Errorf("statement %s allows %s", s.Sid, a)
			}
			allowed[a] = true
		}
		// Everything that is not a read is limited by the cluster tag, except
		// what has to be allowed without one (the resources a launch only uses).
		if withoutClusterTag[s.Sid] {
			continue
		}
		write := slices.ContainsFunc(s.Action, func(a string) bool {
			return !strings.Contains(a, ":Describe") && !strings.HasPrefix(a, "servicequotas:")
		})
		if write && !strings.Contains(string(raw), "CLUSTER_NAME") {
			t.Errorf("statement %s: no cluster placeholder", s.Sid)
		}
		if write && len(s.Condition) == 0 {
			t.Errorf("statement %s allows writes without a condition", s.Sid)
		}
	}
	for _, tc := range []struct {
		prefix string
		typ    reflect.Type
	}{
		{"ec2:", reflect.TypeFor[aws.EC2API]()},
		{"servicequotas:", reflect.TypeFor[aws.ServiceQuotasAPI]()},
	} {
		for method := range tc.typ.Methods() {
			if action := tc.prefix + method.Name; !allowed[action] {
				t.Errorf("the policy does not allow %s, which the driver calls", action)
			}
		}
	}
	// The other way: no EC2 action in the policy that the driver cannot make
	// (tagging and the launch of instances aside).
	known := map[string]bool{
		"ec2:CreateTags": true, "iam:PassRole": true, "kms:CreateGrant": true, "kms:Decrypt": true, "kms:DescribeKey": true,
		"kms:GenerateDataKeyWithoutPlaintext": true,
	}
	for method := range reflect.TypeFor[aws.EC2API]().Methods() {
		known["ec2:"+method.Name] = true
	}
	for method := range reflect.TypeFor[aws.ServiceQuotasAPI]().Methods() {
		known["servicequotas:"+method.Name] = true
	}
	for a := range allowed {
		if !known[a] {
			t.Errorf("the policy allows %s, which the driver never calls", a)
		}
	}
}
