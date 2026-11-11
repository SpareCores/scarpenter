/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package ovh holds the OVHcloud Public Cloud blocks of the SCNodeClass API:
// the vendor block (spec.vendors.ovh) and the OVH blocks of the storage and
// placement profiles (spec.profiles.*.<name>.ovh). OVH has no network profile:
// bandwidth is per flavor and a node has one public Ext-Net port. Field names
// follow the OpenStack APIs (Nova, Cinder, Glance). The built-in defaults named
// in the field documentation live in the driver, not in the CRD, so that "unset"
// stays visible.
//
// +kubebuilder:object:generate=true
package ovh
