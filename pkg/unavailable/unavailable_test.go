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
			name: "quota exceeded without a quota API: (vendor, region, capacity type), 30 minutes",
			mark: func(c *unavailable.Cache) { c.MarkQuotaExceeded("aws", "us-east-1", "spot") },
			ttl:  30 * time.Minute,
			covered: []unavailable.Offering{
				base, with(base, func(o *unavailable.Offering) { o.InstanceType, o.Zone = "aws.c7g.large", "us-east-1c" }),
			},
			free: []unavailable.Offering{
				with(base, func(o *unavailable.Offering) { o.CapacityType = "on-demand" }),
				with(base, func(o *unavailable.Offering) { o.Region = "eu-west-1" }),
			},
		},
		{
			name: "account-wide quota exceeded: (vendor, capacity type) in every region, 30 minutes",
			mark: func(c *unavailable.Cache) { c.MarkQuotaExceededAccountWide("aws", "spot") },
			ttl:  30 * time.Minute,
			covered: []unavailable.Offering{
				base, with(base, func(o *unavailable.Offering) { o.Region, o.Zone = "eu-west-1", "eu-west-1a" }),
			},
			free: []unavailable.Offering{
				with(base, func(o *unavailable.Offering) { o.CapacityType = "on-demand" }),
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
	clk := clocktesting.NewFakeClock(time.Now())
	c := unavailable.New(clk, unavailable.TTLs{})
	c.MarkZoneUnusable("aws", "z")
	s := c.Sequence()
	clk.Step(time.Minute)
	if c.Sequence() != s {
		t.Error("the sequence changed without a change")
	}
}

func TestMarkingAgainRestartsTheTTL(t *testing.T) {
	clk := clocktesting.NewFakeClock(time.Now())
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
	clk := clocktesting.NewFakeClock(time.Now())
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
