/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package azure holds the Azure blocks of the SCNodeClass API: the vendor block
// (spec.vendors.azure) and the Azure blocks of the storage, network and placement
// profiles (spec.profiles.*.<name>.azure). Field names follow the Azure Resource
// Manager API and CLI (storageAccountType, diskSizeGB, acceleratedNetworking). The
// built-in defaults named in the field documentation live in the driver, not in the
// CRD, so that "unset" stays visible; the driver validates the blocks against the
// subscription's SKU table (premium storage, disk counts, ephemeral disks).
//
// +kubebuilder:object:generate=true
package azure
