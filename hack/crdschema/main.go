/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Command crdschema writes the JSON Schema of the SCNodeClass from the
// generated CRD, so that an infrastructure tool can validate the NodeClass it
// renders without a cluster: `make docs-api`. The schema is published with each
// release (docs/reference/infrastructure-handoff.md).
package main

import (
	"fmt"
	"os"

	"github.com/SpareCores/scarpenter/internal/crdschema"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: crdschema <crd.yaml>")
		os.Exit(2)
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "crdschema:", err)
		os.Exit(1)
	}
	out, err := crdschema.Convert(raw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "crdschema:", err)
		os.Exit(1)
	}
	if _, err := os.Stdout.Write(out); err != nil {
		fmt.Fprintln(os.Stderr, "crdschema:", err)
		os.Exit(1)
	}
}
