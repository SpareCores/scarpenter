//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package qemu

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"context"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/SpareCores/scarpenter/pkg/artifacts"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// The files of a release the driver uses. The image is published compressed;
// the firmware files (the same names the artifacts package lists) plain.
const (
	imageArchive = artifacts.FlatcarImage
	imageFile    = "flatcar_production_qemu_uefi_image.img"
	readyMarker  = "ready"
	// zeroBlock is the granularity at which the decompressed image is written
	// sparsely.
	zeroBlock = 64 * 1024
)

// release is the cached files of one Flatcar release and architecture.
type release struct{ dir string }

func (r release) image() string   { return filepath.Join(r.dir, imageFile) }
func (r release) efiCode() string { return filepath.Join(r.dir, artifacts.FlatcarEFICode) }
func (r release) efiVars() string { return filepath.Join(r.dir, artifacts.FlatcarEFIVars) }

// images downloads, verifies and caches Flatcar releases.
type images struct {
	cacheDir string
	resolver *artifacts.Resolver
	client   *http.Client

	// mu serialises the downloads: two launches of one release wait for one
	// download.
	mu sync.Mutex
}

func (im *images) releaseDir(channel, arch, version string) string {
	return filepath.Join(im.cacheDir, "flatcar", channel, arch, version)
}

// resolve returns the release's files, downloading them first when the cache
// does not hold them (a release is downloaded once: the files are verified
// against the release's digest file and the decompressed image is kept).
func (im *images) resolve(ctx context.Context, req vendors.ImageRequest) (release, int64, error) {
	rel := release{dir: im.releaseDir(req.Channel, req.Arch, req.Version)}
	if size, ok := im.cached(rel); ok {
		return rel, size, nil
	}
	im.mu.Lock()
	defer im.mu.Unlock()
	if size, ok := im.cached(rel); ok {
		return rel, size, nil
	}
	if err := os.MkdirAll(rel.dir, 0o755); err != nil {
		return rel, 0, vendors.NewError(vendors.Fatal, "ImageCache", err)
	}
	for _, file := range []string{artifacts.FlatcarEFICode, artifacts.FlatcarEFIVars} {
		if err := im.fetch(ctx, req, file, file); err != nil {
			return rel, 0, err
		}
	}
	if err := im.fetch(ctx, req, imageArchive, imageArchive); err != nil {
		return rel, 0, err
	}
	if _, err := decompress(filepath.Join(rel.dir, imageArchive), rel.image()); err != nil {
		return rel, 0, vendors.NewError(vendors.Fatal, "ImageDecompress", err)
	}
	size, err := qcow2Size(rel.image())
	if err != nil {
		return rel, 0, vendors.NewError(vendors.Fatal, "ImageFormat", err)
	}
	// The archive (600 MB) is not needed any more: the decompressed image is
	// verified by its format and the release is marked ready below.
	_ = os.Remove(filepath.Join(rel.dir, imageArchive))
	marker := filepath.Join(rel.dir, readyMarker)
	if err := os.WriteFile(marker, []byte("ready\n"), 0o644); err != nil { //nolint:gosec // a cache marker
		return rel, 0, vendors.NewError(vendors.Fatal, "ImageCache", err)
	}
	return rel, size, nil
}

// cached reports whether the release is complete and returns the image's
// virtual size.
func (im *images) cached(rel release) (int64, bool) {
	if _, err := os.Stat(filepath.Join(rel.dir, readyMarker)); err != nil {
		return 0, false
	}
	for _, f := range []string{rel.efiCode(), rel.efiVars()} {
		if _, err := os.Stat(f); err != nil {
			return 0, false
		}
	}
	size, err := qcow2Size(rel.image())
	return size, err == nil
}

// qcow2Size returns the virtual size of a qcow2 image from its header: the
// Flatcar QEMU image is a (compressed) qcow2 file, not a raw disk.
func qcow2Size(path string) (int64, error) {
	f, err := os.Open(path) //nolint:gosec // a path of the cache directory
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	var hdr [32]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return 0, fmt.Errorf("reading the header of %s: %w", path, err)
	}
	if string(hdr[:4]) != "QFI\xfb" {
		return 0, fmt.Errorf("%s is not a qcow2 image", path)
	}
	return int64(binary.BigEndian.Uint64(hdr[24:32])), nil //nolint:gosec // an image size
}

