/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package options_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws"
)

// TestForType: the root disk grows with the memory of the type, never below the
// configured size, and not at all when the NodeClass sets no rootVolume.
func TestForType(t *testing.T) {
	const gib = int64(1) << 30
	no := &aws.Driver{}
	in := options.Input{Block: []byte(`{"storage":{"root":{"volumeSize":"128Gi"}}}`)}
	s, err := options.Resolve(no, in, options.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	in.RootVolume = &v1alpha1.RootVolumeSpec{MemoryMultiplier: new(resource.MustParse("1"))}
	for memoryMiB, want := range map[float64]int64{16 << 10: 128, 1 << 20: 1024} {
		if got := options.ForType(no, in, s, memoryMiB).RootDiskBytes; got != want*gib {
			t.Errorf("%v MiB: root disk = %d GiB, want %d", memoryMiB, got/gib, want)
		}
	}
	if got := options.ForType(no, options.Input{}, s, 1<<20); got.MinRootDiskBytes != 0 || got.RootDiskBytes != 128*gib {
		t.Errorf("without rootVolume the settings changed: %+v", got)
	}
}
