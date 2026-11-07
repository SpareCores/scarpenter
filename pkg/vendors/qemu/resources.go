//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package qemu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// KindNetwork is the resource kind of the bridges the VMs attach to, the key
// spec.vendors.qemu.resources.networks. A network is a Linux bridge of the
// machine and its ID the bridge's name.
const KindNetwork vendors.ResourceKind = "networks"

// Kinds implements vendors.ManagedResources. A created bridge stays until its
// NodeClass goes (retain): VMs and the host's own services may depend on it.
func (d *Driver) Kinds() []vendors.KindSpec {
	return []vendors.KindSpec{{
		Kind: KindNetwork, Scope: vendors.ScopeRegion, DefaultPolicy: common.LifecycleRetain, DeleteOrder: 30,
	}}
}

// network is the record of a managed bridge.
type network struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Region    string            `json:"region"`
	Tags      map[string]string `json:"tags"`
	CIDR      string            `json:"cidr"`
	CreatedAt time.Time         `json:"createdAt"`
	LastUsed  time.Time         `json:"lastUsed"`
}

func (n *network) ref() vendors.ResourceRef {
	tags := map[vendors.TagKey]string{}
	for k, v := range n.Tags {
		tags[vendors.TagKey(k)] = v
	}
	return vendors.ResourceRef{
		Kind: KindNetwork, Region: n.Region, ID: n.ID, Name: n.Name, Managed: true, Tags: tags, CreatedAt: n.CreatedAt,
	}
}

func (d *Driver) networkPath(id string) string { return filepath.Join(d.networksDir(), id+".json") }

func (d *Driver) readNetwork(id string) (*network, error) {
	raw, err := os.ReadFile(d.networkPath(id)) //nolint:gosec // the driver's own state
	if err != nil {
		return nil, err
	}
	n := &network{}
	if err := json.Unmarshal(raw, n); err != nil {
		return nil, fmt.Errorf("decoding the network record %s: %w", id, err)
	}
	return n, nil
}

func (d *Driver) writeNetwork(n *network) error {
	raw, err := json.MarshalIndent(n, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(d.networkPath(n.ID)+".tmp", raw, 0o600); err != nil {
		return err
	}
	return os.Rename(d.networkPath(n.ID)+".tmp", d.networkPath(n.ID))
}

func (d *Driver) networks() []*network {
	entries, _ := os.ReadDir(d.networksDir())
	var out []*network
	for _, e := range entries {
		if id, ok := strings.CutSuffix(e.Name(), ".json"); ok {
			if n, err := d.readNetwork(id); err == nil {
				out = append(out, n)
			}
		}
	}
	return out
}

// bridgeName derives the interface name of a managed network from its resource
// name (a Linux interface name has at most 15 characters).
func bridgeName(name string) string {
	sum := sha256.Sum256([]byte("bridge/" + name))
	return "scb" + hex.EncodeToString(sum[:])[:8]
}

// bridgeCIDR derives the address of a created bridge from the same name: a /24
// out of 10.88.0.0/16. The bridge has an address, no DHCP and no NAT: those are
// the machine's job (a bridge brought with `use` normally has them).
func bridgeCIDR(name string) string {
	sum := sha256.Sum256([]byte("cidr/" + name))
	return "10.88." + strconv.Itoa(int(sum[0])) + ".1/24"
}

// Resolve implements vendors.ManagedResources: bridges of the machine by name,
// name pattern, or "key=value" tags in the interface alias; with an empty
// reference the default bridge (--qemu-bridge), when it exists. Managed bridges
// are never returned: they are ours, not the user's.
func (d *Driver) Resolve(_ context.Context, kind vendors.ResourceKind, region string, ref vendors.UseRef,
) ([]vendors.ResourceRef, error) {
	if kind != KindNetwork || region != Region {
		return nil, nil
	}
	bridges, err := d.host.Bridges()
	if err != nil {
		return nil, vendors.NewError(vendors.Retryable, "Bridges", err)
	}
	managed := map[string]bool{}
	for _, n := range d.networks() {
		managed[n.ID] = true
	}
	var out []vendors.ResourceRef
	for _, b := range bridges {
		if managed[b.Name] || !matches(b, ref, d.opts.Bridge) {
			continue
		}
		out = append(out, vendors.ResourceRef{Kind: KindNetwork, Region: region, ID: b.Name, Name: b.Name})
	}
	slices.SortFunc(out, func(a, b vendors.ResourceRef) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func matches(b bridgeInfo, ref vendors.UseRef, def string) bool {
	switch {
	case ref.IsDefault():
		return b.Name == def
	case len(ref.IDs) > 0:
		return slices.Contains(ref.IDs, b.Name)
	case ref.NamePattern != "":
		ok, _ := path.Match(ref.NamePattern, b.Name)
		return ok
	}
	have := map[string]string{}
	for kv := range strings.FieldsSeq(b.Alias) {
		if k, v, ok := strings.Cut(kv, "="); ok {
			have[k] = v
		}
	}
	for k, v := range ref.Tags {
		if have[k] != v {
			return false
		}
	}
	return true
}

// Ensure implements vendors.ManagedResources: a record in the state directory
// names the bridge, which is created if the machine lacks it (after a reboot, for
// example).
func (d *Driver) Ensure(ctx context.Context, kind vendors.ResourceKind, region string, spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	if kind != KindNetwork {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal, "unknown resource kind %q", kind)
	}
	if region != Region {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal, "%q is not a region of the qemu driver", region)
	}
	if spec.Name == "" {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal, "a managed resource needs a name")
	}
	d.netMu.Lock()
	defer d.netMu.Unlock()
	id := bridgeName(spec.Name)
	created := false
	n, err := d.readNetwork(id)
	switch {
	case err == nil:
		if n.Tags[string(vendors.TagCluster)] != spec.Tags[vendors.TagCluster] ||
			n.Tags[string(vendors.TagManagedBy)] != spec.Tags[vendors.TagManagedBy] {
			return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal,
				"a network named %s exists without the ownership marker", spec.Name)
		}
	case errors.Is(err, os.ErrNotExist):
		now := d.clock.Now().UTC()
		n = &network{
			ID: id, Name: spec.Name, Region: region, Tags: tagStrings(spec.Tags), CIDR: bridgeCIDR(spec.Name),
			CreatedAt: now, LastUsed: now,
		}
		maps.Copy(n.Tags, spec.ExtraTags)
		created = true
	default:
		return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, "NetworkRecord", err)
	}
	bridges, err := d.host.Bridges()
	if err != nil {
		return vendors.ResourceRef{}, false, vendors.NewError(vendors.Retryable, "Bridges", err)
	}
	if !slices.ContainsFunc(bridges, func(b bridgeInfo) bool { return b.Name == id }) {
		for _, step := range [][]string{
			{ipLink, ipAdd, id, "type", "bridge"}, {"addr", ipAdd, n.CIDR, "dev", id}, {ipLink, ipSet, id, "up"},
		} {
			if _, err := d.host.Run(ctx, "ip", step...); err != nil {
				return vendors.ResourceRef{}, false, vendors.NewError(vendors.Retryable, "Bridge", err)
			}
		}
	}
	if err := d.writeNetwork(n); err != nil {
		return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, "NetworkRecord", err)
	}
	return n.ref(), created, nil
}

