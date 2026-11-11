/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package main

// The Hetzner Cloud driver is a production driver: its init registers it; it
// runs only where --vendors enables it and HCLOUD_TOKEN is set.
import _ "github.com/SpareCores/scarpenter/pkg/vendors/hcloud"
