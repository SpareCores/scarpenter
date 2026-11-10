/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Command canary is the free check of the outside world that the real-cloud lanes (L5)
// depend on: no instance, no cluster, read-only credentials. It runs the real driver's read
// paths and the artifact resolver against what the lane pins, and fails when the world
// moved: an image that is gone, a quota that shrank, an instance type no longer offered in
// the region, a release that lost its checksum file.
//
//	test/canary/run.sh aws            # loads the lane's pins, redacts the output
//	go run ./test/canary -vendor aws  # the same without the pins' defaults: needs their env
//
// The vendor part is vendorChecks; a new vendor adds one entry and one file. The checks
// every vendor shares (the sysext checksums the nodes download, the k3s release) are in
// artifacts.go. The exit status is 1 when a check failed, 2 for a usage error.
// docs/how-to-guides/testing-and-releasing.md#the-canary describes the checks and the
// read-only role.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
)

// vendorChecks maps a vendor of test/e2e-vendors.txt to its checks.
var vendorChecks = map[string]func(ctx context.Context, cfg *config, rep *report){
	"aws": checkAWS,
}

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("canary", flag.ContinueOnError)
	fs.SetOutput(stderr)
	vendor := fs.String("vendor", "", "the vendor to check (one of: "+strings.Join(knownVendors(), ", ")+")")
	timeout := fs.Duration("timeout", 10*time.Minute, "the limit for all checks together")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	checks, ok := vendorChecks[*vendor]
	if !ok {
		_, _ = fmt.Fprintf(stderr, "canary: -vendor must be one of %s\n", strings.Join(knownVendors(), ", "))
		return 2
	}
	cfg, err := loadConfig(*vendor, getenv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "canary:", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	rep := &report{}
	checkArtifacts(ctx, cfg, rep)
	checks(ctx, cfg, rep)
	rep.write(stdout)
	if n := rep.failures(); n > 0 {
		_, _ = fmt.Fprintf(stderr, "canary: %d of %d checks failed\n", n, len(rep.rows))
		return 1
	}
	return 0
}

func knownVendors() []string {
	out := make([]string, 0, len(vendorChecks))
	for v := range vendorChecks {
		out = append(out, v)
	}
	slices.Sort(out)
	return out
}

// row is the result of one check.
type row struct {
	name   string
	detail string
	err    error
	took   time.Duration
}

// report collects the checks and prints them as a table.
type report struct{ rows []row }

// check runs fn and records its result: detail is what it found, a non-nil error fails it.
func (r *report) check(name string, fn func() (string, error)) {
	start := time.Now()
	detail, err := fn()
	r.rows = append(r.rows, row{name: name, detail: detail, err: err, took: time.Since(start)})
}

func (r *report) failures() int {
	n := 0
	for _, x := range r.rows {
		if x.err != nil {
			n++
		}
	}
	return n
}

// write prints the table; a failure's error goes on lines of its own below it.
func (r *report) write(w io.Writer) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "STATUS\tCHECK\tFOUND\tTOOK")
	for _, x := range r.rows {
		status := "ok"
		if x.err != nil {
			status = "FAIL"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", status, x.name, oneLine(x.detail), x.took.Round(time.Millisecond))
	}
	_ = tw.Flush()
	for _, x := range r.rows {
		if x.err != nil {
			_, _ = fmt.Fprintf(w, "\n%s: %s\n", x.name, oneLine(x.err.Error()))
		}
	}
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
