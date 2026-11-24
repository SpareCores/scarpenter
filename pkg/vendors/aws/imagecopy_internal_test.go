/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws

import (
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func TestCopyNameIsNeverTheOfficialOneAndFitsTheLimit(t *testing.T) {
	rel := vendors.Release{Channel: "stable", Version: "4757.2.3", Arch: "arm64"}
	name := copyName(rel, "my-cluster")
	if name != "scarpenter-flatcar-stable-4757.2.3-arm64-my-cluster" || imageNamePattern.MatchString(name) {
		t.Errorf("copy name %q", name)
	}
	long := copyName(rel, strings.Repeat("c", 200))
	if len(long) != maxImageName {
		t.Errorf("a long name has %d characters, want %d", len(long), maxImageName)
	}
	if other := copyName(rel, strings.Repeat("c", 199)+"d"); other == long {
		t.Error("two cluster names that differ past the cut share a copy name")
	}
}
