/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	maxNameLen   = 63
	truncatedLen = 56
	hashLen      = 6
)

// InstanceTypeName returns the Karpenter instance type name of a server:
// <vendor>.<api_reference> (ADR-008). Characters outside the label-value
// charset become "-"; a name that is longer than 63 characters is truncated to
// 56 and suffixed with "-" and 6 hex characters of the SHA-256 of the full
// name. A name that sanitising changed also gets the hash suffix, so two raw
// names that differ only in replaced characters cannot collide.
func InstanceTypeName(vendor, apiReference string) string {
	full := vendor + "." + apiReference
	clean := sanitise(full)
	if clean == full && len(full) <= maxNameLen {
		return full
	}
	sum := sha256.Sum256([]byte(full))
	suffix := "-" + hex.EncodeToString(sum[:])[:hashLen]
	if len(clean) > truncatedLen {
		clean = clean[:truncatedLen]
	}
	// The name must end with an alphanumeric before the suffix is attached.
	clean = strings.TrimRight(clean, "-_.")
	return clean + suffix
}

func sanitise(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-_.")
}
