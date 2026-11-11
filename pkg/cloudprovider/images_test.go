/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	awsapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	fakedriver "github.com/SpareCores/scarpenter/pkg/vendors/fake"
)

// requestDriver records the image requests of the launches.
type requestDriver struct {
	*fakedriver.Driver
	mu   sync.Mutex
	reqs []vendors.ImageRequest
}

func (d *requestDriver) ResolveImage(ctx context.Context, req vendors.ImageRequest) (*vendors.Image, error) {
	d.mu.Lock()
	d.reqs = append(d.reqs, req)
	d.mu.Unlock()
	return d.Driver.ResolveImage(ctx, req)
}

func imagesBlock(use *common.ResourceUse) func(*v1alpha1.SCNodeClass) {
	return func(nc *v1alpha1.SCNodeClass) {
		nc.Spec.Vendors.AWS.Resources = &awsapi.Resources{Images: &common.Resource{Use: use}}
	}
}

const twoImages = `
- {id: img-amd, region: "*", name: flatcar-4757.2.3-amd64, arch: amd64, channel: stable, version: "4757.2.3"}
- {id: img-arm, region: "*", name: flatcar-4757.2.3-arm64, arch: arm64, channel: stable, version: "4757.2.3"}
- {id: img-old, region: "*", name: flatcar-4500.1.0-amd64, arch: amd64, channel: stable, version: "4500.1.0"}
`

// The core fills ImageRequest.Use from the NodeClass's images selector: the IDs
// of the region, the placeholders expanded for the request's architecture.
func TestCreateGivesTheDriverTheImagesSelector(t *testing.T) {
	var drv *requestDriver
	e := newEnv(t, envOptions{
		config: map[string]string{"images": twoImages},
		transform: imagesBlock(&common.ResourceUse{
			IDs:  map[string]common.IDList{region1: {"img-amd", "img-arm"}, region2: {"img-amd"}},
			Name: "flatcar-{version}-{arch}",
		}),
		wrap: func(d *fakedriver.Driver) vendors.Driver { drv = &requestDriver{Driver: d}; return drv },
	})
	inst, err := e.cp.Create(t.Context(), e.newClaim("claim-images", []string{"aws.m7i.large"}))
	if err != nil {
		t.Fatal(err)
	}
	_ = inst
	drv.mu.Lock()
	defer drv.mu.Unlock()
	if len(drv.reqs) == 0 {
		t.Fatal("no image was requested")
	}
	req := drv.reqs[len(drv.reqs)-1]
	want := "flatcar-4757.2.3-amd64"
	if req.Arch != "amd64" || req.Use.Name != want || req.Use.IsDefault() {
		t.Errorf("request = %+v, want the name %s", req, want)
	}
	if req.Region == region1 && !slices.Equal(req.Use.IDs, []string{"img-amd", "img-arm"}) ||
		req.Region == region2 && !slices.Equal(req.Use.IDs, []string{"img-amd"}) {
		t.Errorf("the IDs of %s = %v", req.Region, req.Use.IDs)
	}
}

// Without an images selector the request carries none: the driver's own convention.
func TestCreateWithoutAnImagesSelectorAsksForTheDefault(t *testing.T) {
	var drv *requestDriver
	e := newEnv(t, envOptions{
		wrap: func(d *fakedriver.Driver) vendors.Driver { drv = &requestDriver{Driver: d}; return drv },
	})
	if _, err := e.cp.Create(t.Context(), e.newClaim("claim-default-image", []string{"aws.m7i.large"})); err != nil {
		t.Fatal(err)
	}
	drv.mu.Lock()
	defer drv.mu.Unlock()
	if len(drv.reqs) == 0 || !drv.reqs[0].Use.IsDefault() {
		t.Errorf("requests = %+v, want the default selector", drv.reqs)
	}
}

// An image that is not the pinned release is never booted: the launch is refused
// as an image failure, not retried against another offering of the region, and no
// instance is created.
func TestCreateNeverBootsAnImageOfAnotherVersion(t *testing.T) {
	e := newEnv(t, envOptions{
		regions: []string{region1},
		config:  map[string]string{"images": twoImages},
		transform: imagesBlock(&common.ResourceUse{
			IDs: map[string]common.IDList{region1: {"img-old"}},
		}),
	})
	_, err := e.cp.Create(t.Context(), e.newClaim("claim-mismatch", []string{"aws.m7i.large"}))
	if err == nil || !strings.Contains(err.Error(), "4500.1.0") {
		t.Fatalf("Create = %v, want the image refused for its version", err)
	}
	if n := len(e.calls("create")); n != 0 {
		t.Errorf("%d instances were created from an image of another version", n)
	}
}
