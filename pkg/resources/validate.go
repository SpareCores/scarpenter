/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package resources

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// ValidateUses checks every `use` block of the NodeClass's vendor resources
// against the driver's capability declaration (ManagedResources.Selectors): a
// field, match attribute or filter name the driver does not support for the
// kind is rejected, never ignored, and a block that selects nothing is invalid
// (an old field pruned by the API server leaves one; it is never "the
// default"). Placeholders are checked here too.
func ValidateUses(nc *v1alpha1.SCNodeClass, vendor string, mr vendors.ManagedResources) error {
	cfgs, err := options.ResourceConfig(nc, vendor)
	if err != nil {
		return err
	}
	caps := mr.Selectors()
	var errs []error
	for _, kind := range slices.Sorted(maps.Keys(cfgs)) {
		cfg := cfgs[kind]
		if cfg.Use == nil {
			continue
		}
		path := fmt.Sprintf("vendors.%s.resources.%s.use", vendor, kind)
		c, ok := caps[vendors.ResourceKind(kind)]
		if !ok {
			var names []string
			for k := range caps {
				names = append(names, string(k))
			}
			slices.Sort(names)
			errs = append(errs, fmt.Errorf("%s: %s of %s does not accept use (kinds with use: %s)", path, kind, vendor,
				strings.Join(names, ", ")))
			continue
		}
		errs = append(errs, selector.Validate(vendor, vendors.ResourceKind(kind), path, cfg.Use, c)...)
	}
	return errors.Join(errs...)
}

// CheckUsesSelect rejects a `use` block that selects nothing, for every vendor
// of the NodeClass, whether or not its driver is enabled. An object stored
// before a field was renamed is pruned on read and CEL does not run again until
// it is written, so its `use` can be present and empty: that is invalid, never
// "nothing given" (which would fall back to the vendor's default or to
// creating the resource).
func CheckUsesSelect(nc *v1alpha1.SCNodeClass) error {
	var errs []error
	for _, vendor := range nc.Spec.Vendors.Names() {
		cfgs, err := options.ResourceConfig(nc, vendor)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, kind := range slices.Sorted(maps.Keys(cfgs)) {
			if u := cfgs[kind].Use; u != nil && !selector.Selects(u) {
				errs = append(errs, fmt.Errorf("vendors.%s.resources.%s.use: selects nothing: set at least one of ids, "+
					"name, tags, match, filters and expression (a field the API no longer has was pruned)", vendor, kind))
			}
		}
	}
	return errors.Join(errs...)
}
