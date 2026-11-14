/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog

import (
	"github.com/SpareCores/scarpenter/pkg/catalog/model"
)

// uniqueAPIReferences is rule R: servers of one vendor that share an
// api_reference take their server ID as APIReference, so that every type has
// its own name (the sized types of OCI share the name of their shape in
// Keeper's rows). A server whose reference is unique keeps it. The rule is
// data-driven and names no vendor; with no duplicates it changes nothing.
//
// Every source calls it once, after its servers are loaded and before the
// metrics and selections are read: they key by APIReference. It returns the
// number of servers renamed.
func uniqueAPIReferences(data *model.Data) int {
	type ref struct{ vendor, apiReference string }
	count := map[ref]int{}
	for i := range data.Servers {
		s := &data.Servers[i]
		count[ref{s.Vendor, s.APIReference}]++
	}
	renamed := 0
	for i := range data.Servers {
		s := &data.Servers[i]
		if count[ref{s.Vendor, s.APIReference}] > 1 {
			s.APIReference = s.ID
			renamed++
		}
	}
	return renamed
}
