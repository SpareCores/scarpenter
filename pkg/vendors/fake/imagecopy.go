//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// The fake's image copies (plan 29): an in-memory account with "official"
// images the test publishes per region, copies that become available after a
// fake duration (on the driver's clock), and injectable limit errors, refusals
// and failed copies. The account survives a restart of the driver in a
// ConfigMap next to the fake resources, like the rest of the fake's account.

const (
	// DefaultCopyDuration is how long a fake copy stays pending.
	DefaultCopyDuration   = 5 * time.Minute
	keyImageCopies        = "imageCopies"
	codeCopiesUnavailable = "ImageCopiesUnavailable"
	imageCopiesSuffix     = "-image-copies"
)

type officialImage struct {
	Region  string          `json:"region"`
	Ref     string          `json:"ref"`
	Release vendors.Release `json:"release"`
}

type copyRecord struct {
	Region  string          `json:"region"`
	Ref     string          `json:"ref"`
	Release vendors.Release `json:"release"`
	Source  string          `json:"source"`
	// Tags are the logical ownership tags the copy was started with.
	Tags     map[string]string `json:"tags"`
	Token    string            `json:"token"`
	Created  time.Time         `json:"created"`
	ReadyAt  time.Time         `json:"readyAt"`
	Failed   bool              `json:"failed,omitempty"`
	LastUsed time.Time         `json:"lastUsed,omitempty"`
}

func (doc *copyDoc) isOfficial(region, ref string) bool {
	return slices.ContainsFunc(doc.Official, func(o officialImage) bool { return o.Region == region && o.Ref == ref })
}

func notACopy(c vendors.ImageCopy) error {
	return vendors.NewError(vendors.Fatal, "NotACopy",
		fmt.Errorf("the image %s in %s is not a copy of this cluster", c.Ref, c.Region))
}

type copyDoc struct {
	Seq      int             `json:"seq"`
	Official []officialImage `json:"official"`
	Copies   []*copyRecord   `json:"copies"`
}

// imageCopies is the state of the fake's copies.
type imageCopies struct {
	mu     sync.Mutex
	loaded bool
	doc    copyDoc
	// Injections and counters live in this process only.
	duration time.Duration
	limit    int
	refuse   int
	fail     int
	starts   int
	inUse    map[string]bool
}

var copyArchs = []string{archAMD64, archARM64}

// ImageCopyControl is the test's handle on the fake's copies.
type ImageCopyControl struct{ d *Driver }

// ImageCopies returns the control of the fake's image copies.
func (d *Driver) ImageCopies() ImageCopyControl { return ImageCopyControl{d} }

// PublishOfficial makes the release's official image appear in the region, for
// both architectures.
func (c ImageCopyControl) PublishOfficial(ctx context.Context, region, channel, version string) error {
	return c.d.withCopies(ctx, true, func(doc *copyDoc) {
		for _, arch := range copyArchs {
			rel := vendors.Release{Channel: channel, Version: version, Arch: arch}
			if !slices.ContainsFunc(doc.Official, func(o officialImage) bool { return o.Region == region && o.Release == rel }) {
				doc.Official = append(doc.Official, officialImage{
					Region: region, Release: rel,
					Ref: fmt.Sprintf("ami-official-%s-%s-%s-%s", region, channel, version, arch),
				})
			}
		}
	})
}

// SetDuration sets how long the copies started from now on stay pending.
func (c ImageCopyControl) SetDuration(dur time.Duration) {
	c.d.cp.mu.Lock()
	defer c.d.cp.mu.Unlock()
	c.d.cp.duration = dur
}

// LimitStarts makes the next n StartCopy calls fail with a limit error
// (QuotaExceeded).
func (c ImageCopyControl) LimitStarts(n int) { c.d.inject(func(s *imageCopies) { s.limit = n }) }

// RefuseStarts makes the next n StartCopy calls fail with a refusal (Fatal).
func (c ImageCopyControl) RefuseStarts(n int) { c.d.inject(func(s *imageCopies) { s.refuse = n }) }

// FailCopies makes the next n copies end failed instead of available.
func (c ImageCopyControl) FailCopies(n int) { c.d.inject(func(s *imageCopies) { s.fail = n }) }

// Finish completes the copies in flight now (their failure, if injected, shows).
func (c ImageCopyControl) Finish(ctx context.Context) error {
	now := c.d.clock.Now()
	return c.d.withCopies(ctx, true, func(doc *copyDoc) {
		for _, r := range doc.Copies {
			if r.ReadyAt.After(now) {
				r.ReadyAt = now
			}
		}
	})
}

