/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func TestTagKeysFitLabelKeys(t *testing.T) {
	for _, k := range vendors.TagKeys {
		key := TagKey(k)
		if err := ValidateLabelKey(key); err != nil {
			t.Errorf("%s: %v", k, err)
		}
		if tagByLabel[key] != k {
			t.Errorf("%s does not map back", key)
		}
	}
	// The default key (scarpenter.sparecores.com/idempotency-key) is the reason for the encoding.
	if got := len(vendors.DefaultTagKey(vendors.TagIdempotencyKey)); got <= MaxLabelKey {
		t.Errorf("the default tag key has %d characters: the encoding would be unnecessary", got)
	}
	if got := TagKey(vendors.TagIdempotencyKey); got != "scarpenter-idempotency-key" || len(got) != 26 {
		t.Errorf("idempotency label = %q (%d)", got, len(got))
	}
}

func TestValidateLabelKey(t *testing.T) {
	for key, ok := range map[string]bool{
		"ab": true, "a": false, "": false, strings.Repeat("k", 32): true, strings.Repeat("k", 33): false,
		"_hidden": false, "has space": false, "ünï": false, "env": true, "team.name": true,
	} {
		if err := ValidateLabelKey(key); (err == nil) != ok {
			t.Errorf("%q: err = %v, want ok=%v", key, err, ok)
		}
	}
}

func TestValidateExtraTags(t *testing.T) {
	if err := ValidateExtraTags(map[string]string{"team": "x"}); err != nil {
		t.Error(err)
	}
	for name, tags := range map[string]map[string]string{
		"long key":        {strings.Repeat("k", 33): "v"},
		"underscore":      {"_k": "v"},
		"reserved prefix": {"scarpenter-cluster": "v"},
		"long value":      {"team": strings.Repeat("v", 256)},
	} {
		if ValidateExtraTags(tags) == nil {
			t.Errorf("%s accepted", name)
		}
	}
	many := map[string]string{}
	for i := range MaxLabels {
		many[string(rune('a'+i%26))+string(rune('a'+i/26%26))+string(rune('a'+i/676))] = "v"
	}
	if ValidateExtraTags(many) == nil {
		t.Error("255 extra tags accepted: no room for the driver's own labels")
	}
}

func TestServerLabels(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 30, 0, 0, time.FixedZone("x", 3600))
	tags := map[vendors.TagKey]string{
		vendors.TagCluster: "c1", vendors.TagNodeClaim: "nc-1", vendors.TagManagedBy: vendors.ManagedByValue,
	}
	got, err := ServerLabels(tags, map[string]string{"team": "ml"}, "uid-1", now)
	if err != nil {
		t.Fatal(err)
	}
	want := []upcloud.Label{
		{Key: "scarpenter-cluster", Value: "c1"},
		{Key: "scarpenter-created", Value: "2026-11-11T09:30:00Z"},
		{Key: "scarpenter-idempotency-key", Value: "uid-1"},
		{Key: "scarpenter-managed-by", Value: "scarpenter"},
		{Key: "scarpenter-nodeclaim", Value: "nc-1"},
		{Key: "team", Value: "ml"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("labels = %v\nwant %v", got, want)
	}
	back := TagsOf(got)
	if back[vendors.TagIdempotencyKey] != "uid-1" || back[vendors.TagCluster] != "c1" || len(back) != 4 {
		t.Errorf("tags back = %v", back)
	}
	if ts, ok := Created(got); !ok || !ts.Equal(now) {
		t.Errorf("created = %v %v", ts, ok)
	}
	if _, err := ServerLabels(tags, map[string]string{"_x": "v"}, "", now); err == nil {
		t.Error("an invalid extra tag was accepted")
	}
	noKey, _ := ServerLabels(tags, nil, "", now)
	if _, ok := TagsOf(noKey)[vendors.TagIdempotencyKey]; ok {
		t.Error("an empty idempotency key was written")
	}
}

