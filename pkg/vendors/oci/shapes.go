/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import (
	"context"
	"fmt"
	"maps"
	"math"

	"github.com/oracle/oci-go-sdk/v65/core"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// adKey identifies the shape table of one availability domain.
type adKey struct{ region, ad string }

// shapeInfo is what the driver keeps of a shape of ListShapes (plan 25, sections
// 2 and 7): whether it is flexible, the bounds of a flexible one, the OCPUs and
// memory of a fixed one and the names of the limits it consumes.
type shapeInfo struct {
	Name     string
	Flexible bool
	// OCPUs and MemoryGB are the size of a fixed shape.
	OCPUs, MemoryGB float64
	// The bounds of a flexible shape; the NUMA ones are zero when the shape has
	// none.
	OCPUMin, OCPUMax, OCPUPerNuma                      float64
	MemMin, MemMax, MemPerNuma, PerOCPUMin, PerOCPUMax float64
	QuotaNames                                         []string
}

func f32(p *float32) float64 {
	if p == nil {
		return 0
	}
	return float64(*p)
}

func shapeInfoOf(s core.Shape) *shapeInfo {
	si := &shapeInfo{
		Name: deref(s.Shape), Flexible: deref(s.IsFlexible), OCPUs: f32(s.Ocpus), MemoryGB: f32(s.MemoryInGBs),
		QuotaNames: s.QuotaNames,
	}
	if o := s.OcpuOptions; o != nil {
		si.OCPUMin, si.OCPUMax, si.OCPUPerNuma = f32(o.Min), f32(o.Max), f32(o.MaxPerNumaNode)
	}
	if m := s.MemoryOptions; m != nil {
		si.MemMin, si.MemMax, si.MemPerNuma = f32(m.MinInGBs), f32(m.MaxInGBs), f32(m.MaxPerNumaNodeInGBs)
		si.PerOCPUMin, si.PerOCPUMax = f32(m.MinPerOcpuInGBs), f32(m.MaxPerOcpuInGBs)
	}
	return si
}

// shapesIn returns the shape table of an availability domain (ListShapes of the
// compartment in the AD), cached for an hour; it also feeds the table the quota
// consumption reads.
func (d *Driver) shapesIn(ctx context.Context, region, ad string) (map[string]*shapeInfo, error) {
	key := adKey{region, ad}
	d.mu.Lock()
	if v, ok := d.shapes[key].get(d.clock.Now()); ok {
		d.mu.Unlock()
		return v, nil
	}
	d.mu.Unlock()
	req := core.ListShapesRequest{CompartmentId: &d.compartment, AvailabilityDomain: &ad}
	out := map[string]*shapeInfo{}
	for {
		res, err := d.api.Compute.ListShapes(ctx, region, req)
		if err != nil {
			return nil, fmt.Errorf("listing the shapes of %s: %w", ad, classify(err, kindRead))
		}
		for _, s := range res.Items {
			if s.Shape != nil {
				out[*s.Shape] = shapeInfoOf(s)
			}
		}
		if res.OpcNextPage == nil || *res.OpcNextPage == "" {
			break
		}
		req.Page = res.OpcNextPage
	}
	d.mu.Lock()
	d.shapes[key] = set(out, d.clock.Now().Add(factsTTL))
	maps.Copy(d.index, out)
	d.mu.Unlock()
	return out, nil
}

// shapeByName is the shape of any table the driver has read: the quota
// consumption has no region or zone to ask for.
func (d *Driver) shapeByName(name string) (*shapeInfo, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	si, ok := d.index[name]
	return si, ok
}

// Check validates a parsed type against the shape table of an availability
// domain, before any launch call (plan 25, section 2). A violation is
// Unsupported: a shape the AD does not offer (ShapeNotInAvailabilityDomain), a
// size outside the shape's bounds or a sized name on a fixed shape (and the
// reverse; InvalidInstanceType), and a size beyond one NUMA node
// (ExtendedMemoryUnsupported: it needs a separate limit and cannot be
// preemptible, so v1 does not launch it).
func (t InstanceType) Check(shapes map[string]*shapeInfo, ad string) error {
	si, ok := shapes[t.Shape]
	if !ok {
		return vendors.NewError(vendors.Unsupported, CodeShapeNotInAD,
			fmt.Errorf("the shape %s is not offered in %s", t.Shape, ad))
	}
	if !t.Sized {
		if si.Flexible {
			return vendors.NewError(vendors.Unsupported, CodeInvalidInstanceType,
				fmt.Errorf("%s is a flexible shape and needs a size: <shape>.<ocpus>-<gb>", t.Shape))
		}
		return nil
	}
	if !si.Flexible {
		return vendors.NewError(vendors.Unsupported, CodeInvalidInstanceType,
			fmt.Errorf("%s is not a flexible shape and has no sizes", t.Shape))
	}
	ocpus, gb := float64(t.OCPUs), float64(t.GB)
	switch {
	case ocpus < si.OCPUMin || ocpus > si.OCPUMax:
		return outOfBounds(t, "ocpus", ocpus, si.OCPUMin, si.OCPUMax)
	case gb < si.MemMin || gb > si.MemMax:
		return outOfBounds(t, "memory", gb, si.MemMin, si.MemMax)
	case si.PerOCPUMax > 0 && (gb/ocpus < si.PerOCPUMin-1e-9 || gb/ocpus > si.PerOCPUMax+1e-9):
		return outOfBounds(t, "memory per OCPU", math.Round(gb/ocpus*100)/100, si.PerOCPUMin, si.PerOCPUMax)
	case (si.OCPUPerNuma > 0 && ocpus > si.OCPUPerNuma) || (si.MemPerNuma > 0 && gb > si.MemPerNuma):
		return vendors.NewError(vendors.Unsupported, CodeExtendedMemory, fmt.Errorf(
			"%s is beyond one NUMA node (%g OCPUs, %g GB): extended memory sizes are not launched", t, si.OCPUPerNuma,
			si.MemPerNuma))
	}
	return nil
}

func outOfBounds(t InstanceType, what string, v, lo, hi float64) error {
	return vendors.NewError(vendors.Unsupported, CodeInvalidInstanceType,
		fmt.Errorf("%s: %s %g is outside %g to %g", t, what, v, lo, hi))
}
