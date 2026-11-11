//go:build kwok

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	awsapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
)

// The scenarios of plan 14 A1: the `images` resource kind. The fake driver
// resolves the selector against the `images` list of its configuration and the
// NodeClass controller reports the result in status.vendors.aws.images and the
// condition ImagesReady.

// images is the fake account's image list: both architectures of the pinned
// release (global), an image of another release, and one that says none.
const s20Images = `
- {id: s20-amd, region: "*", name: flatcar-4757.2.3-amd64, arch: amd64, channel: stable, version: "4757.2.3"}
- {id: s20-arm, region: "*", name: flatcar-4757.2.3-arm64, arch: arm64, channel: stable, version: "4757.2.3"}
- {id: s20-old, region: "*", name: flatcar-4500.1.0-amd64, arch: amd64, channel: stable, version: "4500.1.0"}
- {id: s20-mute, region: "*", name: flatcar-unknown-amd64, arch: amd64}
`

func withImages(use *common.ResourceUse) func(*v1alpha1.SCNodeClass) {
	return func(nc *v1alpha1.SCNodeClass) {
		nc.Spec.Vendors.AWS.Resources = &awsapi.Resources{Images: &common.Resource{Use: use}}
	}
}

// imagesReady waits for the ImagesReady condition of the NodeClass to settle on
// the status and reason, and returns the reported images.
func imagesReady(t *testing.T, class string, status metav1.ConditionStatus, reason string) []v1alpha1.ImageStatus {
	t.Helper()
	var images []v1alpha1.ImageStatus
	eventually(t, 2*time.Minute, time.Second, fmt.Sprintf("%s ImagesReady=%s (%s)", class, status, reason), func() (bool, string) {
		nc := &v1alpha1.SCNodeClass{}
		if err := k8s.Get(ctx(), client.ObjectKey{Name: class}, nc); err != nil {
			return false, err.Error()
		}
		vs := nc.Status.Vendors[vendor]
		images = vs.Images
		for _, c := range vs.Conditions {
			if c.Type == v1alpha1.VendorConditionImagesReady {
				return c.Status == status && c.Reason == reason, fmt.Sprintf("%s %s: %s", c.Status, c.Reason, c.Message)
			}
		}
		return false, "no ImagesReady condition"
	})
	return images
}

// S20: explicit image IDs. The IDs of both architectures are listed under "*"; the
// request's architecture decides which one each region and architecture reports,
// ImagesReady is True, and a node launches.
func TestS20ExplicitImageIDs(t *testing.T) {
	setFake(t, map[string]string{"images": s20Images})
	createNodeClassWith(t, "s20a", withImages(&common.ResourceUse{
		IDs: map[string]common.IDList{"*": {"s20-amd", "s20-arm"}},
	}), us1)
	images := imagesReady(t, "s20a", metav1.ConditionTrue, "Ready")
	got := map[string]string{}
	for _, img := range images {
		got[img.Region+"/"+img.Arch] = img.Ref
		if !img.Ready {
			t.Errorf("image %+v is not ready", img)
		}
	}
	if got[us1+"/amd64"] != "s20-amd" || got[us1+"/arm64"] != "s20-arm" {
		t.Errorf("reported images %v, want s20-amd and s20-arm", got)
	}
	createNodePool(t, "s20a", "s20a", in(labelRegion, us1), in(labelCapacity, "on-demand"))
	typ := takeTypes(t, []string{us1}, 2, 1)[0]
	createPod(t, "s20a", map[string]string{labelType: vendor + "." + typ, labelNodePool: "s20a"}, "100m")
	waitPodRunning(t, "s20a", long)
}

// S20: an image of another Flatcar release, or one that says none, makes the
// images not ready with the reason of the version check, and nothing is
// launched: the region's offerings are unavailable.
func TestS20ImageOfAnotherVersionIsNeverBooted(t *testing.T) {
	setFake(t, map[string]string{"images": s20Images})
	for _, tc := range []struct {
		name, id, reason string
	}{
		{"s20b", "s20-old", "ImageVersionMismatch"},
		{"s20c", "s20-mute", "ImageVersionUnknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			createNodeClassWith(t, tc.name, withImages(&common.ResourceUse{
				IDs: map[string]common.IDList{"*": {tc.id}},
			}), us1)
			imagesReady(t, tc.name, metav1.ConditionFalse, tc.reason)
			if c, ok, _ := vendorCondition(tc.name, v1alpha1.VendorConditionImagesReady); ok && !strings.Contains(c.Message, tc.id) {
				t.Errorf("the condition does not name the image %s: %s", tc.id, c.Message)
			}
			createNodePool(t, tc.name, tc.name, in(labelRegion, us1), in(labelCapacity, "on-demand"))
			typ := takeTypes(t, []string{us1}, 2, 1)[0]
			createPod(t, tc.name, map[string]string{labelType: vendor + "." + typ, labelNodePool: tc.name}, "100m")
			consistently(t, 20*time.Second, 2*time.Second, "no NodeClaim from the wrong image", func() (bool, string) {
				cs, _ := listClaims(tc.name)
				return len(cs) > 0, fmt.Sprintf("%d NodeClaims", len(cs))
			})
			for _, i := range liveInstances(t) {
				if strings.HasPrefix(i.NodeClaim, tc.name) {
					t.Errorf("an instance was launched from an image that is not the pinned release: %+v", i)
				}
			}
		})
	}
}

// S20: a region the NodeClass names but lists no image for is not ready, and its
// offerings are unavailable; the region that has its image still launches.
func TestS20RegionWithoutAnImage(t *testing.T) {
	setFake(t, map[string]string{"images": s20Images})
	createNodeClassWith(t, "s20d", withImages(&common.ResourceUse{
		IDs: map[string]common.IDList{us1: {"s20-amd", "s20-arm"}},
	}), us1, us2)
	images := imagesReady(t, "s20d", metav1.ConditionFalse, "ImagesNotReady")
	ready := map[string]bool{}
	for _, img := range images {
		ready[img.Region+"/"+img.Arch] = img.Ready
		if !img.Ready && !strings.Contains(img.Message, us2) {
			t.Errorf("the message of %s/%s does not name the region: %q", img.Region, img.Arch, img.Message)
		}
	}
	if !ready[us1+"/amd64"] || !ready[us1+"/arm64"] || ready[us2+"/amd64"] || ready[us2+"/arm64"] {
		t.Errorf("readiness %v, want us-east-1 ready and us-east-2 not", ready)
	}
	createNodePool(t, "s20d", "s20d", in(labelCapacity, "on-demand"))
	typ := takeTypes(t, []string{us1, us2}, 2, 1)[0]
	createPod(t, "s20d", map[string]string{labelType: vendor + "." + typ, labelNodePool: "s20d"}, "100m")
	node := waitPodRunning(t, "s20d", long)
	claims := waitClaims(t, "s20d", 1, time.Minute)
	if got := claims[0].Labels[labelRegion]; got != us1 {
		t.Errorf("the node %s is in %q, want %s (the region with an image)", node, got, us1)
	}
}
