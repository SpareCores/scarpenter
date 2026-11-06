/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package testutil

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Golden compares got with the file testdata/<name> of the calling package
// (the test's working directory). With UPDATE_GOLDEN=1 (make golden) it writes
// the file instead; review the diff like code.
func Golden(t testing.TB, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // a test file next to the test
	if err != nil {
		t.Fatalf("reading golden file %s (UPDATE_GOLDEN=1 writes it): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file; run UPDATE_GOLDEN=1 go test and review the diff.\n--- got ---\n%s", path, firstLines(got, 60))
	}
}

func firstLines(b []byte, n int) []byte {
	lines := bytes.SplitAfterN(b, []byte("\n"), n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return bytes.Join(lines, nil)
}
