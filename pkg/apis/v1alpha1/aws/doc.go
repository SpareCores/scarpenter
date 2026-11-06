/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package aws holds the AWS blocks of the SCNodeClass API: the vendor block
// (spec.vendors.aws) and the AWS blocks of the storage, network and placement
// profiles (spec.profiles.*.<name>.aws). Field names follow the EC2 API. The
// built-in defaults named in the field documentation live in the driver, not in
// the CRD, so that "unset" stays visible; the driver validates the blocks
// against the catalog (volume size and IOPS limits, instance type support).
//
// +kubebuilder:object:generate=true
package aws
