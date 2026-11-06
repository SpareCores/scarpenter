/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package version

import (
	"runtime"
	"strings"
	"testing"
)

func TestGetUsesBuildTimeValues(t *testing.T) {
	oldVersion, oldCommit := Version, Commit
	t.Cleanup(func() { Version, Commit = oldVersion, oldCommit })
	Version, Commit = "1.2.3", "0123456789abcdef"

	info := Get()
	if info.Version != "1.2.3" || info.Commit != "0123456789abcdef" || info.Modified {
		t.Fatalf("unexpected info %+v", info)
	}
	if info.GoVersion != runtime.Version() {
		t.Fatalf("GoVersion = %q", info.GoVersion)
	}
	if got := info.String(); !strings.Contains(got, "1.2.3") || !strings.Contains(got, "0123456789abcdef") {
		t.Fatalf("String() = %q", got)
	}
}

func TestStringWithoutCommit(t *testing.T) {
	got := Info{Version: "dev", Modified: true, GoVersion: "go0"}.String()
	if got != "scarpenter dev (commit unknown-dirty, go0)" {
		t.Fatalf("String() = %q", got)
	}
}
