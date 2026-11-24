/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package unavailable_test

import (
	"testing"
	"time"

	clocktesting "k8s.io/utils/clock/testing"

	"github.com/SpareCores/scarpenter/pkg/unavailable"
)

var base = unavailable.Offering{Vendor: "aws", InstanceType: "aws.m7i.large", Region: "us-east-1", Zone: "us-east-1a", CapacityType: "spot"}

func with(o unavailable.Offering, f func(*unavailable.Offering)) unavailable.Offering {
	f(&o)
	return o
}

func TestKeysAndTTLs(t *testing.T) {
	cases := []struct {
		name string
		mark func(c *unavailable.Cache)
		ttl  time.Duration
		// covered are offerings the entry makes unavailable, free are some it does not.
		covered, free []unavailable.Offering
	}{
		{
			name:    "insufficient capacity: (vendor, type, zone, capacity type), 3 minutes",
			mark:    func(c *unavailable.Cache) { c.MarkInsufficientCapacity("aws", "aws.m7i.large", "us-east-1a", "spot") },
			ttl:     3 * time.Minute,
			covered: []unavailable.Offering{base},
			free: []unavailable.Offering{
				with(base, func(o *unavailable.Offering) { o.CapacityType = "on-demand" }),
				with(base, func(o *unavailable.Offering) { o.Zone = "us-east-1b" }),
				with(base, func(o *unavailable.Offering) { o.InstanceType = "aws.m7i.xlarge" }),
				with(base, func(o *unavailable.Offering) { o.Vendor = "gcp" }),
			},
		},
		{
			name:    "interrupted: (vendor, type, zone, capacity type), 10 minutes",
			mark:    func(c *unavailable.Cache) { c.MarkInterrupted("aws", "aws.m7i.large", "us-east-1a", "spot") },
			ttl:     10 * time.Minute,
			covered: []unavailable.Offering{base},
			free: []unavailable.Offering{
				with(base, func(o *unavailable.Offering) { o.CapacityType = "on-demand" }),
				with(base, func(o *unavailable.Offering) { o.Zone = "us-east-1b" }),
				with(base, func(o *unavailable.Offering) { o.InstanceType = "aws.m7i.xlarge" }),
				with(base, func(o *unavailable.Offering) { o.Vendor = "gcp" }),
			},
		},
		{
			name:    "invalid: (vendor, type, zone, capacity type), 1 hour",
			mark:    func(c *unavailable.Cache) { c.MarkInvalid("aws", "aws.m7i.large", "us-east-1a", "spot") },
			ttl:     time.Hour,
			covered: []unavailable.Offering{base},
			free: []unavailable.Offering{
				with(base, func(o *unavailable.Offering) { o.CapacityType = "on-demand" }),
				with(base, func(o *unavailable.Offering) { o.Zone = "us-east-1b" }),
				with(base, func(o *unavailable.Offering) { o.InstanceType = "aws.m7i.xlarge" }),
			},
		},
		{
			name:    "unsupported: (vendor, type, zone) for both capacity types, 24 hours",
			mark:    func(c *unavailable.Cache) { c.MarkUnsupported("aws", "aws.m7i.large", "us-east-1a") },
			ttl:     24 * time.Hour,
			covered: []unavailable.Offering{base, with(base, func(o *unavailable.Offering) { o.CapacityType = "on-demand" })},
			free: []unavailable.Offering{
				with(base, func(o *unavailable.Offering) { o.Zone = "us-east-1b" }),
				with(base, func(o *unavailable.Offering) { o.InstanceType = "aws.c7g.large" }),
			},
		},
		{
			name: "quota exceeded without a quota API: (vendor, region, capacity type), 5 minutes",
			mark: func(c *unavailable.Cache) { c.MarkQuotaExceeded("aws", "us-east-1", "spot") },
			ttl:  5 * time.Minute,
			covered: []unavailable.Offering{
				base, with(base, func(o *unavailable.Offering) { o.InstanceType, o.Zone = "aws.c7g.large", "us-east-1c" }),
			},
			free: []unavailable.Offering{
				with(base, func(o *unavailable.Offering) { o.CapacityType = "on-demand" }),
				with(base, func(o *unavailable.Offering) { o.Region = "eu-west-1" }),
			},
		},
		{
			name: "account-wide quota exceeded: (vendor, capacity type) in every region, 5 minutes",
			mark: func(c *unavailable.Cache) { c.MarkQuotaExceededAccountWide("aws", "spot") },
			ttl:  5 * time.Minute,
			covered: []unavailable.Offering{
				base, with(base, func(o *unavailable.Offering) { o.Region, o.Zone = "eu-west-1", "eu-west-1a" }),
			},
			free: []unavailable.Offering{
				with(base, func(o *unavailable.Offering) { o.CapacityType = "on-demand" }),
				with(base, func(o *unavailable.Offering) { o.Vendor = "gcp" }),
			},
		},
		{
			name:    "region unreachable: (vendor, region), 3 minutes",
			mark:    func(c *unavailable.Cache) { c.MarkRegionUnreachable("aws", "us-east-1") },
			ttl:     3 * time.Minute,
			covered: []unavailable.Offering{base, with(base, func(o *unavailable.Offering) { o.Zone, o.InstanceType = "us-east-1b", "aws.c7g.large" })},
			free: []unavailable.Offering{
				with(base, func(o *unavailable.Offering) { o.Region = "us-east-2" }),
				with(base, func(o *unavailable.Offering) { o.Vendor = "gcp" }),
			},
		},
		{
			name:    "zone unusable: (vendor, zone), 10 minutes",
			mark:    func(c *unavailable.Cache) { c.MarkZoneUnusable("aws", "us-east-1a") },
			ttl:     10 * time.Minute,
			covered: []unavailable.Offering{base, with(base, func(o *unavailable.Offering) { o.CapacityType, o.InstanceType = "on-demand", "aws.c7g.large" })},
			free: []unavailable.Offering{
				with(base, func(o *unavailable.Offering) { o.Zone = "us-east-1b" }),
				with(base, func(o *unavailable.Offering) { o.Vendor = "gcp" }),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clk := clocktesting.NewFakeClock(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
			c := unavailable.New(clk, unavailable.TTLs{})
			before := c.Sequence()
			for _, o := range append(tc.covered, tc.free...) {
				if c.IsUnavailable(o) {
					t.Fatalf("%+v unavailable in an empty cache", o)
				}
			}
			tc.mark(c)
			if c.Sequence() == before {
				t.Error("marking did not change the sequence")
			}
			for _, o := range tc.covered {
				if !c.IsUnavailable(o) {
					t.Errorf("%+v should be unavailable", o)
				}
			}
			for _, o := range tc.free {
				if c.IsUnavailable(o) {
					t.Errorf("%+v should be available", o)
				}
			}

			clk.Step(tc.ttl - time.Second)
			if !c.IsUnavailable(tc.covered[0]) {
				t.Error("expired before the TTL")
			}
			seq := c.Sequence()
			clk.Step(time.Second)
			if c.IsUnavailable(tc.covered[0]) {
				t.Error("still unavailable after the TTL")
			}
			if c.Sequence() == seq {
				t.Error("an expiry did not change the sequence")
			}
			if c.Len() != 0 {
				t.Errorf("%d entries left after the TTL", c.Len())
			}
		})
	}
}

func TestSequenceIsStableWithoutChanges(t *testing.T) {
	clk := clocktesting.NewFakeClock(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	c := unavailable.New(clk, unavailable.TTLs{})
	c.MarkZoneUnusable("aws", "z")
	s := c.Sequence()
	clk.Step(time.Minute)
	if c.Sequence() != s {
		t.Error("the sequence changed without a change")
	}
}

func TestMarkingAgainRestartsTheTTL(t *testing.T) {
	clk := clocktesting.NewFakeClock(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	c := unavailable.New(clk, unavailable.TTLs{})
	c.MarkInsufficientCapacity("aws", "t", "z", "spot")
	clk.Step(2 * time.Minute)
	c.MarkInsufficientCapacity("aws", "t", "z", "spot")
	clk.Step(2 * time.Minute)
	if !c.IsUnavailable(unavailable.Offering{Vendor: "aws", InstanceType: "t", Zone: "z", CapacityType: "spot"}) {
		t.Error("the second mark did not extend the entry")
	}
}

func TestCustomTTLs(t *testing.T) {
	clk := clocktesting.NewFakeClock(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	c := unavailable.New(clk, unavailable.TTLs{InsufficientCapacity: time.Second})
	c.MarkInsufficientCapacity("aws", "t", "z", "spot")
	clk.Step(time.Second)
	if c.IsUnavailable(unavailable.Offering{Vendor: "aws", InstanceType: "t", Zone: "z", CapacityType: "spot"}) {
		t.Error("custom TTL ignored")
	}
}

func TestInterruptedTTLIsConfigurable(t *testing.T) {
	clk := clocktesting.NewFakeClock(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	c := unavailable.New(clk, unavailable.TTLs{Interrupted: time.Minute})
	c.MarkInterrupted("aws", "aws.m7i.large", "us-east-1a", "spot")
	clk.Step(59 * time.Second)
	if !c.IsUnavailable(base) {
		t.Fatal("expired before the configured TTL")
	}
	clk.Step(time.Second)
	if c.IsUnavailable(base) {
		t.Fatal("still unavailable after the configured TTL")
	}
}

func TestWhyNamesTheClassAndExpiryAndPreLaunchMarksAreShort(t *testing.T) {
	clk := clocktesting.NewFakeClock(time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC))
	c := unavailable.New(clk, unavailable.TTLs{})
	o := unavailable.Offering{Vendor: "v", InstanceType: "v.t", Region: "r", Zone: "z", CapacityType: "on-demand"}
	if _, _, ok := c.Why(o); ok {
		t.Fatal("an empty cache has a reason")
	}
	c.MarkUnsupported("v", "v.t", "z")
	class, expiry, ok := c.Why(o)
	if !ok || class != "Unsupported" || !expiry.Equal(clk.Now().Add(unavailable.DefaultUnsupportedTTL)) {
		t.Errorf("Why = %q, %v, %v", class, expiry, ok)
	}
	// The driver's own check keeps its mark for the zone-unusable TTL.
	c.MarkUnsupportedPreLaunch("v", "v.t", "z")
	if _, expiry, _ := c.Why(o); !expiry.Equal(clk.Now().Add(unavailable.DefaultZoneUnusableTTL)) {
		t.Errorf("a pre-launch mark expires at %v", expiry)
	}
	clk.Step(unavailable.DefaultZoneUnusableTTL)
	if c.IsUnavailable(o) {
		t.Error("a pre-launch mark outlives its TTL")
	}
	// Every class has a name, for the reason's detail.
	for mark, do := range map[string]func(){
		"InsufficientCapacity": func() { c.MarkInsufficientCapacity("v", "v.t", "z", "on-demand") },
		"Interrupted":          func() { c.MarkInterrupted("v", "v.t", "z", "on-demand") },
		"InvalidOffering":      func() { c.MarkInvalid("v", "v.t", "z", "on-demand") },
		"QuotaExceeded":        func() { c.MarkQuotaExceeded("v", "r", "on-demand") },
		"ZoneUnusable":         func() { c.MarkZoneUnusable("v", "z") },
	} {
		fresh := unavailable.New(clk, unavailable.TTLs{})
		c = fresh
		do()
		if class, _, ok := c.Why(o); !ok || class != mark {
			t.Errorf("Why = %q, %v, want %q", class, ok, mark)
		}
	}
}

func TestQuotaBucketMarkExpiresAfterFiveMinutesAndIsClearedByADelete(t *testing.T) {
	clk := clocktesting.NewFakeClock(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	c := unavailable.New(clk, unavailable.TTLs{})
	c.SetBucketsOf(func(_, apiReference, _ string) []string {
		if apiReference == "m7i.large" || apiReference == "m7i.xlarge" {
			return []string{"m-family", "all-vcpus"}
		}
		return []string{"c-family", "all-vcpus"}
	})
	m := with(base, func(o *unavailable.Offering) { o.APIReference = "m7i.large" })
	m2 := with(m, func(o *unavailable.Offering) { o.APIReference, o.CapacityType = "m7i.xlarge", "on-demand" })
	other := with(base, func(o *unavailable.Offering) { o.APIReference = "c7g.large" })
	otherRegion := with(m, func(o *unavailable.Offering) { o.Region = "eu-west-1" })

	c.MarkQuotaBucket("aws", "us-east-1", "m-family")
	if !c.IsUnavailable(m) || !c.IsUnavailable(m2) {
		t.Error("the types of the bucket are still available")
	}
	if c.IsUnavailable(other) || c.IsUnavailable(otherRegion) {
		t.Error("an offering outside the bucket and region is unavailable")
	}
	clk.Step(5*time.Minute - time.Second)
	if !c.IsUnavailable(m) {
		t.Error("the bucket mark expired before 5 minutes")
	}
	clk.Step(time.Second)
	if c.IsUnavailable(m) {
		t.Error("the bucket mark outlived 5 minutes")
	}

	c.MarkQuotaBucket("aws", "us-east-1", "m-family")
	c.MarkQuotaExceeded("aws", "us-east-1", "spot")
	c.MarkQuotaExceededAccountWide("aws", "on-demand")
	c.MarkDailyLimit("aws", "us-east-1", clk.Now().Add(time.Hour))
	c.ClearQuotaExceeded("aws", "us-east-1")
	if cls, _, ok := c.Why(m2); !ok || cls != "DailyLimit" {
		t.Errorf("Why = %q, %v after the clear, want only the daily limit to stay", cls, ok)
	}
	if c.Len() != 1 {
		t.Errorf("%d entries after the clear, want only the daily limit", c.Len())
	}
	// Another region keeps its marks.
	c.MarkQuotaBucket("aws", "eu-west-1", "m-family")
	c.ClearQuotaExceeded("aws", "us-east-1")
	if !c.IsUnavailable(otherRegion) {
		t.Error("a delete in us-east-1 cleared the mark of eu-west-1")
	}
}

func TestDailyLimitCoversTheRegionUntilTheResetTime(t *testing.T) {
	clk := clocktesting.NewFakeClock(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	c := unavailable.New(clk, unavailable.TTLs{})
	reset := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	c.MarkDailyLimit("aws", "us-east-1", reset)
	for _, o := range []unavailable.Offering{
		base, with(base, func(o *unavailable.Offering) {
			o.CapacityType, o.Zone, o.InstanceType = "on-demand", "us-east-1c", "aws.c7g.large"
		}),
	} {
		cls, expiry, ok := c.Why(o)
		if !ok || cls != "DailyLimit" || !expiry.Equal(reset) {
			t.Errorf("Why(%+v) = %q, %v, %v", o, cls, expiry, ok)
		}
	}
	if c.IsUnavailable(with(base, func(o *unavailable.Offering) { o.Region = "eu-west-1" })) {
		t.Error("another region is unavailable")
	}
	clk.Step(12*time.Hour - time.Second)
	if !c.IsUnavailable(base) {
		t.Error("the mark ended before the reset")
	}
	clk.Step(time.Second)
	if c.IsUnavailable(base) {
		t.Error("the mark outlived the reset")
	}
}

func TestRegionInsufficientCapacityCoversEveryZoneOfTheRegion(t *testing.T) {
	c := unavailable.New(clocktesting.NewFakeClock(time.Now()), unavailable.TTLs{})
	c.MarkInsufficientCapacityRegion("aws", "aws.m7i.large", "us-east-1", "spot")
	for _, zone := range []string{"us-east-1a", "us-east-1b"} {
		if !c.IsUnavailable(with(base, func(o *unavailable.Offering) { o.Zone = zone })) {
			t.Errorf("zone %s is not unavailable", zone)
		}
	}
	for name, o := range map[string]unavailable.Offering{
		"other region":        with(base, func(o *unavailable.Offering) { o.Region = "us-east-2" }),
		"other capacity type": with(base, func(o *unavailable.Offering) { o.CapacityType = "on-demand" }),
		"other type":          with(base, func(o *unavailable.Offering) { o.InstanceType = "aws.m7i.xlarge" }),
	} {
		if c.IsUnavailable(o) {
			t.Errorf("%s is unavailable", name)
		}
	}
}

// A class mark outlives the shared one for its class only, extends but never
// shortens, and is dropped with the class.
func TestClassInsufficientCapacityMarkIsSeenByItsClassOnly(t *testing.T) {
	clk := clocktesting.NewFakeClock(time.Now())
	c := unavailable.New(clk, unavailable.TTLs{})
	forClass := with(base, func(o *unavailable.Offering) { o.NodeClass = "bench" })
	c.MarkInsufficientCapacity("aws", "aws.m7i.large", "us-east-1a", "spot")
	c.MarkInsufficientCapacityForClass(forClass, time.Hour)

	clk.Step(4 * time.Minute) // the shared 3 minutes are over
	if c.IsUnavailable(base) {
		t.Error("a class without its own mark still sees the offering after the shared TTL")
	}
	if c.IsUnavailable(with(base, func(o *unavailable.Offering) { o.NodeClass = "other" })) {
		t.Error("another class sees the mark of bench")
	}
	_, expiry, ok := c.Why(forClass)
	if !ok || expiry.Sub(clk.Now()) != 56*time.Minute {
		t.Errorf("bench: unavailable = %v until %v, want its own mark for one hour", ok, expiry)
	}

	c.ForgetClass("bench")
	if c.IsUnavailable(forClass) {
		t.Error("the mark survived the class")
	}
}

func TestShorterClassTTLRecordsNothing(t *testing.T) {
	c := unavailable.New(clocktesting.NewFakeClock(time.Now()), unavailable.TTLs{})
	c.MarkInsufficientCapacityForClass(with(base, func(o *unavailable.Offering) { o.NodeClass = "bench" }), time.Minute)
	if n := c.Len(); n != 0 {
		t.Errorf("%d entries, want none: a class TTL below the shared one only extends", n)
	}
}

// With several live entries the offering is unavailable until the last of them
// expires, and Why names that one.
func TestWhyReturnsTheEntryThatExpiresLast(t *testing.T) {
	clk := clocktesting.NewFakeClock(time.Now())
	c := unavailable.New(clk, unavailable.TTLs{})
	forClass := with(base, func(o *unavailable.Offering) { o.NodeClass = "bench" })
	c.MarkInsufficientCapacity("aws", "aws.m7i.large", "us-east-1a", "spot")
	c.MarkInsufficientCapacityForClass(forClass, time.Hour)
	c.MarkUnsupported("aws", "aws.m7i.large", "us-east-1a")

	class, expiry, ok := c.Why(forClass)
	if !ok || class != "Unsupported" || expiry != clk.Now().Add(24*time.Hour) {
		t.Errorf("Why = %s until %v (%v), want Unsupported until 24h", class, expiry, ok)
	}
	class, _, _ = c.Why(base)
	if class != "Unsupported" {
		t.Errorf("Why = %s, want Unsupported", class)
	}
}

func TestMarkInvalidForClassIsSeenByThatClassOnly(t *testing.T) {
	c := unavailable.New(nil, unavailable.TTLs{})
	o := unavailable.Offering{
		Vendor: "v", InstanceType: "v.t", Zone: "z", CapacityType: "on-demand", NodeClass: "a",
	}
	c.MarkInvalidForClass(o)
	if !c.IsUnavailable(o) {
		t.Error("the marked class does not see the mark")
	}
	o.NodeClass = "b"
	if c.IsUnavailable(o) {
		t.Error("another class sees the mark")
	}
	o.NodeClass = ""
	if c.IsUnavailable(o) {
		t.Error("the shared view sees the class mark")
	}
}
