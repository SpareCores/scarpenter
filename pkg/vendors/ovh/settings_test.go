/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func TestParseSettings(t *testing.T) {
	s, err := ParseSettings(nil)
	if err != nil || s.ConfigDrive == nil || !*s.ConfigDrive || s.RootDiskBytes() != 0 {
		t.Fatalf("defaults: %+v %v", s, err)
	}
	if _, err := ParseSettings(json.RawMessage(`null`)); err != nil {
		t.Fatal(err)
	}
	s, err = ParseSettings(json.RawMessage(`{"configDrive":false,"storage":{"root":{"bootFromVolume":{"volumeType":"high-speed","size":"64Gi"}}},"networking":{"publicIPv4":true,"publicIPv6":false}}`))
	if err != nil || *s.ConfigDrive || s.RootDiskBytes() != 64<<30 {
		t.Fatalf("%+v %v", s, err)
	}
	// A decimal size rounds up to whole GiB.
	s, _ = ParseSettings(json.RawMessage(`{"storage":{"root":{"bootFromVolume":{"volumeType":"classic","size":"11G"}}}}`))
	if s.RootDiskBytes() != 11<<30 {
		t.Errorf("11 GB rounds up to 11 GiB, got %d", s.RootDiskBytes())
	}
}

func TestSettingsValidation(t *testing.T) {
	vol := func(n int) string {
		parts := make([]string, 0, n)
		for i := range n {
			parts = append(parts, fmt.Sprintf(`{"name":"d%d","volumeType":"classic","size":"10Gi"}`, i))
		}
		return `{"storage":{"data":[` + strings.Join(parts, ",") + `]}}`
	}
	for name, raw := range map[string]string{
		"private only":        `{"networking":{"publicIPv4":false}}`,
		"local storage":       `{"storage":{"localStorage":{"mode":"mount"}}}`,
		"volume type":         `{"storage":{"root":{"bootFromVolume":{"volumeType":"nvme","size":"64Gi"}}}}`,
		"too small":           `{"storage":{"root":{"bootFromVolume":{"volumeType":"classic","size":"5Gi"}}}}`,
		"too big":             `{"storage":{"root":{"bootFromVolume":{"volumeType":"classic","size":"12001Gi"}}}}`,
		"nine data volumes":   vol(9),
		"repeated name":       `{"storage":{"data":[{"name":"a","volumeType":"classic","size":"10Gi"},{"name":"a","volumeType":"classic","size":"10Gi"}]}}`,
		"unnamed":             `{"storage":{"data":[{"volumeType":"classic","size":"10Gi"}]}}`,
		"data volume type":    `{"storage":{"data":[{"name":"a","volumeType":"x","size":"10Gi"}]}}`,
		"unknown field":       `{"flavour":"b3-8"}`,
		"not json":            `{`,
		"wrong type for size": `{"storage":{"root":{"bootFromVolume":{"volumeType":"classic","size":{}}}}}`,
	} {
		_, err := ParseSettings(json.RawMessage(raw))
		if err == nil || !vendors.IsClass(err, vendors.Fatal) || vendors.CodeOf(err) != CodeInvalidSettings {
			t.Errorf("%s: want InvalidSettings, got %v", name, err)
		}
	}
	if _, err := ParseSettings(json.RawMessage(vol(8))); err != nil {
		t.Errorf("eight data volumes are fine: %v", err)
	}
}
