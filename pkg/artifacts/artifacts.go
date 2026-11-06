/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package artifacts builds the URLs of the files a node boots from and
// resolves the checksums of the sysext images (overview section 8.4). It has
// no Kubernetes client: the NodeClass controller fetches the SHA256SUMS of a
// release once, caches it by URL and writes the digests to the NodeClass
// status, so rendering never fetches anything.
package artifacts

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"k8s.io/utils/clock"
)

// Upstream bases and cache lifetimes.
const (
	// DefaultSysextBase is where the sysext-bakery publishes its releases.
	DefaultSysextBase = "https://github.com/flatcar/sysext-bakery/releases/download"
	// DefaultFlatcarBase is the Flatcar release server; {channel} is replaced
	// by the channel name (stable, beta, alpha, lts).
	DefaultFlatcarBase = "https://{channel}.release.flatcar-linux.net"

	// DefaultTTL is how long a fetched SHA256SUMS is served from the cache. A
	// release's checksums do not change, but a mirror may be fixed up.
	DefaultTTL = time.Hour
	// DefaultErrorTTL is how long a failed fetch is remembered, so that a
	// NodeClass pointing at a missing release does not hit the host on every
	// reconcile.
	DefaultErrorTTL = time.Minute
	// maxChecksumsSize bounds the SHA256SUMS body.
	maxChecksumsSize = 1 << 20
)

// Architectures of the node (the Kubernetes spelling) and the spelling of the
// sysext-bakery's file names.
const (
	ArchAMD64 = "amd64"
	ArchARM64 = "arm64"
)

// ErrNotFound is returned (wrapped) when the release or its SHA256SUMS does
// not exist (HTTP 404): the NodeClass names a version that is not published.
var ErrNotFound = errors.New("not found")

// ErrChecksumMissing is returned (wrapped) when the SHA256SUMS of a release has
// no entry for the image of the architecture.
var ErrChecksumMissing = errors.New("no checksum for the image")

// Artifact is a downloadable image pinned by its checksum.
type Artifact struct {
	URL    string
	SHA256 string
}

// SysextArch returns the architecture spelling of the sysext-bakery's file
// names for a Kubernetes architecture.
func SysextArch(arch string) (string, error) {
	switch arch {
	case ArchAMD64:
		return "x86-64", nil
	case ArchARM64:
		return "arm64", nil
	}
	return "", fmt.Errorf("unsupported architecture %q (want %s or %s)", arch, ArchAMD64, ArchARM64)
}

// Config configures a Resolver.
type Config struct {
	// SysextBase and FlatcarBase are the (mirror) bases; empty means upstream.
	SysextBase  string
	FlatcarBase string
	// HTTPClient fetches the checksum files; nil uses a client with a 30 s
	// timeout.
	HTTPClient *http.Client
	Clock      clock.Clock
	// TTL and ErrorTTL default to DefaultTTL and DefaultErrorTTL.
	TTL      time.Duration
	ErrorTTL time.Duration
}

// Resolver builds artifact URLs and resolves checksums. It is safe for
// concurrent use.
type Resolver struct {
	sysextBase  string
	flatcarBase string
	client      *http.Client
	clock       clock.Clock
	ttl         time.Duration
	errorTTL    time.Duration

	mu    sync.Mutex
	cache map[string]*entry
}

type entry struct {
	sums    map[string]string
	err     error
	expires time.Time
	// fetching is closed when the fetch that fills the entry is done, so that
	// concurrent callers share one request.
	fetching chan struct{}
}

// New returns a Resolver.
func New(cfg Config) *Resolver {
	r := &Resolver{
		sysextBase:  strings.TrimRight(cfg.SysextBase, "/"),
		flatcarBase: strings.TrimRight(cfg.FlatcarBase, "/"),
		client:      cfg.HTTPClient,
		clock:       cfg.Clock,
		ttl:         cfg.TTL,
		errorTTL:    cfg.ErrorTTL,
		cache:       map[string]*entry{},
	}
	if r.sysextBase == "" {
		r.sysextBase = DefaultSysextBase
	}
	if r.flatcarBase == "" {
		r.flatcarBase = DefaultFlatcarBase
	}
	if r.client == nil {
		r.client = &http.Client{Timeout: 30 * time.Second}
	}
	if r.clock == nil {
		r.clock = clock.RealClock{}
	}
	if r.ttl <= 0 {
		r.ttl = DefaultTTL
	}
	if r.errorTTL <= 0 {
		r.errorTTL = DefaultErrorTTL
	}
	return r
}

