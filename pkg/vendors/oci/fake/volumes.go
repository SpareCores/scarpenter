/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import "slices"

// AttachVolume makes a block volume of another owner, as a user attaches one
// after the launch. Terminating an instance never touches it
// (PreserveDataVolumesCreatedAtLaunch covers only volumes made at launch), so the
// volume is a tripwire for a driver that starts deleting by attachment. It returns
// the volume's ID.
func (c *Cloud) AttachVolume(regionName string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.volumes == nil {
		c.volumes = map[string]bool{}
	}
	id := c.nextID("volume", regionName)
	c.volumes[id] = true
	return id
}

// Volumes returns the IDs of the block volumes of AttachVolume that exist.
func (c *Cloud) Volumes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.volumes))
	for id := range c.volumes {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}
