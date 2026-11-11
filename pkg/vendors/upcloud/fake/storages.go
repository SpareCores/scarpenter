/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"context"
	"time"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"
)

// AddStorage adds a private, online, unattached disk (a storage a failed
// create left behind) and returns its UUID.
func (f *Fake) AddStorage(zone, title, tier string, sizeGB int, labels map[string]string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := &storage{}
	st.d.UUID = f.uuid('s')
	st.d.Zone, st.d.Title, st.d.Tier, st.d.Size = zone, title, tier, sizeGB
	st.d.Access, st.d.Type = upcloud.StorageAccessPrivate, upcloud.StorageTypeDisk
	st.d.State = upcloud.StorageStateOnline
	for _, k := range sortedKeys(labels) {
		st.d.Labels = append(st.d.Labels, upcloud.Label{Key: k, Value: labels[k]})
	}
	f.storages[st.d.UUID] = st
	return st.d.UUID
}

// DeleteStorage deletes a storage that no server uses (STORAGE_IN_USE, 409,
// otherwise).
func (f *Fake) DeleteStorage(ctx context.Context, r *request.DeleteStorageRequest) error {
	done, err := f.begin(ctx, "DeleteStorage")
	if err != nil {
		return err
	}
	defer done()
	st, ok := f.storages[r.UUID]
	if !ok {
		return notFound("STORAGE_NOT_FOUND")
	}
	for _, s := range f.servers {
		for _, d := range s.d.StorageDevices {
			if d.UUID == st.d.UUID {
				return conflict("STORAGE_IN_USE")
			}
		}
	}
	delete(f.storages, r.UUID)
	return nil
}

// AddTemplateWithID adds a private template under a chosen ID (a fixture that
// launches with a fixed image reference).
func (f *Fake) AddTemplateWithID(id, zone, title, tier string, sizeGB int, labels map[string]string) {
	uuid := f.AddTemplate(zone, title, tier, sizeGB, labels)
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.storages[uuid]
	delete(f.storages, uuid)
	st.d.UUID = id
	f.storages[id] = st
}

// SetStorageCreated sets the creation time of a storage (a template that reports one).
func (f *Fake) SetStorageCreated(uuid string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st, ok := f.storages[uuid]; ok {
		st.d.Created = at
	}
}
