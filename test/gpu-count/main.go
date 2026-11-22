/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Command gpu-count prints the GPU count Keeper's committed snapshot
// (test/data/sc-snapshot) gives a server type, for the manual `gpu` scenario of the
// L5 lanes that cannot read it from the vendor's API:
//
//	gpu-count <vendor ID> <api reference>
//
// It exits 1 when the type is not in the snapshot or has no GPU.
package main

import (
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"

	"github.com/SpareCores/sc-keeper-go/columnar"
)

type server struct {
	APIReference string  `json:"api_reference"`
	Accelerators float64 `json:"accelerator_count"`
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: gpu-count <vendor ID> <api reference>")
		os.Exit(2)
	}
	n, err := count(os.Args[1], os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "gpu-count:", err)
		os.Exit(1)
	}
	fmt.Println(n)
}

func count(vendor, apiReference string) (int, error) {
	path := filepath.Join("test", "data", "sc-snapshot", "snapshot", "server", vendor+".json.gz")
	f, err := os.Open(path) //nolint:gosec // the argument names a vendor file
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return 0, err
	}
	rows, err := columnar.DecodeRows[server](zr, -1)
	if err != nil {
		return 0, err
	}
	for _, s := range rows {
		if s.APIReference == apiReference {
			if s.Accelerators < 1 || s.Accelerators != float64(int(s.Accelerators)) {
				return 0, fmt.Errorf("%s/%s has %v accelerators, not a whole GPU count", vendor, apiReference, s.Accelerators)
			}
			return int(s.Accelerators), nil
		}
	}
	return 0, fmt.Errorf("%s/%s is not in Keeper's snapshot", vendor, apiReference)
}
