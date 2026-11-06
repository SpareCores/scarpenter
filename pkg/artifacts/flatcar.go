/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package artifacts

import (
	"fmt"
	"regexp"
	"strings"
)

// Well-known files of a Flatcar release; both architectures publish the UEFI
// qemu image, which the qemu driver boots.
const (
	// FlatcarImage is the compressed disk image of a release.
	FlatcarImage = "flatcar_production_qemu_uefi_image.img.bz2"
	// FlatcarImageSignature and FlatcarDigests are its detached signature and
	// its digests.
	FlatcarImageSignature = FlatcarImage + ".sig"
	FlatcarDigests        = FlatcarImage + ".DIGESTS"
	// FlatcarEFICode and FlatcarEFIVars are the firmware files of the image.
	FlatcarEFICode = "flatcar_production_qemu_uefi_efi_code.qcow2"
	FlatcarEFIVars = "flatcar_production_qemu_uefi_efi_vars.qcow2"
	// FlatcarVersionFile describes the release.
	FlatcarVersionFile = "version.txt"
)

var (
	channelPattern        = regexp.MustCompile(`^(stable|beta|alpha|lts)$`)
	flatcarVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	releaseFilePattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// FlatcarURL is the URL of a file of a Flatcar release:
// <base>/<arch>-usr/<version>/<file>, where the base defaults to
// https://<channel>.release.flatcar-linux.net. arch is amd64 or arm64.
func (r *Resolver) FlatcarURL(channel, arch, version, file string) (string, error) {
	switch {
	case !channelPattern.MatchString(channel):
		return "", fmt.Errorf("invalid Flatcar channel %q", channel)
	case arch != ArchAMD64 && arch != ArchARM64:
		return "", fmt.Errorf("unsupported architecture %q (want %s or %s)", arch, ArchAMD64, ArchARM64)
	case !flatcarVersionPattern.MatchString(version):
		return "", fmt.Errorf("invalid Flatcar version %q", version)
	case !releaseFilePattern.MatchString(file):
		return "", fmt.Errorf("invalid release file name %q", file)
	}
	base := strings.ReplaceAll(r.flatcarBase, "{channel}", channel)
	return fmt.Sprintf("%s/%s-usr/%s/%s", base, arch, version, file), nil
}
