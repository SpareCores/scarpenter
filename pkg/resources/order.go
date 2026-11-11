/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package resources

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// The resolution order of ADR-022, for a kind in a region:
//
//  1. explicit `use` (resolved by the callers: it never falls through);
//  2. discovery: resources carrying the opt-in tag, with the kind's default select;
//  3. the vendor's default, where it exists and honours the request (ADR-017);
//  4. creation (ensure), unless the kind is forbidden;
//  5. a failure that names the kind and what was tried.
//
// Steps 2 and 3 are skipped for a kind with an explicit `create`. This file is
// steps 2 and 3, which both the launch (require) and the status check run. A step
// that finds nothing goes on; a step that errors, a vendor error or an ambiguous
// opt-in, stops the order: it never falls through to creation.

// orderIn is what steps 2 and 3 need.
type orderIn struct {
	nc     *v1alpha1.SCNodeClass
	drv    vendors.Driver
	mr     vendors.ManagedResources
	vendor string
	spec   vendors.KindSpec
	region string
	// settings are the resolved settings of the launch, for the default's suitability;
	// nil means the NodeClass's default settings.
	settings *vendors.VendorSettings
	// networks are the networks the kind is constrained to (the kinds that live
	// in a network); empty: unconstrained.
	networks []string
}

// found is what steps 2 and 3 produced.
type found struct {
	refs   []vendors.ResourceRef
	source v1alpha1.ResourceSource
	// tried says what the steps that found nothing did, for the failure message.
	tried []string
}

// optIn is the opt-in tag as the vendor spells it.
func (in orderIn) optIn() string {
	return in.drv.TagKey(vendors.TagUse) + "=" + vendors.OptInValue
}

// discoverAndDefault runs steps 2 and 3. An empty result with tried set means
// that both found nothing; err is a vendor error or an ambiguous opt-in
// (isSelection), which end the order.
func (e *Engine) discoverAndDefault(ctx context.Context, in orderIn, forbid bool) (found, error) {
	var out found
	if in.spec.Scope != vendors.ScopeRegion && !forbid {
		// A run or node kind is per placement run or per instance: one tagged or default
		// resource cannot stand for them. (A forbidden kind still honours a default the
		// driver declares, as before.)
		return out, nil
	}
	if in.spec.Scope == vendors.ScopeRegion {
		refs, note, err := e.discover(ctx, in)
		if err != nil {
			return out, err
		}
		if len(refs) > 0 {
			return found{refs: refs, source: v1alpha1.ResourceSourceDiscovered}, nil
		}
		out.tried = append(out.tried, note)
	}

	refs, note, err := e.vendorDefault(ctx, in)
	if err != nil {
		return out, err
	}
	if len(refs) > 0 {
		return found{refs: refs, source: v1alpha1.ResourceSourceDefault}, nil
	}
	out.tried = append(out.tried, note)
	return out, nil
}

// discover is step 2: the resources tagged with the opt-in tag. A kind whose
// capability declaration has no tags skips the step.
func (e *Engine) discover(ctx context.Context, in orderIn) ([]vendors.ResourceRef, string, error) {
	caps := in.mr.Selectors()[in.spec.Kind]
	if caps.Tags == vendors.TagsNone {
		return nil, "discovery skipped: " + string(in.spec.Kind) + " has no tags", nil
	}
	tag := in.optIn()
	nothing := "the opt-in tag " + tag + ": nothing is tagged"
	var refs []vendors.ResourceRef
	var err error
	if d, ok := in.drv.(vendors.ResourceDiscoverer); ok {
		key := fmt.Sprintf("%s/%s/%s/discover", in.vendor, in.spec.Kind, in.region)
		refs, err = e.cached(key, func() ([]vendors.ResourceRef, error) {
			return d.Discover(ctx, in.spec.Kind, in.region)
		})
	} else {
		ref := vendors.UseRef{
			Tags:   map[string]string{in.drv.TagKey(vendors.TagUse): vendors.OptInValue},
			Select: caps.DefaultSelect,
		}
		in.constrain(&ref, caps)
		refs, err = e.resolve(ctx, in.mr, in.vendor, in.spec.Kind, in.region, ref)
	}
	switch {
	case isSelection(err) && !isAmbiguous(err):
		return nil, nothing, nil
	case isSelection(err):
		return nil, "", fmt.Errorf("the opt-in tag %s is ambiguous: %w", tag, err)
	case err != nil:
		return nil, "", err
	}
	refs = in.inNetworks(refs)
	if len(refs) == 0 {
		return nil, nothing + networkNote(in.networks), nil
	}
	return refs, "", nil
}

