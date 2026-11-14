/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Codes of the Unsupported and Fatal errors the driver raises itself (plan 25,
// section 2).
const (
	CodeInvalidInstanceType = "InvalidInstanceType"
	CodeExtendedMemory      = "ExtendedMemoryUnsupported"
	CodeShapeNotInAD        = "ShapeNotInAvailabilityDomain"
)

var (
	// shapeRe is the shape grammar: a VM or BM prefix and the characters of
	// Oracle's shape names.
	shapeRe = regexp.MustCompile(`^(VM|BM)\.[A-Za-z0-9._-]+$`)
	// sizedTailRe is the size suffix of a named size: <ocpus>-<GB>, positive
	// integers without leading zeros.
	sizedTailRe = regexp.MustCompile(`^([1-9][0-9]*)-([1-9][0-9]*)$`)
	// malformedTailRe is a tail that looks like a size but is not one.
	malformedTailRe = regexp.MustCompile(`^[0-9]+(-[0-9]+)+$`)
	// armRe finds the Arm generation of a shape name (VM.Standard.A1.Flex,
	// BM.Standard.A4.Ax.48).
	armRe = regexp.MustCompile(`\.A([0-9]+)(?:\.|$)`)
)

// InstanceType is a parsed catalog type name: a fixed shape (BM.Standard3.64,
// VM.Standard2.1) or a flexible shape with a size (VM.Standard.E5.Flex.2-16).
type InstanceType struct {
	Shape string
	// Sized says that the name carries a size: OCPUs and GB are set. A fixed
	// shape has neither.
	Sized bool
	OCPUs int
	GB    int
}

// String is the type name, FormatType of the parts.
func (t InstanceType) String() string {
	if !t.Sized {
		return t.Shape
	}
	return FormatType(t.Shape, float64(t.OCPUs), float64(t.GB))
}

// ParseType parses a type name by the rule of plan 25, section 2: split at the
// last dot; a tail of the form <ocpus>-<gb> makes a sized name, anything else
// is a fixed shape. It checks the syntax only: whether the shape exists, is
// flexible and allows the size is checked against the availability domain's
// shape table (InstanceType.Check). A name that does not parse is rejected,
// never guessed (ADR-017): Unsupported with the code InvalidInstanceType, Fatal
// when the shape prefix is not VM. or BM. at all.
func ParseType(name string) (InstanceType, error) {
	if !shapeRe.MatchString(name) {
		return InstanceType{}, vendors.NewError(vendors.Fatal, CodeInvalidInstanceType,
			fmt.Errorf("%q is not an OCI shape or sized shape name (VM.<shape> or BM.<shape>)", name))
	}
	i := strings.LastIndexByte(name, '.')
	if m := sizedTailRe.FindStringSubmatch(name[i+1:]); m != nil {
		shape := name[:i]
		// "VM.Standard.E5.Flex.1.5-8" splits as shape "...Flex.1" with the size 5-8:
		// the sizes are integers, and nothing follows ".Flex" in a shape name.
		if !shapeRe.MatchString(shape) || (strings.Contains(shape, ".Flex") && !strings.HasSuffix(shape, ".Flex")) {
			return InstanceType{}, vendors.NewError(vendors.Unsupported, CodeInvalidInstanceType,
				fmt.Errorf("%q has the size %s on %q, which is not a shape name", name, name[i+1:], shape))
		}
		ocpus, errO := strconv.Atoi(m[1])
		gb, errG := strconv.Atoi(m[2])
		if errO != nil || errG != nil {
			return InstanceType{}, vendors.NewError(vendors.Unsupported, CodeInvalidInstanceType,
				fmt.Errorf("%q has a size out of range", name))
		}
		return InstanceType{Shape: shape, Sized: true, OCPUs: ocpus, GB: gb}, nil
	}
	// Not sized: a flexible shape without a (valid) size, or a size that breaks the
	// grammar (0, a leading zero, a third number), is rejected here; the digits
	// of the fixed shapes (BM.Standard3.64) have no hyphen and no Flex segment.
	if strings.HasSuffix(name, ".Flex") || strings.Contains(name, ".Flex.") || malformedTailRe.MatchString(name[i+1:]) {
		return InstanceType{}, vendors.NewError(vendors.Unsupported, CodeInvalidInstanceType,
			fmt.Errorf("%q is not a fixed shape and not a valid size: names are <shape>.<ocpus>-<gb>, "+
				"integers from 1 without leading zeros", name))
	}
	return InstanceType{Shape: name}, nil
}

// FormatType is the inverse of ParseType: the name of a shape, with the size of
// a flexible one. The sizes of our own types are integers and print as such; a
// foreign instance with fractional sizes gets %g formatting and is never one of
// ours (the name does not parse).
func FormatType(shape string, ocpus, gb float64) string {
	return shape + "." + formatSize(ocpus) + "-" + formatSize(gb)
}

func formatSize(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// typeOfInstance rebuilds the type name of an instance from its shape and shape
// configuration. A flexible shape is the one named *.Flex; the shape table
// (isFlexible) decides at launch, this is only for reading instances back.
func typeOfInstance(shape string, ocpus, gb float64, hasConfig bool) string {
	if !hasConfig || !strings.HasSuffix(shape, ".Flex") {
		return shape
	}
	return FormatType(shape, ocpus, gb)
}

// IsFlexibleName reports whether the shape name is of a flexible shape.
func IsFlexibleName(shape string) bool { return strings.HasSuffix(shape, ".Flex") }

// VCPUsPerOCPU is the driver's only vCPU knowledge (plan 25, section 2): an OCPU
// is two vCPUs on x86 and on the A2 and A4 Arm generations, one on A1. ok is
// false for an Arm generation the table does not know: it is never guessed.
func VCPUsPerOCPU(shape string) (n int, ok bool) {
	if m := armRe.FindStringSubmatch(shape); m != nil {
		switch m[1] {
		case "1":
			return 1, true
		case "2", "4":
			return 2, true
		}
		return 0, false
	}
	return 2, true
}

// Arm reports whether the shape is an Arm shape (A1, A2, A4 and later).
func Arm(shape string) bool { return armRe.MatchString(shape) }
