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

// The data of a Keeper hash does not change and is expensive to read (the
// server table is megabytes), so tests and restarts of the driver in one
// process share it, by hash and vendor.
var (
	dataCacheMu sync.Mutex
	dataCache   = map[string]*catalogData{}
)

// loadCatalogData reads the region, zone and server files of the vendor from
// the Keeper snapshot of the hash /healthcheck pins; failures are not cached,
// so a replay server that starts later is picked up by the next call.
func loadCatalogData(ctx context.Context, c *keeper.Client, vendor string) (*catalogData, error) {
	if c == nil {
		return nil, errors.New("the fake driver needs a Keeper client (vendors.Config.Keeper)")
	}
	health, err := c.Health(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading Keeper health: %w", err)
	}
	hash := health.DatabaseHash
	if hash == "" {
		return nil, errors.New("keeper serves no data version")
	}
	cacheKey := hash + "|" + vendor
	dataCacheMu.Lock()
	if d, ok := dataCache[cacheKey]; ok {
		dataCacheMu.Unlock()
		return d, nil
	}
	dataCacheMu.Unlock()
	regions, err := snapshotRows[keeper.Region](ctx, c, hash, keeper.VendorTablePath("region", vendor))
	if err != nil {
		return nil, err
	}
	zones, err := snapshotRows[keeper.Zone](ctx, c, hash, keeper.VendorTablePath("zone", vendor))
	if err != nil {
		return nil, err
	}
	servers, err := snapshotRows[keeper.TableServer](ctx, c, hash, keeper.VendorTablePath("server", vendor))
	if err != nil {
		return nil, err
	}
	d := &catalogData{zones: map[string][]vendors.Zone{}, servers: map[string]serverInfo{}}
	regionName := map[string]string{}
	for _, r := range regions {
		if r.Status == statusActive {
			regionName[r.RegionID] = r.APIReference
			d.regions = append(d.regions, r.APIReference)
		}
	}
	for _, z := range zones {
		if name, ok := regionName[z.RegionID]; ok && z.Status == statusActive {
			d.zones[name] = append(d.zones[name], vendors.Zone{ID: z.ZoneID, Name: z.APIReference})
		}
	}
	for i := range servers {
		s := &servers[i]
		if s.Status != statusActive {
			continue
		}
		fam, _, _ := strings.Cut(s.APIReference, ".")
		// Keeper's family is a category at some vendors ("General Purpose") and the
		// type family at others (a1); its series is the type family where set.
		if s.Family != nil && *s.Family != "" {
			fam = *s.Family
		}
		if s.Series != nil && *s.Series != "" {
			fam = *s.Series
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

// snapshotRows reads one snapshot file of the hash as rows of T.
func snapshotRows[T any](ctx context.Context, c *keeper.Client, hash, path string) ([]T, error) {
	body, err := c.SnapshotFile(ctx, hash, path)
	if err != nil {
		return nil, fmt.Errorf("reading Keeper snapshot file %s: %w", path, err)
	}
	rows, err := keeper.DecodeSnapshotRows[T](body, -1)
	if err != nil {
		return nil, fmt.Errorf("decoding Keeper snapshot file %s: %w", path, err)
	}
	return rows, nil
}
