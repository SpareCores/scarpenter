//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package qemu

import (
	"errors"
	"slices"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

func ids(refs []vendors.ResourceRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.ID)
	}
	return out
}

func TestResolveFindsBridgesByIDNamePatternTagsAndDefaultAndNeverManagedOnes(t *testing.T) {
	d, h := newTestDriver(t)
	h.bridges["lab-a"] = "env=lab tier=1"
	h.bridges["lab-b"] = "env=lab"
	h.bridges["other"] = ""
	managed, _, err := d.Ensure(t.Context(), KindNetwork, Region, vendors.CreateSpec{
		Name: "scarpenter-x-networks-local", Tags: map[vendors.TagKey]string{vendors.TagCluster: "x", vendors.TagManagedBy: "scarpenter"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		ref  vendors.UseRef
		want []string
	}{
		"default":    {vendors.UseRef{}, []string{DefaultBridge}},
		"ids":        {vendors.UseRef{IDs: []string{"other", "nope"}}, []string{"other"}},
		"pattern":    {vendors.UseRef{Name: "lab-*", Select: common.SelectAll}, []string{"lab-a", "lab-b"}},
		"ambiguous":  {vendors.UseRef{Name: "lab-*"}, nil},
		"tags":       {vendors.UseRef{Tags: map[string]string{"env": "lab", "tier": "1"}}, []string{"lab-a"}},
		"no match":   {vendors.UseRef{Tags: map[string]string{"env": "prod"}}, nil},
		"managed id": {vendors.UseRef{IDs: []string{managed.ID}}, nil},
	} {
		var got []vendors.ResourceRef
		var err error
		if tc.ref.IsDefault() {
			got, err = d.Default(t.Context(), KindNetwork, Region)
		} else {
			got, err = d.Resolve(t.Context(), KindNetwork, Region, tc.ref)
		}
		_, none := errors.AsType[*selector.NoMatchError](err)
		_, many := errors.AsType[*selector.AmbiguousError](err)
		if (err != nil && !none && !many) || (many != (name == "ambiguous")) || (!slices.Equal(ids(got), tc.want) && (len(got) != 0 || len(tc.want) != 0)) {
			t.Errorf("%s: %v, %v; want %v", name, ids(got), err, tc.want)
		}
	}
	if got, _ := d.Default(t.Context(), KindNetwork, "elsewhere"); len(got) != 0 {
		t.Errorf("another region: %v", got)
	}
}

func TestTheOptInAliasIsDiscoveredAndTheDefaultBridgeIsTheDefault(t *testing.T) {
	d, h := newTestDriver(t)
	h.bridges["lab-a"] = d.TagKey(vendors.TagUse) + "=" + vendors.OptInValue
	h.bridges["lab-b"] = d.TagKey(vendors.TagUse) + "=false"
	// Discovery is the selector the core builds: the opt-in tag with the kind's default select.
	got, err := d.Resolve(t.Context(), KindNetwork, Region, vendors.UseRef{
		Tags: map[string]string{d.TagKey(vendors.TagUse): vendors.OptInValue}, Select: d.Selectors()[KindNetwork].DefaultSelect})
	if err != nil || len(got) != 1 || got[0].ID != "lab-a" {
		t.Errorf("discovery = %v, %v; want lab-a", ids(got), err)
	}
	def, err := d.Default(t.Context(), KindNetwork, Region)
	if err != nil || len(def) != 1 || def[0].ID != DefaultBridge {
		t.Errorf("default = %v, %v; want %s", ids(def), err, DefaultBridge)
	}
}

func TestEnsureRecreatesAManagedBridgeThatTheMachineLost(t *testing.T) {
	d, h := newTestDriver(t)
	spec := vendors.CreateSpec{Name: "scarpenter-x-networks-local", Tags: map[vendors.TagKey]string{vendors.TagCluster: "x"}}
	first, created, err := d.Ensure(t.Context(), KindNetwork, Region, spec)
	if err != nil || !created {
		t.Fatalf("%+v, %v, %v", first, created, err)
	}
	if _, ok := h.bridges[first.ID]; !ok || len(first.ID) > 15 {
		t.Fatalf("bridge %q (present: %v)", first.ID, ok)
	}
	delete(h.bridges, first.ID) // a reboot of the machine
	again, created, err := d.Ensure(t.Context(), KindNetwork, Region, spec)
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("%+v, %v, %v", again, created, err)
	}
	if _, ok := h.bridges[again.ID]; !ok {
		t.Error("the bridge was not created again")
	}
	other := spec
	other.Tags = map[vendors.TagKey]string{vendors.TagCluster: "someone-else"}
	if _, _, err := d.Ensure(t.Context(), KindNetwork, Region, other); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("a name owned by another cluster: %v, want Fatal", err)
	}
}

func TestInstancesAttachToTheNetworkOfTheirPreparedResources(t *testing.T) {
	d, h := newTestDriver(t)
	h.bridges["lab-a"] = ""
	req := createRequest("claim-1")
	req.Prepared = &vendors.Prepared{Resources: []vendors.ResourceRef{{Kind: KindNetwork, Region: Region, ID: "lab-a"}}}
	inst, err := d.CreateInstance(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if rec := record1(t, d, inst); h.taps[rec.Tap] != "lab-a" || rec.Bridge != "lab-a" {
		t.Errorf("the tap is on %q (record: %s)", h.taps[rec.Tap], rec.Bridge)
	}
}
