/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package alicloud holds the Alibaba Cloud blocks of the SCNodeClass API: the
// vendor block (spec.vendors.alicloud) and the alicloud blocks of the storage,
// network and placement profiles (spec.profiles.*.<name>.alicloud). Field names
// follow the ECS API (RunInstances, DescribeDeploymentSets). The built-in
// defaults named in the field documentation live in the driver, not in the CRD,
// so that "unset" stays visible; the driver validates the blocks against the
// catalog (disk sizes and performance levels, instance type support).
//
// +kubebuilder:object:generate=true
package alicloud
