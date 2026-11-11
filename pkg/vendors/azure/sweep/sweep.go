/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package sweep finds and deletes what a test cluster of the Azure L5 lane left
// in the lane's resource group (docs/how-to-guides/testing-and-releasing.md,
// plan 17 section 9): virtual machines, network interfaces, public IP addresses,
// managed disks, network security groups with their application security group,
// virtual networks, proximity placement groups and, for a disposable group, the
// resource group itself. It is the engine of the sweeper workflow (every three
// hours, everything of a cluster named scarpenter-ci-* older than two hours) and
// the last line of defence of the e2e teardown (one cluster, no age limit).
//
// It talks to ARM through the driver's narrow interfaces (azure.Clients), so the
// in-memory fake of the driver tests runs it unchanged. Only one resource group
// is ever listed: the sweeper's role is scoped to it.
//
// Selection is conservative on purpose. A resource is touched only when its
// cluster tag (scarpenter-cluster) matches and, with an age limit, when the tag
// scarpenter-ci-created-at (RFC 3339) says it is old enough: a resource without
// the tag has no known age and is left alone. Inline disks carry no tags, so
// they are matched by the name of their VM (<vm>-os, <vm>-<datadisk>), and only
// for a VM of the run that was swept now or is already gone. The resource group
// is deleted only when it is a managed, marked group named like the ones the
// driver creates for a run (scarpenter-<cluster>), and empty: the lane's own
// pre-provisioned group is never deleted, whatever its tags.
package sweep

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/azure"
)

const (
	// DefaultClusterPrefix is the cluster name prefix of the L5 lane.
	DefaultClusterPrefix = "scarpenter-ci-"
	// groupPrefix precedes the cluster name in the name of a resource group the
	// driver creates for a run.
	groupPrefix = "scarpenter-"
)

// CreatedAtTag is the tag that holds the creation time of a resource (RFC 3339),
// put on every resource of the lane through spec.tags.
var CreatedAtTag = azure.TagKey("ci-created-at")

// ClusterTag is the tag every SCarpenter resource carries with the cluster name.
var ClusterTag = azure.TagKey(vendors.TagCluster)

// The resource types of the kinds the sweeper deletes (the "type" of a generic
// resource, compared in lower case).
const (
	typeVM   = "microsoft.compute/virtualmachines"
	typeNIC  = "microsoft.network/networkinterfaces"
	typePIP  = "microsoft.network/publicipaddresses"
	typeDisk = "microsoft.compute/disks"
	typeNSG  = "microsoft.network/networksecuritygroups"
	typeASG  = "microsoft.network/applicationsecuritygroups"
	typeVNet = "microsoft.network/virtualnetworks"
	typePPG  = "microsoft.compute/proximityplacementgroups"
)

// The kinds of an Item.
const (
	KindVM            = "vm"
	KindNIC           = "nic"
	KindPublicIP      = "public-ip"
	KindDisk          = "disk"
	KindNSG           = "nsg"
	KindASG           = "asg"
	KindVNet          = "vnet"
	KindPPG           = "ppg"
	KindResourceGroup = "resource-group"
)

// Disks deletes managed disks. The driver has no disk API (inline disks go with
// their VM); the sweeper needs one for the disks a VM deleted with the Detach
// option left behind.
type Disks interface {
	Delete(ctx context.Context, resourceGroup, name string) error
}

// Options select what is swept.
type Options struct {
	// ResourceGroup is the group to sweep (the lane's own); required.
	ResourceGroup string
	// Cluster selects the resources of one cluster by exact name. Without it,
	// ClusterPrefix selects every cluster whose name starts with it (the default
	// prefix when both are empty).
	Cluster       string
	ClusterPrefix string
	// MaxAge is the age a resource must have; 0 means any age (and then also
	// resources without a known age are swept).
	MaxAge time.Duration
	// DryRun reports what would be deleted and deletes nothing.
	DryRun bool
	// Now is the clock (default time.Now).
	Now func() time.Time
	// Logf receives one line per decision (default: discard).
	Logf func(format string, args ...any)
}

// Outcome is what happened to a resource.
type Outcome string

// The outcomes.
const (
	Deleted     Outcome = "deleted"
	WouldDelete Outcome = "would-delete"
	Skipped     Outcome = "skipped"
	Failed      Outcome = "failed"
)