func tagStrings(in map[vendors.TagKey]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[string(k)] = v
	}
	return out
}

// usedBy returns the number of live instances attached to the network.
func (d *Driver) usedBy(id string) int {
	n := 0
	for _, rec := range d.records() {
		if rec.Bridge == id && (rec.Phase == phaseCreating || d.alive(rec)) {
			n++
		}
	}
	return n
}

// touchNetworks records the last use of managed networks (best effort).
func (d *Driver) touchNetworks(ids []string) {
	if len(ids) == 0 {
		return
	}
	d.netMu.Lock()
	defer d.netMu.Unlock()
	for _, id := range ids {
		if n, err := d.readNetwork(id); err == nil {
			n.LastUsed = d.clock.Now().UTC()
			if err := d.writeNetwork(n); err != nil {
				d.log.Error(err, "recording the last use of a network", "network", id)
			}
		}
	}
}

// InUse implements vendors.ManagedResources.
func (d *Driver) InUse(_ context.Context, ref vendors.ResourceRef) (bool, time.Time, error) {
	d.netMu.Lock()
	defer d.netMu.Unlock()
	n, err := d.readNetwork(ref.ID)
	if err != nil {
		return false, time.Time{}, vendors.Errorf(vendors.NotFound, "network %s does not exist", ref.ID)
	}
	if d.usedBy(ref.ID) > 0 {
		return true, d.clock.Now(), nil
	}
	return false, n.LastUsed, nil
}

// Delete implements vendors.ManagedResources: only managed networks, and not
// while a VM is attached; a missing one is not an error.
func (d *Driver) Delete(ctx context.Context, ref vendors.ResourceRef) error {
	d.netMu.Lock()
	defer d.netMu.Unlock()
	if _, err := d.readNetwork(ref.ID); err != nil {
		bridges, _ := d.host.Bridges()
		if slices.ContainsFunc(bridges, func(b bridgeInfo) bool { return b.Name == ref.ID }) {
			return vendors.Errorf(vendors.Fatal, "the bridge %s was not created by SCarpenter and is never deleted", ref.ID)
		}
		return nil
	}
	if users := d.usedBy(ref.ID); users > 0 {
		return vendors.NewError(vendors.Retryable, "NetworkInUse",
			fmt.Errorf("the network %s is used by %d instances", ref.ID, users))
	}
	bridges, err := d.host.Bridges()
	if err != nil {
		return vendors.NewError(vendors.Retryable, "Bridges", err)
	}
	if slices.ContainsFunc(bridges, func(b bridgeInfo) bool { return b.Name == ref.ID }) {
		if _, err := d.host.Run(ctx, "ip", ipLink, "delete", ref.ID); err != nil {
			return vendors.NewError(vendors.Retryable, "Bridge", err)
		}
	}
	if err := os.Remove(d.networkPath(ref.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return vendors.NewError(vendors.Fatal, "NetworkRecord", err)
	}
	return nil
}

// List implements vendors.ManagedResources.
func (d *Driver) List(
	_ context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]vendors.ResourceRef, error) {
	var out []vendors.ResourceRef
	for _, n := range d.networks() {
		if len(regions) > 0 && !slices.Contains(regions, n.Region) {
			continue
		}
		ok := true
		for k, v := range tags {
			if n.Tags[string(k)] != v {
				ok = false
			}
		}
		if ok {
			out = append(out, n.ref())
		}
	}
	slices.SortFunc(out, func(a, b vendors.ResourceRef) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}
