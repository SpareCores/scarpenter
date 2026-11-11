/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/SpareCores/scarpenter/pkg/artifacts"
)

// defaultK3sBase is where k3s publishes its releases (test/e2e-aws/bootstrap.sh downloads
// the control plane from here).
const defaultK3sBase = "https://github.com/k3s-io/k3s/releases/download"

// checkArtifacts is what every vendor shares: what the nodes and the control plane
// download. Network only, no cloud API. The node side is the resolver the NodeClass
// controller uses (it is what ArtifactsReady means): the kubelet and the Tailscale
// sysext of the pinned versions must have a checksum for both architectures.
func checkArtifacts(ctx context.Context, cfg *config, rep *report) {
	res := artifacts.New(artifacts.Config{SysextBase: cfg.sysextBase})
	for _, sx := range []struct{ name, version string }{
		{"kubernetes", cfg.kubernetesVersion},
		{"tailscale", cfg.tailscaleVersion},
	} {
		rep.check(fmt.Sprintf("sysext %s %s", sx.name, sx.version), func() (string, error) {
			var found []string
			for _, arch := range []string{artifacts.ArchAMD64, artifacts.ArchARM64} {
				a, err := res.Sysext(ctx, sx.name, sx.version, arch)
				if err != nil {
					return "", err
				}
				found = append(found, fmt.Sprintf("%s sha256:%.12s", arch, a.SHA256))
			}
			return strings.Join(found, ", "), nil
		})
	}
	rep.check("k3s "+cfg.k3sVersion, func() (string, error) { return k3sRelease(ctx, cfg) })
	if cfg.flatcarImageFile != "" {
		for _, arch := range []string{archAMD64, archARM64} {
			rep.check(fmt.Sprintf("Flatcar release %s %s %s", cfg.flatcarChannel, cfg.flatcarVersion, arch),
				func() (string, error) { return flatcarImage(ctx, cfg, arch) })
		}
	}
}

// defaultFlatcarBase is the Flatcar release server (per channel).
const defaultFlatcarBase = "https://{channel}.release.flatcar-linux.net"

// flatcarImage checks that the release server still serves the image the vendor's snapshots are
// made from (a HEAD: the file is hundreds of megabytes) and its .DIGESTS, for the pinned version.
func flatcarImage(ctx context.Context, cfg *config, arch string) (string, error) {
	base := cfg.flatcarBase
	if base == "" {
		base = defaultFlatcarBase
	}
	base = strings.TrimRight(strings.ReplaceAll(base, "{channel}", cfg.flatcarChannel), "/")
	url := fmt.Sprintf("%s/%s-usr/%s/%s", base, arch, cfg.flatcarVersion, cfg.flatcarImageFile)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	size, err := httpStatus(ctx, http.MethodHead, url)
	if err != nil {
		return "", err
	}
	if _, err := httpStatus(ctx, http.MethodGet, url+".DIGESTS"); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s, %d MB, with .DIGESTS", cfg.flatcarImageFile, size>>20), nil
}

// httpStatus requests the URL and reports the content length of a 200 answer.
func httpStatus(ctx context.Context, method, url string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%s %s: %s", method, url, resp.Status)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.ContentLength, nil
}

// k3sRelease reads the checksum file of the k3s release the lane installs and finds the
// binary in it (what bootstrap.sh verifies the download against).
func k3sRelease(ctx context.Context, cfg *config) (string, error) {
	base := cfg.k3sBase
	if base == "" {
		base = defaultK3sBase
	}
	url := strings.TrimRight(base, "/") + "/" + strings.ReplaceAll(cfg.k3sVersion, "+", "%2B") + "/sha256sum-amd64.txt"
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetching %s: %s", url, resp.Status)
	}
	sums, err := artifacts.ParseSHA256SUMS(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("parsing %s: %w", url, err)
	}
	sum, ok := sums["k3s"]
	if !ok {
		return "", fmt.Errorf("%s has no checksum for the k3s binary", url)
	}
	return fmt.Sprintf("amd64 sha256:%.12s", sum), nil
}