// vendorDefault is step 3: the vendor's default, if the driver has one and it
// honours the request.
func (e *Engine) vendorDefault(ctx context.Context, in orderIn) ([]vendors.ResourceRef, string, error) {
	d, ok := in.drv.(vendors.ResourceDefaults)
	if !ok {
		return nil, "no vendor default for " + string(in.spec.Kind), nil
	}
	key := fmt.Sprintf("%s/%s/%s/default", in.vendor, in.spec.Kind, in.region)
	refs, err := e.cached(key, func() ([]vendors.ResourceRef, error) {
		return d.Default(ctx, in.spec.Kind, in.region)
	})
	if err != nil {
		return nil, "", err
	}
	refs = in.inNetworks(refs)
	if len(refs) == 0 {
		return nil, "the vendor's default: none" + networkNote(in.networks), nil
	}
	if dc, ok := in.drv.(vendors.DefaultChecker); ok {
		s := in.settings
		if s == nil {
			s = e.defaultSettings(in.nc, in.vendor)
		}
		if s != nil {
			suits, reason, err := dc.DefaultSuits(ctx, in.spec.Kind, in.region, refs, *s)
			if err != nil {
				return nil, "", err
			}
			if !suits {
				return nil, "the vendor's default cannot honour the request: " + reason, nil
			}
		}
	}
	return refs, "", nil
}

// defaultSettings are the settings of the NodeClass without a profile: what the
// status check asks a default to honour. nil when the driver has no node
// options or the NodeClass does not resolve (the validation reports that).
func (e *Engine) defaultSettings(nc *v1alpha1.SCNodeClass, vendor string) *vendors.VendorSettings {
	drv, ok := e.opts.Registry.Driver(vendor)
	if !ok {
		return nil
	}
	no, ok := drv.(vendors.NodeOptions)
	if !ok {
		return nil
	}
	in, has, err := options.InputFromNodeClass(nc, vendor)
	if err != nil || !has {
		return nil
	}
	s, err := options.Resolve(no, in, options.Selection{})
	if err != nil {
		return nil
	}
	return &s
}

// constrain narrows a selector to the networks the kind is constrained to, where
// its capability declaration has match.network and the selector names none (an
// explicit match.network is the operator's and is kept).
func (in orderIn) constrain(ref *vendors.UseRef, caps vendors.SelectorCaps) {
	if len(in.networks) == 0 || len(ref.Match.Network) > 0 {
		return
	}
	if _, ok := caps.Match[vendors.MatchNetwork]; ok {
		ref.Match.Network = slices.Clone(in.networks)
	}
}

// inNetworks keeps the resources in the networks. A resource whose network the
// driver does not know is kept: the vendor-side match.network is the constraint
// there.
func (in orderIn) inNetworks(refs []vendors.ResourceRef) []vendors.ResourceRef {
	if len(in.networks) == 0 {
		return refs
	}
	return slices.DeleteFunc(slices.Clone(refs), func(r vendors.ResourceRef) bool {
		return r.Network != "" && !slices.Contains(in.networks, r.Network)
	})
}

func networkNote(networks []string) string {
	if len(networks) == 0 {
		return ""
	}
	return " in " + strings.Join(networks, ", ")
}

// failureMessage is the message of step 5: the kind and what was tried, ending with
// the creation step.
// The region is named when the message stands on its own (a launch's error), not
// when the caller prefixes it (the status condition).
func failureMessage(kind vendors.ResourceKind, region string, tried []string, create string) string {
	steps := slices.Clone(tried)
	if create != "" {
		steps = append(steps, create)
	}
	where := ""
	if region != "" {
		where = " in " + region
	}
	return fmt.Sprintf("%s%s: no resource found; tried %s", kind, where, strings.Join(steps, "; "))
}

// networksOf are the distinct networks of the resources the driver knows.
func networksOf(refs []vendors.ResourceRef) []string {
	var out []string
	for _, r := range refs {
		if r.Network != "" && !slices.Contains(out, r.Network) {
			out = append(out, r.Network)
		}
	}
	slices.Sort(out)
	return out
}

// inNetworkOrder sorts the kinds so that a kind that lives in a network comes
// after the kind that provides it (stable: the driver's order otherwise).
func inNetworkOrder(kinds []vendors.KindSpec) []vendors.KindSpec {
	out := make([]vendors.KindSpec, 0, len(kinds))
	for _, k := range kinds {
		if k.InNetwork == "" {
			out = append(out, k)
		}
	}
	for _, k := range kinds {
		if k.InNetwork != "" {
			out = append(out, k)
		}
	}
	return out
}
