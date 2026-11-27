/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import "testing"

func TestCheckShapeArch(t *testing.T) {
	for _, tc := range []struct {
		shape, arch string
		refused     bool
	}{
		{"VM.Standard.E5.Flex", "amd64", false},
		{"VM.Standard.E5.Flex", "arm64", true},
		{"VM.Standard.A2.Flex", "arm64", false},
		{"VM.Standard.A2.Flex", "amd64", true},
		{"BM.Standard.E6.Ax.192", "amd64", false},
		{"BM.Standard.E6.Ax.192", "arm64", true},
		{"VM.GPU.A10.1", "arm64", false},
		{"BM.GPU.GB200.4", "arm64", false},
		{"BM.GPU.GB200.4", "amd64", false},
	} {
		if err := checkShapeArch(tc.shape, tc.arch); (err != nil) != tc.refused {
			t.Errorf("checkShapeArch(%s, %s) = %v, refused want %v", tc.shape, tc.arch, err, tc.refused)
		}
	}
}
