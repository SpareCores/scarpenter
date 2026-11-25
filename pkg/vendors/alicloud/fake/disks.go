/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import "slices"

// diskRec is a data disk of another owner that is attached to an instance.
type diskRec struct {
	instance string // "" when detached
}

// AttachDisk creates a data disk of another owner and attaches it to the
// instance with the default DeleteWithInstance=false: releasing the instance
// detaches the disk and keeps it. It returns the disk's ID.
func (f *Cloud) AttachDisk(regionName, instanceID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.mustRegion(regionName)
	if r.disks == nil {
		r.disks = map[string]*diskRec{}
	}
	id := f.nextID("d")
	r.disks[id] = &diskRec{instance: instanceID}
	return id
}

// DiskIDs lists the IDs of the disks of AttachDisk that exist, sorted.
func (f *Cloud) DiskIDs(regionName string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.mustRegion(regionName).disks))
	for id := range f.mustRegion(regionName).disks {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// releaseDisks handles the disks of a released instance.
func (r *regionState) releaseDisks(instanceID string) {
	for _, d := range r.disks {
		if d.instance == instanceID {
			d.instance = ""
		}
	}
}
