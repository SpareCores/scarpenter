//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	keeper "github.com/SpareCores/sc-keeper-go"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

const statusActive = "active"

// serverInfo is what the driver needs of a catalog server.
type serverInfo struct {
	vcpus  int64
	family string
	arch   string
}

// catalogData is the impersonated vendor's regions, zones and servers.
type catalogData struct {
	regions []string
	zones   map[string][]vendors.Zone
	servers map[string]serverInfo
}

// The data does not change while the controller runs and is expensive to read
// (the server table is megabytes), so tests and restarts of the driver in one
// process share it.
var (
	dataCacheMu sync.Mutex
	dataCache   = map[string]*catalogData{}
)

// loadCatalogData reads the tables from Keeper; failures are not cached, so a
// replay server that starts later is picked up by the next call.
func loadCatalogData(ctx context.Context, url, vendor string) (*catalogData, error) {
	cacheKey := url + "|" + vendor
	dataCacheMu.Lock()
	if d, ok := dataCache[cacheKey]; ok {
		dataCacheMu.Unlock()
		return d, nil
	}
	dataCacheMu.Unlock()
	if url == "" {
		return nil, errors.New("the fake driver needs the Keeper URL")
	}
	c := keeper.New(url, keeper.WithUserAgent("scarpenter-fake"))
	regions, err := c.Regions(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading Keeper regions: %w", err)
	}
	zones, err := c.Zones(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading Keeper zones: %w", err)
	}
	servers, err := c.TableServers(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading Keeper servers: %w", err)
	}
	d := &catalogData{zones: map[string][]vendors.Zone{}, servers: map[string]serverInfo{}}
	regionName := map[string]string{}
	for _, r := range regions {
		if r.VendorID == vendor && r.Status == statusActive {
			regionName[r.RegionID] = r.APIReference
			d.regions = append(d.regions, r.APIReference)
		}
	}
	for _, z := range zones {
		if name, ok := regionName[z.RegionID]; ok && z.VendorID == vendor && z.Status == statusActive {
			d.zones[name] = append(d.zones[name], vendors.Zone{ID: z.ZoneID, Name: z.APIReference})
		}
	}
	for i := range servers {
		s := &servers[i]
		if s.VendorID != vendor || s.Status != statusActive {
			continue
		}
		fam, _, _ := strings.Cut(s.APIReference, ".")
		if s.Family != nil && *s.Family != "" {
			fam = *s.Family
		}
		arch := "amd64"
		if a := strings.ToLower(s.CPUArchitecture); strings.Contains(a, "arm") || strings.Contains(a, "aarch") {
			arch = "arm64"
		}
		d.servers[s.APIReference] = serverInfo{vcpus: int64(s.VCPUs), family: fam, arch: arch}
	}
	if len(d.regions) == 0 {
		return nil, fmt.Errorf("no active Keeper regions for vendor %q", vendor)
	}
	dataCacheMu.Lock()
	dataCache[cacheKey] = d
	dataCacheMu.Unlock()
	return d, nil
}
