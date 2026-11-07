/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider

import (
	"fmt"
	"sync"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// settingsResolver resolves the vendor settings of the offerings of one
// Create (00a section 3: defaults, the NodeClass's vendor block, the selected
// profiles). The result depends only on the vendor and the selected profiles,
// so it is computed once per combination.
type settingsResolver struct {
	nc       *v1alpha1.SCNodeClass
	registry *vendors.Registry

	mu       sync.Mutex
	inputs   map[string]options.Input
	resolved map[string]resolvedSettings
}

type resolvedSettings struct {
	settings vendors.VendorSettings
	err      error
}

func newSettingsResolver(nc *v1alpha1.SCNodeClass, reg *vendors.Registry) *settingsResolver {
	return &settingsResolver{
		nc: nc, registry: reg, inputs: map[string]options.Input{}, resolved: map[string]resolvedSettings{},
	}
}

// nodeOptions returns the driver's NodeOptions capability; ok is false for a
// driver without it (its nodes have no settings).
func (r *settingsResolver) nodeOptions(vendor string) (vendors.NodeOptions, bool) {
	drv, ok := r.registry.Driver(vendor)
	if !ok {
		return nil, false
	}
	no, ok := drv.(vendors.NodeOptions)
	return no, ok
}

func (r *settingsResolver) input(vendor string) (options.Input, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if in, ok := r.inputs[vendor]; ok {
		return in, nil
	}
	in, ok, err := options.InputFromNodeClass(r.nc, vendor)
	if err != nil {
		return options.Input{}, err
	}
	if !ok {
		return options.Input{}, fmt.Errorf("the SCNodeClass has no block for the vendor %s", vendor)
	}
	r.inputs[vendor] = in
	return in, nil
}

// settings returns the settings of the selection; a driver without NodeOptions
// gets the zero value with the applied profile names.
func (r *settingsResolver) settings(vendor string, sel options.Selection) (vendors.VendorSettings, error) {
	no, ok := r.nodeOptions(vendor)
	if !ok {
		return vendors.VendorSettings{Profiles: sel.Applied()}, nil
	}
	key := vendor + "/" + sel.Storage + "/" + sel.Network + "/" + sel.Placement
	r.mu.Lock()
	if c, ok := r.resolved[key]; ok {
		r.mu.Unlock()
		return c.settings, c.err
	}
	r.mu.Unlock()
	in, err := r.input(vendor)
	var s vendors.VendorSettings
	if err == nil {
		s, err = options.Resolve(no, in, sel)
	}
	r.mu.Lock()
	r.resolved[key] = resolvedSettings{settings: s, err: err}
	r.mu.Unlock()
	return s, err
}

// priceOf returns the USD price and completeness of an offering with the
// settings: the instance price plus the driver's price delta (00a section 10).
// Without a selected profile the catalog's own delta is used.
func (r *settingsResolver) priceOf(
	c *candidate, s vendors.VendorSettings, sel options.Selection,
) (usd, delta float64, complete bool) {
	no, ok := r.nodeOptions(c.vendor)
	if !ok || sel.IsDefault() || !c.priced {
		return c.catalogPrice.USD, c.catalogPrice.Delta, c.catalogPrice.Complete || !c.priced
	}
	d, complete := no.PriceDelta(s, vendors.Offering{
		Region: c.region, Zone: c.zone, CapacityType: c.capacityType, APIReference: c.apiReference,
		Type: catalogTypeOf(c),
	})
	return c.catalogPrice.Base + d, d, complete
}
