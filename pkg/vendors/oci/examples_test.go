/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci_test

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/resources"
)

// The OCI examples of docs/examples name only what the driver's declaration supports for each
// kind, and pass the driver's own validation (the API server's CRD check is in pkg/apis/v1alpha1).
func TestThePublishedOCIExamplesUseWhatTheDriverSupports(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(testutil.RepoRoot(), "docs", "examples", "oci-*.yaml"))
	if err != nil || len(files) < 3 {
		t.Fatalf("expected the OCI examples, got %v (err %v)", files, err)
	}
	e := newEnv(t)
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var nc v1alpha1.SCNodeClass
			if err := yaml.UnmarshalStrict(raw, &nc); err != nil {
				t.Fatal(err)
			}
			if err := resources.ValidateUses(&nc, "oci", e.d); err != nil {
				t.Error(err)
			}
			if err := resources.CheckUsesSelect(&nc); err != nil {
				t.Error(err)
			}
			if err := e.d.ValidateNodeClass(t.Context(), &nc); err != nil {
				t.Errorf("ValidateNodeClass: %v", err)
			}
		})
	}
}
