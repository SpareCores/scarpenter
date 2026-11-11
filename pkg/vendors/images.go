/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vendors

import (
	"fmt"
	"strings"
)

// Codes of the errors of an image that is not the pinned release (ADR-017,
// plan 14 A1). They are the reasons of the ImagesReady condition.
const (
	CodeImageVersionMismatch = "ImageVersionMismatch"
	CodeImageVersionUnknown  = "ImageVersionUnknown"
)

// IsImageVersionCode says whether an error code is one of the version check.
func IsImageVersionCode(code string) bool {
	return code == CodeImageVersionMismatch || code == CodeImageVersionUnknown
}

// CheckImageVersion compares the release an image says it is (its channel and
// version, read from its labels, tags, properties or name) with the pinned one
// in the request. A mismatch is an *Error of class Fatal with the code
// ImageVersionMismatch, an image that does not say (no version) is
// ImageVersionUnknown: the image is not ready and never booted, whoever named
// it. id is how the error names the image. The channel is compared only when
// the image gives one: a vendor whose images carry a version and no channel is
// still read by the version, which identifies a release.
func CheckImageVersion(id, channel, version string, req ImageRequest) error {
	if version == "" {
		return NewError(Fatal, CodeImageVersionUnknown, fmt.Errorf(
			"the image %s in %s does not say which Flatcar version it is (name, tags or labels); "+
				"%s %s is pinned and an image of unknown version is never booted",
			id, req.Region, req.Channel, req.Version))
	}
	if version != req.Version || (channel != "" && !strings.EqualFold(channel, req.Channel)) {
		have := version
		if channel != "" {
			have = channel + " " + version
		}
		return NewError(Fatal, CodeImageVersionMismatch, fmt.Errorf(
			"the image %s in %s is Flatcar %s, but %s %s is pinned", id, req.Region, have, req.Channel, req.Version))
	}
	return nil
}
