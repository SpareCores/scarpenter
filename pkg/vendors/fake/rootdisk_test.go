//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake_test

import (
	"encoding/json"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/fake"
)

func TestMinRootDiskBytes(t *testing.T) {
	const gib = int64(1) << 30
	d := &fake.Driver{}
	for min, want := range map[int64]int64{0: 128 * gib, 64 * gib: 128 * gib, 1024 * gib: 1024 * gib, 1024*gib + 1: 1025 * gib} {
		s := vendors.VendorSettings{Raw: json.RawMessage(`{"storage":{"root":{"volumeSize":"128Gi"}}}`), MinRootDiskBytes: min}
		if got := d.RootDiskBytes(s); got != want {
			t.Errorf("minimum %d: root disk = %d GiB, want %d", min, got/gib, want/gib)
		}
	}
}
