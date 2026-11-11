/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package qemu

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/resources"
)

// The qemu example of docs/examples names only what the driver's declaration supports.
func TestThePublishedQemuExampleUsesWhatTheDriverSupports(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(testutil.RepoRoot(), "docs", "examples", "qemu.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var nc v1alpha1.SCNodeClass
	if err := yaml.UnmarshalStrict(raw, &nc); err != nil {
		t.Fatal(err)
	}
	d, _ := newTestDriver(t)
	if err := resources.ValidateUses(&nc, "qemu", d); err != nil {
		t.Error(err)
	}
}
