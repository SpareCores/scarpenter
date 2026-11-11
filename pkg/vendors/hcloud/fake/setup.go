/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"fmt"
	"time"
)

// Kinds SetCreated knows.
const (
	KindImage          = "image"
	KindNetwork        = "network"
	KindFirewall       = "firewall"
	KindPlacementGroup = "placementgroup"
	KindVolume         = "volume"
)

// SetCreated sets the creation time of an object (tests that order by creation
// time seed objects in the past).
func (f *Fake) SetCreated(kind string, id int64, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch kind {
	case KindImage:
		if o := f.images[id]; o != nil {
			o.Created = at
			return
		}
	case KindNetwork:
		if o := f.networks[id]; o != nil {
			o.Created = at
			return
		}
	case KindFirewall:
		if o := f.firewalls[id]; o != nil {
			o.Created = at
			return
		}
	case KindPlacementGroup:
		if o := f.groups[id]; o != nil {
			o.Created = at
			return
		}
	case KindVolume:
		if o := f.volumes[id]; o != nil {
			o.Created = at
			return
		}
	}
	panic(fmt.Sprintf("fake: no %s with the ID %d", kind, id))
}

// SetLimits replaces the project limits (a zero field is unlimited).
func (f *Fake) SetLimits(l Limits) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limits = l
}
