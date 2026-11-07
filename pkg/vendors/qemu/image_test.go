//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package qemu

import (
	"bytes"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"

	"github.com/SpareCores/scarpenter/pkg/artifacts"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func newResolver(base string) *artifacts.Resolver {
	return artifacts.New(artifacts.Config{FlatcarBase: base})
}

// qcow2Header is the start of a qcow2 file with the virtual size.
func qcow2Header(virtual uint64) []byte {
	hdr := make([]byte, 32)
	copy(hdr, "QFI\xfb")
	binary.BigEndian.PutUint32(hdr[4:], 2)
	binary.BigEndian.PutUint64(hdr[24:], virtual)
	return hdr
}

// imageContent is what testdata/image.img.bz2 holds: data around long runs of
// zeros, which the decompression must write sparsely and reproduce exactly.
func imageContent() []byte {
	var b bytes.Buffer
	b.Write(qcow2Header(9 << 30))
	b.WriteString("FLATCAR")
	b.Write(make([]byte, 200*1024))
	b.WriteString("middle-of-the-image")
	b.Write(make([]byte, 64*1024*3))
	b.WriteString("tail")
	return b.Bytes()
}

// releaseServer serves a Flatcar release with digest files, and counts the
// requests per path.
type releaseServer struct {
	*httptest.Server
	files map[string][]byte
	hits  map[string]*atomic.Int32
	// wrongDigest makes the digest file of the image list another SHA512.
	wrongDigest bool
}

func newReleaseServer(t *testing.T) *releaseServer {
	t.Helper()
	archive, err := os.ReadFile("testdata/image.img.bz2")
	if err != nil {
		t.Fatal(err)
	}
	s := &releaseServer{files: map[string][]byte{
		"flatcar_production_qemu_uefi_image.img.bz2":  archive,
		"flatcar_production_qemu_uefi_efi_code.qcow2": []byte("code"),
		"flatcar_production_qemu_uefi_efi_vars.qcow2": []byte("vars"),
	}, hits: map[string]*atomic.Int32{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/amd64-usr/4757.2.3/"
		name, ok := strings.CutPrefix(r.URL.Path, prefix)
		if !ok {
			http.NotFound(w, r)
			return
		}
		c := s.hits[name]
		if c == nil {
			c = &atomic.Int32{}
			s.hits[name] = c
		}
		c.Add(1)
		if file, ok := strings.CutSuffix(name, ".DIGESTS"); ok {
			body, known := s.files[file]
			if !known {
				http.NotFound(w, r)
				return
			}
			sum := sha512.Sum512(body)
			digest := hex.EncodeToString(sum[:])
			if s.wrongDigest && strings.HasSuffix(file, ".bz2") {
				digest = strings.Repeat("0", len(digest))
			}
			_, _ = fmt.Fprintf(w, "# MD5 HASH\n%s  %s\n# SHA1 HASH\n%s  %s\n# SHA512 HASH\n%s  %s\n",
				strings.Repeat("1", 32), file, strings.Repeat("2", 40), file, digest, file)
			return
		}
		body, known := s.files[name]
		if !known {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func (s *releaseServer) hitCount(name string) int32 {
	if c := s.hits[name]; c != nil {
		return c.Load()
	}
	return 0
}

func imageDriver(t *testing.T, base string) *Driver {
	t.Helper()
	root := t.TempDir()
	opts := DefaultOptions()
	opts.StateDir, opts.CacheDir = filepath.Join(root, "state"), filepath.Join(root, "cache")
	d := New(opts)
	d.host = newFakeHost()
	if err := d.Init(t.Context(), vendors.Config{Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
	d.imgs.resolver = newResolver(base)
	return d
}

var imageReq = vendors.ImageRequest{Region: Region, Arch: "amd64", Channel: "stable", Version: "4757.2.3"}

func TestResolveImageDownloadsVerifiesDecompressesAndCaches(t *testing.T) {
	srv := newReleaseServer(t)
	d := imageDriver(t, srv.URL)
	img, err := d.ResolveImage(t.Context(), imageReq)
	if err != nil {
		t.Fatal(err)
	}
	want := imageContent()
	if img.RootDiskBytes != 9<<30 {
		t.Errorf("RootDiskBytes %d, want the image's virtual size %d", img.RootDiskBytes, int64(9<<30))
	}
	got, err := os.ReadFile(filepath.Join(d.opts.CacheDir, img.Ref, imageFile))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("the decompressed image differs from the original (%v)", err)
	}
	downloads := srv.hitCount("flatcar_production_qemu_uefi_image.img.bz2")
	if downloads != 1 {
		t.Errorf("the image was downloaded %d times", downloads)
	}

	// A second resolution, also by a new driver (a restart), is a cache hit.
	d2 := New(d.opts)
	d2.host = d.host
	if err := d2.Init(t.Context(), vendors.Config{Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
	d2.imgs.resolver = d.imgs.resolver
	if _, err := d2.ResolveImage(t.Context(), imageReq); err != nil {
		t.Fatal(err)
	}
	if n := srv.hitCount("flatcar_production_qemu_uefi_image.img.bz2"); n != downloads {
		t.Errorf("a cached release was downloaded again (%d requests)", n)
	}
}

func TestResolveImageRefusesAFileThatDoesNotMatchTheDigest(t *testing.T) {
	srv := newReleaseServer(t)
	srv.wrongDigest = true
	d := imageDriver(t, srv.URL)
	_, err := d.ResolveImage(t.Context(), imageReq)
	if !vendors.IsClass(err, vendors.Fatal) || vendors.CodeOf(err) != "ImageDigest" {
		t.Fatalf("a corrupt download: %v, want a Fatal ImageDigest error", err)
	}
	if _, err := os.Stat(filepath.Join(d.opts.CacheDir, "flatcar/stable/amd64/4757.2.3", readyMarker)); err == nil {
		t.Error("the release was marked ready")
	}
	// Once the mirror is fixed, the next attempt downloads again and succeeds.
	srv.wrongDigest = false
	if _, err := d.ResolveImage(t.Context(), imageReq); err != nil {
		t.Fatalf("after the digest was fixed: %v", err)
	}
}

func TestResolveImageVerifiesAPreSeededFileInsteadOfDownloadingIt(t *testing.T) {
	srv := newReleaseServer(t)
	d := imageDriver(t, srv.URL)
	dir := filepath.Join(d.opts.CacheDir, "flatcar/stable/amd64/4757.2.3")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, imageArchive), srv.files[imageArchive], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ResolveImage(t.Context(), imageReq); err != nil {
		t.Fatal(err)
	}
	if n := srv.hitCount(imageArchive); n != 0 {
		t.Errorf("the verified, pre-seeded archive was downloaded (%d requests)", n)
	}
}

func TestResolveImageOfAMissingReleaseIsNotFound(t *testing.T) {
	srv := newReleaseServer(t)
	d := imageDriver(t, srv.URL)
	req := imageReq
	req.Version = "1.2.3"
	if _, err := d.ResolveImage(t.Context(), req); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("an unknown release: %v, want NotFound", err)
	}
}

func TestParseDigests(t *testing.T) {
	sha := strings.Repeat("ab", 64)
	for name, tc := range map[string]struct {
		body    string
		wantErr bool
	}{
		"listed":        {"# MD5 HASH\n" + strings.Repeat("1", 32) + "  f\n# SHA512 HASH\n" + sha + "  f\n", false},
		"other file":    {"# SHA512 HASH\n" + sha + "  g\n", true},
		"only md5":      {"# MD5 HASH\n" + strings.Repeat("1", 32) + "  f\n", true},
		"malformed sum": {"# SHA512 HASH\nzz  f\n", true},
		"short sum":     {"# SHA512 HASH\nabcd  f\n", true},
	} {
		got, err := parseDigests([]byte(tc.body), "f")
		if (err != nil) != tc.wantErr || (err == nil && got != sha) {
			t.Errorf("%s: %q, %v", name, got, err)
		}
	}
}

func TestCopySparseSkipsZeroBlocksAndKeepsTheSize(t *testing.T) {
	want := imageContent()
	f, err := os.Create(filepath.Join(t.TempDir(), "img"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	size, err := copySparse(f, bytes.NewReader(want))
	if err != nil || size != int64(len(want)) {
		t.Fatalf("copySparse = %d, %v", size, err)
	}
	got, _ := os.ReadFile(f.Name())
	if !bytes.Equal(got, want) {
		t.Error("the sparse copy differs")
	}
}
