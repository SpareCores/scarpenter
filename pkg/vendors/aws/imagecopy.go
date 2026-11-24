/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// The AWS ImageCopier (plan 29, ADR-032): CopyImage of Flatcar's official AMI
// into a region that has none, tagged as this cluster's, and the listing, use
// check and deletion of those copies. The policy (which copies, when, and
// when to delete them) is the copy manager's (pkg/imagecopy); the region names
// are EC2's, the same the NodeClass spec and Pin.Regions use.
//
// Plan 29 step 4 settles by a live run what is marked "to verify in step 4":
// the codes of the copy limit and of a region that is not enabled, encryption by
// default, the condition keys CopyImage evaluates, and whether
// DeleteAssociatedSnapshots needs ec2:DeleteSnapshot.

var _ vendors.ImageCopier = (*Driver)(nil)

const (
	// copySourcesTTL is how long a non-empty CopySources answer is kept.
	copySourcesTTL = time.Hour
	// maxImageName is EC2's limit of an AMI name.
	maxImageName = 128
	// codeNotACopy refuses the deletion of an image that is not this cluster's copy.
	codeNotACopy = "NotACopy"
)

// copySourcePreference is the order in which regions are preferred as the
// source of a copy (plan 29, decision 5): the cheapest transfer first. Any
// other region that has the official image follows, in name order.
var copySourcePreference = []string{"us-east-1", "us-east-2", "us-west-2", "eu-west-1", "eu-central-1"}

// instanceStatesInUse are the states of an instance that keeps its image: all
// but terminated.
var instanceStatesInUse = []string{"pending", "running", "shutting-down", "stopping", "stopped"}

type cachedSources struct {
	srcs  []vendors.ImageSource
	until time.Time
}

func isCopyCode(code string) bool {
	switch code {
	case vendors.CodeImageCopyOnDemand, vendors.CodeImageCopying, vendors.CodeImageCopyFailed,
		vendors.CodeImageCopySourceMissing:
		return true
	}
	return false
}

// imageOrCopy is the lookup of a launch (demand) or of a report: the primary
// image, else, with copies on, the copy manager's answer. The manager is asked
// after the primary lookup missed and outside its caches, so that a copy that
// was deleted is never served from them.
func (d *Driver) imageOrCopy(ctx context.Context, req vendors.ImageRequest, demand bool) (*vendors.Image, error) {
	img, err := d.primaryImage(ctx, req)
	if err == nil || d.cfg.ImageCopies == nil ||
		!vendors.IsClass(err, vendors.NotFound) || vendors.CodeOf(err) != codeImageNotFound {
		return img, err
	}
	cp, merr := d.cfg.ImageCopies.Lookup(ctx, req, demand)
	switch {
	case merr != nil:
		return nil, merr
	case cp == nil:
		return nil, err // the manager has no say (the request names images or an owner)
	}
	if err := d.copyRoot(ctx, req.Region, cp); err != nil {
		return nil, err
	}
	return cp, nil
}

// copyRoot sets the root disk size of a copy the manager answered with, and
// remembers its root device name for the launch. It reads the image once per
// AMI ID.
func (d *Driver) copyRoot(ctx context.Context, region string, img *vendors.Image) error {
	key := region + "/" + img.Ref
	d.mu.Lock()
	size, ok := d.rootBytes[key]
	d.mu.Unlock()
	if ok {
		img.RootDiskBytes = size
		return nil
	}
	cl, err := d.clients(ctx, region)
	if err != nil {
		return err
	}
	out, err := cl.EC2.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{img.Ref}})
	switch {
	case isCode(err, "InvalidAMIID.NotFound"):
		return vendors.NewError(vendors.NotFound, codeImageNotFound,
			fmt.Errorf("the copy %s in %s is gone: %w", img.Ref, region, err))
	case err != nil:
		return classify(err, errContext{})
	case len(out.Images) != 1:
		return vendors.NewError(vendors.NotFound, codeImageNotFound, fmt.Errorf("the copy %s in %s is gone", img.Ref, region))
	}
	im := out.Images[0]
	for _, bd := range im.BlockDeviceMappings {
		if bd.DeviceName != nil && im.RootDeviceName != nil && *bd.DeviceName == *im.RootDeviceName && bd.Ebs != nil &&
			bd.Ebs.VolumeSize != nil {
			img.RootDiskBytes = int64(*bd.Ebs.VolumeSize) * giB
		}
	}
	d.mu.Lock()
	d.rootBytes[key] = img.RootDiskBytes
	if im.RootDeviceName != nil {
		d.roots[key] = *im.RootDeviceName
	}
	d.mu.Unlock()
	return nil
}

