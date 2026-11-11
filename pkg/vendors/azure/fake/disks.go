/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"context"
	"fmt"
	"net/http"
)

// ManagedDisks is the managed disk delete call. The driver has no disk API (the
// disks are inline and go with the VM); the sweeper deletes the ones a VM
// deleted with the Detach option left behind.
type ManagedDisks struct{ s *Subscription }

// ManagedDisks returns the disk delete API.
func (s *Subscription) ManagedDisks() *ManagedDisks { return &ManagedDisks{s} }

// Delete removes a disk; like ARM it refuses one that a VM still has attached
// and succeeds for one that does not exist.
func (a *ManagedDisks) Delete(ctx context.Context, rg, name string) error {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "disk.delete"); err != nil {
		return err
	}
	k := key(rg, name)
	owner, ok := s.disks[k]
	if !ok {
		return nil
	}
	if owner != "" {
		return Error(http.StatusConflict, "OperationNotAllowed",
			fmt.Sprintf("Disk %s is attached to virtual machine %s and cannot be deleted.", name, owner))
	}
	delete(s.disks, k)
	return nil
}