func TestCreatedFormats(t *testing.T) {
	for name, tc := range map[string]struct {
		value string
		ok    bool
		want  time.Time
	}{
		"rfc3339": {"2026-11-11T09:30:00Z", true, time.Date(2026, 11, 11, 9, 30, 0, 0, time.UTC)},
		"epoch":   {"1794389400", true, time.Unix(1794389400, 0).UTC()},
		"junk":    {"yesterday", false, time.Time{}},
	} {
		got, ok := Created([]upcloud.Label{{Key: LabelCreated, Value: tc.value}})
		if ok != tc.ok || !got.Equal(tc.want) {
			t.Errorf("%s: %v %v", name, got, ok)
		}
	}
	if _, ok := Created(nil); ok {
		t.Error("no label gave a time")
	}
}

func TestProviderID(t *testing.T) {
	id := ProviderID("fi-hel1", "nc-abc")
	if id != "scarpenter://upcloud/fi-hel1/nc-abc" {
		t.Fatal(id)
	}
	zone, nc, err := ParseProviderID(id)
	if err != nil || zone != "fi-hel1" || nc != "nc-abc" {
		t.Fatalf("%q %q %v", zone, nc, err)
	}
	for _, bad := range []string{"", "aws:///x", "scarpenter://aws/us-east-1/nc", "scarpenter://upcloud/fi-hel1", "scarpenter://upcloud//nc"} {
		if _, _, err := ParseProviderID(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestInstanceRef(t *testing.T) {
	const id = "00a1b2c3-0000-4000-8000-000000000001"
	zone, uuid, ok := ParseRef(NewRef("de-fra1", id))
	if !ok || zone != "de-fra1" || uuid != id {
		t.Fatalf("%q %q %v", zone, uuid, ok)
	}
	for _, bad := range []vendors.InstanceRef{"", "de-fra1", "de-fra1/", "/" + id, "de-fra1/i-never-existed", "de-fra1/" + id + "x"} {
		if _, _, ok := ParseRef(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestInstanceState(t *testing.T) {
	for state, want := range map[string]string{
		"started": vendors.StateRunning, "maintenance": vendors.StatePending,
		"stopped": vendors.StateTerminating, "error": vendors.StateTerminating, "new-state": vendors.StatePending,
	} {
		if got := InstanceState(state); got != want {
			t.Errorf("%s -> %s, want %s", state, got, want)
		}
	}
}

func TestToInstanceAndAddresses(t *testing.T) {
	d := &upcloud.ServerDetails{
		Server: upcloud.Server{UUID: "u", Zone: "fi-hel1", Plan: "1xCPU-2GB", State: "started"},
		Labels: []upcloud.Label{
			{Key: "scarpenter-cluster", Value: "c1"}, {Key: LabelCreated, Value: "2026-11-11T09:30:00Z"}, {Key: "x", Value: "y"},
		},
		IPAddresses: upcloud.IPAddressSlice{
			{Access: "private", Address: "172.16.0.5"}, {Access: "utility", Address: "10.1.0.5"},
			{Access: "public", Address: "2001:db8::5"}, {Access: "public", Address: "203.0.113.5"},
		},
	}
	in := ToInstance(d)
	if in.Ref != "fi-hel1/u" || in.Region != "fi-hel1" || in.Zone != "fi-hel1" || in.Type != "1xCPU-2GB" ||
		in.State != vendors.StateRunning || in.CapacityType != vendors.CapacityOnDemand {
		t.Errorf("instance = %+v", in)
	}
	if !in.CreatedAt.Equal(time.Date(2026, 11, 11, 9, 30, 0, 0, time.UTC)) || in.Tags[vendors.TagCluster] != "c1" {
		t.Errorf("created/tags = %v %v", in.CreatedAt, in.Tags)
	}
	want := []string{"2001:db8::5", "203.0.113.5", "10.1.0.5", "172.16.0.5"}
	if !slices.Equal(in.Addresses, want) {
		t.Errorf("addresses = %v, want %v", in.Addresses, want)
	}
	if _, ok := BillingStart(&upcloud.ServerDetails{}); ok {
		t.Error("a server without the label has a billing start")
	}
}

func TestValidateName(t *testing.T) {
	for name, ok := range map[string]bool{"nc-abc": true, "": false, "NC": false, strings.Repeat("a", 128): true, strings.Repeat("a", 129): false} {
		if err := ValidateName(name); (err == nil) != ok {
			t.Errorf("%q: %v", name, err)
		}
	}
	if z := Zone("fi-hel1"); z.ID != "fi-hel1" || z.Name != "fi-hel1" {
		t.Errorf("zone = %+v", z)
	}
}