// Architectures implements vendors.ImageCopier: both, in every region.
func (d *Driver) Architectures(context.Context, string) ([]string, error) {
	return []string{archAMD64, archARM64}, nil
}

// Primary implements vendors.ImageCopier: the official AMI of the release, by
// the same lookup (and caches) as a launch without copies.
func (d *Driver) Primary(ctx context.Context, region string, rel vendors.Release) (string, bool, error) {
	img, err := d.primaryImage(ctx, vendors.ImageRequest{
		Region: region, Arch: rel.Arch, Channel: rel.Channel, Version: rel.Version,
	})
	switch {
	case err == nil:
		return img.Ref, true, nil
	case vendors.IsClass(err, vendors.NotFound) && vendors.CodeOf(err) == codeImageNotFound:
		return "", false, nil
	}
	return "", false, err
}

// CopySources implements vendors.ImageCopier: the official AMI of the release
// in the regions of the account, the preference list first. The regions are
// asked through Primary, so each answer passes the version check of ADR-017.
// Only a non-empty answer is cached (for an hour): an empty one is not, so a
// release Flatcar publishes later is found at the next ask (Primary's own miss
// cache is 5 minutes).
func (d *Driver) CopySources(ctx context.Context, rel vendors.Release) ([]vendors.ImageSource, error) {
	d.mu.Lock()
	if c, ok := d.sources[rel]; ok && d.clock.Now().Before(c.until) {
		d.mu.Unlock()
		return slices.Clone(c.srcs), nil
	}
	d.mu.Unlock()
	all, err := d.Regions(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, r := range all {
		names = append(names, r.Name)
	}
	slices.SortStableFunc(names, func(a, b string) int { return preferenceRank(a) - preferenceRank(b) })

	refs := make([]string, len(names))
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	sem := make(chan struct{}, reportParallel)
	for i, name := range names {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			refs[i], _, errs[i] = d.Primary(ctx, name, rel)
		}()
	}
	wg.Wait()
	var srcs []vendors.ImageSource
	for i, name := range names {
		if refs[i] != "" {
			srcs = append(srcs, vendors.ImageSource{Region: name, Ref: refs[i]})
		}
	}
	if len(srcs) == 0 {
		return nil, errors.Join(errs...) // nil when every region answered "none"
	}
	d.mu.Lock()
	d.sources[rel] = cachedSources{srcs: srcs, until: d.clock.Now().Add(copySourcesTTL)}
	d.mu.Unlock()
	return slices.Clone(srcs), nil
}

// preferenceRank orders the regions: the preference list by position, then the
// rest (SortStableFunc keeps their name order).
func preferenceRank(region string) int {
	if i := slices.Index(copySourcePreference, region); i >= 0 {
		return i
	}
	return len(copySourcePreference)
}

// ownershipFilters select the images this cluster made as copies.
func (d *Driver) ownershipFilters() []ec2types.Filter {
	return []ec2types.Filter{
		{Name: aws.String("tag:" + d.TagKey(vendors.TagCluster)), Values: []string{d.cfg.ClusterName}},
		{Name: aws.String("tag:" + d.TagKey(vendors.TagManagedBy)), Values: []string{vendors.ManagedByValue}},
		{Name: aws.String("tag:" + d.TagKey(vendors.TagResource)), Values: []string{vendors.ResourceKindImageCopy}},
	}
}

// isOurCopy says whether the image carries the ownership tags of this cluster.
func (d *Driver) isOurCopy(im ec2types.Image) bool {
	tags := tagMap(im.Tags)
	return d.cfg.ClusterName != "" &&
		tags[d.TagKey(vendors.TagCluster)] == d.cfg.ClusterName &&
		tags[d.TagKey(vendors.TagManagedBy)] == vendors.ManagedByValue &&
		tags[d.TagKey(vendors.TagResource)] == vendors.ResourceKindImageCopy
}

