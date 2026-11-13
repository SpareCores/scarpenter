/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/SpareCores/scarpenter/pkg/catalog/model"
)

// MuxSource serves some vendors from their own source (a development driver's
// static catalog) and the rest from the default one (Keeper). A request that
// concerns only the vendors of the own sources never reaches the default source,
// so a development lane runs without a Keeper.
type MuxSource struct {
	// Default serves every vendor without an entry in Vendors.
	Default Source
	// Vendors maps a vendor ID to the source that serves it.
	Vendors map[string]Source

	mu       sync.Mutex
	lastPart []*model.Data
	lastData *model.Data
}

var _ Source = (*MuxSource)(nil)

// Load splits the request by vendor, loads each part and merges the data.
func (m *MuxSource) Load(ctx context.Context, req Request) (*model.Data, error) {
	parts := map[string]Request{}
	for vendor, allowed := range req.Scope {
		if _, own := m.Vendors[vendor]; own {
			r := parts[vendor]
			if r.Scope == nil {
				r.Scope = Scope{}
			}
			r.Scope[vendor] = allowed
			parts[vendor] = r
		}
	}
	rest := Request{Scope: Scope{}}
	for vendor, allowed := range req.Scope {
		if _, own := m.Vendors[vendor]; !own {
			rest.Scope[vendor] = allowed
		}
	}
	for _, q := range req.Metrics {
		if _, own := m.Vendors[q.Vendor]; own {
			r := parts[q.Vendor]
			r.Metrics = append(r.Metrics, q)
			parts[q.Vendor] = r
		} else {
			rest.Metrics = append(rest.Metrics, q)
		}
	}
	for _, q := range req.Selections {
		if _, own := m.Vendors[q.Vendor]; own {
			r := parts[q.Vendor]
			r.Selections = append(r.Selections, q)
			parts[q.Vendor] = r
		} else {
			rest.Selections = append(rest.Selections, q)
		}
	}

	var loaded []*model.Data
	for _, vendor := range slices.Sorted(maps.Keys(parts)) {
		r := parts[vendor]
		if r.Scope == nil {
			r.Scope = Scope{}
		}
		d, err := m.Vendors[vendor].Load(ctx, r)
		if err != nil {
			return nil, fmt.Errorf("loading the catalog of %s: %w", vendor, err)
		}
		loaded = append(loaded, d)
	}
	if len(rest.Scope) > 0 || len(rest.Metrics) > 0 || len(rest.Selections) > 0 || len(loaded) == 0 {
		d, err := m.Default.Load(ctx, rest)
		if err != nil {
			return nil, err
		}
		loaded = append([]*model.Data{d}, loaded...)
	}
	// Parts that are the very data of the last load (a source that found its
	// data unchanged) merge to the last result, so the catalog can tell that
	// nothing changed.
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastData != nil && slices.Equal(loaded, m.lastPart) {
		return m.lastData, nil
	}
	m.lastPart, m.lastData = loaded, mergeData(loaded)
	return m.lastData, nil
}

// mergeData merges data of disjoint vendors: the version is a digest of the
// parts' versions (it changes when any part does), the fetch time is the oldest.
func mergeData(parts []*model.Data) *model.Data {
	if len(parts) == 1 {
		return parts[0]
	}
	out := &model.Data{
		Scores: map[string]map[string]float64{}, Selected: map[string]map[string]model.Member{},
		Invalid: map[string]string{},
	}
	h := sha256.New()
	var oldest time.Time
	for _, p := range parts {
		_, _ = fmt.Fprintln(h, p.Version)
		if oldest.IsZero() || p.FetchedAt.Before(oldest) {
			oldest = p.FetchedAt
		}
		out.Servers = append(out.Servers, p.Servers...)
		out.Regions = append(out.Regions, p.Regions...)
		out.Zones = append(out.Zones, p.Zones...)
		out.Prices = append(out.Prices, p.Prices...)
		out.Vendors = append(out.Vendors, p.Vendors...)
		out.StoragePrices = append(out.StoragePrices, p.StoragePrices...)
		out.IPv4Prices = append(out.IPv4Prices, p.IPv4Prices...)
		out.SnapshotRead = out.SnapshotRead || p.SnapshotRead
		if !p.PublishedAt.IsZero() && (out.PublishedAt.IsZero() || p.PublishedAt.Before(out.PublishedAt)) {
			out.PublishedAt = p.PublishedAt
		}
		maps.Copy(out.Scores, p.Scores)
		maps.Copy(out.Selected, p.Selected)
		maps.Copy(out.Invalid, p.Invalid)
	}
	out.Version = hex.EncodeToString(h.Sum(nil))[:16]
	out.FetchedAt = oldest
	return out
}
