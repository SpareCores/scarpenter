/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package v1alpha1 contains the API schema of the scarpenter.sparecores.com
// group: SCNodeClass, one node definition for many vendors, with the vendor and
// profile blocks of the vendors in the sub-packages (aws, fake, qemu), the
// label and annotation constants, and the registration of the labels with
// Karpenter core.
//
// Adding a vendor touches only its own sub-package and vendors.go in this
// package (one field in each aggregating struct).
//
// +kubebuilder:object:generate=true
// +groupName=scarpenter.sparecores.com
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Group is the API group of the SCarpenter API; it is also the domain of the
// labels and annotations SCarpenter owns.
const Group = "scarpenter.sparecores.com"

var (
	// SchemeGroupVersion is the group version used to register these objects.
	SchemeGroupVersion = schema.GroupVersion{Group: Group, Version: "v1alpha1"}

	// GroupVersion is an alias for SchemeGroupVersion.
	GroupVersion = SchemeGroupVersion

	// SchemeBuilder is used to add Go types to the GroupVersionKind scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(SchemeGroupVersion, &SCNodeClass{}, &SCNodeClassList{})
		metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
		return nil
	})

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