// ListCopies implements vendors.ImageCopier: the account's own images with
// this cluster's ownership tags, in any state; an available one with the time
// of its last launch (the image attribute lastLaunchedTime, which AWS updates
// within about a day of a launch).
func (d *Driver) ListCopies(ctx context.Context, regions []string) ([]vendors.ImageCopy, error) {
	if d.cfg.ClusterName == "" {
		return nil, nil // without a cluster tag nothing can be told as ours
	}
	var out []vendors.ImageCopy
	for _, region := range regions {
		cl, err := d.clients(ctx, region)
		if err != nil {
			return nil, err
		}
		in := &ec2.DescribeImagesInput{Owners: []string{"self"}, Filters: d.ownershipFilters()}
		for {
			res, err := cl.EC2.DescribeImages(ctx, in)
			if err != nil {
				return nil, classify(err, errContext{})
			}
			for _, im := range res.Images {
				if d.isOurCopy(im) {
					out = append(out, d.toCopy(ctx, cl, region, im))
				}
			}
			if res.NextToken == nil || *res.NextToken == "" {
				break
			}
			in.NextToken = res.NextToken
		}
	}
	return out, nil
}

func (d *Driver) toCopy(ctx context.Context, cl Clients, region string, im ec2types.Image) vendors.ImageCopy {
	tags := tagMap(im.Tags)
	channel, version, _ := imageRelease(im)
	arch := tags[d.TagKey(vendors.TagImageArch)]
	if arch == "" && im.Architecture == ec2types.ArchitectureValuesArm64 {
		arch = archARM64
	} else if arch == "" {
		arch = archAMD64
	}
	c := vendors.ImageCopy{
		Region: region, Ref: deref(im.ImageId), Release: vendors.Release{Channel: channel, Version: version, Arch: arch},
	}
	if im.SourceImageId != nil && im.SourceImageRegion != nil {
		c.Source = *im.SourceImageRegion + "/" + *im.SourceImageId
	}
	c.Created, _ = time.Parse(time.RFC3339, deref(im.CreationDate))
	switch im.State {
	case ec2types.ImageStateAvailable:
		c.State = vendors.CopyAvailable
		c.LastUsed = d.lastLaunched(ctx, cl, c.Ref)
	case ec2types.ImageStatePending, ec2types.ImageStateTransient:
		c.State = vendors.CopyPending
	default: // failed, error, invalid, disabled, deregistered
		c.State = vendors.CopyFailed
		c.Message = string(im.State)
		if r := im.StateReason; r != nil && (r.Code != nil || r.Message != nil) {
			c.Message = deref(r.Code) + ": " + deref(r.Message)
		}
	}
	return c
}

// lastLaunched reads the image's lastLaunchedTime; zero when the image was
// never launched or the attribute cannot be read (logged: the GC then goes by
// the manager's own record and the creation time, and never deletes an image an
// instance runs from).
func (d *Driver) lastLaunched(ctx context.Context, cl Clients, ref string) time.Time {
	out, err := cl.EC2.DescribeImageAttribute(ctx, &ec2.DescribeImageAttributeInput{
		ImageId: aws.String(ref), Attribute: ec2types.ImageAttributeNameLastLaunchedTime,
	})
	if err != nil {
		d.log.Info("the last launch of an image copy cannot be read", "image", ref, "error", err.Error())
		return time.Time{}
	}
	if out.LastLaunchedTime == nil || out.LastLaunchedTime.Value == nil {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, *out.LastLaunchedTime.Value)
	return t
}

