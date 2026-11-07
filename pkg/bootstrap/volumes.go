/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package bootstrap

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// VolumeHint says how the guest finds an extra volume and what to put on it
// (00a section 11); see vendors.DeviceHint.
type VolumeHint struct {
	Name       string
	Filesystem string
	MountPath  string
	// ByID is the stable device path, SerialPattern with SizeBytes the vendor
	// serial pattern: exactly one of them is set.
	ByID          string
	SerialPattern string
	SizeBytes     int64
}

// LocalHint configures the instance-local disks (policy mount or raid0).
type LocalHint struct {
	Policy        string
	MountPath     string
	DevicePattern string
	Count         int
}

// VolumeData is a volume as the volumes template sees it.
type VolumeData struct {
	Name, Filesystem, Label, Where string
	ServiceUnit, MountUnit, Args   string
}

// LocalMount is one mount of the local storage.
type LocalMount struct {
	Label, Where, MountUnit string
}

// LocalData is the local storage as the volumes template sees it.
type LocalData struct {
	Args   string
	Mounts []LocalMount
}

var (
	volumeNamePattern   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	byIDPattern         = regexp.MustCompile(`^/dev/disk/by-[a-z]+/[A-Za-z0-9._:+@=-]+$`)
	serialPattern       = regexp.MustCompile(`^[A-Za-z0-9._*?-]+$`)
	devicePatternRegexp = regexp.MustCompile(`^[A-Za-z0-9._:+@=*?-]+$`)
	mountPathPattern    = regexp.MustCompile(`^/[A-Za-z0-9._/-]*$`)
	filesystems         = map[string]bool{"ext4": true, "xfs": true, "btrfs": true}
)

// volumeLabel is the filesystem label of a volume: short enough for XFS (12
// characters) and derived from the name, so that it never changes.
func volumeLabel(name string) string {
	sum := sha256.Sum256([]byte(name))
	return "scv-" + hex.EncodeToString(sum[:])[:8]
}

// systemdPath escapes a path the way systemd-escape --path does, for the name
// of a mount unit.
func systemdPath(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return "-"
	}
	var b strings.Builder
	for i := range len(p) {
		c := p[i]
		switch {
		case c == '/':
			b.WriteByte('-')
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == ':', c == '_', c == '.' && i > 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, `\x%02x`, c)
		}
	}
	return b.String()
}

func mountUnit(path string) string { return systemdPath(path) + ".mount" }

func cleanMountPath(what, p string) error {
	if !mountPathPattern.MatchString(p) || strings.Contains(p, "//") || strings.Contains(p, "/../") ||
		strings.HasSuffix(p, "/..") || p == "/" {
		return fmt.Errorf("the mount path %q of %s is not a plain absolute path (letters, digits, . _ - and /)", p, what)
	}
	return nil
}

// volumesData validates the hints and builds the template data; the checks are
// strict because the values end up in systemd units.
func volumesData(volumes []VolumeHint, local *LocalHint) ([]VolumeData, *LocalData, error) {
	var out []VolumeData
	seenName, seenPath := map[string]bool{}, map[string]bool{}
	for _, v := range volumes {
		what := "the volume " + v.Name
		switch {
		case !volumeNamePattern.MatchString(v.Name):
			return nil, nil, fmt.Errorf("the volume name %q is not a lower-case label", v.Name)
		case seenName[v.Name]:
			return nil, nil, fmt.Errorf("the volume %s appears twice", v.Name)
		case !filesystems[v.Filesystem]:
			return nil, nil, fmt.Errorf("%s has the unsupported filesystem %q", what, v.Filesystem)
		case (v.ByID == "") == (v.SerialPattern == ""):
			return nil, nil, fmt.Errorf("%s needs exactly one of a by-id path and a serial pattern", what)
		case v.ByID != "" && !byIDPattern.MatchString(v.ByID):
			return nil, nil, fmt.Errorf("%s has the invalid device path %q", what, v.ByID)
		case v.SerialPattern != "" && !serialPattern.MatchString(v.SerialPattern):
			return nil, nil, fmt.Errorf("%s has the invalid serial pattern %q", what, v.SerialPattern)
		case v.SizeBytes < 0:
			return nil, nil, fmt.Errorf("%s has a negative size", what)
		}
		if err := cleanMountPath(what, v.MountPath); err != nil {
			return nil, nil, err
		}
		if seenPath[v.MountPath] {
			return nil, nil, fmt.Errorf("the mount path %s is used twice", v.MountPath)
		}
		seenName[v.Name], seenPath[v.MountPath] = true, true
		label := volumeLabel(v.Name)
		args := []string{"--label", label, "--fs", v.Filesystem}
		if v.ByID != "" {
			args = append(args, "--by-id", v.ByID)
		} else {
			args = append(args, "--serial", v.SerialPattern, "--size", strconv.FormatInt(v.SizeBytes, 10))
		}
		out = append(out, VolumeData{
			Name: v.Name, Filesystem: v.Filesystem, Label: label, Where: v.MountPath,
			ServiceUnit: "scarpenter-volume-" + v.Name + ".service", MountUnit: mountUnit(v.MountPath),
			Args: strings.Join(args, " "),
		})
	}
	if local == nil {
		return out, nil, nil
	}
	if local.Policy != "mount" && local.Policy != "raid0" {
		return nil, nil, fmt.Errorf("the local storage policy %q is not mount or raid0", local.Policy)
	}
	if !devicePatternRegexp.MatchString(local.DevicePattern) {
		return nil, nil, fmt.Errorf("the local disk pattern %q is invalid", local.DevicePattern)
	}
	if local.Count < 0 || local.Count > 64 {
		return nil, nil, fmt.Errorf("the local disk count %d is out of range", local.Count)
	}
	if err := cleanMountPath("the local storage", local.MountPath); err != nil {
		return nil, nil, err
	}
	if seenPath[local.MountPath] {
		return nil, nil, fmt.Errorf("the mount path %s is used twice", local.MountPath)
	}
	ld := &LocalData{Args: fmt.Sprintf("--policy %s --pattern %s --count %d",
		local.Policy, local.DevicePattern, local.Count)}
	if local.Policy == "raid0" || local.Count <= 1 {
		label := "scl-raid"
		if local.Policy == "mount" {
			label = "scl-0"
		}
		ld.Mounts = []LocalMount{{Label: label, Where: local.MountPath, MountUnit: mountUnit(local.MountPath)}}
	} else {
		for i := range local.Count {
			where := local.MountPath
			if i > 0 {
				where = fmt.Sprintf("%s-%d", local.MountPath, i)
			}
			ld.Mounts = append(ld.Mounts, LocalMount{
				Label: "scl-" + strconv.Itoa(i), Where: where, MountUnit: mountUnit(where),
			})
		}
	}
	return out, ld, nil
}
