/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package operator

import (
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/SpareCores/scarpenter/internal/version"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/controllers/catalogsync"
	"github.com/SpareCores/scarpenter/pkg/unavailable"
)

// WithCatalog builds the catalog from the flags (--keeper-url,
// --keeper-api-key-file, --catalog-refresh-interval, --catalog-max-age), exposes
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
	key, err := readAPIKey(o.Options.KeeperAPIKeyFile)
	if err != nil {
		return nil, err
	}
	cat := catalog.New(catalog.Options{
		Source: catalog.NewKeeperSource(catalog.KeeperOptions{
			URL: o.Options.KeeperURL, APIKey: key, UserAgent: "scarpenter/" + version.Get().Version,
			PricesRegionsPerRequest: o.Options.KeeperPricesRegionsPerRequest,
		}),
		Facts:           facts,
		Unavailable:     un,
		Gate:            gate,
		ClassGate:       classGate,
		RefreshInterval: o.Options.CatalogRefreshInterval,
		MaxAge:          o.Options.CatalogMaxAge,
		Registerer:      metrics.Registry,
	})
	if err := o.Add(cat); err != nil {
		return nil, fmt.Errorf("adding the catalog to the manager: %w", err)
	}
	if err := catalogsync.New(o.GetClient(), cat).Register(o.Manager); err != nil {
		return nil, fmt.Errorf("registering the catalog controller: %w", err)
	}
	return cat, nil
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
