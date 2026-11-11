/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"k8s.io/utils/clock"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// imageCacheTTL is how long a resolved image is cached (plan 17: 1 hour).
const imageCacheTTL = time.Hour

// Resolved is a marketplace image the resolver found.
type Resolved struct {
	Image ImageRef
	// Plan is the purchase plan, nil when the image has none.
	Plan *ImagePlan
}

// Images resolves the official Flatcar marketplace images (rung 1 of the image
// ladder, ADR-021) and checks their terms. The images kind of plan 14 (A1)
// plugs in before the marketplace lookup (TODO(plan 14)): its URNs are checked
// the same way, through Get.
type Images struct {
	API    ImageAPI
	Terms  AgreementAPI
	Clock  clock.Clock
	mu     sync.Mutex
	cached map[string]cachedImage
}

type cachedImage struct {
	r   *Resolved
	err error
	at  time.Time
}

// offerOf returns the offer of the architecture; the SKU names to try are
// skuCandidates's: amd64 is Gen2 only: <channel>-gen2; for lts the lts*-gen2
// SKU that has the version (see Resolve). arm64 is Gen2 by nature: <channel>.
func offerOf(arch string) (string, error) {
	switch arch {
	case archAMD64:
		return OfferAMD64, nil
	case archARM64:
		return OfferARM64, nil
	}
	return "", invalidf("architecture %q has no Flatcar image on Azure", arch)
}

// Resolve finds the image of the request: exactly one image must exist, none is
// NotFound (ImageNotFound). Results (also the failures) are cached for an hour.
func (i *Images) Resolve(ctx context.Context, req vendors.ImageRequest) (*Resolved, error) {
	key := strings.Join([]string{req.Region, req.Arch, req.Channel, req.Version, req.Owner}, "|")
	if r, err, ok := i.cacheGet(key); ok {
		return r, err
	}
	r, err := i.resolve(ctx, req)
	if err == nil || vendors.IsClass(err, vendors.NotFound) {
		i.cachePut(key, r, err)
	}
	return r, err
}

func (i *Images) now() time.Time {
	if i.Clock != nil {
		return i.Clock.Now()
	}
	return time.Now()
}

func (i *Images) cacheGet(key string) (*Resolved, error, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	c, ok := i.cached[key]
	if !ok || i.now().Sub(c.at) > imageCacheTTL {
		return nil, nil, false
	}
	return c.r, c.err, true
}

func (i *Images) cachePut(key string, r *Resolved, err error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.cached == nil {
		i.cached = map[string]cachedImage{}
	}
	i.cached[key] = cachedImage{r: r, err: err, at: i.now()}
}

func (i *Images) resolve(ctx context.Context, req vendors.ImageRequest) (*Resolved, error) {
	offer, err := offerOf(req.Arch)
	if err != nil {
		return nil, err
	}
	publisher := req.Owner
	if publisher == "" {
		publisher = DefaultPublisher
	}
	candidates, err := i.skuCandidates(ctx, req, publisher, offer)
	if err != nil {
		return nil, err
	}
	for _, sku := range candidates {
		img, err := i.API.Get(ctx, req.Region, publisher, offer, sku, req.Version)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, classify(err, errContext{})
		}
		ref := ImageRef{Publisher: publisher, Offer: offer, SKU: sku, Version: req.Version}
		out := &Resolved{Image: ref}
		if img.Properties != nil && img.Properties.Plan != nil && deref(img.Properties.Plan.Name) != "" {
			p := img.Properties.Plan
			out.Plan = &ImagePlan{Name: deref(p.Name), Product: deref(p.Product), Publisher: deref(p.Publisher)}
		}
		return out, nil
	}
	return nil, vendors.NewError(vendors.NotFound, codeImageNotFound,
		fmt.Errorf("no %s image of %s %s version %s in %s (tried %s)",
			req.Arch, offer, req.Channel, req.Version, req.Region, strings.Join(candidates, ", ")))
}

// skuCandidates lists the SKU names that can hold the release: the channel's
// for the regular channels, and for lts the lts* SKUs of the offer (the stream
// name carries the year, lts2024 today), newest first, Gen2 only for amd64.
func (i *Images) skuCandidates(
	ctx context.Context, req vendors.ImageRequest, publisher, offer string,
) ([]string, error) {
	suffix := ""
	if req.Arch == archAMD64 {
		suffix = "-gen2"
	}
	if req.Channel != "lts" {
		return []string{req.Channel + suffix}, nil
	}
	skus, err := i.API.ListSKUs(ctx, req.Region, publisher, offer)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, classify(err, errContext{})
	}
	var out []string
	for _, s := range skus {
		gen2 := strings.HasSuffix(s, "-gen2")
		if strings.HasPrefix(s, "lts") && strings.HasSuffix(s, suffix) && (suffix != "" || !gen2) {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	slices.Reverse(out)
	return out, nil
}

// CheckTerms is the check before launching an image with a plan: the
// subscription must have accepted the marketplace terms. SCarpenter never
// accepts them (legal consent belongs to the account owner): the error is
// Fatal with the code MarketplaceTermsNotAccepted and the exact command.
func (i *Images) CheckTerms(ctx context.Context, r *Resolved) error {
	if r == nil || r.Plan == nil {
		return nil
	}
	ok, err := i.Terms.Accepted(ctx, r.Plan.Publisher, r.Plan.Product, r.Plan.Name)
	if err != nil {
		return classify(err, errContext{})
	}
	if !ok {
		return vendors.NewError(vendors.Fatal, CodeMarketplaceTermsNotAccepted, fmt.Errorf(
			"the marketplace terms of %s are not accepted in the subscription: "+
				"run `az vm image terms accept --publisher %s --offer %s --plan %s`",
			r.Image.URN(), r.Plan.Publisher, r.Plan.Product, r.Plan.Name))
	}
	return nil
}
