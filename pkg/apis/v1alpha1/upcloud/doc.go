/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package upcloud holds the UpCloud blocks of the SCNodeClass API: the vendor
// block (spec.vendors.upcloud) and the UpCloud blocks of the storage and
// placement profiles (spec.profiles.*.<name>.upcloud). UpCloud has no network
// profile: nicModel exists and has no use (ADR-017). Field names follow the
// UpCloud API. The built-in defaults named in the field documentation live in
// the driver, not in the CRD, so that "unset" stays visible; the driver
// validates the blocks against the account's plans and storage limits.
//
// +kubebuilder:object:generate=true
package upcloud
