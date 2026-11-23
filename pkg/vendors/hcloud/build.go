/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package hcloud

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/apricote/hcloud-upload-image/hcloudimages/v2"
	"github.com/apricote/hcloud-upload-image/hcloudimages/v2/contextlogger"
	"github.com/go-logr/logr"
	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Image building (ADR-030): when the label lookup finds no snapshot of the
// pinned release for an architecture, the driver makes it the way
// hcloud-upload-image does (a temporary server in rescue mode writes Flatcar's
// hetzner image, is snapshotted and deleted). One build at a time; it starts
// from ReportImages, which only the leader's NodeClass controller calls.
const (
	// LabelImageBuild marks the temporary servers and SSH keys of a build, and
	// the snapshot it makes, with the cluster's name.
	LabelImageBuild = "scarpenter.sparecores.com/image-build"

	// LabelImageDisk is the disk size in GB of the build server's type, set on the
	// snapshot: a snapshot without it, or with a bigger one, is not the one built
	// now.
	LabelImageDisk = "scarpenter.sparecores.com/image-disk"

	imageBuildTimeout = 30 * time.Minute
	imageBuildRetry   = 15 * time.Minute
	sweepTimeout      = 2 * time.Minute
)

// Uploader makes a snapshot from an image URL: hcloudimages.Client, or a fake
// in tests.
type Uploader interface {
	Upload(ctx context.Context, opts hcloudimages.UploadOptions) (*hc.Image, error)
}

// buildKey is one snapshot: a Flatcar release and an architecture.
type buildKey struct{ channel, version, arch, location string }

func (k buildKey) String() string { return k.channel + " " + k.version + " " + k.arch }

func (k buildKey) labels() map[string]string {
	return map[string]string{LabelOS: "flatcar", LabelFlatcarChannel: k.channel, LabelFlatcarVersion: k.version}
}

func (k buildKey) selector() string {
	return fmt.Sprintf("%s=flatcar,%s=%s,%s=%s", LabelOS, LabelFlatcarChannel, k.channel, LabelFlatcarVersion, k.version)
}

// builds is the state of the driver's builds.
type builds struct {
	mu      sync.Mutex
	swept   bool // this process has deleted the cluster's leftovers
	running *buildKey
	started time.Time
	failed  map[buildKey]buildFailure
}

type buildFailure struct {
	err error
	at  time.Time
}

// canBuild says whether the driver may build snapshots.
func (d *Driver) canBuild() bool { return d.buildImages && d.uploader != nil }

// ensureImage is called for a snapshot the lookup did not find. It starts its
// build unless its last build failed less than imageBuildRetry ago, a build is
// running, or the snapshot is being made elsewhere. The answer is always an error
// (the image is not ready yet): ImageBuilding or ImageBuildFailed, both of
// class NotFound, or a Retryable one when the checks cannot be read.
func (d *Driver) ensureImage(ctx context.Context, k buildKey) error {
	b := &d.builds
	b.mu.Lock()
	defer b.mu.Unlock()
	now := d.clock.Now()
	if f, ok := b.failed[k]; ok && now.Sub(f.at) < imageBuildRetry {
		return vendors.NewError(vendors.NotFound, vendors.CodeImageBuildFailed, fmt.Errorf(
			"building the Flatcar snapshot %s failed, retried at %s: %w",
			k, f.at.Add(imageBuildRetry).UTC().Format(time.RFC3339), f.err))
	}
	if b.running != nil {
		if *b.running == k {
			return building(k, "started %s ago, at most %s", now.Sub(b.started).Round(time.Second), imageBuildTimeout)
		}
		return building(k, "waiting for the build of %s", *b.running)
	}
	if !b.swept {
		if err := d.sweepBuilds(ctx); err != nil {
			return err
		}
		b.swept = true
	}
	busy, err := d.buildingElsewhere(ctx, k)
	if err != nil {
		return err
	}
	if busy != "" {
		return building(k, "%s", busy)
	}
	b.running, b.started = &k, now
	go d.runBuild(k)
	return building(k, "started")
}

func building(k buildKey, format string, args ...any) error {
	return vendors.NewError(vendors.NotFound, vendors.CodeImageBuilding, fmt.Errorf(
		"building the Flatcar snapshot %s: %s", k, fmt.Sprintf(format, args...)))
}

// buildingElsewhere says why the snapshot need not be built here: a snapshot
// of the release in any status (being created, or made since the lookup), or
// the build server of another controller. Empty: build it.
func (d *Driver) buildingElsewhere(ctx context.Context, k buildKey) (string, error) {
	ha, _ := ImageArch(k.arch)
	st, err := d.p.smallestType(ctx, ha, k.location)
	if err != nil {
		return "", err
	}
	sel := k.selector()
	if st != nil {
		// Only a snapshot of the size built now counts; an older, bigger one is
		// what the build replaces.
		sel += fmt.Sprintf(",%s=%d", LabelImageDisk, st.Disk)
	}
	imgs, err := d.api.Images(ctx, hc.ImageListOpts{
		ListOpts:     hc.ListOpts{LabelSelector: sel},
		Type:         []hc.ImageType{hc.ImageTypeSnapshot},
		Architecture: []hc.Architecture{ha},
	})
	if err != nil {
		return "", classify(err)
	}
	if len(imgs) > 0 {
		return fmt.Sprintf("the snapshot %d is %s", imgs[0].ID, imgs[0].Status), nil
	}
	servers, err := d.api.Servers(ctx, hc.ServerListOpts{
		ListOpts: hc.ListOpts{LabelSelector: LabelImageBuild + "," + k.selector()},
	})
	if err != nil {
		return "", classify(err)
	}
	for _, s := range servers {
		if s.ServerType != nil && s.ServerType.Architecture == ha {
			return fmt.Sprintf("the server %d of the cluster %q is building it", s.ID, s.Labels[LabelImageBuild]), nil
		}
	}
	return "", nil
}

