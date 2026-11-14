/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package main

// The OCI driver is a production driver: its init registers it (overview section
// 7.3); it runs only where --vendors enables it, with --oci-compartment and the
// credentials of --oci-auth.
import _ "github.com/SpareCores/scarpenter/pkg/vendors/oci"
