/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package oci holds the Oracle Cloud Infrastructure API types of SCNodeClass: the
// vendor block (spec.vendors.oci). OCI has no storage, network or placement
// profile blocks in v1. Field names follow the Compute API. The built-in
// defaults named in the field documentation live in the driver, not in the CRD,
// so that "unset" stays visible; the driver validates the block against the
// tenancy (regions, shapes, resources).
//
// +kubebuilder:object:generate=true
package oci
