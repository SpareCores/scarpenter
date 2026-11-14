/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/log"

	keeper "github.com/SpareCores/sc-keeper-go"
	"github.com/SpareCores/sc-keeper-go/keepertest"
	selapi "github.com/SpareCores/sc-keeper-go/selection/api"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
	"github.com/SpareCores/scarpenter/pkg/catalog/model"
)

// recordedHash is the data version of the committed recording.
func recordedHash(t *testing.T) string {
	t.Helper()
	h, err := keepertest.NewHandler(catalogtest.SnapshotDir())
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthcheck", nil))
	var hc struct {
		DatabaseHash string `json:"database_hash"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &hc); err != nil || hc.DatabaseHash == "" {
		t.Fatalf("healthcheck: %v %s", err, rec.Body.String())
	}
	return hc.DatabaseHash
}

// keeperEnv is a replay server whose requests and answers are recorded, and
// whose answers a test can replace.
type keeperEnv struct {
	t *testing.T
	H *keepertest.Handler
	// URL is the server's address.
	URL string

	mu        sync.Mutex
	reqs      []string
	status    []int
	intercept func(w http.ResponseWriter, r *http.Request) bool
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

func newKeeperEnv(t *testing.T) *keeperEnv {
	t.Helper()
	h, err := keepertest.NewHandler(catalogtest.SnapshotDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &keeperEnv{t: t, H: h}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		hook := e.intercept
		e.mu.Unlock()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		if hook == nil || !hook(sw, r) {
			h.ServeHTTP(sw, r)
		}
		e.mu.Lock()
		e.reqs = append(e.reqs, r.URL.Path)
		e.status = append(e.status, sw.code)
		e.mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	e.URL = srv.URL
	return e
}

func (e *keeperEnv) setIntercept(f func(w http.ResponseWriter, r *http.Request) bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.intercept = f
}

// take returns the requests since the last call, as "status path".
func (e *keeperEnv) take() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.reqs))
	for i := range e.reqs {
		out[i] = fmt.Sprintf("%d %s", e.status[i], e.reqs[i])
	}
	e.reqs, e.status = nil, nil
	return out
}

// paths strips the statuses and, with the hash, the snapshot prefix.
func paths(reqs []string) []string {
	out := make([]string, len(reqs))
	for i, r := range reqs {
		_, p, _ := strings.Cut(r, " ")
		if rest, ok := strings.CutPrefix(p, "/snapshot/"); ok {
			_, file, _ := strings.Cut(rest, "/")
			p = "/snapshot/" + file
		}
		out[i] = p
	}
	return out
}

func problemNotAvailable(w http.ResponseWriter, requested, current string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = fmt.Fprintf(w, `{"type":"/problems/snapshot-not-available","title":"not available","status":404,`+
		`"requested":%q,"current":%q,"available":[%q]}`, requested, current, current)
}

// loadAWS builds a catalog over the source for one class of AWS us-east-1.
func catalogOver(t *testing.T, src catalog.Source, scope catalog.Scope, opts ...func(*catalog.Options)) *catalog.Catalog {
	t.Helper()
	o := catalog.Options{Source: src, Facts: catalogtest.Facts(t, "aws", "hcloud")}
	for _, f := range opts {
		f(&o)
	}
	c := catalog.New(o)
	c.SetClasses([]catalog.Class{catalog.NewClass("c", scope)})
	return c
}

var awsFiles = []string{
	"/snapshot/server/aws", "/snapshot/region/aws", "/snapshot/zone/aws", "/snapshot/server_price/aws",
	"/snapshot/storage_price/aws", "/snapshot/ipv4_price/aws",
}

func TestRefreshReadsTheSnapshotOfTheHealthcheckHash(t *testing.T) {
	e := newKeeperEnv(t)
	hash := recordedHash(t)
	c := catalogOver(t, catalogtest.SourceAt(e.URL), catalog.Scope{"aws": {"us-east-1"}})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := paths(e.take())
	want := append([]string{"/healthcheck", "/snapshot/", "/snapshot/vendor"}, awsFiles...)
	if !slices.Equal(got, want) {
		t.Errorf("requests:\n got %v\nwant %v", got, want)
	}
	if v := c.Version(); v != hash {
		t.Errorf("version = %s, want %s", v, hash)
	}
	if n := len(all(t, c, catalog.Filter{})); n < 500 {
		t.Errorf("%d types", n)
	}
	// The data age is the recording's publication time, and CatalogReady names the hash.
	st := c.Status("c")
	if !st.Ready || !strings.Contains(st.Message, hash) || !strings.Contains(st.Message, "2026-11-13T19:50:14Z") {
		t.Errorf("status = %+v", st)
	}
	if !st.KeeperAuth.Known || !st.KeeperAuth.Authorized {
		t.Errorf("keeper auth = %+v", st.KeeperAuth)
	}
}

func TestEverySnapshotRouteIsOneOfTheFew(t *testing.T) {
	e := newKeeperEnv(t)
	cl := catalog.NewClass("c", catalog.Scope{"aws": {"us-east-1", "eu-west-1"}, "hcloud": nil})
	cl.Metrics = map[string]v1alpha1.Metric{"single-core": {Benchmark: benchmark("geekbench:score", `{"cores":"single"}`)}}
	cl.Finalize()
	c := catalog.New(catalog.Options{Source: catalogtest.SourceAt(e.URL), Facts: catalogtest.Facts(t, "aws", "hcloud")})
	c.SetClasses([]catalog.Class{cl})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, p := range paths(e.take()) {
		if p != "/healthcheck" && p != "/benchmark_configs" && !strings.HasPrefix(p, "/snapshot/") {
			t.Errorf("unexpected request %s", p)
		}
	}
}

func TestUnchangedHashNeedsOnlyTheHealthcheck(t *testing.T) {
	e := newKeeperEnv(t)
	c := catalogOver(t, catalogtest.SourceAt(e.URL), catalog.Scope{"aws": {"us-east-1"}})
	clk := time.Now()
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	first := all(t, c, catalog.Filter{})
	e.take()
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := paths(e.take()); !slices.Equal(got, []string{"/healthcheck"}) {
		t.Errorf("requests = %v", got)
	}
	if again := all(t, c, catalog.Filter{}); again[0] != first[0] {
		t.Error("an unchanged refresh rebuilt the types")
	}
	if st := c.Status("c"); !st.Ready || st.Age > time.Since(clk) {
		t.Errorf("status = %+v", st)
	}
}

func TestRestartOnAFilledCacheDirReadsOnlyTheHealthcheck(t *testing.T) {
	e := newKeeperEnv(t)
	dir := t.TempDir()
	opts := catalog.KeeperOptions{URL: e.URL, CacheDir: dir}
	c := catalogOver(t, catalog.NewKeeperSource(opts), catalog.Scope{"aws": {"us-east-1"}})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	hash := recordedHash(t)
	for _, f := range []string{"manifest.json.gz", "server_price/aws.json.gz", "vendor.json.gz", "region/aws.json.gz"} {
		if _, err := os.Stat(filepath.Join(dir, hash, f)); err != nil {
			t.Errorf("the cache lacks %s: %v", f, err)
		}
	}
	e.take()
	// A new process: a new source, the same directory.
	c2 := catalogOver(t, catalog.NewKeeperSource(opts), catalog.Scope{"aws": {"us-east-1"}})
	if err := c2.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := paths(e.take()); !slices.Equal(got, []string{"/healthcheck"}) {
		t.Errorf("requests after the restart = %v", got)
	}
	if got, want := len(all(t, c2, catalog.Filter{})), len(all(t, c, catalog.Filter{})); got != want || got == 0 {
		t.Errorf("types after the restart = %d, want %d", got, want)
	}
	if st := c2.Status("c"); !strings.Contains(st.Message, "2026-11-13T19:50:14Z") {
		t.Errorf("the restart lost the publication time: %+v", st)
	}
}

func TestANewHashFetchesOnlyTheChangedFile(t *testing.T) {
	e := newKeeperEnv(t)
	dir := t.TempDir()
	src := catalog.NewKeeperSource(catalog.KeeperOptions{URL: e.URL, CacheDir: dir})
	c := catalogOver(t, src, catalog.Scope{"aws": {"us-east-1"}})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	old := recordedHash(t)
	if err := e.H.AddHash(catalogtest.NewHash, map[string][]byte{"vendor": catalogtest.Columns(t, []catalogtest.VendorRow{
		{VendorID: "aws", Status: "active", BillingIncrementSeconds: new(1), MinimumBillingSeconds: new(61)},
	})}); err != nil {
		t.Fatal(err)
	}
	if err := e.H.SetCurrent(catalogtest.NewHash); err != nil {
		t.Fatal(err)
	}
	e.take()
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, want := paths(e.take()), []string{"/healthcheck", "/snapshot/", "/snapshot/vendor"}; !slices.Equal(got, want) {
		t.Errorf("requests = %v, want %v", got, want)
	}
	if v := c.Version(); v != catalogtest.NewHash {
		t.Errorf("version = %s", v)
	}
	if f, ok := c.BillingFacts("aws"); !ok || f.Minimum != 61*time.Second {
		t.Errorf("billing facts = %+v", f)
	}
	// Only the directory of the new hash stays.
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != catalogtest.NewHash {
		t.Errorf("cache dir = %v (%v), old hash %s", entries, err, old)
	}
	if _, err := os.Stat(filepath.Join(dir, catalogtest.NewHash, "server_price", "aws.json.gz")); err != nil {
		t.Errorf("the unchanged file was not carried over: %v", err)
	}
}

func TestACorruptedCacheFileIsFetchedAgain(t *testing.T) {
	e := newKeeperEnv(t)
	dir := t.TempDir()
	opts := catalog.KeeperOptions{URL: e.URL, CacheDir: dir}
	c := catalogOver(t, catalog.NewKeeperSource(opts), catalog.Scope{"aws": {"us-east-1"}})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := len(all(t, c, catalog.Filter{}))
	victim := filepath.Join(dir, recordedHash(t), "server", "aws.json.gz")
	if err := os.WriteFile(victim, []byte("not gzip at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.take()
	c2 := catalogOver(t, catalog.NewKeeperSource(opts), catalog.Scope{"aws": {"us-east-1"}})
	if err := c2.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := paths(e.take()); !slices.Equal(got, []string{"/healthcheck", "/snapshot/server/aws"}) {
		t.Errorf("requests = %v", got)
	}
	if got := len(all(t, c2, catalog.Filter{})); got != want {
		t.Errorf("types = %d, want %d", got, want)
	}
	if b, err := os.ReadFile(victim); err != nil || string(b) == "not gzip at all" {
		t.Error("the corrupted file was not replaced")
	}
}

func TestSnapshotNotAvailableRepinsOnceThenKeepsTheCatalog(t *testing.T) {
	e := newKeeperEnv(t)
	orig := recordedHash(t)
	c := catalogOver(t, catalogtest.SourceAt(e.URL), catalog.Scope{"hcloud": nil})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := len(all(t, c, catalog.Filter{}))
	if want == 0 {
		t.Fatal("no types")
	}

	// The healthcheck names a hash whose snapshot the replica no longer has; it
	// serves another one now: one re-pin, then the refresh goes on.
	if err := e.H.AddHash(catalogtest.NewHash, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.H.SetCurrent(catalogtest.NewHash); err != nil {
		t.Fatal(err)
	}
	stale := strings.Replace(orig, orig[:2], "ff", 1)
	e.setIntercept(func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path == "/healthcheck":
			rec := httptest.NewRecorder()
			e.H.ServeHTTP(rec, r)
			var body map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			body["database_hash"] = stale
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(body)
			return true
		case strings.HasPrefix(r.URL.Path, "/snapshot/"+stale):
			problemNotAvailable(w, stale, catalogtest.NewHash)
			return true
		}
		return false
	})
	e.take()
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatalf("the refresh must pin to the current hash: %v", err)
	}
	if v := c.Version(); v != catalogtest.NewHash {
		t.Errorf("version = %s, want the current hash", v)
	}
	notAvailable := 0
	for _, r := range e.take() {
		if strings.HasPrefix(r, "404 ") {
			notAvailable++
		}
	}
	if notAvailable != 1 {
		t.Errorf("%d not-available answers, want one", notAvailable)
	}

	// A second one fails the refresh and keeps the catalog: every hash is gone.
	const third = "2222222222222222222222222222222222222222"
	e.setIntercept(func(w http.ResponseWriter, r *http.Request) bool {
		if rest, ok := strings.CutPrefix(r.URL.Path, "/snapshot/"); ok {
			hash, _, _ := strings.Cut(rest, "/")
			problemNotAvailable(w, hash, "4444444444444444444444444444444444444444")
			return true
		}
		return false
	})
	if err := e.H.AddHash(third, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.H.SetCurrent(third); err != nil {
		t.Fatal(err)
	}
	e.take()
	if err := c.Refresh(t.Context()); err == nil || !keeper.IsSnapshotNotAvailable(err) {
		t.Fatalf("the second not-available must fail the refresh, got %v", err)
	}
	if got := e.take(); len(got) != 3 {
		t.Errorf("requests = %v, want the healthcheck and two manifest requests", got)
	}
	if c.Version() != catalogtest.NewHash || !c.Status("c").Ready {
		t.Error("the previous catalog was not kept")
	}
	if got := len(all(t, c, catalog.Filter{})); got != want {
		t.Errorf("types = %d, want %d", got, want)
	}
}

func TestSnapshotPreparingIsRetried(t *testing.T) {
	e := newKeeperEnv(t)
	hash := recordedHash(t)
	e.H.Preparing(hash, 2)
	src := catalog.NewKeeperSource(catalog.KeeperOptions{URL: e.URL})
	c := catalogOver(t, src, catalog.Scope{"hcloud": nil})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	var preparing int
	for _, r := range e.take() {
		if strings.HasPrefix(r, "503 ") {
			preparing++
		}
	}
	if preparing != 2 {
		t.Errorf("%d 503 answers, want 2", preparing)
	}
	if len(all(t, c, catalog.Filter{})) == 0 {
		t.Error("no types")
	}
}

func TestNotFoundFailsTheRefreshAndNamesThePath(t *testing.T) {
	e := newKeeperEnv(t)
	e.setIntercept(func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/zone/hcloud") {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"type":"/problems/not-found","title":"unknown table","status":404}`))
			return true
		}
		return false
	})
	c := catalogOver(t, catalogtest.SourceAt(e.URL), catalog.Scope{"hcloud": nil})
	err := c.Refresh(t.Context())
	if err == nil || !strings.Contains(err.Error(), "zone/hcloud") || !keeper.IsNotFound(err) {
		t.Fatalf("err = %v", err)
	}
	// Not retried.
	n := 0
	for _, r := range e.take() {
		if strings.HasSuffix(r, "/zone/hcloud") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d requests for the missing file", n)
	}
}

