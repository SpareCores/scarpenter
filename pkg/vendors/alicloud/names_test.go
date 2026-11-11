/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func TestProviderID(t *testing.T) {
	id := ProviderID("ap-southeast-1", "default-abc12")
	if id != "scarpenter://alicloud/ap-southeast-1/default-abc12" {
		t.Fatalf("id = %q", id)
	}
	region, claim, err := ParseProviderID(id)
	if err != nil || region != "ap-southeast-1" || claim != "default-abc12" {
		t.Fatalf("parse = %q %q %v", region, claim, err)
	}
	for _, bad := range []string{
		"", "alicloud/ap-southeast-1/x", "scarpenter://aws/us-east-1/x", "scarpenter://alicloud/ap-southeast-1",
		"scarpenter://alicloud//x", "scarpenter://alicloud/a/b/c",
	} {
		if _, _, err := ParseProviderID(bad); err == nil {
			t.Errorf("ParseProviderID(%q) did not fail", bad)
		}
	}
}

// ECS tag keys: 1 to 128 characters, no aliyun or acs: prefix, no http:// or
// https:// inside (plan 21 section 3).
func TestTagKeysAreValidAndInvertible(t *testing.T) {
	seen := map[string]bool{}
	for _, k := range vendors.TagKeys {
		key := TagKey(k)
		if len(key) > 128 || strings.HasPrefix(key, "aliyun") || strings.HasPrefix(key, "acs:") || strings.Contains(key, "http") {
			t.Errorf("%s: %q is not a valid ECS tag key", k, key)
		}
		if seen[key] {
			t.Errorf("%q is used twice", key)
		}
		seen[key] = true
		if back, ok := logicalKey(key); !ok || back != k {
			t.Errorf("logicalKey(%q) = %q, %v", key, back, ok)
		}
	}
	if _, ok := logicalKey("team"); ok {
		t.Error("a NodeClass tag is not a logical key")
	}
}

func TestHostName(t *testing.T) {
	short := "default-abc12"
	if HostName(short) != short {
		t.Fatalf("a short name is kept: %q", HostName(short))
	}
	long := strings.Repeat("a", 40) + "." + strings.Repeat("b", 60)
	h := HostName(long)
	if len(h) > 64 || len(h) < 2 || !strings.HasPrefix(h, strings.Repeat("a", 40)) {
		t.Fatalf("host name %q (%d)", h, len(h))
	}
	if !regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`).MatchString(h) {
		t.Fatalf("host name %q is not valid", h)
	}
	if HostName(long) != h || HostName(long+"x") == h {
		t.Fatal("the shortened name must be deterministic and depend on the whole name")
	}
	// The cut must not leave a dot or a dash before the hash.
	dotted := strings.Repeat("a", 54) + "." + strings.Repeat("b", 30)
	if got := HostName(dotted); strings.Contains(got, ".-") || strings.Contains(got, "--") {
		t.Fatalf("bad cut in %q", got)
	}
	if _, err := InstanceName("x"); err == nil {
		t.Error("an instance name of one character must fail")
	}
	if _, err := InstanceName(strings.Repeat("a", 129)); err == nil {
		t.Error("an instance name of 129 characters must fail")
	}
	if n, err := InstanceName(long); err != nil || n != long {
		t.Errorf("InstanceName(long) = %q, %v", n, err)
	}
}

func TestClientToken(t *testing.T) {
	base := func() string {
		return ClientToken("uid", "ap-southeast-1", "ap-southeast-1a", "ecs.g8y.large", "spot", "m-1", "vsw-1", 0)
	}
	tok := base()
	if len(tok) != 64 || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(tok) {
		t.Fatalf("token %q is not 64 hex characters", tok)
	}
	if base() != tok {
		t.Fatal("the token must be deterministic")
	}
	variants := []string{
		ClientToken("uid2", "ap-southeast-1", "ap-southeast-1a", "ecs.g8y.large", "spot", "m-1", "vsw-1", 0),
		ClientToken("uid", "eu-central-1", "ap-southeast-1a", "ecs.g8y.large", "spot", "m-1", "vsw-1", 0),
		ClientToken("uid", "ap-southeast-1", "ap-southeast-1b", "ecs.g8y.large", "spot", "m-1", "vsw-1", 0),
		ClientToken("uid", "ap-southeast-1", "ap-southeast-1a", "ecs.g8y.xlarge", "spot", "m-1", "vsw-1", 0),
		ClientToken("uid", "ap-southeast-1", "ap-southeast-1a", "ecs.g8y.large", "on-demand", "m-1", "vsw-1", 0),
		ClientToken("uid", "ap-southeast-1", "ap-southeast-1a", "ecs.g8y.large", "spot", "m-2", "vsw-1", 0),
		ClientToken("uid", "ap-southeast-1", "ap-southeast-1a", "ecs.g8y.large", "spot", "m-1", "vsw-2", 0),
		ClientToken("uid", "ap-southeast-1", "ap-southeast-1a", "ecs.g8y.large", "spot", "m-1", "vsw-1", 1),
		// Field boundaries are part of the hash: moving a character across them differs.
		ClientToken("uid", "ap-southeast-1a", "ap-southeast-1", "ecs.g8y.large", "spot", "m-1", "vsw-1", 0),
	}
	seen := map[string]bool{tok: true}
	for i, v := range variants {
		if seen[v] {
			t.Errorf("variant %d collides", i)
		}
		seen[v] = true
	}
}

func TestStateOf(t *testing.T) {
	cases := map[string]string{
		"Pending": vendors.StatePending, "Starting": vendors.StatePending,
		"Running":  vendors.StateRunning,
		"Stopping": vendors.StateTerminating, "Stopped": vendors.StateTerminating,
		"": vendors.StatePending, "SomethingNew": vendors.StatePending,
	}
	for status, want := range cases {
		if got := StateOf(status); got != want {
			t.Errorf("StateOf(%q) = %s, want %s", status, got, want)
		}
	}
}

func TestParseTime(t *testing.T) {
	want := time.Date(2026, 11, 10, 8, 30, 0, 0, time.UTC)
	for _, s := range []string{"2026-11-10T08:30Z", "2026-11-10T08:30:00Z", "2026-11-10T08:30:00+00:00"} {
		if got := parseTime(s); !got.Equal(want) {
			t.Errorf("parseTime(%q) = %v", s, got)
		}
	}
	if !parseTime("yesterday").IsZero() || !parseTime("").IsZero() {
		t.Error("an unreadable time must be zero")
	}
}