// fetch makes sure the release file is in the cache and matches the SHA512 of
// its digest file; a file that is already there (a download that finished
// earlier, a pre-seeded cache) is verified, not fetched again.
func (im *images) fetch(ctx context.Context, req vendors.ImageRequest, file, name string) error {
	url, err := im.resolver.FlatcarURL(req.Channel, req.Arch, req.Version, file)
	if err != nil {
		return vendors.NewError(vendors.Fatal, "ImageURL", err)
	}
	want, err := im.digest(ctx, url+".DIGESTS", file)
	if err != nil {
		return err
	}
	dst := filepath.Join(im.releaseDir(req.Channel, req.Arch, req.Version), name)
	if got, err := fileSHA512(dst); err == nil {
		if got == want {
			return nil
		}
		_ = os.Remove(dst)
	}
	part := dst + ".part"
	if err := im.download(ctx, url, part); err != nil {
		return err
	}
	got, err := fileSHA512(part)
	if err != nil {
		return vendors.NewError(vendors.Fatal, "ImageDigest", err)
	}
	if got != want {
		_ = os.Remove(part)
		return vendors.NewError(vendors.Fatal, "ImageDigest",
			fmt.Errorf("%s has the SHA512 %s, the release's digest file says %s", url, got, want))
	}
	if err := os.Rename(part, dst); err != nil {
		return vendors.NewError(vendors.Fatal, "ImageCache", err)
	}
	return nil
}

func (im *images) get(ctx context.Context, url string) (*http.Response, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, "ImageURL", err)
	}
	resp, err := im.client.Do(hreq)
	if err != nil {
		return nil, vendors.NewError(vendors.Retryable, "ImageDownload", fmt.Errorf("fetching %s: %w", url, err))
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		class := vendors.Retryable
		if resp.StatusCode == http.StatusNotFound {
			class = vendors.NotFound
		}
		return nil, vendors.NewError(class, "ImageDownload", fmt.Errorf("fetching %s: %s", url, resp.Status))
	}
	return resp, nil
}

func (im *images) download(ctx context.Context, url, dst string) error {
	resp, err := im.get(ctx, url)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := os.Create(dst) //nolint:gosec // a path of the cache directory
	if err != nil {
		return vendors.NewError(vendors.Fatal, "ImageCache", err)
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		_ = out.Close()
		return vendors.NewError(vendors.Retryable, "ImageDownload", fmt.Errorf("downloading %s: %w", url, err))
	}
	if err := out.Close(); err != nil {
		return vendors.NewError(vendors.Fatal, "ImageCache", err)
	}
	return nil
}

// digest fetches a release digest file and returns the SHA512 it lists for the
// file.
func (im *images) digest(ctx context.Context, url, file string) (string, error) {
	resp, err := im.get(ctx, url)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", vendors.NewError(vendors.Retryable, "ImageDownload", fmt.Errorf("reading %s: %w", url, err))
	}
	sum, err := parseDigests(body, file)
	if err != nil {
		return "", vendors.NewError(vendors.Fatal, "ImageDigest", fmt.Errorf("%s: %w", url, err))
	}
	return sum, nil
}

// parseDigests returns the SHA512 of the file from a Flatcar DIGESTS file:
// sections headed "# SHA512 HASH" (also MD5 and SHA1), each line "<hex>  <name>".
func parseDigests(body []byte, file string) (string, error) {
	section := ""
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if rest, ok := strings.CutPrefix(line, "#"); ok {
			section = strings.TrimSpace(rest)
			continue
		}
		fields := strings.Fields(line)
		if section == "SHA512 HASH" && len(fields) == 2 && fields[1] == file {
			if _, err := hex.DecodeString(fields[0]); err != nil || len(fields[0]) != sha512.Size*2 {
				return "", fmt.Errorf("the SHA512 of %s is malformed", file)
			}
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("no SHA512 for %s", file)
}

func fileSHA512(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // a path of the cache directory
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	h := sha512.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hashing %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// decompress unpacks a bzip2 file into a sparse file (blocks of zeros are
// skipped, not written) and returns its size. The file appears under its name
// only when complete.
func decompress(src, dst string) (int64, error) {
	in, err := os.Open(src) //nolint:gosec // a path of the cache directory
	if err != nil {
		return 0, fmt.Errorf("opening %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()
	part := dst + ".part"
	out, err := os.Create(part) //nolint:gosec // a path of the cache directory
	if err != nil {
		return 0, fmt.Errorf("creating %s: %w", part, err)
	}
	size, err := copySparse(out, bzip2.NewReader(in))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(part)
		return 0, fmt.Errorf("decompressing %s: %w", src, err)
	}
	if err := os.Rename(part, dst); err != nil {
		return 0, fmt.Errorf("moving %s into place: %w", part, err)
	}
	return size, nil
}

func copySparse(out *os.File, r io.Reader) (int64, error) {
	buf := make([]byte, zeroBlock)
	var off int64
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 && !isZero(buf[:n]) {
			if _, werr := out.WriteAt(buf[:n], off); werr != nil {
				return 0, werr
			}
		}
		off += int64(n)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	return off, out.Truncate(off)
}

func isZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