var (
	sysextNamePattern    = regexp.MustCompile(`^[a-z0-9]([-a-z0-9_.]*[a-z0-9])?$`)
	sysextVersionPattern = regexp.MustCompile(`^v?[0-9]+(\.[0-9]+)*([-+][0-9A-Za-z.+-]+)?$`)
)

// SysextRelease is the release a sysext-bakery image belongs to: the release
// tag is "<name>-<version>", for example kubernetes-v1.37.1.
func SysextRelease(name, version string) (string, error) {
	if !sysextNamePattern.MatchString(name) {
		return "", fmt.Errorf("invalid sysext name %q", name)
	}
	if !sysextVersionPattern.MatchString(version) {
		return "", fmt.Errorf("invalid sysext version %q", version)
	}
	return name + "-" + version, nil
}

// SysextURL is the URL of the image: <base>/<name>-<version>/<name>-<version>-<arch>.raw.
func (r *Resolver) SysextURL(name, version, arch string) (string, error) {
	release, err := SysextRelease(name, version)
	if err != nil {
		return "", err
	}
	sa, err := SysextArch(arch)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/%s/%s-%s.raw", r.sysextBase, release, release, sa), nil
}

// ChecksumsURL is the URL of the SHA256SUMS of the release.
func (r *Resolver) ChecksumsURL(name, version string) (string, error) {
	release, err := SysextRelease(name, version)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/%s/SHA256SUMS", r.sysextBase, release), nil
}

// Sysext resolves the image of the release for the architecture: its URL and
// the sha256 from the release's SHA256SUMS. The checksum file is fetched once
// per TTL however many architectures and callers ask.
func (r *Resolver) Sysext(ctx context.Context, name, version, arch string) (Artifact, error) {
	u, err := r.SysextURL(name, version, arch)
	if err != nil {
		return Artifact{}, err
	}
	sumsURL, err := r.ChecksumsURL(name, version)
	if err != nil {
		return Artifact{}, err
	}
	sums, err := r.checksums(ctx, sumsURL)
	if err != nil {
		return Artifact{}, fmt.Errorf("resolving %s %s (%s): %w", name, version, arch, err)
	}
	file := u[strings.LastIndex(u, "/")+1:]
	sum, ok := sums[file]
	if !ok {
		return Artifact{}, fmt.Errorf("resolving %s %s (%s): %w: %s is not in %s", name, version, arch,
			ErrChecksumMissing, file, sumsURL)
	}
	return Artifact{URL: u, SHA256: sum}, nil
}

// checksums returns the parsed SHA256SUMS at the URL, cached by URL.
func (r *Resolver) checksums(ctx context.Context, url string) (map[string]string, error) {
	for {
		r.mu.Lock()
		e, ok := r.cache[url]
		switch {
		case ok && e.fetching != nil:
			wait := e.fetching
			r.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		case ok && r.clock.Now().Before(e.expires):
			sums, err := e.sums, e.err
			r.mu.Unlock()
			return sums, err
		}
		e = &entry{fetching: make(chan struct{})}
		r.cache[url] = e
		r.mu.Unlock()

		sums, err := r.fetch(ctx, url)

		r.mu.Lock()
		switch {
		case err == nil:
			e.sums, e.expires = sums, r.clock.Now().Add(r.ttl)
		case ctx.Err() != nil:
			// The caller gave up: that says nothing about the host.
			delete(r.cache, url)
		default:
			e.err, e.expires = err, r.clock.Now().Add(r.errorTTL)
		}
		close(e.fetching)
		e.fetching = nil
		r.mu.Unlock()
		return sums, err
	}
}

func (r *Resolver) fetch(ctx context.Context, url string) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building the request for %s: %w", url, err)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("fetching %s: %w", url, ErrNotFound)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("fetching %s: %s", url, resp.Status)
	}
	sums, err := ParseSHA256SUMS(io.LimitReader(resp.Body, maxChecksumsSize))
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", url, err)
	}
	return sums, nil
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ParseSHA256SUMS parses the output of sha256sum: "<hex>  <file>" per line
// (a "*" before the file name marks binary mode). Digests are returned in
// lower case.
func ParseSHA256SUMS(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		sum, file, ok := strings.Cut(line, " ")
		file = strings.TrimPrefix(strings.TrimSpace(file), "*")
		sum = strings.ToLower(sum)
		if !ok || file == "" || !sha256Pattern.MatchString(sum) {
			return nil, fmt.Errorf("line %d is not a sha256sum line", n)
		}
		if prev, dup := out[file]; dup && prev != sum {
			return nil, fmt.Errorf("line %d: %s has two different checksums", n, file)
		}
		out[file] = sum
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading the checksums: %w", err)
	}
	if len(out) == 0 {
		return nil, errors.New("no checksums in the file")
	}
	return out, nil
}