// A vendor the manifest has no files of is reported and skipped; the others load.
func TestAVendorWithoutDataDoesNotBlockTheOthers(t *testing.T) {
	e := newKeeperEnv(t)
	var lines []string
	ctx := log.IntoContext(t.Context(), funcr.New(func(prefix, args string) { lines = append(lines, prefix+args) }, funcr.Options{}))
	c := catalogOver(t, catalogtest.SourceAt(e.URL), catalog.Scope{"hcloud": nil, "nosuchvendor": nil})
	if err := c.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if len(all(t, c, catalog.Filter{})) == 0 {
		t.Error("hcloud did not load")
	}
	if !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, "nosuchvendor") && strings.Contains(l, "no data") }) {
		t.Errorf("the missing vendor was not reported: %v", lines)
	}
}

func oneMetricClass(vendor string, ref *selapi.BenchmarkRef) catalog.Class {
	cl := catalog.NewClass("c", catalog.Scope{vendor: nil})
	cl.Metrics = map[string]v1alpha1.Metric{"single-core": {Benchmark: ref}}
	cl.Finalize()
	return cl
}

func TestMetricsComeFromTheScoreFilesByteForByte(t *testing.T) {
	e := newKeeperEnv(t)
	cl := oneMetricClass("hcloud", benchmark("geekbench:score", `{"cores":"single"}`))
	c := catalog.New(catalog.Options{Source: catalogtest.SourceAt(e.URL), Facts: catalogtest.Facts(t, "hcloud")})
	c.SetClasses([]catalog.Class{cl})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	metrics, _ := c.Invalid(&cl)
	if len(metrics) != 0 {
		t.Fatalf("invalid metrics %v", metrics)
	}
	scored := func(c *catalog.Catalog, cl catalog.Class) int {
		n := 0
		for _, it := range all(t, c, catalog.Filter{}) {
			if r := it.Requirements.Get(v1alpha1.MetricLabel("single-core")); r.Operator() == "In" {
				n++
			}
		}
		return n
	}
	full := scored(c, cl)
	if full == 0 {
		t.Fatal("no type has a score")
	}

	// A score row whose config text is the same JSON with other spacing is not
	// the config: only rows that match byte for byte count. The recording's
	// text is {"cores": "single"}; the override keeps one server's row with the
	// compact spelling a re-encoding would produce.
	rows := catalogtest.Rows[keeper.BenchmarkScore](t, "benchmark_score/hcloud/geekbench:score")
	var ids []string
	for _, r := range rows {
		if string(r.Config) == `{"cores": "single"}` && r.ResourceType == "server" && r.Score != nil && !slices.Contains(ids, r.ResourceID) {
			ids = append(ids, r.ResourceID)
		}
	}
	if len(ids) < 2 {
		t.Fatal("fewer than two single-core servers in the recording")
	}
	override := func(first, second string) int {
		file := fmt.Sprintf(`{"vendor_id":["hcloud","hcloud"],"benchmark_id":["geekbench:score","geekbench:score"],`+
			`"resource_type":["server","server"],"resource_id":[%q,%q],"config":[%s,%s],`+
			`"score":[1.5,2.5],"status":["active","active"]}`, ids[0], ids[1], first, second)
		e := newKeeperEnv(t)
		if err := e.H.AddHash(catalogtest.NewHash, map[string][]byte{"benchmark_score/hcloud/geekbench:score": []byte(file)}); err != nil {
			t.Fatal(err)
		}
		if err := e.H.SetCurrent(catalogtest.NewHash); err != nil {
			t.Fatal(err)
		}
		c := catalog.New(catalog.Options{Source: catalogtest.SourceAt(e.URL), Facts: catalogtest.Facts(t, "hcloud")})
		c.SetClasses([]catalog.Class{cl})
		if err := c.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		if metrics, _ := c.Invalid(&cl); len(metrics) != 0 {
			for _, why := range metrics {
				if !strings.Contains(why, "no scores") {
					t.Errorf("reason = %q", why)
				}
			}
			return -1
		}
		return scored(c, cl)
	}
	// Both rows spell the config as a re-encoding would ({"cores":"single"}): no
	// row counts, and the metric is invalid.
	if n := override(`{"cores":"single"}`, `{"cores":"single"}`); n != -1 {
		t.Errorf("rows that do not match: %d types scored, want an invalid metric", n)
	}
	// One row has the recording's text: exactly its server is scored.
	if n := override(`{"cores": "single"}`, `{"cores":"single"}`); n != 1 {
		t.Errorf("one matching row: %d types scored, want 1", n)
	}
}

