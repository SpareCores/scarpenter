/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Command replay serves the committed Spare Cores snapshot as a Keeper API, and
// the SHA256SUMS fixtures of the sysext releases under /sysext, for the L3 lane,
// so the controller under test never leaves the cluster.
package main

import (
	"errors"
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/SpareCores/sc-keeper-go/keepertest"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dir := flag.String("snapshot", "test/data/sc-snapshot", "snapshot directory")
	sysext := flag.String("sysext", "pkg/artifacts/testdata", "<release>/SHA256SUMS fixtures, served under /sysext")
	flag.Parse()

	h, err := keepertest.NewHandler(*dir)
	if err != nil {
		log.Fatalf("replay: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/sysext/", http.StripPrefix("/sysext", http.FileServer(http.Dir(*sysext))))
	mux.Handle("/", h)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("replaying snapshot %s on %s", *dir, *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("replay: %v", err)
	}
}
