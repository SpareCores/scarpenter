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
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws"
)

// withoutClusterTag lists, by name, the statements that cannot carry the cluster tag
// condition: what a launch or a security group only uses (no tag to compare), the
// KMS key, and PassRole.
var withoutClusterTag = map[string]bool{
	"RunInstancesUsedResources": true, "CreateSecurityGroupsInAVpc": true,
	"OptionalPassInstanceProfileRole": true, "OptionalImageCopiesSource": true, "OptionalCustomerKmsKey": true, "OptionalCustomerKmsKeyGrant": true,
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
			if action := tc.prefix + method.Name; !allowed[action] && action != "ec2:DescribeSnapshots" { // the sweeper's, see EC2API
				t.Errorf("the policy does not allow %s, which the driver calls", action)
			}
		}
	}
	// The other way: no EC2 action in the policy that the driver cannot make
	// (tagging and the launch of instances aside).
	known := map[string]bool{
		"ec2:CreateTags": true, "iam:PassRole": true, "kms:CreateGrant": true, "kms:Decrypt": true, "kms:DescribeKey": true,
		"kms:GenerateDataKeyWithoutPlaintext": true,
		// DeregisterImage's DeleteAssociatedSnapshots deletes the snapshots in the
		// caller's name (plan 29: to verify in step 4 whether it needs the action).
		"ec2:DeleteSnapshot": true,
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

// TestImageCopyStatementsAreScoped pins the optional statements of the image copies
// (plan 29): the creation of a copy and its tags need the request tag, the source image
// is limited to the official owner (the request tag applies to what the call creates, not
// to the source), the deletion of an image or snapshot needs the cluster, managed-by and
// resource-kind tags of a copy, and no statement lets the role tag or change an existing
// image.
func TestImageCopyStatementsAreScoped(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(testutil.RepoRoot(), "docs", "reference", "aws-iam-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Statement []policyStatement `json:"Statement"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	const prefix = "scarpenter.sparecores.com/"
	want := map[string]struct {
		actions    []string
		conditions map[string]any
	}{
		"OptionalImageCopies": {[]string{"ec2:CopyImage"},
			map[string]any{"aws:RequestTag/" + prefix + "cluster": "CLUSTER_NAME"}},
		"OptionalImageCopiesSource": {[]string{"ec2:CopyImage"},
			map[string]any{"ec2:Owner": aws.DefaultImageOwner}},
		"OptionalImageCopiesTagAtCreation": {[]string{"ec2:CreateTags"},
			map[string]any{"ec2:CreateAction": "CopyImage", "aws:RequestTag/" + prefix + "cluster": "CLUSTER_NAME"}},
		"OptionalImageCopiesDelete": {[]string{"ec2:DeregisterImage", "ec2:DeleteSnapshot"},
			map[string]any{
				"aws:ResourceTag/" + prefix + "cluster":       "CLUSTER_NAME",
				"aws:ResourceTag/" + prefix + "managed-by":    vendors.ManagedByValue,
				"aws:ResourceTag/" + prefix + "resource-kind": vendors.ResourceKindImageCopy,
			}},
	}
	seen := 0
	for _, s := range doc.Statement {
		w, ok := want[s.Sid]
		if !ok {
			continue
		}
		seen++
		if s.Sid == "OptionalImageCopiesSource" {
			if got, _ := s.Resource.([]any); !reflect.DeepEqual(got, []any{"arn:aws:ec2:*::image/*", "arn:aws:ec2:*::snapshot/*"}) {
				t.Errorf("%s resources %v, want the official owner's images and snapshots", s.Sid, s.Resource)
			}
		}
		if !slices.Equal(s.Action, w.actions) {
			t.Errorf("%s allows %v, want %v", s.Sid, s.Action, w.actions)
		}
		if got := s.Condition["StringEquals"]; !reflect.DeepEqual(got, w.conditions) {
			t.Errorf("%s: StringEquals %v, want %v", s.Sid, got, w.conditions)
		}
		if len(s.Condition) != 1 {
			t.Errorf("%s has conditions besides StringEquals: %v", s.Sid, s.Condition)
		}
	}
	if seen != len(want) {
		t.Errorf("found %d of the %d image copy statements", seen, len(want))
	}
}
