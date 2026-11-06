/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog_test

import (
	"os"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/catalog/labels"
)

// The operator registers the per-NodeClass label keys with core before core's
// controllers start; the tests do the same, once, before any test runs.
func TestMain(m *testing.M) {
	labels.RegisterKeys(labels.FlagKeys()...)
	labels.RegisterKeys(labels.MetricLabelKeys([]string{"single-core", "multi-core", "stress", "latency", "broken"})...)
	os.Exit(m.Run())
}
