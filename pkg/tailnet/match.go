/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package tailnet

import (
	"fmt"
	"slices"
	"strings"
)

func validateKeyRequest(op string, r KeyRequest) error {
	if len(r.Tags) == 0 {
		return &Error{Class: Fatal, Op: op, Message: "a per-node key needs at least one tag"}
	}
	for _, t := range r.Tags {
		if !strings.HasPrefix(t, "tag:") || t != strings.ToLower(t) || strings.ContainsAny(t, " \t") {
			return &Error{Class: Fatal, Op: op, Message: fmt.Sprintf("tag %q must be lowercase and start with \"tag:\"", t)}
		}
	}
	if r.Expiry <= 0 {
		return &Error{Class: Fatal, Op: op, Message: "the key's expiry must be positive"}
	}
	return nil
}

func validateDeviceQuery(op string, q DeviceQuery) error {
	if len(q.KeyIDs) == 0 && !q.IP.IsValid() && q.Hostname == "" {
		return &Error{Class: Fatal, Op: op, Message: "a device query needs a key ID, an IP or a hostname"}
	}
	return nil
}

// device is a listed device with the key that admitted it, where the API tells.
type device struct {
	Device
	keyID string
}

// pickDevice applies the query to a listing and returns the single match.
//
// With KeyIDs the match is exact on the admitting key. Otherwise every given field must
// match (IP among the addresses, hostname equal, all tags present, created after). Several
// matches are never resolved by guessing.
func pickDevice(op string, devs []device, q DeviceQuery) (Device, error) {
	var found []Device
	for _, d := range devs {
		if matches(d, q) {
			found = append(found, d.Device)
		}
	}
	switch len(found) {
	case 0:
		return Device{}, &Error{Class: NotFound, Op: op, Message: "no device matches the query"}
	case 1:
		return found[0], nil
	}
	return Device{}, &Error{Class: Ambiguous, Op: op, Message: fmt.Sprintf("%d devices match the query", len(found))}
}

func matches(d device, q DeviceQuery) bool {
	if len(q.KeyIDs) > 0 && !slices.Contains(q.KeyIDs, d.keyID) {
		return false
	}
	if q.IP.IsValid() && !slices.Contains(d.Addresses, q.IP) {
		return false
	}
	if q.Hostname != "" && !strings.EqualFold(d.Hostname, q.Hostname) {
		return false
	}
	for _, t := range q.Tags {
		if !slices.Contains(d.Tags, t) {
			return false
		}
	}
	return q.CreatedAfter.IsZero() || d.Created.After(q.CreatedAfter)
}
