/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package gcp holds the GCP (Compute Engine) blocks of the SCNodeClass API: the
// vendor block (spec.vendors.gcp) and the GCP blocks of the storage, network and
// placement profiles (spec.profiles.*.<name>.gcp). Field names follow the Compute
// API. The built-in defaults named in the field documentation live in the
// driver, not in the CRD, so that "unset" stays visible; the driver validates the
// blocks against the catalog (disk sizes, IOPS, series support).
//
// +kubebuilder:object:generate=true
package gcp
