/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vendors_test

import (
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func TestCheckImageVersion(t *testing.T) {
	req := vendors.ImageRequest{Region: "r1", Arch: "amd64", Channel: "stable", Version: "4757.2.3"}
	for name, tc := range map[string]struct {
		channel, version string
		code             string
	}{
		"the pinned release":                {"stable", "4757.2.3", ""},
		"a version without a channel":       {"", "4757.2.3", ""},
		"the channel in another case":       {"Stable", "4757.2.3", ""},
		"another version":                   {"stable", "4757.2.4", vendors.CodeImageVersionMismatch},
		"another channel":                   {"beta", "4757.2.3", vendors.CodeImageVersionMismatch},
		"another version without a channel": {"", "4081.3.1", vendors.CodeImageVersionMismatch},
		"no version":                        {"stable", "", vendors.CodeImageVersionUnknown},
		"says nothing":                      {"", "", vendors.CodeImageVersionUnknown},
	} {
		err := vendors.CheckImageVersion("img-1", tc.channel, tc.version, req)
		if tc.code == "" {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if vendors.CodeOf(err) != tc.code || vendors.ClassOf(err) != vendors.Fatal ||
			!strings.Contains(err.Error(), "img-1") || !strings.Contains(err.Error(), "r1") {
			t.Errorf("%s: %v, want a Fatal %s naming the image and the region", name, err, tc.code)
		}
	}
}