// SetInUse says that an instance runs from the image, without launching one.
func (c ImageCopyControl) SetInUse(ref string, inUse bool) {
	c.d.inject(func(s *imageCopies) {
		if s.inUse == nil {
			s.inUse = map[string]bool{}
		}
		s.inUse[ref] = inUse
	})
}

// Starts is the number of StartCopy calls so far, refused ones included.
func (c ImageCopyControl) Starts() int {
	c.d.cp.mu.Lock()
	defer c.d.cp.mu.Unlock()
	return c.d.cp.starts
}

// DeleteByHand removes a copy without the driver's DeleteCopy.
func (c ImageCopyControl) DeleteByHand(ctx context.Context, ref string) error {
	return c.d.withCopies(ctx, true, func(doc *copyDoc) {
		doc.Copies = slices.DeleteFunc(doc.Copies, func(r *copyRecord) bool { return r.Ref == ref })
	})
}

func (d *Driver) inject(f func(*imageCopies)) {
	d.cp.mu.Lock()
	defer d.cp.mu.Unlock()
	f(&d.cp)
}

// withCopies loads the account on first use, runs f on it under the lock and,
// when write is set, saves it.
func (d *Driver) withCopies(ctx context.Context, write bool, f func(*copyDoc)) error {
	d.cp.mu.Lock()
	defer d.cp.mu.Unlock()
	if !d.cp.loaded {
		if err := d.loadCopies(ctx); err != nil {
			return err
		}
		d.cp.loaded = true
	}
	f(&d.cp.doc)
	if write {
		return d.saveCopies(ctx)
	}
	return nil
}

func (d *Driver) copiesKey() types.NamespacedName {
	return types.NamespacedName{Namespace: d.opts.ConfigNamespace, Name: d.opts.ResourcesName + imageCopiesSuffix}
}

func (d *Driver) loadCopies(ctx context.Context) error {
	if d.cfg.Client == nil {
		return nil
	}
	reader := d.cfg.Reader
	if reader == nil {
		reader = d.cfg.Client
	}
	cm := &corev1.ConfigMap{}
	switch err := reader.Get(ctx, d.copiesKey(), cm); {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return vendors.NewError(vendors.Retryable, codeCopiesUnavailable, fmt.Errorf("reading the copies: %w", err))
	}
	if raw := cm.Data[keyImageCopies]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &d.cp.doc); err != nil {
			return vendors.NewError(vendors.Fatal, "ImageCopiesCorrupt", fmt.Errorf("decoding the fake image copies: %w", err))
		}
	}
	return nil
}

func (d *Driver) saveCopies(ctx context.Context) error {
	if d.cfg.Client == nil {
		return nil
	}
	raw, err := json.Marshal(d.cp.doc)
	if err != nil {
		return err
	}
	reader := d.cfg.Reader
	if reader == nil {
		reader = d.cfg.Client
	}
	var last error
	for range 5 {
		cm := &corev1.ConfigMap{}
		err := reader.Get(ctx, d.copiesKey(), cm)
		switch {
		case apierrors.IsNotFound(err):
			k := d.copiesKey()
			cm = &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Namespace: k.Namespace, Name: k.Name},
				Data:       map[string]string{keyImageCopies: string(raw)},
			}
			err = d.cfg.Client.Create(ctx, cm)
		case err == nil:
			cm.Data = map[string]string{keyImageCopies: string(raw)}
			err = d.cfg.Client.Update(ctx, cm)
		}
		if err == nil {
			return nil
		}
		last = err
	}
	return vendors.NewError(vendors.Retryable, codeCopiesUnavailable, fmt.Errorf("writing the copies: %w", last))
}

// copiesSteered says that the controller has the copy manager of the fake and
// the steering ConfigMap has the imageCopies key: the fake then has official
// images only where the key says. Without either, every region has one.
func (d *Driver) copiesSteered(ctx context.Context) bool {
	if d.cfg.ImageCopies == nil {
		return false
	}
	d.refreshConfig(ctx)
	return d.current().imageCopies != nil
}

func isCopyCode(code string) bool {
	switch code {
	case vendors.CodeImageCopyOnDemand, vendors.CodeImageCopying, vendors.CodeImageCopyFailed,
		vendors.CodeImageCopySourceMissing:
		return true
	}
	return false
}

