/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package scope resolves which regions of which vendors are in use: the ones
// that any SCNodeClass allows (every region the driver can use when a class
// lists none) and the ones where NodeClaims run. The quota tracker, the
// instance listing of the CloudProvider and the garbage collector all work over
// this set.
package scope

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// DriverRegions says which regions the driver of a vendor can use.
type DriverRegions interface {
	Regions(vendor string) []string
}

// Resolver resolves the regions in use. It reads through Reader, normally the
// manager's cached client.
type Resolver struct {
	Reader   client.Reader
	Registry *vendors.Registry
	Regions  DriverRegions
}

// Allowed returns, per enabled vendor with at least one SCNodeClass, the
// regions the classes allow: the union of their allow-lists, or the driver's
// regions when one class lists none (until the driver's regions are loaded that
// is empty).
func (r *Resolver) Allowed(ctx context.Context) (map[string][]string, error) {
	var classes v1alpha1.SCNodeClassList
	if err := r.Reader.List(ctx, &classes); err != nil {
		return nil, fmt.Errorf("listing SCNodeClasses: %w", err)
	}
	out := map[string][]string{}
	all := map[string]bool{}
	for i := range classes.Items {
		spec := &classes.Items[i].Spec
		for _, vendor := range spec.Vendors.Names() {
			if !r.Registry.Enabled(vendor) {
				continue
			}
			regions, _ := spec.Vendors.Regions(vendor)
			if _, ok := out[vendor]; !ok {
				out[vendor] = nil
			}
			if len(regions) == 0 {
				all[vendor] = true
				continue
			}
			out[vendor] = append(out[vendor], regions...)
		}
	}
	for vendor := range out {
		if all[vendor] {
			out[vendor] = r.Regions.Regions(vendor)
		}
		slices.Sort(out[vendor])
		out[vendor] = slices.Compact(out[vendor])
	}
	return out, nil
}

// InUse is Allowed plus the regions of the existing NodeClaims, read from their
// provider IDs: instances in a region that no NodeClass allows any more still
// need to be listed, observed and deleted.
func (r *Resolver) InUse(ctx context.Context) (map[string][]string, error) {
	out, err := r.Allowed(ctx)
	if err != nil {
		return nil, err
	}
	var claims karpv1.NodeClaimList
	if err := r.Reader.List(ctx, &claims); err != nil {
		return nil, fmt.Errorf("listing NodeClaims: %w", err)
	}
	for i := range claims.Items {
		vendor, region, _, err := ParseProviderID(claims.Items[i].Status.ProviderID)
		if err != nil || !r.Registry.Enabled(vendor) {
			continue
		}
		if !slices.Contains(out[vendor], region) {
			out[vendor] = append(out[vendor], region)
		}
	}
	for vendor := range out {
		slices.Sort(out[vendor])
	}
	return out, nil
}

// ProviderIDPrefix starts every provider ID (overview section 4.2).
const ProviderIDPrefix = "scarpenter://"

// ProviderID is scarpenter://<vendor>/<region>/<nodeclaim> (ADR-007).
func ProviderID(vendor, region, nodeClaim string) string {
	return ProviderIDPrefix + vendor + "/" + region + "/" + nodeClaim
}

// ParseProviderID splits a provider ID into vendor, region and NodeClaim name.
func ParseProviderID(id string) (vendor, region, nodeClaim string, err error) {
	rest, ok := strings.CutPrefix(id, ProviderIDPrefix)
	if !ok {
		return "", "", "", fmt.Errorf("provider ID %q does not start with %s", id, ProviderIDPrefix)
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || slices.Contains(parts, "") {
		return "", "", "", fmt.Errorf("provider ID %q is not %s<vendor>/<region>/<nodeclaim>", id, ProviderIDPrefix)
	}
	return parts[0], parts[1], parts[2], nil
}
