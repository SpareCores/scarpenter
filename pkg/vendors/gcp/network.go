/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/googleapi"

	gcpapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/gcp"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// The managed network (plan 16, section 2): one global custom-mode VPC per cluster
// with one subnetwork per region. A subnetwork is a /20 taken from a pool of
// private addresses: the first /20 of the pool that overlaps no subnetwork of the
// network. The allocation is derived from what the network lists, so it is the
// same after a restart and never collides with an existing subnetwork.
const (
	// DefaultPool is the pool of the managed subnetworks, unless the NodeClass's
	// create says otherwise.
	DefaultPool = "10.240.0.0/12"
	// SubnetPrefixLen is the size of a managed subnetwork.
	SubnetPrefixLen = 20
	// allocateAttempts bounds the retries of an insert that lost a race for a range
	// with another process.
	allocateAttempts = 5
)

// SubnetParams are the create parameters of the subnets kind.
type SubnetParams struct {
	// Pool overrides DefaultPool.
	Pool string `json:"pool,omitempty"`
}

// privateRanges are the ranges a pool must lie inside (RFC 1918).
var privateRanges = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.168.0.0/16"),
}

// ParsePool checks a pool: an IPv4 CIDR in canonical form inside a private range,
// at least a /20 so that one subnetwork fits.
func ParsePool(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil || !p.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("%q is not an IPv4 CIDR", s)
	}
	if p != p.Masked() {
		return netip.Prefix{}, fmt.Errorf("%q has host bits set (did you mean %s?)", s, p.Masked())
	}
	if p.Bits() > SubnetPrefixLen {
		return netip.Prefix{}, fmt.Errorf("%q is smaller than a /%d: no subnetwork fits", s, SubnetPrefixLen)
	}
	for _, r := range privateRanges {
		if r.Bits() <= p.Bits() && r.Contains(p.Addr()) {
			return p, nil
		}
	}
	return netip.Prefix{}, fmt.Errorf("%q is not inside 10.0.0.0/8, 172.16.0.0/12 or 192.168.0.0/16", s)
}

// validatePool checks the pool of the create block of the subnets kind.
func validatePool(r *gcpapi.SubnetResource) []error {
	if r == nil || r.Create == nil || r.Create.Pool == "" {
		return nil
	}
	if _, err := ParsePool(r.Create.Pool); err != nil {
		return []error{fmt.Errorf("vendors.gcp.resources.subnets.create.pool: %w", err)}
	}
	return nil
}

// poolOf is the pool of the subnets create block of the NodeClass's resources;
// empty for the default. The resources member is not part of the merged block
// (Raw): it reaches the driver as VendorSettings.Resources.
func poolOf(s vendors.VendorSettings) (string, error) {
	if len(s.Resources) == 0 {
		return "", nil
	}
	var r gcpapi.Resources
	if err := json.Unmarshal(s.Resources, &r); err != nil {
		return "", fmt.Errorf("decoding the GCP resources: %w", err)
	}
	if r.Subnets == nil || r.Subnets.Create == nil {
		return "", nil
	}
	return r.Subnets.Create.Pool, nil
}

// managedNetworkName is the name of the cluster's managed network.
func (d *Driver) managedNetworkName() string {
	return shorten(ResourceNamePrefix(d.cfg.ClusterName) + "subnets")
}

// networkMarker is the description of the managed network: global, so without a
// region.
func networkMarker(spec vendors.CreateSpec) string {
	return MarkerDescription(spec.Tags[vendors.TagCluster], spec.Tags[vendors.TagNodeClass], string(KindSubnets))
}

// usedRanges lists the ranges taken in the network: the primary and secondary
// ranges of every subnetwork it has, in any region (the network lists them).
func (d *Driver) usedRanges(ctx context.Context, api ComputeAPI, network string) ([]netip.Prefix, error) {
	n, err := api.GetNetwork(ctx, &computepb.GetNetworkRequest{Project: d.project, Network: network})
	if err != nil {
		return nil, classify(err, errContext{})
	}
	var used []netip.Prefix
	add := func(s string) {
		if p, err := netip.ParsePrefix(s); err == nil {
			used = append(used, p)
		}
	}
	for _, url := range n.GetSubnetworks() {
		p, region, name, ok := subnetParts(url)
		if !ok {
			continue
		}
		s, err := api.GetSubnetwork(ctx, &computepb.GetSubnetworkRequest{Project: p, Region: region, Subnetwork: name})
		if err != nil {
			if IsNotFound(err) {
				continue // deleted since the network was read
			}
			return nil, classify(err, errContext{})
		}
		add(s.GetIpCidrRange())
		for _, sec := range s.GetSecondaryIpRanges() {
			add(sec.GetIpCidrRange())
		}
	}
	return used, nil
}

// freeSubnet is the first /20 of the pool that overlaps none of the used ranges.
func freeSubnet(pool netip.Prefix, used []netip.Prefix) (netip.Prefix, error) {
	step := uint32(1) << (32 - SubnetPrefixLen)
	a := pool.Addr().As4()
	start := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	count := uint64(1) << (SubnetPrefixLen - pool.Bits())
	for i := range count {
		v := start + uint32(i)*step
		cand := netip.PrefixFrom(netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}),
			SubnetPrefixLen)
		clash := false
		for _, u := range used {
			if u.Overlaps(cand) {
				clash = true
				break
			}
		}
		if !clash {
			return cand, nil
		}
	}
	return netip.Prefix{}, fmt.Errorf("the pool %s has no free /%d left", pool, SubnetPrefixLen)
}

// isRangeConflict says whether an insert was refused because its range overlaps
// another subnetwork of the network (Compute: "Invalid IPCidrRange: <r> conflicts
// with existing subnetwork <n> in region <r>").
func isRangeConflict(err error) bool {
	const text = "conflicts with existing subnetwork"
	if ge, ok := errors.AsType[*googleapi.Error](err); ok && strings.Contains(ge.Message, text) {
		return true
	}
	return err != nil && strings.Contains(err.Error(), text)
}

// insertSubnet creates the subnetwork of the region in the network with the next
// free /20 of the pool. The caller holds netMu, so the allocation and the insert
// are one critical section in this process (the engine serialises one region, not
// the regions among themselves); another process may still win a range, which
// Compute refuses, in the call or in its operation, and the allocation then
// retries with a fresh listing.
func (d *Driver) insertSubnet(
	ctx context.Context, api ComputeAPI, region, name, network, desc string, pool netip.Prefix,
) error {
	var last error
	for range allocateAttempts {
		used, err := d.usedRanges(ctx, api, network)
		if err != nil {
			return err
		}
		cidr, err := freeSubnet(pool, used)
		if err != nil {
			return vendors.NewError(vendors.Fatal, vendors.CodeResourceMissing, err)
		}
		op, err := api.InsertSubnetwork(ctx, &computepb.InsertSubnetworkRequest{Project: d.project, Region: region,
			SubnetworkResource: &computepb.Subnetwork{
				Name: &name, Description: &desc, Network: new(networkID(d.project, network)),
				IpCidrRange: new(cidr.String()), PrivateIpGoogleAccess: new(true),
				StackType: new("IPV4_IPV6"), Ipv6AccessType: new("EXTERNAL"),
			}})
		if err == nil {
			if err = d.finish(ctx, api, op); err == nil {
				return nil
			}
		}
		switch {
		case IsAlreadyExists(err):
			return nil // an earlier attempt of ours, or a concurrent one: the caller reads it
		case isRangeConflict(err):
			last = err
			continue
		}
		return classify(err, errContext{})
	}
	return classify(last, errContext{})
}