// resolveOrCopy is the default lookup of a driver with copies on: the official
// image of the region if there is one, else what the copy manager says.
func (d *Driver) resolveOrCopy(ctx context.Context, req vendors.ImageRequest, demand bool) (*vendors.Image, error) {
	rel := vendors.Release{Channel: req.Channel, Version: req.Version, Arch: req.Arch}
	ref, found, err := d.Primary(ctx, req.Region, rel)
	if err != nil {
		return nil, err
	}
	if found {
		return &vendors.Image{Ref: ref, RootDiskBytes: rootDiskBytes.Value(), Channel: req.Channel, Version: req.Version}, nil
	}
	img, err := d.cfg.ImageCopies.Lookup(ctx, req, demand)
	if err != nil {
		return nil, err
	}
	if img == nil {
		return nil, vendors.Errorf(vendors.NotFound, "no %s image of Flatcar %s %s in %s",
			req.Arch, req.Channel, req.Version, req.Region)
	}
	img.RootDiskBytes = rootDiskBytes.Value()
	return img, nil
}

// officialRef is the ref of the official image the steering gives the region
// for the release.
func (d *Driver) officialRef(region string, rel vendors.Release) (string, bool) {
	cfg := d.current().imageCopies
	if cfg == nil {
		return "", false
	}
	for _, r := range cfg.Official {
		if r.Region == region && (r.Version == "" || r.Version == rel.Version) {
			return fmt.Sprintf("ami-official-%s-%s-%s-%s", region, rel.Channel, rel.Version, rel.Arch), true
		}
	}
	return "", false
}

// Architectures implements vendors.ImageCopier: both, everywhere.
func (d *Driver) Architectures(context.Context, string) ([]string, error) {
	return slices.Clone(copyArchs), nil
}

// Primary implements vendors.ImageCopier: the official image the test
// published in the region, or the steering gives it.
func (d *Driver) Primary(ctx context.Context, region string, rel vendors.Release) (string, bool, error) {
	var ref string
	err := d.withCopies(ctx, false, func(doc *copyDoc) {
		for _, o := range doc.Official {
			if o.Region == region && o.Release == rel {
				ref = o.Ref
			}
		}
	})
	if ref == "" {
		ref, _ = d.officialRef(region, rel)
	}
	return ref, ref != "", err
}

// CopySources implements vendors.ImageCopier: the official images of the
// release, in region name order.
func (d *Driver) CopySources(ctx context.Context, rel vendors.Release) ([]vendors.ImageSource, error) {
	var out []vendors.ImageSource
	err := d.withCopies(ctx, false, func(doc *copyDoc) {
		for _, o := range doc.Official {
			if o.Release == rel {
				out = append(out, vendors.ImageSource{Region: o.Region, Ref: o.Ref})
			}
		}
	})
	if cfg := d.current().imageCopies; cfg != nil {
		for _, r := range cfg.Official {
			ref, ok := d.officialRef(r.Region, rel)
			src := vendors.ImageSource{Region: r.Region, Ref: ref}
			if ok && !slices.Contains(out, src) {
				out = append(out, src)
			}
		}
	}
	slices.SortFunc(out, func(a, b vendors.ImageSource) int { return strings.Compare(a.Region, b.Region) })
	return out, err
}

// ours says whether the copy carries this cluster's ownership tags.
func (d *Driver) ours(r *copyRecord) bool {
	return r.Tags[string(vendors.TagCluster)] == d.cfg.ClusterName && d.cfg.ClusterName != "" &&
		r.Tags[string(vendors.TagManagedBy)] == vendors.ManagedByValue &&
		r.Tags[string(vendors.TagResource)] == vendors.ResourceKindImageCopy
}

func (d *Driver) copyOf(r *copyRecord, now time.Time) vendors.ImageCopy {
	c := vendors.ImageCopy{
		Region: r.Region, Ref: r.Ref, Release: r.Release, Source: r.Source, State: vendors.CopyAvailable,
		Created: r.Created, LastUsed: r.LastUsed,
	}
	switch {
	case r.ReadyAt.After(now):
		c.State = vendors.CopyPending
	case r.Failed:
		c.State, c.Message = vendors.CopyFailed, "injected failure"
	}
	return c
}

// ListCopies implements vendors.ImageCopier.
func (d *Driver) ListCopies(ctx context.Context, regions []string) ([]vendors.ImageCopy, error) {
	now := d.clock.Now()
	var out []vendors.ImageCopy
	err := d.withCopies(ctx, false, func(doc *copyDoc) {
		for _, r := range doc.Copies {
			if d.ours(r) && slices.Contains(regions, r.Region) {
				out = append(out, d.copyOf(r, now))
			}
		}
	})
	slices.SortFunc(out, func(a, b vendors.ImageCopy) int {
		return strings.Compare(a.Region+"/"+a.Ref, b.Region+"/"+b.Ref)
	})
	return out, err
}