// copyName is the name of a copy: never the official one (a name lookup of the
// official owner cannot find it), at most EC2's limit with a hash of the whole
// name when it has to be cut.
func copyName(rel vendors.Release, cluster string) string {
	name := fmt.Sprintf("scarpenter-flatcar-%s-%s-%s-%s", rel.Channel, rel.Version, rel.Arch, cluster)
	if len(name) <= maxImageName {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return name[:maxImageName-9] + "-" + hex.EncodeToString(sum[:4])
}

// StartCopy implements vendors.ImageCopier: CopyImage into region, tagged at
// creation on the image and its snapshot (the logical tags, minus the image
// source, which Image.SourceImageId says, plus the release tags the version
// check reads). The token is CopyImage's ClientToken. No Encrypted flag: the
// account's encryption by default decides (to verify in step 4: whether and with
// which key it applies to a copy). A limit or throttling error is QuotaExceeded
// or Retryable; every other error (a region that is not enabled, a missing
// permission) refuses the copy, and the manager does not retry for hours.
func (d *Driver) StartCopy(
	ctx context.Context, src vendors.ImageSource, region string, rel vendors.Release,
	tags map[vendors.TagKey]string, token string,
) (string, error) {
	cl, err := d.clients(ctx, region)
	if err != nil {
		return "", err
	}
	ec2Tags := map[string]string{tagFlatcarChannel: rel.Channel, tagFlatcarVersion: rel.Version}
	for k, v := range tags {
		if k != vendors.TagImageSource {
			ec2Tags[d.TagKey(k)] = v
		}
	}
	list := tagList(ec2Tags)
	out, err := cl.EC2.CopyImage(ctx, &ec2.CopyImageInput{
		Name: aws.String(copyName(rel, d.cfg.ClusterName)),
		Description: aws.String(fmt.Sprintf("Flatcar %s %s (%s), copied by SCarpenter from %s",
			rel.Channel, rel.Version, rel.Arch, src.Region)),
		SourceRegion:  aws.String(src.Region),
		SourceImageId: aws.String(src.Ref),
		ClientToken:   aws.String(token),
		TagSpecifications: []ec2types.TagSpecification{
			{ResourceType: ec2types.ResourceTypeImage, Tags: list},
			{ResourceType: ec2types.ResourceTypeSnapshot, Tags: list},
		},
	})
	if err != nil {
		return "", classify(err, errContext{})
	}
	return deref(out.ImageId), nil
}

// CopyInUse implements vendors.ImageCopier: an instance of the account in the
// region, of any cluster and in any state but terminated, launched from the AMI.
func (d *Driver) CopyInUse(ctx context.Context, c vendors.ImageCopy) (bool, error) {
	insts, err := d.describeInstances(ctx, c.Region, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("image-id"), Values: []string{c.Ref}},
			{Name: aws.String("instance-state-name"), Values: instanceStatesInUse},
		},
	})
	return len(insts) > 0, err
}

// TouchCopy implements vendors.ImageCopier: a no-op, AWS records the launches
// itself (lastLaunchedTime).
func (d *Driver) TouchCopy(context.Context, vendors.ImageCopy, time.Time) error { return nil }

// DeleteCopy implements vendors.ImageCopier: DeregisterImage with
// DeleteAssociatedSnapshots, only for an image that carries this cluster's
// ownership tags (IAM enforces the cluster tag as well); a missing image is not
// an error. A snapshot the call could not delete is logged, never retried (the
// image is gone): to verify in step 4 whether this needs ec2:DeleteSnapshot, which
// the policy grants for the cluster's snapshots meanwhile.
func (d *Driver) DeleteCopy(ctx context.Context, c vendors.ImageCopy) error {
	cl, err := d.clients(ctx, c.Region)
	if err != nil {
		return err
	}
	res, err := cl.EC2.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{c.Ref}})
	switch {
	case isCode(err, "InvalidAMIID.NotFound"):
		return nil
	case err != nil:
		return classify(err, errContext{})
	}
	if len(res.Images) != 1 || !d.isOurCopy(res.Images[0]) {
		return vendors.NewError(vendors.Fatal, codeNotACopy,
			fmt.Errorf("the image %s in %s is not a copy of this cluster: not deleted", c.Ref, c.Region))
	}
	out, err := cl.EC2.DeregisterImage(ctx, &ec2.DeregisterImageInput{
		ImageId: aws.String(c.Ref), DeleteAssociatedSnapshots: aws.Bool(true),
	})
	switch {
	case isCode(err, "InvalidAMIID.NotFound"):
		return nil
	case err != nil:
		return classify(err, errContext{})
	}
	for _, r := range out.DeleteSnapshotResults {
		// "skipped" is a snapshot another AMI still uses: it stays.
		if r.ReturnCode != ec2types.SnapshotReturnCodesSuccess && r.ReturnCode != ec2types.SnapshotReturnCodesWarnSkipped {
			d.log.Info("a snapshot of a deleted image copy was not deleted",
				"image", c.Ref, "region", c.Region, "snapshot", deref(r.SnapshotId), "result", string(r.ReturnCode))
		}
	}
	return nil
}
