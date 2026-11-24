/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vendors

import (
	"context"
	"time"
)

// Codes of the image status while copies are on (ADR-032, plan 29). They are
// reasons of the image status and of ImagesReady, like CodeImageBuilding.
const (
	// CodeImageCopyOnDemand: the region has no image; a copy is made when a
	// launch wants the region.
	CodeImageCopyOnDemand = "ImageCopyOnDemand"
	// CodeImageCopying: a copy is queued or in flight.
	CodeImageCopying = "ImageCopying"
	// CodeImageCopyFailed: the last copy failed a short time ago.
	CodeImageCopyFailed = "ImageCopyFailed"
	// CodeImageCopySourceMissing: copies are on but no source has the image.
	CodeImageCopySourceMissing = "ImageCopySourceMissing"
)

// Logical tag keys of the images SCarpenter copies. The release is tagged with
// the unprefixed keys flatcar-channel and flatcar-version, which the driver
// writes itself (the version check of ADR-017 reads them); TagResource holds
// ResourceKindImageCopy.
const (
	TagImageArch   TagKey = "image-arch"
	TagImageSource TagKey = "image-source"
)

// ResourceKindImageCopy is the value of the resource-kind tag of a copy.
const ResourceKindImageCopy = "image-copy"

// Release is the Flatcar release of one boot image: what a copy is made of.
type Release struct {
	Channel string
	Version string
	// Arch is amd64 or arm64.
	Arch string
}

// ImageSource is an image a copy can be made from.
type ImageSource struct {
	Region string
	Ref    string
}

// CopyState is the state of a copy as the vendor reports it.
type CopyState string

// The states of a copy.
const (
	CopyPending   CopyState = "Pending"
	CopyAvailable CopyState = "Available"
	CopyFailed    CopyState = "Failed"
)

// ImageCopy is an image SCarpenter copied (or is copying) into a region.
type ImageCopy struct {
	Region  string
	Ref     string
	Release Release
	// Source is the image the copy was made from, "<region>/<ref>"; empty when
	// the vendor does not say.
	Source string
	State  CopyState
	// Message is the vendor's reason of a failed copy.
	Message string
	Created time.Time
	// LastUsed is the vendor's own record of the last launch; zero when the
	// vendor has none or the image was never launched.
	LastUsed time.Time
}

// ImageCopier is the optional capability of a driver whose vendor has regional
// images and a copy API (plan 29, ADR-032): it copies a boot image the account
// can already read into another region. The copy manager (pkg/imagecopy) owns
// the policy; the driver only maps these calls onto the vendor's API.
//
// Error classes the manager reads: StartCopy answers QuotaExceeded or
// Retryable for a limit or throttling error (the copy waits and is retried
// with a backoff), any other class for a refusal (the copy fails). The other
// calls return Retryable for transient errors.
type ImageCopier interface {
	// Architectures lists the architectures the driver serves in the region.
	// The manager copies all of them as one unit (the image gate is per region).
	Architectures(ctx context.Context, region string) ([]string, error)
	// Primary finds the image the vendor's convention resolves in the region
	// (the official image, the operator's imported one), never a copy of
	// SCarpenter's.
	Primary(ctx context.Context, region string, rel Release) (ref string, found bool, err error)
	// CopySources lists the images a copy of the release can be made from, in
	// the order of preference; each has passed the version check of ADR-017.
	CopySources(ctx context.Context, rel Release) ([]ImageSource, error)
	// ListCopies lists this cluster's copies (any state) in the regions, found
	// by the ownership tags, with the vendor's last-used time where it has one.
	// Images without the ownership tags are never listed.
	ListCopies(ctx context.Context, regions []string) ([]ImageCopy, error)
	// StartCopy starts a copy into region, tagged at creation (tags are the
	// logical ownership tags; the driver adds the release tags). Calling it
	// again with the same token returns the same image.
	StartCopy(
		ctx context.Context, src ImageSource, region string, rel Release, tags map[TagKey]string, token string,
	) (ref string, err error)
	// CopyInUse says whether an instance of the account, of any cluster and in
	// any state but terminated, runs from the copy.
	CopyInUse(ctx context.Context, c ImageCopy) (bool, error)
	// TouchCopy records a use; a no-op for a vendor that records launches itself.
	TouchCopy(ctx context.Context, c ImageCopy, at time.Time) error
	// DeleteCopy deletes the image and its snapshots. An image without this
	// cluster's ownership tags is refused (class Fatal) and left alone; a copy
	// that does not exist is not an error.
	DeleteCopy(ctx context.Context, c ImageCopy) error
}

// ImageCopies is the copy manager as a driver sees it (pkg/imagecopy.Manager);
// Config.ImageCopies is nil when copies are off for the vendor.
type ImageCopies interface {
	// Lookup answers an image request after the driver's primary lookup missed.
	// It returns (nil, nil) for a request the manager has no say in (a
	// NodeClass that names its images or an owner). demand says the request is
	// a launch: it enqueues a copy and counts as a use; a report only reads.
	// An image that is not ready is an *Error of class NotFound with one of the
	// ImageCopy codes. It never calls the vendor except to read a region's
	// copies the first time, and answers from memory afterwards.
	Lookup(ctx context.Context, req ImageRequest, demand bool) (*Image, error)
}
