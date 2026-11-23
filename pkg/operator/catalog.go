/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package operator

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keeper "github.com/SpareCores/sc-keeper-go"

	"github.com/SpareCores/scarpenter/internal/version"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/static"
	"github.com/SpareCores/scarpenter/pkg/controllers/catalogsync"
	"github.com/SpareCores/scarpenter/pkg/nvidia"
	"github.com/SpareCores/scarpenter/pkg/unavailable"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// WithCatalog builds the catalog from the flags (--keeper-url,
// --keeper-api-key-file, --keeper-cache-dir, --catalog-refresh-interval,
// --catalog-max-age), exposes
// its metrics, and registers it and the controller that feeds it the
// SCNodeClasses with the manager. The catalog reads Keeper only for the vendors
// and regions the SCNodeClasses allow, so it is idle until one exists.
//
// gate (nil: none) marks more offerings unavailable, for example for lack of
// quota, and classGate (nil: none) those of a NodeClass whose vendor resources
// are missing; the caller bumps un's sequence when their answers change.
func (o *Operator) WithCatalog(
	facts catalog.Facts, un *unavailable.Cache, gate catalog.Gate, classGate catalog.ClassGate,
) (*catalog.Catalog, error) {
	// The key is read at every refresh; a file that is missing or empty at the
	// start is a configuration error worth failing for.
	if _, err := readAPIKey(o.Options.KeeperAPIKeyFile); err != nil {
		return nil, err
	}
	var source catalog.Source = catalog.NewKeeperSource(catalog.KeeperOptions{
		URL: o.Options.KeeperURL, APIKeyFile: o.Options.KeeperAPIKeyFile, UserAgent: "scarpenter/" + version.Get().Version,
		CacheDir: o.Options.KeeperCacheDir,
	})
	// The development drivers' static catalogs; a request that concerns only
	// these vendors never reaches Keeper.
	if len(o.staticCatalogs) > 0 {
		source = &catalog.MuxSource{Default: source, Vendors: o.staticCatalogs}
	}
	cat := catalog.New(catalog.Options{
		Source:          source,
		Facts:           facts,
		Unavailable:     un,
		Gate:            gate,
		ClassGate:       classGate,
		RefreshInterval: o.Options.CatalogRefreshInterval,
		MaxAge:          o.Options.CatalogMaxAge,
		Registerer:      metrics.Registry,
		NVIDIAGPUs:      nvidia.NodeBootstrap || nvidiaBootstrapDev(),
	})
	if err := o.Add(cat); err != nil {
		return nil, fmt.Errorf("adding the catalog to the manager: %w", err)
	}
	if err := catalogsync.New(o.GetClient(), cat).Register(o.Manager); err != nil {
		return nil, fmt.Errorf("registering the catalog controller: %w", err)
	}
	return cat, nil
}

// CatalogWarmTimeout bounds WarmCatalog. The health endpoints start with the
// manager, after it: with the chart's liveness probe (first at 15 s, every
// 20 s, three failures) the kubelet restarts the container about 55 s after
// its start, which leaves the rest of the start-up some 25 s.
const CatalogWarmTimeout = 30 * time.Second

// WarmCatalog loads the catalog once, before the manager starts: it runs the
// catalog-sync controller's reconcile with the API reader (the manager's cache
// is not started yet), then refreshes the catalog. Karpenter core prices the
// existing NodeClaims with GetInstanceTypes as soon as its controllers start;
// without this the catalog is still empty then (it waits for the catalog-sync
// controller and a refresh) and core logs "offering price unknown" for every
// NodeClaim. Every replica warms, a standby too (its snapshot then ages until
// it leads). On an error or the timeout it logs and returns: the manager starts
// anyway and the catalog's own refresh loop keeps trying. The loop's first
// refresh after a warm one costs one /healthcheck request.
func (o *Operator) WarmCatalog(ctx context.Context, cat *catalog.Catalog, timeout time.Duration) {
	logger := log.FromContext(ctx).WithName("catalog")
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if _, err := catalogsync.New(o.GetAPIReader(), cat).Reconcile(ctx, reconcile.Request{}); err != nil {
		logger.Error(err, "the catalog did not load before the start, starting anyway")
		return
	}
	if err := cat.Refresh(ctx); err != nil {
		logger.Error(err, "the catalog did not load before the start, starting anyway",
			"timeout", timeout.String())
		return
	}
	logger.Info("catalog loaded before the start")
}

// keeperClient is the Keeper client the drivers get (the fake driver reads its
// vendor's snapshot files with it). Unlike the catalog, which reads the key file
// at every refresh, this client takes the key once, at start: a rotated key
// reaches the fake driver (a development driver) with a restart.
func (o *Operator) keeperClient() (*keeper.Client, error) {
	opts := []keeper.Option{keeper.WithUserAgent("scarpenter/" + version.Get().Version)}
	key, err := readAPIKey(o.Options.KeeperAPIKeyFile)
	if err != nil {
		return nil, err
	}
	if key != "" {
		opts = append(opts, keeper.WithAPIKey(key))
	}
	return keeper.New(o.Options.KeeperURL, opts...), nil
}

// readAPIKey reads the optional Keeper API key file ("" means no key).
func readAPIKey(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the path is a flag the operator sets
	if err != nil {
		return "", fmt.Errorf("reading the Keeper API key file: %w", err)
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return "", fmt.Errorf("the Keeper API key file %s is empty", path)
	}
	return key, nil
}

// staticCatalogs collects the catalogs of the drivers that bring their own
// instead of reading Keeper's (the development drivers).
func staticCatalogs(reg *vendors.Registry) (map[string]catalog.Source, error) {
	out := map[string]catalog.Source{}
	for _, vendor := range reg.Vendors() {
		drv, _ := reg.Driver(vendor)
		p, ok := drv.(vendors.CatalogProvider)
		if !ok {
			continue
		}
		src, err := static.Parse(p.CatalogYAML())
		if err != nil {
			return nil, fmt.Errorf("the catalog of the %s driver: %w", vendor, err)
		}
		out[vendor] = src
	}
	return out, nil
}
