/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog_test

import (
	"bufio"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
)

// countingTransport counts the requests and the bytes of the (gzip) response
// bodies of a refresh.
type countingTransport struct {
	next     http.RoundTripper
	requests atomic.Int64
	bytes    atomic.Int64
}

type countingBody struct {
	io.ReadCloser
	n *atomic.Int64
}

func (b countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n.Add(int64(n))
	return n, err
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.requests.Add(1)
	resp, err := c.next.RoundTrip(r)
	if err == nil {
		resp.Body = countingBody{resp.Body, &c.bytes}
	}
	return resp, err
}

// peakRSSMiB is the process's high-water resident set (Linux).
func peakRSSMiB() int {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "VmHWM:"); ok {
			var kb int
			for _, c := range strings.TrimSpace(rest) {
				if c < '0' || c > '9' {
					break
				}
				kb = kb*10 + int(c-'0')
			}
			return kb >> 10
		}
	}
	return 0
}

// TestRefreshCost logs what one refresh costs: requests, bytes on the wire,
// time, and the heap the catalog retains, for AWS with three regions, AWS with
// all regions and all vendors, with the three test metrics. It reads the
// committed recording; with SCARPENTER_COST_URL set it reads that Keeper
// instead (with the key of KEEPER_API_KEY_FILE), which is how the live numbers
// of the plan report are taken. Run one subtest alone for a meaningful peak:
//
//	go test ./pkg/catalog -run 'TestRefreshCost/aws-three-regions' -v
func TestRefreshCost(t *testing.T) {
	url := os.Getenv("SCARPENTER_COST_URL")
	scopes := map[string]catalog.Scope{
		"aws-three-regions": {"aws": threeRegions},
		"aws-all-regions":   {"aws": nil},
		"all-vendors": {"aws": nil, "gcp": nil, "azure": nil, "alicloud": nil, "hcloud": nil, "ovh": nil,
			"upcloud": nil, "vultr": nil},
	}
	for name, scope := range scopes {
		t.Run(name, func(t *testing.T) {
			if url == "" {
				url = catalogtest.Server(t).URL
			}
			var vendors []string
			for v := range scope {
				vendors = append(vendors, v)
			}
			ct := &countingTransport{next: http.DefaultTransport}
			src := catalog.NewKeeperSource(catalog.KeeperOptions{
				URL: url, UserAgent: "scarpenter-cost-test", HTTPClient: &http.Client{Transport: ct, Timeout: 5 * time.Minute},
				APIKeyFile: os.Getenv("KEEPER_API_KEY_FILE"),
			})
			cl := catalog.NewClass("c", scope)
			cl.Metrics = map[string]v1alpha1.Metric{
				"single-core": {Benchmark: benchmark("geekbench:score", `{"cores":"single"}`)},
				"stress":      {Benchmark: benchmark("stress_ng:bestn", "")},
				"latency":     {Benchmark: benchmark("membench:latency", `{"scope":"RAM"}`)},
			}
			cl.Finalize()
			c := catalog.New(catalog.Options{Source: src, Facts: catalogtest.Facts(t, vendors...)})
			c.SetClasses([]catalog.Class{cl})

			start := time.Now()
			if err := c.Refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			took := time.Since(start)
			peak := peakRSSMiB()
			its, err := c.InstanceTypes(t.Context(), catalog.Query{Class: "c"})
			if err != nil {
				t.Fatal(err)
			}
			offerings := 0
			for _, it := range its {
				offerings += len(it.Offerings)
			}
			types := len(its)
			runtime.GC()
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			t.Logf("%s: refresh %s, %d requests, %.1f MiB gzip on the wire; %d types, %d offerings; "+
				"heap in use after a GC %d MiB (all of the test process); peak resident set %d MiB",
				name, took.Round(time.Millisecond), ct.requests.Load(), float64(ct.bytes.Load())/(1<<20),
				types, offerings, ms.HeapInuse>>20, peak)
			runtime.KeepAlive(c)
		})
	}
}
