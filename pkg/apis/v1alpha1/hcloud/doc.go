/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package hcloud holds the Hetzner Cloud blocks of the SCNodeClass API: the
// vendor block (spec.vendors.hcloud) and the hcloud blocks of the storage and
// placement profiles (spec.profiles.*.<name>.hcloud). There is no network
// profile: Hetzner has no NIC options. Field names follow the Hetzner Cloud API.
// The root disk is the server type's and is never configured here; locations are
// the regions. The driver validates the blocks against the API's limits (volume
// sizes, SSH key and ID forms).
//
// +kubebuilder:object:generate=true
package hcloud
