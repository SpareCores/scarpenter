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
	"io/fs"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log"

	keeper "github.com/SpareCores/sc-keeper-go"

	"github.com/SpareCores/scarpenter/pkg/catalog/model"
)

// manifestPath is the manifest of an embedded snapshot, next to its files.
const manifestPath = "manifest"

// EmbeddedSource serves one vendor from files of Keeper's snapshot layout that
// are compiled into the binary: a stand-in for a vendor Keeper does not serve
// yet. It reads the files with the functions KeeperSource uses (loadVendor),
// so a real Keeper snapshot of the vendor gives the same catalog, and applies
// rule R like it. It has no network access.
//
// The data is static: a Load for the same request returns the same data. It
// reports a zero PublishedAt and no SnapshotRead, so that Keeper's data version
// and age (the status message, the published-at metric, the Keeper auth state)
// stay those of Keeper alone; it sets AuxPricesRead, so that the vendor's driver
// gets the storage and IPv4 prices. Benchmark scores and selection profiles
// are not supported: their queries are reported invalid.
type EmbeddedSource struct {
	vendor   string
	fsys     fs.FS
	manifest keeper.Manifest
	version  string
	dataDate string
	now      func() time.Time

	mu   sync.Mutex
	last *embeddedLoad
}

type embeddedLoad struct {
	requestKey string
	data       *model.Data
}

var _ Source = (*EmbeddedSource)(nil)

// NewEmbeddedSource reads and checks the manifest of the files in fsys (as
// Keeper's snapshot paths with ".json.gz": "server/<vendor>.json.gz",
// "manifest.json.gz") for the vendor. Every file is checked against the
// manifest's SHA-256 and row count when it is read.
func NewEmbeddedSource(vendor string, fsys fs.FS) (*EmbeddedSource, error) {
	s := &EmbeddedSource{vendor: vendor, fsys: fsys, now: time.Now}
	if !safeName(vendor) {
		return nil, fmt.Errorf("embedded catalog: %q is not a vendor id", vendor)
	}
	body, err := s.read(manifestPath)
	if err != nil {
		return nil, err
	}
	if s.manifest, err = keeper.DecodeManifest(body); err != nil {
		return nil, fmt.Errorf("embedded catalog of %s: reading the manifest: %w", vendor, err)
	}
	h := sha256.New()
	for _, f := range s.manifest.Files {
		_, _ = fmt.Fprintf(h, "%s %d %s\n", f.Path, f.Rows, f.SHA256)
	}
	s.version = hex.EncodeToString(h.Sum(nil))
	for _, t := range requiredTables {
		if _, ok := s.manifest.Lookup(keeper.VendorTablePath(t, vendor)); !ok {
			return nil, fmt.Errorf("embedded catalog of %s: the manifest has no %s file",
				vendor, keeper.VendorTablePath(t, vendor))
		}
	}
	// The data date is the vendor row's observation time; it goes to the log
	// and the gauge's documentation only.
	rd := embeddedReader{s, logr.Discard()}
	rows, err := decodeFile[vendorObservation](context.Background(), rd, keeper.TablePath("vendor"))
	if err != nil {
		return nil, fmt.Errorf("embedded catalog of %s: %w", vendor, err)
	}
	for _, r := range rows {
		if r.VendorID == vendor {
			s.dataDate, _, _ = strings.Cut(r.ObservedAt, "T")
		}
	}
	return s, nil
}

// Vendor is the vendor the source serves.
func (s *EmbeddedSource) Vendor() string { return s.vendor }

// DataDate is the day of the data's observation (YYYY-MM-DD), empty when the
// vendor row has none.
func (s *EmbeddedSource) DataDate() string { return s.dataDate }

// Version is a digest of the manifest: it changes when a file does.
func (s *EmbeddedSource) Version() string { return s.version }

// read returns the gzip file of a snapshot path.
func (s *EmbeddedSource) read(path string) (*keeper.SnapshotBody, error) {
	gz, err := fs.ReadFile(s.fsys, path+".json.gz")
	if err != nil {
		return nil, fmt.Errorf("embedded catalog of %s: %w", s.vendor, err)
	}
	return &keeper.SnapshotBody{Gzip: gz}, nil
}

func (s *EmbeddedSource) lookup(path string) (keeper.ManifestFile, bool) {
	return s.manifest.Lookup(path)
}

// vendorObservation is the part of the vendor row that dates the data.
type vendorObservation struct {
	VendorID   string `json:"vendor_id"`
	ObservedAt string `json:"observed_at"`
}

// embeddedReader is the snapshotFiles of one Load: the source's files with
// the load's logger.
type embeddedReader struct {
	*EmbeddedSource
	logger logr.Logger
}

func (r embeddedReader) snapLogger() logr.Logger { return r.logger }

// file implements snapshotFiles. A path that is not listed does not exist.
func (s *EmbeddedSource) file(_ context.Context, path string) (*keeper.SnapshotBody, int, error) {
	mf, ok := s.manifest.Lookup(path)
	if !ok {
		return nil, 0, fmt.Errorf("embedded catalog of %s: %s is not in the manifest: %w", s.vendor, path, fs.ErrNotExist)
	}
	body, err := s.read(path)
	if err != nil {
		return nil, 0, err
	}
	if err := body.Verify(mf.SHA256); err != nil {
		return nil, 0, fmt.Errorf("embedded catalog of %s: %s: %w", s.vendor, path, err)
	}
	return body, mf.Rows, nil
}

var _ snapshotFiles = embeddedReader{}

// Load builds the data of the vendor for the request's scope. Metrics and
// selections are reported invalid. A request that does not name the vendor
// gets no data of it.
func (s *EmbeddedSource) Load(ctx context.Context, req Request) (*model.Data, error) {
	key := requestKey(req)
	s.mu.Lock()
	last := s.last
	s.mu.Unlock()
	if last != nil && last.requestKey == key {
		return last.data, nil
	}
	data := &model.Data{
		Version: s.version, FetchedAt: s.now(), AuxPricesRead: true,
		Scores:   map[string]map[string]float64{},
		Selected: map[string]map[string]model.Member{},
		Invalid:  map[string]string{},
	}
	reason := "the embedded catalog of " + s.vendor + " has no benchmark data"
	for _, q := range req.Metrics {
		data.Invalid[q.ID] = reason
	}
	for _, q := range req.Selections {
		data.Invalid[q.ID] = reason
	}
	if allowed, in := req.Scope[s.vendor]; in {
		rd := embeddedReader{s, log.FromContext(ctx)}
		if err := loadVendorTable(ctx, rd, data); err != nil {
			return nil, err
		}
		if err := loadVendor(ctx, rd, s.vendor, allowed, data); err != nil {
			return nil, fmt.Errorf("reading the embedded catalog of %s: %w", s.vendor, err)
		}
		if n := uniqueAPIReferences(data); n > 0 {
			log.FromContext(ctx).V(1).Info("servers that share an api_reference are named by their server id",
				"vendor", s.vendor, "servers", n)
		}
	}
	s.mu.Lock()
	s.last = &embeddedLoad{requestKey: key, data: data}
	s.mu.Unlock()
	return data, nil
}
