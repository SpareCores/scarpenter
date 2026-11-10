/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package facts_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr/funcr"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/facts"
)

// regionDriver has several regions whose zone listing can be made to fail.
type regionDriver struct {
	driver
	mu   sync.Mutex
	errs map[string]error
}

func (d *regionDriver) set(region string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.errs == nil {
		d.errs = map[string]error{}
	}
	if err == nil {
		delete(d.errs, region)
	} else {
		d.errs[region] = err
	}
}

func (d *regionDriver) Regions(context.Context) ([]vendors.Region, error) {
	return []vendors.Region{{Name: "af-south-1"}, {Name: "eu-west-1"}, {Name: "us-east-2"}}, nil
}

func (d *regionDriver) Zones(_ context.Context, region string) ([]vendors.Zone, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[region]; err != nil {
		return nil, err
	}
	return []vendors.Zone{{ID: region + "-az1", Name: region + "a"}}, nil
}

var (
	errDenied    = vendors.NewError(vendors.RegionUnavailable, "UnauthorizedOperation", errors.New("explicit deny"))
	errOptIn     = vendors.NewError(vendors.RegionUnavailable, "OptInRequired", errors.New("not opted in"))
	errThrottled = vendors.NewError(vendors.Retryable, "RequestLimitExceeded", errors.New("slow down"))
	errBroken    = vendors.NewError(vendors.Fatal, "InvalidParameterValue", errors.New("bad"))
)

// capture returns a context whose logger records the lines (verbosity 0 only).
func capture(t *testing.T) (context.Context, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var lines []string
	l := funcr.New(func(prefix, args string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, prefix+" "+args)
	}, funcr.Options{})
	return log.IntoContext(t.Context(), l), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(lines)
	}
}

func TestADeniedRegionIsSkippedAndTheOthersLoad(t *testing.T) {
	d := &regionDriver{}
	d.set("af-south-1", errDenied)
	f := facts.New(facts.Options{Registry: vendors.NewRegistryFrom(d)})
	ctx, lines := capture(t)
	if err := f.Load(ctx); err != nil {
		t.Fatalf("a denied region failed the load: %v", err)
	}
	if !f.Loaded() {
		t.Error("not loaded")
	}
	if got := f.Regions("aws"); !slices.Equal(got, []string{"eu-west-1", "us-east-2"}) {
		t.Errorf("regions %v", got)
	}
	zones, ok := f.Zones("aws")
	if !ok || len(zones["us-east-2"]) != 1 || len(zones["af-south-1"]) != 0 {
		t.Errorf("zones %v", zones)
	}
	if sk := f.SkippedRegions("aws"); len(sk) != 1 || !strings.Contains(sk["af-south-1"], "UnauthorizedOperation") {
		t.Errorf("skipped %v", sk)
	}
	if got := lines(); len(got) != 1 || !strings.Contains(got[0], "af-south-1") {
		t.Errorf("want one warning naming the region, got %q", got)
	}
	// Later loads do not warn again (V(1) only), also when the message differs
	// (a real API error carries a new request ID every time).
	for i := range 3 {
		d.set("af-south-1", vendors.NewError(vendors.RegionUnavailable, "UnauthorizedOperation",
			fmt.Errorf("explicit deny, RequestID: %d", i)))
		if err := f.Load(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := lines(); len(got) != 1 {
		t.Errorf("the warning repeated: %q", got)
	}
	// The region becomes readable: it is loaded and the recovery is logged.
	d.set("af-south-1", nil)
	if err := f.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.Regions("aws"); len(got) != 3 || len(f.SkippedRegions("aws")) != 0 {
		t.Errorf("regions %v, skipped %v", got, f.SkippedRegions("aws"))
	}
	if got := lines(); len(got) != 2 || !strings.Contains(got[1], "again") {
		t.Errorf("recovery not logged: %q", got)
	}
}

func TestAnOptInRegionIsSkipped(t *testing.T) {
	d := &regionDriver{}
	d.set("af-south-1", errOptIn)
	f := facts.New(facts.Options{Registry: vendors.NewRegistryFrom(d)})
	if err := f.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.Regions("aws"); len(got) != 2 || slices.Contains(got, "af-south-1") {
		t.Errorf("regions %v", got)
	}
}

func TestLoadFailsWhenNoRegionCanBeRead(t *testing.T) {
	d := &regionDriver{}
	for _, r := range []string{"af-south-1", "eu-west-1", "us-east-2"} {
		d.set(r, errDenied)
	}
	f := facts.New(facts.Options{Registry: vendors.NewRegistryFrom(d)})
	err := f.Load(t.Context())
	if err == nil || !strings.Contains(err.Error(), "no region could be read") {
		t.Fatalf("err %v", err)
	}
	if f.Loaded() {
		t.Error("loaded")
	}
}

func TestATransientRegionErrorKeepsTheLastKnownZones(t *testing.T) {
	d := &regionDriver{}
	f := facts.New(facts.Options{Registry: vendors.NewRegistryFrom(d)})
	if err := f.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	d.set("eu-west-1", errThrottled)
	err := f.Load(t.Context())
	if err == nil || !strings.Contains(err.Error(), "eu-west-1") {
		t.Fatalf("a transient error must still be reported so that the load is retried: %v", err)
	}
	zones, _ := f.Zones("aws")
	if len(zones["eu-west-1"]) != 1 || len(f.Regions("aws")) != 3 {
		t.Errorf("the last-known zones were dropped: %v", zones)
	}
	if !f.Loaded() {
		t.Error("a transient region error must not unload the facts")
	}
}

func TestATransientErrorWithoutLastKnownZonesLeavesTheRegionOut(t *testing.T) {
	d := &regionDriver{}
	d.set("eu-west-1", errBroken)
	f := facts.New(facts.Options{Registry: vendors.NewRegistryFrom(d)})
	if err := f.Load(t.Context()); err == nil {
		t.Fatal("the failure was not reported")
	}
	if got := f.Regions("aws"); !slices.Equal(got, []string{"af-south-1", "us-east-2"}) {
		t.Errorf("regions %v: the other regions must load", got)
	}
}
