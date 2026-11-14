/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// publishedFile holds when the hash was published, next to its manifest.
const publishedFile = "published-at"

// manifestFile is the name of a hash's manifest below its directory.
const manifestFile = "manifest.json.gz"

// diskCache keeps the snapshot files of one Keeper data version as the gzip
// bytes received: <dir>/<hash>/manifest.json.gz and
// <dir>/<hash>/<path>.json.gz. It trusts nothing it holds: the caller checks
// every file against the manifest's SHA-256. An empty dir disables it.
type diskCache struct{ dir string }

func (c diskCache) enabled() bool { return c.dir != "" }

func (c diskCache) hashDir(hash string) string { return filepath.Join(c.dir, hash) }

// file is the location of a snapshot file (rel is a path of the manifest, or
// manifestFile itself).
func (c diskCache) file(hash, rel string) string {
	if rel == "" {
		return filepath.Join(c.hashDir(hash), manifestFile)
	}
	return filepath.Join(c.hashDir(hash), filepath.FromSlash(rel)+".json.gz")
}

// read returns the stored bytes of a file.
func (c diskCache) read(hash, rel string) ([]byte, bool) {
	if !c.enabled() {
		return nil, false
	}
	b, err := os.ReadFile(c.file(hash, rel))
	return b, err == nil
}

// write stores a file through a temporary file and a rename, so that a reader
// never sees a partial one.
func (c diskCache) write(hash, rel string, gz []byte) error {
	if !c.enabled() {
		return nil
	}
	dst := c.file(hash, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return fmt.Errorf("writing the Keeper cache: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return fmt.Errorf("writing the Keeper cache: %w", err)
	}
	_, werr := tmp.Write(gz)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("writing the Keeper cache: %w", err)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("writing the Keeper cache: %w", err)
	}
	return nil
}

func (c diskCache) remove(hash, rel string) {
	if c.enabled() {
		_ = os.Remove(c.file(hash, rel))
	}
}

// previous returns the hash of the most recently written complete directory
// other than hash (one with a manifest), or "".
func (c diskCache) previous(hash string) string {
	if !c.enabled() {
		return ""
	}
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return ""
	}
	best, bestAt := "", int64(-1)
	for _, e := range entries {
		if !e.IsDir() || e.Name() == hash || !hashPattern.MatchString(e.Name()) {
			continue
		}
		info, err := os.Stat(c.file(e.Name(), ""))
		if err != nil {
			continue
		}
		if at := info.ModTime().UnixNano(); at > bestAt {
			best, bestAt = e.Name(), at
		}
	}
	return best
}

// prune deletes the directories of every hash but keep, and stray temporary
// files of the kept one.
func (c diskCache) prune(keep string) error {
	if !c.enabled() {
		return nil
	}
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("pruning the Keeper cache: %w", err)
	}
	var errs []error
	for _, e := range entries {
		// Only what the cache itself made: a directory of a hash. The directory
		// may be shared with other files, which stay.
		if e.Name() == keep || !e.IsDir() || !hashPattern.MatchString(e.Name()) {
			continue
		}
		errs = append(errs, os.RemoveAll(filepath.Join(c.dir, e.Name())))
	}
	_ = filepath.WalkDir(c.hashDir(keep), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), ".tmp-") {
			_ = os.Remove(p)
		}
		return nil
	})
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("pruning the Keeper cache: %w", err)
	}
	return nil
}

// safeName reports whether s can be a vendor or benchmark id in a file name:
// Keeper accepts only these (^[A-Za-z0-9_][A-Za-z0-9_:.-]*$), and they cannot
// climb out of the cache directory.
func safeName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		alnum := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_'
		if !alnum && (i == 0 || !slices.Contains([]rune{':', '.', '-'}, r)) {
			return false
		}
	}
	return true
}

// readMeta and writeMeta keep a small plain file of a hash's directory.
func (c diskCache) readMeta(hash, name string) ([]byte, bool) {
	if !c.enabled() {
		return nil, false
	}
	b, err := os.ReadFile(filepath.Join(c.hashDir(hash), name))
	return b, err == nil
}

func (c diskCache) writeMeta(hash, name string, b []byte) error {
	if !c.enabled() {
		return nil
	}
	if err := os.MkdirAll(c.hashDir(hash), 0o750); err != nil {
		return fmt.Errorf("writing the Keeper cache: %w", err)
	}
	if err := os.WriteFile(filepath.Join(c.hashDir(hash), name), b, 0o600); err != nil {
		return fmt.Errorf("writing the Keeper cache: %w", err)
	}
	return nil
}
