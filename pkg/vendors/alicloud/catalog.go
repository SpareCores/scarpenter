/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"slices"
	"strings"
	"time"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// dropFamilies are the instance type families the driver never launches
// (plan 21 section 3, Catalog). The catalog keeps them in Keeper's data, as AWS
// keeps mac*. Name is matched against the family segment of the type (the part
// after "ecs."): as a prefix when Prefix is set, else exactly (c1 must not drop
// c10a).
const reasonTest = "test family"

var dropFamilies = []struct {
	Name   string
	Prefix bool
	Reason string
}{
	{"ebm", true, "bare metal (ecs.ebm*)"},
	{"scc", true, "Super Computing Cluster (ecs.scc*); Flatcar on them is unverified"},
	{"poc-test", true, reasonTest},
	{"e-test", true, reasonTest},
	{"vfx", true, reasonTest},
	{"hbm", true, reasonTest},
	// Classic-network-only legacy families: the list is unverified (step 0).
	{"c1", false, "classic-network-only legacy family (unverified list)"},
	{"s1", false, "classic-network-only legacy family (unverified list)"},
}

// FamilyOf returns the family segment of an instance type with the "ecs."
// prefix: ecs.g8y.large is ecs.g8y, ecs.e-c1m1.large is ecs.e-c1m1. The names
// are not always three segments (ecs.u0.c1m1.large, ecs.ebmgn9ge-1.5t.64xlarge
// in Keeper's data), so the family is the second segment, not "all but the
// size". ok is false for a name that is not ecs.<family>.<size...>.
func FamilyOf(instanceType string) (family string, ok bool) {
	parts := strings.Split(instanceType, ".")
	if len(parts) < 3 || parts[0] != "ecs" || slices.Contains(parts, "") {
		return "", false
	}
	return parts[0] + "." + parts[1], true
}

// DropReason says why the driver never launches the type, "" when it may
// launch it. A name that is not an ECS type is dropped.
func DropReason(instanceType string) string {
	family, ok := FamilyOf(instanceType)
	if !ok {
		return "not an ecs.<family>.<size> type name"
	}
	name := strings.TrimPrefix(family, "ecs.")
	for _, d := range dropFamilies {
		if name == d.Name || (d.Prefix && strings.HasPrefix(name, d.Name)) {
			return d.Reason
		}
	}
	return ""
}

// Launchable reports whether the driver may launch the type.
func Launchable(instanceType string) bool { return DropReason(instanceType) == "" }

// HasLocalDisks reports whether the family bundles local disks (the i and d
// families: i1, i2, i3g, d1, d2s ...). They are launchable but not supported
// for local-storage profiles in v1 (ADR-017: infeasible, not degraded).
func HasLocalDisks(instanceType string) bool {
	family, ok := FamilyOf(instanceType)
	if !ok {
		return false
	}
	name := strings.TrimPrefix(family, "ecs.")
	return len(name) >= 2 && (name[0] == 'i' || name[0] == 'd') && name[1] >= '0' && name[1] <= '9'
}

// Instance states of ECS (DescribeInstances Status).
const (
	statusPending  = "Pending"
	statusStarting = "Starting"
	statusRunning  = "Running"
	statusStopping = "Stopping"
	statusStopped  = "Stopped"
)

// StateOf maps an ECS instance status to the driver's instance state. A
// stopped instance is terminating, never running (plan 14, A4): it exists, its
// disks cost, and the garbage collector deletes it. A released instance is not
// returned by ECS at all. A status the driver does not know is pending: not
// usable yet, and not deleted.
func StateOf(status string) string {
	switch status {
	case statusRunning:
		return vendors.StateRunning
	case statusStopping, statusStopped:
		return vendors.StateTerminating
	default:
		return vendors.StatePending
	}
}

// parseTime reads the time formats ECS uses: RFC 3339 and the minute-precision
// form of DescribeInstances (2017-12-10T04:04Z). Zero for a text it cannot
// read.
func parseTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04Z", "2006-01-02T15:04:05Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
