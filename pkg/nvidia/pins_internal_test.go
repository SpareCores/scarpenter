/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package nvidia

import "testing"

// A type stays unsupported while the pins it needs are not verified: an
// unpinned flavour is skipped, an unpinned runtime drops the release.
func TestUnpinnedBuildsAndRuntimeAreNotSelected(t *testing.T) {
	rel := Release{
		Version: "1.0.0",
		Branches: []Branch{
			{Number: 570, Version: "570.1.1", Open: Build{}, Proprietary: Build{PayloadSHA256: "a", RawSHA256: "b"}},
			{Number: 550, Version: "550.1.1", Open: Build{PayloadSHA256: "c", RawSHA256: "d"}},
		},
		RuntimeVersion: "v1.0.0", RuntimeSHA256: "e",
	}
	// Turing prefers 570-open, which has no pins: the pinned proprietary build
	// of the same branch (it supports Turing too) is used, and the label says so.
	sel, ok := rel.pick(Turing)
	if !ok || sel.Branch != "570" || sel.Flavour != Proprietary {
		t.Errorf("Turing: %+v, %v (570 proprietary supports it and is pinned)", sel, ok)
	}
	// Blackwell needs open modules from 580: nothing here.
	if _, ok := rel.pick(Blackwell); ok {
		t.Error("Blackwell was selected without a 580-open build")
	}
	rel.RuntimeSHA256 = ""
	if _, ok := rel.pick(Turing); ok {
		t.Error("a release without a runtime pin offers a type")
	}
}
