/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import "slices"

// AttachVolume attaches a block volume of another owner to the instance, the way
// a user does after the launch. Terminating the instance detaches it and keeps
// it: PreserveDataVolumesCreatedAtLaunch covers only volumes made at launch. It
// returns the volume's ID.
func (c *Cloud) AttachVolume(regionName, instanceID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.volumes == nil {
		c.volumes = map[string]string{}
	}
	id := c.nextID("volume", regionName)
	c.volumes[id] = instanceID
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

// detachVolumes detaches the volumes of a terminated instance. The caller holds
// the lock.
func (c *Cloud) detachVolumes(instanceID string) {
	for id, inst := range c.volumes {
		if inst == instanceID {
			c.volumes[id] = ""
		}
	}
}
