/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vendors

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// Factory returns a fresh, uninitialised driver.
type Factory func() Driver

var (
	factoriesMu sync.Mutex
	factories   = map[string]Factory{}
)

// Register adds a driver factory under its registration name. Driver packages
// call it from init(); the production drivers are imported by cmd/controller,
// the development drivers only with the scarpenter_dev build tag (overview
// section 7.3). It panics on a duplicate name, which is a programming error.
func Register(name string, f Factory) {
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	if _, dup := factories[name]; dup {
		panic(fmt.Sprintf("vendors: driver %q registered twice", name))
	}
	factories[name] = f
}

// Registered returns the registration names, sorted.
func Registered() []string {
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	names := make([]string, 0, len(factories))
	for n := range factories {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// Registry holds the initialised drivers of the enabled vendors, by the
// vendor ID they report (a driver may serve another vendor's ID than its
// registration name: the fake driver impersonates a Keeper vendor).
type Registry struct {
	drivers map[string]Driver
}

// NewRegistry instantiates and initialises the enabled drivers (the --vendors
// flag): every name must be registered.
func NewRegistry(ctx context.Context, enabled []string, cfg Config) (*Registry, error) {
	r := &Registry{drivers: map[string]Driver{}}
	for _, name := range enabled {
		factoriesMu.Lock()
		f, ok := factories[name]
		factoriesMu.Unlock()
		if !ok {
			return nil, fmt.Errorf("vendor %q is not available in this build (available: %s)",
				name, strings.Join(Registered(), ", "))
		}
		d := f()
		if err := d.Init(ctx, cfg); err != nil {
			return nil, fmt.Errorf("initialising the %s driver: %w", name, err)
		}
		if prev, dup := r.drivers[d.Vendor()]; dup {
			return nil, fmt.Errorf("vendors %q and %q both serve vendor ID %q", prev.Vendor(), name, d.Vendor())
		}
		r.drivers[d.Vendor()] = d
	}
	return r, nil
}

// NewRegistryFrom builds a registry from initialised drivers, for tests.
func NewRegistryFrom(drivers ...Driver) *Registry {
	r := &Registry{drivers: map[string]Driver{}}
	for _, d := range drivers {
		r.drivers[d.Vendor()] = d
	}
	return r
}

// Driver returns the driver serving the vendor ID.
func (r *Registry) Driver(vendor string) (Driver, bool) {
	d, ok := r.drivers[vendor]
	return d, ok
}

// Vendors returns the enabled vendor IDs, sorted.
func (r *Registry) Vendors() []string {
	out := make([]string, 0, len(r.drivers))
	for v := range r.drivers {
		out = append(out, v)
	}
	slices.Sort(out)
	return out
}

// Enabled reports whether a driver serves the vendor ID.
func (r *Registry) Enabled(vendor string) bool {
	_, ok := r.drivers[vendor]
	return ok
}
