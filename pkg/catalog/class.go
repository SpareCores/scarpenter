/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog/labels"
)

// Scope says which vendors and regions to read: vendor to the region
// allow-list, where an empty list means every active region.
type Scope map[string][]string

// Class is what the catalog reads of an SCNodeClass: the vendors and regions it
// allows and everything that changes what its instance types look like. It is
// plain data, so the catalog needs no Kubernetes client.
type Class struct {
	// Name is the SCNodeClass name.
	Name string
	// Hash identifies the content below; it is part of the cache keys.
	Hash string

	// Vendors maps each vendor of the NodeClass to its region allow-list
	// (empty: every active region).
	Vendors map[string][]string

	MaxPods                 int64
	VMMemoryOverheadPercent float64
	KubeReserved            corev1.ResourceList
	SystemReserved          corev1.ResourceList
	// EvictionHard holds the hard eviction thresholds as written: quantities or
	// percentages of the capacity.
	EvictionHard map[string]string

	// CPUFlags are the flags that get a cpu-flag-<flag> label.
	CPUFlags []string
	// Metrics and Selections are the NodeClass's definitions (00a sections 7.2
	// and 7.3).
	Metrics    map[string]v1alpha1.Metric
	Selections map[string]v1alpha1.Selection
	// Profiles are the profile names the NodeClass defines, per vendor that has
	// a block in the profile, by kind.
	Profiles map[string]labels.Profiles

	// AllowIncompletePrice says whether offerings with an incomplete price (a
	// component the data does not have) are offered (00a section 10).
	AllowIncompletePrice bool
}

// NewClass returns a class with the defaults of an SCNodeClass that sets
// nothing, for the given vendors and regions. Finalize it after changing
// fields.
func NewClass(name string, vendors Scope) Class {
	c := Class{
		Name: name, Vendors: map[string][]string{}, MaxPods: DefaultMaxPods,
		VMMemoryOverheadPercent: DefaultVMMemoryOverheadPercent, AllowIncompletePrice: true,
		CPUFlags: slices.Clone(v1alpha1.DefaultCPUFlagLabels), Profiles: map[string]labels.Profiles{},
	}
	for v, r := range vendors {
		c.Vendors[v] = slices.Clone(r)
		slices.Sort(c.Vendors[v])
	}
	slices.Sort(c.CPUFlags)
	c.Finalize()
	return c
}

// Finalize recomputes the hash after fields changed.
func (c *Class) Finalize() { c.Hash = c.hash() }

// ClassFromNodeClass extracts the catalog's view of an SCNodeClass. It fails
// when a resource quantity of the kubelet settings cannot be parsed; the
// error says which.
func ClassFromNodeClass(nc *v1alpha1.SCNodeClass) (Class, error) {
	spec := &nc.Spec
	c := Class{
		Name:                    nc.Name,
		Vendors:                 map[string][]string{},
		MaxPods:                 DefaultMaxPods,
		VMMemoryOverheadPercent: DefaultVMMemoryOverheadPercent,
		EvictionHard:            maps.Clone(spec.Kubelet.EvictionHard),
		Metrics:                 maps.Clone(spec.Metrics),
		Selections:              maps.Clone(spec.Selections),
		Profiles:                map[string]labels.Profiles{},
		AllowIncompletePrice:    spec.Pricing.AllowIncomplete == nil || *spec.Pricing.AllowIncomplete,
	}
	if spec.Kubelet.MaxPods != nil {
		c.MaxPods = int64(*spec.Kubelet.MaxPods)
	}
	if spec.Kubelet.VMMemoryOverheadPercent != nil {
		c.VMMemoryOverheadPercent = *spec.Kubelet.VMMemoryOverheadPercent
	}
	var errs []error
	var err error
	if c.KubeReserved, err = parseResources("kubeReserved", spec.Kubelet.KubeReserved); err != nil {
		errs = append(errs, err)
	}
	if c.SystemReserved, err = parseResources("systemReserved", spec.Kubelet.SystemReserved); err != nil {
		errs = append(errs, err)
	}
	for signal, v := range spec.Kubelet.EvictionHard {
		if _, _, _, err := parseThreshold(v); err != nil {
			errs = append(errs, fmt.Errorf("evictionHard[%s]: %w", signal, err))
		}
	}

	c.CPUFlags = v1alpha1.DefaultCPUFlagLabels
	if spec.Catalog != nil && spec.Catalog.CPUFlagLabels != nil {
		c.CPUFlags = spec.Catalog.CPUFlagLabels
	}
	c.CPUFlags = slices.Clone(c.CPUFlags)
	slices.Sort(c.CPUFlags)
	c.CPUFlags = slices.Compact(c.CPUFlags)

	for _, vendor := range spec.Vendors.Names() {
		regions, _ := spec.Vendors.Regions(vendor)
		c.Vendors[vendor] = slices.Clone(regions)
		slices.Sort(c.Vendors[vendor])
		p := labels.Profiles{}
		for name, prof := range spec.Profiles.Storage {
			if _, ok := prof.For(vendor); ok {
				p[labels.ProfileStorage] = append(p[labels.ProfileStorage], name)
			}
		}
		for name, prof := range spec.Profiles.Network {
			if _, ok := prof.For(vendor); ok {
				p[labels.ProfileNetwork] = append(p[labels.ProfileNetwork], name)
			}
		}
		for name, prof := range spec.Profiles.Placement {
			if _, ok := prof.For(vendor); ok {
				p[labels.ProfilePlacement] = append(p[labels.ProfilePlacement], name)
			}
		}
		for k := range p {
			slices.Sort(p[k])
		}
		c.Profiles[vendor] = p
	}
	c.Hash = c.hash()
	return c, errors.Join(errs...)
}