// Item is one resource the sweep acted on or skipped. Resources whose cluster
// tag does not select them are not reported: they are not the sweeper's.
type Item struct {
	Kind    string
	Name    string
	Cluster string
	Outcome Outcome
	// Reason says why the resource was skipped or what failed.
	Reason string
}

// Report is the result of a sweep, in the order the resources were handled.
type Report struct {
	Items []Item
}

// Count returns how many items had the outcome.
func (r *Report) Count(o Outcome) int {
	n := 0
	for _, i := range r.Items {
		if i.Outcome == o {
			n++
		}
	}
	return n
}

// entry is a resource of the group.
type entry struct {
	typ     string
	name    string
	id      string
	tags    map[string]string // lower-cased names
	cluster string
}

type sweeper struct {
	c     azure.Clients
	disks Disks
	o     Options
	rep   *Report
	// gone holds the lower-cased IDs of what was (or would be) deleted, to tell
	// whether the group is empty afterwards.
	gone map[string]bool
}

// Run sweeps the resource group. It returns an error only for a failure that
// makes the sweep meaningless (bad options, the group cannot be listed);
// failures of single resources are items with the outcome Failed. A group that
// does not exist is a sweep of nothing.
func Run(ctx context.Context, c azure.Clients, disks Disks, o Options) (*Report, error) {
	if o.ResourceGroup == "" {
		return nil, errors.New("sweep: the resource group is required")
	}
	if o.Cluster == "" && o.ClusterPrefix == "" {
		o.ClusterPrefix = DefaultClusterPrefix
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if disks == nil && !o.DryRun {
		return nil, errors.New("sweep: a disk client is required unless dry-run")
	}
	s := &sweeper{c: c, disks: disks, o: o, rep: &Report{}, gone: map[string]bool{}}
	res, err := c.Groups.Resources(ctx, o.ResourceGroup)
	if err != nil {
		if isNotFound(err) {
			s.o.Logf("resource group %s does not exist: nothing to sweep", o.ResourceGroup)
			return s.rep, nil
		}
		return nil, fmt.Errorf("listing the resources of the group %s: %w", o.ResourceGroup, err)
	}
	s.run(ctx, res)
	return s.rep, nil
}

func (s *sweeper) run(ctx context.Context, res []*armresources.GenericResourceExpanded) {
	var entries []entry
	for _, r := range res {
		if r == nil || r.Name == nil || r.Type == nil {
			continue
		}
		e := entry{typ: strings.ToLower(*r.Type), name: *r.Name, tags: map[string]string{}}
		if r.ID != nil {
			e.id = *r.ID
		}
		for k, v := range r.Tags {
			if v != nil {
				e.tags[strings.ToLower(k)] = *v
			}
		}
		e.cluster = e.tags[strings.ToLower(ClusterTag)]
		entries = append(entries, e)
	}

	// Decide every tagged resource once; a cluster with a resource that is kept
	// (too young, no age) is not "ready": its other resources go, but the disks
	// of VMs that are already gone are not guessed at.
	selected := map[string]bool{} // by lower-cased id of name
	blocked := map[string]bool{}
	seen := map[string]bool{}
	for _, e := range entries {
		if e.typ == typeDisk || !s.matches(e.cluster) {
			continue
		}
		seen[e.cluster] = true
		ok, reason := s.old(e.tags)
		if !ok {
			blocked[e.cluster] = true
			if e.typ == typeASG {
				continue // reported with its NSG, or not at all
			}
			s.add(Item{Kind: kindOf(e.typ), Name: e.name, Cluster: e.cluster, Outcome: Skipped, Reason: reason})
			continue
		}
		selected[key(e)] = true
	}
	var ready []string
	for c := range seen {
		if !blocked[c] {
			ready = append(ready, c)
		}
	}

	swept := map[string]string{} // VM name (lower) -> cluster, deleted now or in dry-run
	live := map[string]bool{}    // VMs that stay: kept, or whose deletion failed
	for _, e := range entries {
		if e.typ != typeVM {
			continue
		}
		if !selected[key(e)] {
			live[strings.ToLower(e.name)] = true
			continue
		}
		if s.del(ctx, e, KindVM, func() error { return s.c.VMs.Delete(ctx, s.o.ResourceGroup, e.name, true) }) {
			swept[strings.ToLower(e.name)] = e.cluster
		} else {
			live[strings.ToLower(e.name)] = true
		}
	}
	s.each(ctx, entries, selected, typeNIC, KindNIC, func(e entry) error {
		return s.c.NICs.Delete(ctx, s.o.ResourceGroup, e.name)
	})
	s.each(ctx, entries, selected, typePIP, KindPublicIP, func(e entry) error {
		return s.c.PIPs.Delete(ctx, s.o.ResourceGroup, e.name)
	})
	s.sweepDisks(ctx, entries, swept, live, ready)
	s.sweepNSGs(ctx, entries, selected)
	s.each(ctx, entries, selected, typeVNet, KindVNet, func(e entry) error {
		return s.c.VNets.Delete(ctx, s.o.ResourceGroup, e.name)
	})
	s.each(ctx, entries, selected, typePPG, KindPPG, func(e entry) error {
		return s.c.PPGs.Delete(ctx, s.o.ResourceGroup, e.name)
	})
	s.sweepGroup(ctx, entries)
}

// each deletes the selected resources of one type.
func (s *sweeper) each(
	ctx context.Context, entries []entry, selected map[string]bool, typ, kind string, fn func(entry) error,
) {
	for _, e := range entries {
		if e.typ == typ && selected[key(e)] {
			s.del(ctx, e, kind, func() error { return fn(e) })
		}
	}
}

// sweepDisks deletes the managed disks of the VMs of the run: those swept now,
// and those of VMs that are already gone, found by the hash of the cluster in
// the VM name (<nodeclaim>-<hash8(cluster)>, then -os or -<datadisk>) for a
// cluster none of whose resources was kept. A disk whose VM still exists is
// never touched.
func (s *sweeper) sweepDisks(
	ctx context.Context, entries []entry, swept map[string]string, live map[string]bool, ready []string,
) {
	for _, e := range entries {
		if e.typ != typeDisk {
			continue
		}
		name := strings.ToLower(e.name)
		if vm, ok := prefixIn(name, live); ok {
			s.add(Item{Kind: KindDisk, Name: e.name, Outcome: Skipped, Reason: "its VM " + vm + " is not swept"})
			continue
		}
		cluster := ""
		for vm, c := range swept {
			if strings.HasPrefix(name, vm+"-") {
				cluster = c
				break
			}
		}
		if cluster == "" {
			for _, c := range ready {
				if strings.Contains(name, "-"+clusterHash(c)+"-") {
					cluster = c
					break
				}
			}
		}
		if cluster == "" {
			continue
		}
		s.del(ctx, entry{typ: e.typ, name: e.name, id: e.id, cluster: cluster}, KindDisk, func() error {
			return s.disks.Delete(ctx, s.o.ResourceGroup, e.name)
		})
	}
}

// sweepNSGs deletes the selected network security groups, each followed by its
// application security group (<nsg>-nodes), which the group's rules refer to and
// which therefore goes after it. The application security group is deleted only
// when it carries the same cluster tag: a lookalike name is not enough.
func (s *sweeper) sweepNSGs(ctx context.Context, entries []entry, selected map[string]bool) {
	for _, e := range entries {
		if e.typ != typeNSG || !selected[key(e)] {
			continue
		}
		if !s.del(ctx, e, KindNSG, func() error { return s.c.NSGs.Delete(ctx, s.o.ResourceGroup, e.name) }) {
			continue
		}
		asg := azure.ASGName(e.name)
		for _, a := range entries {
			if a.typ == typeASG && strings.EqualFold(a.name, asg) && a.cluster == e.cluster {
				s.del(ctx, a, KindASG, func() error { return s.c.ASGs.Delete(ctx, s.o.ResourceGroup, a.name) })
			}
		}
	}
}

// sweepGroup deletes the resource group itself when it is a disposable one: a
// managed, marked group with the name of a group of a run, left empty by the
// sweep. Any other group, the lane's own pre-provisioned one first, stays.
func (s *sweeper) sweepGroup(ctx context.Context, entries []entry) {
	name := s.o.ResourceGroup
	g, err := s.c.Groups.Get(ctx, name)
	if err != nil {
		if !isNotFound(err) {
			s.add(Item{Kind: KindResourceGroup, Name: name, Outcome: Failed, Reason: err.Error()})
		}
		return
	}
	tags := map[string]string{}
	for k, v := range g.Tags {
		if v != nil {
			tags[strings.ToLower(k)] = *v
		}
	}
	cluster := tags[strings.ToLower(ClusterTag)]
	if tags[strings.ToLower(azure.TagKey(vendors.TagManagedBy))] != vendors.ManagedByValue || !s.matches(cluster) ||
		!strings.HasPrefix(strings.ToLower(name), groupPrefix+s.prefix()) {
		s.o.Logf("resource group %s is not a disposable group of a run: kept", name)
		return
	}
	if ok, reason := s.old(tags); !ok {
		s.add(Item{Kind: KindResourceGroup, Name: name, Cluster: cluster, Outcome: Skipped, Reason: reason})
		return
	}
	left := 0
	for _, e := range entries {
		if !s.gone[key(e)] {
			left++
		}
	}
	if left > 0 {
		s.add(Item{Kind: KindResourceGroup, Name: name, Cluster: cluster, Outcome: Skipped,
			Reason: fmt.Sprintf("not empty: %d resources left", left)})
		return
	}
	s.del(ctx, entry{name: name, cluster: cluster}, KindResourceGroup, func() error {
		return s.c.Groups.Delete(ctx, name)
	})
}

// prefix is the cluster name a disposable group's name continues from.
func (s *sweeper) prefix() string {
	if s.o.Cluster != "" {
		return strings.ToLower(s.o.Cluster)
	}
	return strings.ToLower(s.o.ClusterPrefix)
}

// del runs one deletion and records it; it returns whether the resource is gone
// (or would be).
func (s *sweeper) del(ctx context.Context, e entry, kind string, fn func() error) bool {
	it := Item{Kind: kind, Name: e.name, Cluster: e.cluster}
	if s.o.DryRun {
		it.Outcome = WouldDelete
		s.add(it)
		s.gone[key(e)] = true
		return true
	}
	err := ctx.Err()
	if err == nil {
		err = fn()
	}
	if err != nil && !isNotFound(err) {
		it.Outcome, it.Reason = Failed, err.Error()
		s.add(it)
		return false
	}
	it.Outcome = Deleted
	s.add(it)
	s.gone[key(e)] = true
	return true
}

func (s *sweeper) add(i Item) {
	s.rep.Items = append(s.rep.Items, i)
	line := fmt.Sprintf("%s %s %s", i.Kind, i.Name, i.Outcome)
	if i.Cluster != "" {
		line += " cluster=" + i.Cluster
	}
	if i.Reason != "" {
		line += ": " + i.Reason
	}
	s.o.Logf("%s", line)
}

// matches reports whether the cluster tag selects the resource.
func (s *sweeper) matches(cluster string) bool {
	if cluster == "" {
		return false
	}
	if s.o.Cluster != "" {
		return cluster == s.o.Cluster
	}
	return strings.HasPrefix(cluster, s.o.ClusterPrefix) && len(cluster) > len(s.o.ClusterPrefix)
}

// old applies the age limit by the created-at tag.
func (s *sweeper) old(tags map[string]string) (bool, string) {
	if s.o.MaxAge <= 0 {
		return true, ""
	}
	v, ok := tags[strings.ToLower(CreatedAtTag)]
	if !ok {
		return false, "no creation time (tag " + CreatedAtTag + " missing)"
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return false, "no creation time (tag " + CreatedAtTag + " is not RFC 3339)"
	}
	if age := s.o.Now().Sub(t); age < s.o.MaxAge {
		return false, fmt.Sprintf("only %s old (limit %s)", age.Round(time.Second), s.o.MaxAge)
	}
	return true, ""
}

func key(e entry) string { return e.typ + "/" + strings.ToLower(e.name) }

func kindOf(typ string) string {
	switch typ {
	case typeVM:
		return KindVM
	case typeNIC:
		return KindNIC
	case typePIP:
		return KindPublicIP
	case typeNSG:
		return KindNSG
	case typeASG:
		return KindASG
	case typeVNet:
		return KindVNet
	case typePPG:
		return KindPPG
	}
	return typ
}

// prefixIn finds the VM of the set whose name followed by a dash starts name.
func prefixIn(name string, vms map[string]bool) (string, bool) {
	for vm := range vms {
		if strings.HasPrefix(name, vm+"-") {
			return vm, true
		}
	}
	return "", false
}

// clusterHash is the hash8 of the cluster that ends every VM name of it.
func clusterHash(cluster string) string {
	return strings.TrimPrefix(azure.VMName(cluster, "x"), "x-")
}

// isNotFound reports a deletion that found nothing left to delete.
func isNotFound(err error) bool {
	if re, ok := errors.AsType[*azcore.ResponseError](err); ok {
		return re.StatusCode == http.StatusNotFound
	}
	return false
}