// StartCopy implements vendors.ImageCopier.
func (d *Driver) StartCopy(
	ctx context.Context, src vendors.ImageSource, region string, rel vendors.Release,
	tags map[vendors.TagKey]string, token string,
) (string, error) {
	d.cp.mu.Lock()
	d.cp.starts++
	var inject error
	switch {
	case d.cp.limit > 0:
		d.cp.limit--
		inject = vendors.NewError(vendors.QuotaExceeded, "ResourceLimitExceeded", errors.New("too many concurrent copies"))
	case d.cp.refuse > 0:
		d.cp.refuse--
		inject = vendors.NewError(vendors.Fatal, "OptInRequired", fmt.Errorf("the region %s is not enabled", region))
	}
	failed := false
	if inject == nil && d.cp.fail > 0 {
		d.cp.fail--
		failed = true
	}
	duration := d.cp.duration
	d.cp.mu.Unlock()
	if cfg := d.current().imageCopies; duration == 0 && cfg != nil && cfg.CopyDuration != nil {
		duration = cfg.CopyDuration.Duration
	}
	if duration == 0 {
		duration = DefaultCopyDuration
	}
	if inject != nil {
		return "", inject
	}

	var ref string
	var rerr error
	now := d.clock.Now()
	err := d.withCopies(ctx, true, func(doc *copyDoc) {
		for _, r := range doc.Copies {
			if r.Region == region && r.Token == token {
				ref = r.Ref
				return
			}
		}
		steeredRef, steered := d.officialRef(src.Region, rel)
		if !doc.isOfficial(src.Region, src.Ref) && (!steered || steeredRef != src.Ref) {
			rerr = vendors.Errorf(vendors.NotFound, "the source image %s in %s does not exist", src.Ref, src.Region)
			return
		}
		doc.Seq++
		ref = fmt.Sprintf("ami-copy-%04d", doc.Seq)
		doc.Copies = append(doc.Copies, &copyRecord{
			Region: region, Ref: ref, Release: rel, Source: src.Region + "/" + src.Ref, Tags: tagStrings(tags),
			Token: token, Created: now, ReadyAt: now.Add(duration), Failed: failed,
		})
	})
	if err == nil {
		err = rerr
	}
	return ref, err
}

// CopyInUse implements vendors.ImageCopier: a launched instance of the fake
// account that is not terminated, from the copy.
func (d *Driver) CopyInUse(_ context.Context, c vendors.ImageCopy) (bool, error) {
	d.cp.mu.Lock()
	marked := d.cp.inUse[c.Ref]
	d.cp.mu.Unlock()
	if marked {
		return true, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, i := range d.instances {
		if i.image == c.Ref && i.State != vendors.StateTerminated {
			return true, nil
		}
	}
	return false, nil
}

// TouchCopy implements vendors.ImageCopier: a no-op, the fake records the
// launches itself (usedImage), like AWS.
func (d *Driver) TouchCopy(context.Context, vendors.ImageCopy, time.Time) error { return nil }

// usedImage records the launch of an instance from the image in the copy's
// last-used time.
func (d *Driver) usedImage(ctx context.Context, ref string) {
	if !strings.HasPrefix(ref, "ami-copy-") {
		return
	}
	now := d.clock.Now()
	_ = d.withCopies(ctx, true, func(doc *copyDoc) {
		for _, r := range doc.Copies {
			if r.Ref == ref {
				r.LastUsed = now
			}
		}
	})
}

// DeleteCopy implements vendors.ImageCopier: only an image with the cluster's
// ownership tags; a missing one is not an error.
func (d *Driver) DeleteCopy(ctx context.Context, c vendors.ImageCopy) error {
	var rerr error
	err := d.withCopies(ctx, true, func(doc *copyDoc) {
		if doc.isOfficial(c.Region, c.Ref) || strings.HasPrefix(c.Ref, "ami-official-") {
			rerr = notACopy(c)
			return
		}
		doc.Copies = slices.DeleteFunc(doc.Copies, func(r *copyRecord) bool {
			if r.Region != c.Region || r.Ref != c.Ref {
				return false
			}
			if !d.ours(r) {
				rerr = notACopy(c)
				return false
			}
			return true
		})
	})
	if err == nil {
		err = rerr
	}
	return err
}
