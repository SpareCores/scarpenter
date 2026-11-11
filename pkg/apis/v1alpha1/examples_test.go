/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package v1alpha1_test

import (
	"path/filepath"
	"testing"

	"github.com/SpareCores/scarpenter/internal/testutil"
)

// TestPublishedExamplesAreAccepted sends every example NodeClass of docs/examples to the
// API server with the generated CRD installed, the way kubectl apply does: the schema,
// the defaults and the CEL rules accept it, so an example never drifts from the API.
// (internal/crdschema checks the same files against the published JSON Schema, and the
// driver packages against their selector declarations.)
func TestPublishedExamplesAreAccepted(t *testing.T) {
	examples, err := filepath.Glob(filepath.Join(testutil.RepoRoot(), "docs", "examples", "*.yaml"))
	if err != nil || len(examples) < 4 {
		t.Fatalf("expected the examples under docs/examples, got %v (err %v)", examples, err)
	}
	for _, f := range examples {
		t.Run(filepath.Base(f), func(t *testing.T) {
			if err := create(t, loadObject(t, f)); err != nil {
				t.Fatalf("create: %v", err)
			}
		})
	}
}
