/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package common holds the building blocks every vendor block of the
// SCNodeClass API is made of: vendor resources with use, create or forbid and
// their lifecycles, the generic networking settings, the guest side of extra
// volumes and local storage. Vendor packages (../aws, ../fake, ../qemu) compose
// them with their own native fields; this package knows no vendor.
//
// +kubebuilder:object:generate=true
package common
