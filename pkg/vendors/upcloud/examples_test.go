/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud_test

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/resources"
)

// The UpCloud examples of docs/examples name only what the driver's declaration supports
// for each kind (the API server's CRD check is in pkg/apis/v1alpha1), and the driver
// accepts the NodeClass.
func TestThePublishedUpCloudExamplesUseWhatTheDriverSupports(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(testutil.RepoRoot(), "docs", "examples", "upcloud-*.yaml"))
	if err != nil || len(files) < 2 {
		t.Fatalf("expected the UpCloud examples, got %v (err %v)", files, err)
	}
	e := newEnv(t, envOptions{})
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
			if err := resources.ValidateUses(&nc, "upcloud", e.d); err != nil {
				t.Error(err)
			}
			if err := resources.CheckUsesSelect(&nc); err != nil {
				t.Error(err)
			}
			if err := e.d.ValidateNodeClass(e.ctx(), &nc); err != nil {
				t.Error(err)
			}
		})
	}
}
