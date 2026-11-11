/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package vultr holds the Vultr blocks of the SCNodeClass API: the vendor block
// (spec.vendors.vultr) and the empty Vultr blocks of the storage, network and
// placement profiles (spec.profiles.*.<name>.vultr). Vultr has no zones (the
// region is the unit), no Arm instances, and no tags on anything but instances,
// so the block is small: regions, a networking override and the resources.
//
// +kubebuilder:object:generate=true
package vultr