func TestAnUnknownBenchmarkIsAnInvalidMetric(t *testing.T) {
	e := newKeeperEnv(t)
	cl := oneMetricClass("hcloud", benchmark("geekbench:score", `{"cores":"nonesuch"}`))
	c := catalog.New(catalog.Options{Source: catalogtest.SourceAt(e.URL), Facts: catalogtest.Facts(t, "hcloud")})
	c.SetClasses([]catalog.Class{cl})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatalf("an invalid metric must not fail the refresh: %v", err)
	}
	if metrics, _ := c.Invalid(&cl); len(metrics) != 1 {
		t.Errorf("invalid metrics = %v", metrics)
	}
	if len(all(t, c, catalog.Filter{})) == 0 {
		t.Error("the rest of the catalog did not load")
	}

	// A benchmark Keeper has no scores of at a vendor: the 200 with empty
	// arrays of an id the manifest does not list is an invalid metric as well.
	cl = oneMetricClass("hcloud", benchmark("nvbandwidth:all:duplex", `{}`))
	c = catalog.New(catalog.Options{Source: catalogtest.SourceAt(e.URL), Facts: catalogtest.Facts(t, "hcloud")})
	c.SetClasses([]catalog.Class{cl})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatalf("a benchmark without scores must not fail the refresh: %v", err)
	}
	metrics, _ := c.Invalid(&cl)
	if len(metrics) != 1 {
		t.Errorf("invalid metrics = %v", metrics)
	}
	for _, why := range metrics {
		if !strings.Contains(why, "no scores") {
			t.Errorf("reason = %q", why)
		}
	}
}

func keyFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "api-key")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// logSink collects the log lines of a refresh.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (s *logSink) ctx(t *testing.T) context.Context {
	return log.IntoContext(t.Context(), logr.New(funcr.New(func(prefix, args string) {
		s.mu.Lock()
		s.lines = append(s.lines, prefix+" "+args)
		s.mu.Unlock()
	}, funcr.Options{}).GetSink()))
}

func (s *logSink) count(sub string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, l := range s.lines {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

func unauthorizedCount(reqs []string) int {
	n := 0
	for _, r := range reqs {
		if strings.HasPrefix(r, "401 ") {
			n++
		}
	}
	return n
}

func TestWithoutAKeyTheRefreshStopsAtTheFirst401(t *testing.T) {
	e := newKeeperEnv(t)
	e.H.RequireAPIKey("secret")
	reg := prometheus.NewRegistry()
	c := catalogOver(t, catalogtest.SourceAt(e.URL), catalog.Scope{"hcloud": nil},
		func(o *catalog.Options) { o.Registerer = reg })
	sink := &logSink{}
	err := c.Refresh(sink.ctx(t))
	if err == nil {
		t.Fatal("the refresh succeeded without a key")
	}
	if got := e.take(); unauthorizedCount(got) != 1 || len(got) != 2 {
		t.Errorf("requests = %v, want the healthcheck and one 401", got)
	}
	if n := sink.count("Keeper needs an API key"); n != 1 {
		t.Errorf("%d log lines, want one: %v", n, sink.lines)
	}
	if !strings.Contains(strings.Join(sink.lines, "\n"), "keyConfigured") {
		t.Errorf("the log line lacks keyConfigured: %v", sink.lines)
	}
	st := c.Status("c")
	if st.Ready || st.Reason != catalog.ReasonKeyRequired {
		t.Errorf("status = %+v", st)
	}
	if a := st.KeeperAuth; !a.Known || a.Authorized || a.Reason != catalog.ReasonAPIKeyMissing ||
		!strings.Contains(a.Message, "--keeper-api-key-file") || !strings.Contains(a.Message, "Keeper API key") {
		t.Errorf("keeper auth = %+v", a)
	}
	if v := gaugeValue(t, reg, "scarpenter_catalog_keeper_unauthorized"); v != 1 {
		t.Errorf("unauthorized gauge = %v", v)
	}
}

func TestARejectedKeyKeepsThePreviousCatalog(t *testing.T) {
	e := newKeeperEnv(t)
	reg := prometheus.NewRegistry()
	file := keyFile(t, "good\n")
	src := catalog.NewKeeperSource(catalog.KeeperOptions{URL: e.URL, APIKeyFile: file})
	c := catalogOver(t, src, catalog.Scope{"hcloud": nil}, func(o *catalog.Options) { o.Registerer = reg })
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := len(all(t, c, catalog.Filter{}))
	if st := c.Status("c"); !st.KeeperAuth.Authorized || gaugeValue(t, reg, "scarpenter_catalog_keeper_unauthorized") != 0 {
		t.Fatalf("after a success: %+v", st.KeeperAuth)
	}

	// Keeper now wants another key, and serves a new data version.
	e.H.RequireAPIKey("rotated")
	if err := e.H.AddHash(catalogtest.NewHash, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.H.SetCurrent(catalogtest.NewHash); err != nil {
		t.Fatal(err)
	}
	e.take()
	sink := &logSink{}
	if err := c.Refresh(sink.ctx(t)); err == nil {
		t.Fatal("the refresh succeeded with a rejected key")
	}
	if got := e.take(); unauthorizedCount(got) != 1 {
		t.Errorf("requests = %v, want exactly one 401", got)
	}
	st := c.Status("c")
	if !st.Ready || st.KeeperAuth.Reason != catalog.ReasonAPIKeyRejected || st.KeeperAuth.Authorized {
		t.Errorf("status = %+v", st)
	}
	if got := len(all(t, c, catalog.Filter{})); got != want {
		t.Errorf("the previous catalog changed: %d, want %d", got, want)
	}
	if n := sink.count("Keeper needs an API key"); n != 1 {
		t.Errorf("%d log lines: %v", n, sink.lines)
	}

	// The Secret is updated: the next refresh reads the file again, no restart.
	if err := os.WriteFile(file, []byte("rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.take()
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatalf("the rotated key was not picked up: %v", err)
	}
	if c.Version() != catalogtest.NewHash {
		t.Errorf("version = %s", c.Version())
	}
	if st := c.Status("c"); !st.KeeperAuth.Known || !st.KeeperAuth.Authorized {
		t.Errorf("keeper auth after the rotation = %+v", st.KeeperAuth)
	}
	if v := gaugeValue(t, reg, "scarpenter_catalog_keeper_unauthorized"); v != 0 {
		t.Errorf("unauthorized gauge = %v", v)
	}
}

func TestAnEmptyKeyFileIsAnError(t *testing.T) {
	e := newKeeperEnv(t)
	src := catalog.NewKeeperSource(catalog.KeeperOptions{URL: e.URL, APIKeyFile: keyFile(t, " \n")})
	c := catalogOver(t, src, catalog.Scope{"hcloud": nil})
	if err := c.Refresh(t.Context()); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("err = %v", err)
	}
	if got := e.take(); len(got) != 0 {
		t.Errorf("requests with an unusable key file: %v", got)
	}
}

func TestTheKeyIsSentAsABearerToken(t *testing.T) {
	e := newKeeperEnv(t)
	var mu sync.Mutex
	var auths []string
	e.setIntercept(func(_ http.ResponseWriter, r *http.Request) bool {
		mu.Lock()
		auths = append(auths, r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		return false
	})
	src := catalog.NewKeeperSource(catalog.KeeperOptions{URL: e.URL, APIKeyFile: keyFile(t, "  k-1  \n")})
	if _, err := src.Load(t.Context(), catalog.Request{Scope: catalog.Scope{"hcloud": nil}}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(auths) == 0 {
		t.Fatal("no request")
	}
	for _, a := range auths {
		if !strings.HasSuffix(a, " Bearer k-1") {
			t.Errorf("request %q", a)
		}
	}
}

func gaugeValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("metric %s is not exported", name)
	return 0
}

func TestPublishedTimestampMetric(t *testing.T) {
	e := newKeeperEnv(t)
	reg := prometheus.NewRegistry()
	c := catalogOver(t, catalogtest.SourceAt(e.URL), catalog.Scope{"hcloud": nil}, func(o *catalog.Options) { o.Registerer = reg })
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := float64(time.Date(2026, 11, 13, 19, 50, 14, 0, time.UTC).Unix())
	if got := gaugeValue(t, reg, "scarpenter_catalog_data_published_timestamp_seconds"); got != want {
		t.Errorf("published = %v, want %v", got, want)
	}
}

func TestPricesAndStorageRowsReachThePriceSink(t *testing.T) {
	e := newKeeperEnv(t)
	sink := &priceSink{StaticFacts: catalogtest.Facts(t, "aws")}
	c := catalog.New(catalog.Options{Source: catalogtest.SourceAt(e.URL), Facts: sink})
	c.SetClasses([]catalog.Class{catalog.NewClass("c", catalog.Scope{"aws": {"us-east-1"}})})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if sink.calls != 1 || sink.version != recordedHash(t) {
		t.Fatalf("sink: %d calls, version %q", sink.calls, sink.version)
	}
	var gp3, ipv4 bool
	for _, r := range sink.storage {
		if r.Vendor != "aws" || r.USD <= 0 || r.Unit == "" {
			t.Fatalf("bad storage row %+v", r)
		}
		gp3 = gp3 || (r.RegionID == "us-east-1" && r.StorageID == "gp3" && r.Unit == "GB/month")
	}
	for _, r := range sink.ipv4 {
		ipv4 = ipv4 || (r.RegionID == "us-east-1" && r.Unit == "hour" && r.USD > 0)
	}
	if !gp3 || !ipv4 {
		t.Errorf("gp3 %v, ipv4 %v of %d and %d rows", gp3, ipv4, len(sink.storage), len(sink.ipv4))
	}
	// The unchanged refresh does not call it again.
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if sink.calls != 1 {
		t.Errorf("%d sink calls after an unchanged refresh", sink.calls)
	}
}

type priceSink struct {
	catalog.StaticFacts
	calls   int
	version string
	storage []model.StoragePrice
	ipv4    []model.IPv4Price
}

func (p *priceSink) SetKeeperPrices(version string, storage []model.StoragePrice, ipv4 []model.IPv4Price) {
	p.calls++
	p.version, p.storage, p.ipv4 = version, storage, ipv4
}

// Selections read the score files like metrics and send no /servers request.
func TestSelectionsRunOnTheSnapshotData(t *testing.T) {
	e := newKeeperEnv(t)
	two := int32(2)
	three := int32(3)
	spec := selapi.Spec{
		Selector: &selapi.Selector{VCPUs: &selapi.IntRange{Min: &two}},
		Ranking: &selapi.Ranking{
			Score:     selapi.RankingScore{Benchmark: benchmark("geekbench:score", `{"cores":"single"}`)},
			Objective: selapi.ObjectiveScorePerPrice, Band: &selapi.RankingBand{WithinPercent: 20},
			Families: &selapi.RankingFamilies{Min: &three},
		},
	}
	id := catalog.SelectionQueryID("aws", []string{"us-east-1"}, &spec)
	data, err := catalogtest.SourceAt(e.URL).Load(t.Context(), catalog.Request{
		Scope:      catalog.Scope{"aws": {"us-east-1"}},
		Selections: []catalog.SelectionQuery{{ID: id, Vendor: "aws", Regions: []string{"us-east-1"}, Spec: spec}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if data.Invalid[id] != "" || len(data.Selected[id]) == 0 {
		t.Fatalf("members %d, invalid %q", len(data.Selected[id]), data.Invalid[id])
	}
	got := paths(e.take())
	if !slices.Contains(got, "/snapshot/benchmark_score/aws/geekbench:score") || !slices.Contains(got, "/benchmark_configs") {
		t.Errorf("requests = %v", got)
	}
	for _, p := range got {
		if !strings.HasPrefix(p, "/snapshot/") && p != "/healthcheck" && p != "/benchmark_configs" {
			t.Errorf("legacy request %s", p)
		}
	}
}

// Keeper's family is a broad class since sc-crawler 0.10.4; the catalog's family
// is the series, or the family where a vendor has none.
func TestFamilyIsTheSeries(t *testing.T) {
	e := newKeeperEnv(t)
	data, err := catalogtest.SourceAt(e.URL).Load(t.Context(), catalog.Request{
		Scope: catalog.Scope{"aws": {"us-east-1"}, "hcloud": nil, "upcloud": nil},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"aws/m7i.large": "m7i", "aws/t3a.small": "t3a"}
	got := map[string]string{}
	hcloud := map[string]bool{}
	for _, s := range data.Servers {
		got[s.Vendor+"/"+s.APIReference] = s.Family
		if s.Vendor == "hcloud" {
			hcloud[s.Family] = true
		}
		if s.Family == "" || (s.Vendor != "upcloud" && strings.Contains(s.Family, " ")) {
			t.Errorf("%s/%s has family %q", s.Vendor, s.APIReference, s.Family)
		}
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s family = %q, want %q", k, got[k], v)
		}
	}
	for _, f := range []string{"cx", "cpx", "ccx", "cax"} {
		if !hcloud[f] {
			t.Errorf("no hcloud family %s in %v", f, hcloud)
		}
	}
}