// hash is a content hash of everything but the name and the hash itself: two
// NodeClasses that would produce the same instance types share cache entries.
func (c *Class) hash() string {
	cp := *c
	cp.Name, cp.Hash = "", ""
	raw, err := json.Marshal(cp)
	if err != nil {
		// All fields marshal; a failure would be a programming error.
		panic(fmt.Sprintf("hashing class: %v", err))
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:16]
}

// MetricNames returns the metric names, sorted.
func (c *Class) MetricNames() []string { return slices.Sorted(maps.Keys(c.Metrics)) }

// VendorNames returns the vendors of the class, sorted.
func (c *Class) VendorNames() []string { return slices.Sorted(maps.Keys(c.Vendors)) }

// UnionScope returns the scope that covers every class: per vendor the union
// of the regions, and every active region as soon as one class lists none.
func UnionScope(classes []Class) Scope {
	scope := Scope{}
	all := map[string]bool{}
	for i := range classes {
		for vendor, regions := range classes[i].Vendors {
			if _, seen := scope[vendor]; !seen {
				scope[vendor] = nil
			}
			if len(regions) == 0 {
				all[vendor] = true
				continue
			}
			scope[vendor] = append(scope[vendor], regions...)
		}
	}
	for vendor := range scope {
		if all[vendor] {
			scope[vendor] = nil
			continue
		}
		slices.Sort(scope[vendor])
		scope[vendor] = slices.Compact(scope[vendor])
	}
	return scope
}

// Equal reports whether two scopes read the same vendors and regions.
func (s Scope) Equal(o Scope) bool {
	if len(s) != len(o) {
		return false
	}
	for v, r := range s {
		or, ok := o[v]
		if !ok || !slices.Equal(r, or) {
			return false
		}
	}
	return true
}

func parseResources(field string, in map[string]string) (corev1.ResourceList, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := corev1.ResourceList{}
	var errs []error
	for k, v := range in {
		q, err := resource.ParseQuantity(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s[%s]: %w", field, k, err))
			continue
		}
		out[corev1.ResourceName(k)] = q
	}
	return out, errors.Join(errs...)
}

// parseThreshold parses an eviction threshold: a quantity, or a percentage of
// the capacity ("10%", isPercent).
func parseThreshold(v string) (q resource.Quantity, percent float64, isPercent bool, err error) {
	if p, ok := strings.CutSuffix(v, "%"); ok {
		f, err := strconv.ParseFloat(p, 64)
		if err != nil || f < 0 || f > 100 {
			return q, 0, false, fmt.Errorf("%q is not a percentage between 0 and 100", v)
		}
		return q, f, true, nil
	}
	q, err = resource.ParseQuantity(v)
	if err != nil {
		return q, 0, false, fmt.Errorf("%q is not a quantity or a percentage: %w", v, err)
	}
	return q, 0, false, nil
}
