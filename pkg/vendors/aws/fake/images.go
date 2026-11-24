/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// AccountID is the ID of the fake account: the owner of the images it copies
// and what the owner "self" means.
const AccountID = "123456789012"

// DefaultCopyDuration is how long a copy stays pending, unless the account
// says otherwise.
const DefaultCopyDuration = 5 * time.Minute

// timestampLayout is the layout of the image dates EC2 returns.
const timestampLayout = "2006-01-02T15:04:05.000Z"

// WithCopyDuration sets how long the images CopyImage makes stay pending.
func WithCopyDuration(d time.Duration) Option { return func(a *Account) { a.copyDuration = d } }

// copyToken is the image a CopyImage ClientToken made, with the request's
// fingerprint (the service rejects a token reused for another request).
type copyToken struct {
	image string
	hash  string
}

// settleImages makes the pending copies whose time has come available.
func (a *Account) settleImages(r *region) {
	for _, img := range r.images {
		if img.State == "pending" && !img.ReadyAt.IsZero() && !a.now().Before(img.ReadyAt) {
			img.State = "available"
		}
	}
}

// CopyImage implements EC2API: the call is made in the destination region. The
// copy is pending for the account's copy duration and carries the tags of the
// image and snapshot specifications; the snapshot is created with it.
func (e *EC2) CopyImage(ctx context.Context, in *ec2.CopyImageInput, _ ...func(*ec2.Options),
) (*ec2.CopyImageOutput, error) {
	r, err := e.begin(ctx, "CopyImage", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	a := e.a
	hash := hashInput(in)
	if tok := deref(in.ClientToken); tok != "" {
		if prev, ok := r.copies[tok]; ok {
			if prev.hash != hash {
				return nil, apiError("IdempotentParameterMismatch", "Client token "+tok+" was used with different parameters")
			}
			return &ec2.CopyImageOutput{ImageId: new(prev.image)}, nil
		}
	}
	if deref(in.Name) == "" || deref(in.SourceImageId) == "" || deref(in.SourceRegion) == "" {
		return nil, apiError("MissingParameter", "CopyImage needs Name, SourceImageId and SourceRegion")
	}
	src, ok := a.regions[deref(in.SourceRegion)]
	if !ok {
		return nil, apiError("InvalidParameterValue", "unknown source region "+deref(in.SourceRegion))
	}
	srcImg, ok := src.images[deref(in.SourceImageId)]
	if !ok {
		return nil, apiError("InvalidAMIID.NotFound", fmt.Sprintf("The image id '[%s]' does not exist", deref(in.SourceImageId)))
	}
	if srcImg.State != "available" {
		return nil, apiError("IncorrectState", "the source image is not available")
	}
	for _, img := range r.images {
		if img.Name == deref(in.Name) && img.Owner == AccountID {
			return nil, apiError("InvalidAMIName.Duplicate", fmt.Sprintf("AMI name %s is already in use by AMI %s", img.Name, img.ID))
		}
	}
	snap := &snapshot{id: a.id("snap"), tags: tagsFromSpecs(in.TagSpecifications, types.ResourceTypeSnapshot)}
	r.snaps[snap.id] = snap
	dur := a.copyDuration
	if dur == 0 {
		dur = DefaultCopyDuration
	}
	img := &Image{
		ID: a.id("ami"), Name: deref(in.Name), Owner: AccountID, Arch: srcImg.Arch, State: "pending",
		RootDevice: srcImg.RootDevice, RootGiB: srcImg.RootGiB, Description: deref(in.Description),
		Tags: tagsFromSpecs(in.TagSpecifications, types.ResourceTypeImage), Created: a.now(),
		SourceImageID: srcImg.ID, SourceRegion: src.name, ReadyAt: a.now().Add(dur), snapshots: []string{snap.id},
	}
	r.images[img.ID] = img
	if tok := deref(in.ClientToken); tok != "" {
		r.copies[tok] = copyToken{image: img.ID, hash: hash}
	}
	return &ec2.CopyImageOutput{ImageId: new(img.ID)}, nil
}

// DeregisterImage implements EC2API: only an image of the account; its
// snapshots go with it when DeleteAssociatedSnapshots is set.
func (e *EC2) DeregisterImage(ctx context.Context, in *ec2.DeregisterImageInput, _ ...func(*ec2.Options),
) (*ec2.DeregisterImageOutput, error) {
	r, err := e.begin(ctx, "DeregisterImage", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	img, ok := r.images[deref(in.ImageId)]
	if !ok {
		return nil, apiError("InvalidAMIID.NotFound", fmt.Sprintf("The image id '[%s]' does not exist", deref(in.ImageId)))
	}
	if img.Owner != AccountID {
		return nil, apiError("AuthFailure", "Not authorized for image: ["+img.ID+"]")
	}
	delete(r.images, img.ID)
	out := &ec2.DeregisterImageOutput{}
	if deref(in.DeleteAssociatedSnapshots) {
		for _, id := range img.snapshots {
			delete(r.snaps, id)
			out.DeleteSnapshotResults = append(out.DeleteSnapshotResults, types.DeleteSnapshotReturnCode{
				SnapshotId: new(id), ReturnCode: types.SnapshotReturnCodesSuccess,
			})
		}
	}
	return out, nil
}

// DescribeImageAttribute implements EC2API: lastLaunchedTime only.
func (e *EC2) DescribeImageAttribute(ctx context.Context, in *ec2.DescribeImageAttributeInput, _ ...func(*ec2.Options),
) (*ec2.DescribeImageAttributeOutput, error) {
	r, err := e.begin(ctx, "DescribeImageAttribute", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	img, ok := r.images[deref(in.ImageId)]
	if !ok {
		return nil, apiError("InvalidAMIID.NotFound", fmt.Sprintf("The image id '[%s]' does not exist", deref(in.ImageId)))
	}
	if in.Attribute != types.ImageAttributeNameLastLaunchedTime {
		return nil, apiError("InvalidParameterValue", "the fake answers lastLaunchedTime only")
	}
	out := &ec2.DescribeImageAttributeOutput{ImageId: new(img.ID)}
	if !img.LastLaunched.IsZero() {
		out.LastLaunchedTime = &types.AttributeValue{Value: new(img.LastLaunched.UTC().Format(timestampLayout))}
	}
	return out, nil
}

// noteLaunch records a launch from the image (the real attribute follows a
// launch within about 24 hours; the fake updates it at once).
func (a *Account) noteLaunch(r *region, ami string) {
	if img, ok := r.images[ami]; ok && img.Owner == AccountID {
		img.LastLaunched = a.now()
	}
}