// runBuild makes the snapshot and records the outcome. A failed build deletes
// what it left behind.
func (d *Driver) runBuild(k buildKey) {
	ctx, cancel := context.WithTimeout(context.Background(), imageBuildTimeout)
	defer cancel()
	log := d.log.WithValues("channel", k.channel, "version", k.version, "arch", k.arch)
	log.Info("building the Flatcar snapshot")
	var img *hc.Image
	opts, err := d.uploadOptions(ctx, k)
	if err == nil {
		img, err = d.uploader.Upload(contextlogger.New(ctx, slog.New(logr.ToSlogHandler(log))), opts)
	}

	b := &d.builds
	b.mu.Lock()
	defer b.mu.Unlock()
	b.running = nil
	if err != nil {
		log.Error(err, "building the Flatcar snapshot failed", "retryAfter", imageBuildRetry)
		b.failed[k] = buildFailure{err: err, at: d.clock.Now()}
		sctx, scancel := context.WithTimeout(context.Background(), sweepTimeout)
		defer scancel()
		if err := d.sweepBuilds(sctx); err != nil {
			log.Error(err, "deleting the servers and keys of the failed build")
		}
		return
	}
	delete(b.failed, k)
	d.forgetImage(k)
	log.Info("built the Flatcar snapshot", "image", img.ID)
}

func (d *Driver) uploadOptions(ctx context.Context, k buildKey) (hcloudimages.UploadOptions, error) {
	ha, _ := ImageArch(k.arch)
	st, err := d.p.smallestType(ctx, ha, k.location)
	if err != nil {
		return hcloudimages.UploadOptions{}, err
	}
	if st == nil {
		return hcloudimages.UploadOptions{}, fmt.Errorf(
			"no %s server type is launchable in %s for the build server", ha, k.location)
	}
	labels := k.labels()
	labels[LabelImageDisk] = strconv.Itoa(st.Disk)
	labels[LabelImageBuild] = LabelValue(d.cfg.ClusterName)
	description := fmt.Sprintf("Flatcar %s %s %s", k.channel, k.version, k.arch)
	return hcloudimages.UploadOptions{
		WriteOptions: hcloudimages.WriteOptions{
			ImageURL:         FlatcarImageURL(k.channel, k.version, k.arch),
			ImageCompression: hcloudimages.CompressionBZ2,
		},
		Architecture: ha,
		ServerType:   &hc.ServerType{Name: st.Name},
		Location:     &hc.Location{Name: k.location},
		Description:  &description,
		Labels:       labels,
	}, nil
}

// FlatcarImageURL is Flatcar's hetzner image of a release on its release server.
func FlatcarImageURL(channel, version, arch string) *url.URL {
	return &url.URL{
		Scheme: "https",
		Host:   channel + ".release.flatcar-linux.net",
		Path:   fmt.Sprintf("/%s-usr/%s/flatcar_production_hetzner_image.bin.bz2", arch, version),
	}
}

// forgetImage drops the cached lookups of a release and architecture, so the
// new snapshot is found at the next report.
func (d *Driver) forgetImage(k buildKey) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for key := range d.images {
		if key.channel == k.channel && key.version == k.version && key.arch == k.arch {
			delete(d.images, key)
		}
	}
}

// sweepBuilds deletes this cluster's build servers and SSH keys. Called before
// the first build of the process and after a failed one, when no build of this
// controller is running.
func (d *Driver) sweepBuilds(ctx context.Context) error {
	sel := hc.ListOpts{LabelSelector: LabelImageBuild + "=" + LabelValue(d.cfg.ClusterName)}
	servers, err := d.api.Servers(ctx, hc.ServerListOpts{ListOpts: sel})
	if err != nil {
		return classify(err)
	}
	var errs []error
	for _, s := range servers {
		if _, err := d.api.DeleteServer(ctx, s); err != nil {
			errs = append(errs, fmt.Errorf("server %d: %w", s.ID, err))
			continue
		}
		d.log.Info("deleted a leftover image build server", "server", s.ID)
	}
	keys, err := d.api.SSHKeys(ctx, hc.SSHKeyListOpts{ListOpts: sel})
	if err != nil {
		errs = append(errs, err)
	}
	for _, k := range keys {
		if err := d.api.DeleteSSHKey(ctx, k); err != nil {
			errs = append(errs, fmt.Errorf("SSH key %d: %w", k.ID, err))
		}
	}
	if len(errs) > 0 {
		return vendors.NewError(vendors.Retryable, "", fmt.Errorf("deleting leftover image builds: %w", errors.Join(errs...)))
	}
	return nil
}
