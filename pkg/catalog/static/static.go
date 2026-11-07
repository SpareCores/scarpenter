/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package static is a catalog source read from a YAML file: instance types and
// their offerings, for development drivers and tests that need no Keeper. It
// supports no benchmark metrics and no selection profiles: their labels are
// DoesNotExist.
//
// The file lists vendors with regions, zones and types; a type's offerings give
// the hourly USD price per region and capacity type, replicated into every zone
// of the region (like a Keeper region-level price):
//
//	vendors:
//	  - name: fake
//	    regions:
//	      - {name: region-a, country: DE, city: Frankfurt, greenEnergy: true, zones: [region-a1, region-a2]}
//	    types:
//	      - name: small-2
//	        family: small
//	        arch: amd64
//	        vcpus: 2
//	        memoryMiB: 4096
//	        prices:
//	          - {region: region-a, onDemand: 0.05, spot: 0.02}
package static

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"slices"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/model"
)

// File is the YAML document.
type File struct {
	Vendors []Vendor `json:"vendors"`
}

// Vendor is one vendor of a static catalog.
type Vendor struct {
	Name    string   `json:"name"`
	Regions []Region `json:"regions"`
	Types   []Type   `json:"types"`
}

// Region is a region with its zones.
type Region struct {
	Name        string   `json:"name"`
	Country     string   `json:"country,omitempty"`
	City        string   `json:"city,omitempty"`
	State       string   `json:"state,omitempty"`
	GreenEnergy *bool    `json:"greenEnergy,omitempty"`
	Zones       []string `json:"zones,omitempty"`
}

// Type is an instance type.
type Type struct {
	Name         string   `json:"name"`
	Family       string   `json:"family,omitempty"`
	Arch         string   `json:"arch,omitempty"`
	VCPUs        int      `json:"vcpus"`
	MemoryMiB    float64  `json:"memoryMiB"`
	CPUCores     *int     `json:"cpuCores,omitempty"`
	Manufacturer string   `json:"cpuManufacturer,omitempty"`
	CPUFamily    string   `json:"cpuFamily,omitempty"`
	ClockGHz     *float64 `json:"cpuClockGHz,omitempty"`
	Flags        []string `json:"cpuFlags,omitempty"`
	GPUs         float64  `json:"gpus,omitempty"`
	GPUModel     string   `json:"gpuModel,omitempty"`
	GPUMaker     string   `json:"gpuManufacturer,omitempty"`
	// LocalStorageGB is the size of the bundled local disks together and
	// LocalStorageCount their number.
	LocalStorageGB    float64 `json:"localStorageGB,omitempty"`
	LocalStorageCount int     `json:"localStorageCount,omitempty"`
	Prices            []Price `json:"prices"`
}

// Price is the hourly USD price of a type in a region.
type Price struct {
	Region   string  `json:"region"`
	OnDemand float64 `json:"onDemand,omitempty"`
	Spot     float64 `json:"spot,omitempty"`
}

// Source serves a parsed file.
type Source struct {
	file    File
	version string
	now     func() time.Time
}

var _ catalog.Source = (*Source)(nil)

// Load reads and parses a YAML file.
func Load(path string) (*Source, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading static catalog: %w", err)
	}
	return Parse(raw)
}

// Parse parses the YAML document.
func Parse(raw []byte) (*Source, error) {
	var f File
	if err := yaml.UnmarshalStrict(raw, &f); err != nil {
		return nil, fmt.Errorf("parsing static catalog: %w", err)
	}
	for _, v := range f.Vendors {
		regions := map[string]bool{}
		for _, r := range v.Regions {
			regions[r.Name] = true
		}
		for _, t := range v.Types {
			if t.Name == "" || t.VCPUs <= 0 || t.MemoryMiB <= 0 {
				return nil, fmt.Errorf("static catalog: type %q of %s needs a name, vcpus and memoryMiB", t.Name, v.Name)
			}
			if t.Arch != "" && t.Arch != model.ArchAMD64 && t.Arch != model.ArchARM64 {
				return nil, fmt.Errorf("static catalog: type %q of %s: arch must be amd64 or arm64", t.Name, v.Name)
			}
			for _, p := range t.Prices {
				if !regions[p.Region] {
					return nil, fmt.Errorf("static catalog: type %q of %s prices unknown region %q", t.Name, v.Name, p.Region)
				}
			}
		}
	}
	return &Source{file: f, version: version(raw), now: time.Now}, nil
}

// Load returns the data of the vendors and regions in scope. Metrics and
// selections are not supported: their queries are reported invalid.
func (s *Source) Load(_ context.Context, req catalog.Request) (*model.Data, error) {
	d := &model.Data{
		Version: s.version, FetchedAt: s.now(),
		Scores: map[string]map[string]float64{}, Selected: map[string]map[string]model.Member{},
		Invalid: map[string]string{},
	}
	for _, q := range req.Metrics {
		d.Invalid[q.ID] = "the static catalog has no benchmark data"
	}
	for _, q := range req.Selections {
		d.Invalid[q.ID] = "the static catalog has no benchmark data"
	}
	for _, v := range s.file.Vendors {
		allowed, in := req.Scope[v.Name]
		if !in {
			continue
		}
		for _, r := range v.Regions {
			if len(allowed) > 0 && !slices.Contains(allowed, r.Name) {
				continue
			}
			d.Regions = append(d.Regions, model.Region{
				Vendor: v.Name, ID: r.Name, Name: r.Name, Active: true,
				CountryID: r.Country, City: r.City, State: r.State, GreenEnergy: r.GreenEnergy,
			})
			for _, z := range r.Zones {
				d.Zones = append(d.Zones, model.Zone{Vendor: v.Name, RegionID: r.Name, ID: z, Name: z, Active: true})
			}
		}
		for _, t := range v.Types {
			arch := t.Arch
			if arch == "" {
				arch = model.ArchAMD64
			}
			d.Servers = append(d.Servers, model.Server{
				Vendor: v.Name, ID: t.Name, APIReference: t.Name, Family: t.Family,
				VCPUs: t.VCPUs, CPUCores: t.CPUCores, CPUAllocation: "Dedicated", Arch: arch,
				CPUManufacturer: t.Manufacturer, CPUFamily: t.CPUFamily, CPUSpeedGHz: t.ClockGHz, CPUFlags: t.Flags,
				MemoryMiB: t.MemoryMiB, AcceleratorCount: t.GPUs, AcceleratorModel: t.GPUModel,
				AcceleratorManufacturer: t.GPUMaker,
				LocalStorageGB:          t.LocalStorageGB, LocalStorageCount: t.LocalStorageCount,
			})
			for _, p := range t.Prices {
				if len(allowed) > 0 && !slices.Contains(allowed, p.Region) {
					continue
				}
				if p.OnDemand > 0 {
					d.Prices = append(d.Prices, model.Price{Vendor: v.Name, RegionID: p.Region, ServerID: t.Name,
						CapacityType: model.CapacityOnDemand, USDPerHour: p.OnDemand})
				}
				if p.Spot > 0 {
					d.Prices = append(d.Prices, model.Price{Vendor: v.Name, RegionID: p.Region, ServerID: t.Name,
						CapacityType: model.CapacitySpot, USDPerHour: p.Spot})
				}
			}
		}
	}
	return d, nil
}

func version(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:16]
}
