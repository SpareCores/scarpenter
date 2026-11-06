/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package v1alpha1

import (
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/fake"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/qemu"
)

// This file is the only place of the shared API that names vendors. The
// structs here have one typed field per vendor, because controller-gen
// generates one schema per vendor key (ADR-014, ADR-020); a vendor's driver
// plan adds its package under this directory and one line to each of
// VendorBlocks, StorageProfile, NetworkProfile and PlacementProfile (and to
// Names, Get and the For methods). Nothing else in the shared types changes.

// VendorBlocks has one block per vendor (spec.vendors). A vendor is enabled
// for the SCNodeClass by having a block; an empty block ({}) means "all
// defaults".
// +kubebuilder:validation:MinProperties=1
type VendorBlocks struct {
	// aws configures AWS nodes.
	// +optional
	AWS *aws.Settings `json:"aws,omitempty"`

	// fake configures the fake (KWOK) development vendor.
	// +optional
	Fake *fake.Settings `json:"fake,omitempty"`

	// qemu configures the qemu development vendor.
	// +optional
	QEMU *qemu.Settings `json:"qemu,omitempty"`
}

// Vendor names, as they appear as keys of the vendor and profile blocks and in
// the scarpenter.sparecores.com/vendor label.
const (
	VendorAWS  = "aws"
	VendorFake = "fake"
	VendorQEMU = "qemu"
)

// Names returns the vendors that have a block, in a fixed order.
func (v VendorBlocks) Names() []string {
	var names []string
	if v.AWS != nil {
		names = append(names, VendorAWS)
	}
	if v.Fake != nil {
		names = append(names, VendorFake)
	}
	if v.QEMU != nil {
		names = append(names, VendorQEMU)
	}
	return names
}

// StorageProfile is a named storage setup, in the vendor's own terms, for each
// vendor it applies to. On a vendor without a block the profile does not exist.
// +kubebuilder:validation:MinProperties=1
type StorageProfile struct {
	// +optional
	AWS *aws.Storage `json:"aws,omitempty"`
	// +optional
	Fake *fake.Storage `json:"fake,omitempty"`
	// +optional
	QEMU *qemu.Storage `json:"qemu,omitempty"`
}

// NetworkProfile is a named network setup (NIC features) per vendor.
// +kubebuilder:validation:MinProperties=1
type NetworkProfile struct {
	// +optional
	AWS *aws.Network `json:"aws,omitempty"`
	// +optional
	Fake *fake.Network `json:"fake,omitempty"`
	// +optional
	QEMU *qemu.Network `json:"qemu,omitempty"`
}

// PlacementProfile is a named placement and tenancy setup per vendor.
// +kubebuilder:validation:MinProperties=1
type PlacementProfile struct {
	// +optional
	AWS *aws.Placement `json:"aws,omitempty"`
	// +optional
	Fake *fake.Placement `json:"fake,omitempty"`
	// +optional
	QEMU *qemu.Placement `json:"qemu,omitempty"`
}

// The accessors below give the rest of the code generic access to a vendor's
// block by name, so nothing outside this file switches on vendor names.

// Get returns the vendor's block (a pointer to the vendor package's Settings)
// and whether the vendor has one.
func (v VendorBlocks) Get(vendor string) (any, bool) {
	switch vendor {
	case VendorAWS:
		return orNil(v.AWS), v.AWS != nil
	case VendorFake:
		return orNil(v.Fake), v.Fake != nil
	case VendorQEMU:
		return orNil(v.QEMU), v.QEMU != nil
	}
	return nil, false
}

// For returns the vendor's block of the profile (a pointer to the vendor
// package's storage type) and whether the profile has one.
func (p StorageProfile) For(vendor string) (any, bool) {
	switch vendor {
	case VendorAWS:
		return orNil(p.AWS), p.AWS != nil
	case VendorFake:
		return orNil(p.Fake), p.Fake != nil
	case VendorQEMU:
		return orNil(p.QEMU), p.QEMU != nil
	}
	return nil, false
}

// For returns the vendor's block of the profile and whether it has one.
func (p NetworkProfile) For(vendor string) (any, bool) {
	switch vendor {
	case VendorAWS:
		return orNil(p.AWS), p.AWS != nil
	case VendorFake:
		return orNil(p.Fake), p.Fake != nil
	case VendorQEMU:
		return orNil(p.QEMU), p.QEMU != nil
	}
	return nil, false
}

// For returns the vendor's block of the profile and whether it has one.
func (p PlacementProfile) For(vendor string) (any, bool) {
	switch vendor {
	case VendorAWS:
		return orNil(p.AWS), p.AWS != nil
	case VendorFake:
		return orNil(p.Fake), p.Fake != nil
	case VendorQEMU:
		return orNil(p.QEMU), p.QEMU != nil
	}
	return nil, false
}

// orNil returns the pointer as an any, or a plain nil when it is nil, so that
// callers can compare the result with nil.
func orNil[T any](p *T) any {
	if p == nil {
		return nil
	}
	return p
}
