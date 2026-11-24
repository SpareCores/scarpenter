//go:build kwok

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// The scenarios of plan 29 (image copies). The controller runs with
// --aws-copy-images and --image-copy-idle-ttl=1m (test/kwok/manifests/chart-values.yaml);
// the fake driver has official images only where the steering key imageCopies
// lists regions, so every other scenario of the lane is unaffected. Each
// scenario pins a release of its own: the copies are per release, and the fake's
// account keeps them across scenarios.

// copySteering lists the regions that have the official image; the copies take
// 10 s (the manager looks at them every 30 s).
func copySteering(official ...string) map[string]string {
	var b strings.Builder
	b.WriteString("official:\n")
	for _, r := range official {
		fmt.Fprintf(&b, "- {region: %s}\n", r)
	}
	b.WriteString("copyDuration: 10s\n")
	return map[string]string{"imageCopies": b.String()}
}

func withRelease(version string) func(*v1alpha1.SCNodeClass) {
	return func(nc *v1alpha1.SCNodeClass) { nc.Spec.Flatcar.Version = version }
}

// copyView is what the scenarios read of the fake account's copies.
type copyView struct {
	Region  string `json:"region"`
	Ref     string `json:"ref"`
	Release struct{ Channel, Version, Arch string }
}

// copiesOf returns the copies of the release in the region, as the fake account
// holds them.
func copiesOf(t *testing.T, region, version string) []copyView {
	t.Helper()
	cm := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: ns, Name: "scarpenter-fake-resources-image-copies"}
	if err := k8s.Get(ctx(), key, cm); err != nil {
		return nil
	}
	var doc struct {
		Copies []copyView `json:"copies"`
	}
	if err := json.Unmarshal([]byte(cm.Data["imageCopies"]), &doc); err != nil {
		t.Fatalf("the fake's copies: %v", err)
	}
	var out []copyView
	for _, c := range doc.Copies {
		if c.Region == region && c.Release.Version == version {
			out = append(out, c)
		}
	}
	return out
}

// imageStatuses returns the images of the NodeClass's vendor status in the region by architecture.
func imageStatuses(class, region string) map[string]v1alpha1.ImageStatus {
	nc := &v1alpha1.SCNodeClass{}
	if err := k8s.Get(ctx(), client.ObjectKey{Name: class}, nc); err != nil {
		return nil
	}
	out := map[string]v1alpha1.ImageStatus{}
	for _, img := range nc.Status.Vendors[vendor].Images {
		if img.Region == region {
			out[img.Arch] = img
		}
	}
	return out
}

// waitImages waits until both architectures of the region have the reason ("" is
// ready).
func waitImages(t *testing.T, class, region, reason string, timeout time.Duration) {
	t.Helper()
	eventually(t, timeout, time.Second, fmt.Sprintf("%s images in %s: %q", class, region, reason), func() (bool, string) {
		imgs := imageStatuses(class, region)
		if len(imgs) != 2 {
			return false, fmt.Sprintf("%+v", imgs)
		}
		for _, img := range imgs {
			if img.Reason != reason || img.Ready != (reason == "") {
				return false, fmt.Sprintf("%+v", imgs)
			}
		}
		return true, ""
	})
}

// S29a: a region without an official image is skipped by the first launch (no
// attempt, no call to the vendor), the pod runs in the dearer region meanwhile, the
// copy is made and served to the next launch, the idle copy is deleted after the idle TTL, and a
// launch after the delete resolves afresh instead of being given the deleted image.
func TestS29OnDemandCopy(t *testing.T) {
	const release = "4757.2.7"
	typ, cheap, other := typeWithUniqueCheapestRegion(t, []string{us1, eu1}, 2, keys2(taken)...)
	taken[typ] = true
	setFake(t, copySteering(other))
	createNodeClassWith(t, "s29a", withRelease(release), cheap, other)
	waitImages(t, "s29a", cheap, vendors.CodeImageCopyOnDemand, time.Minute)
	waitImages(t, "s29a", other, "", time.Minute)
	createNodePool(t, "s29a", "s29a", in(labelType, vendor+"."+typ), in(labelCapacity, "on-demand"))
	sel := map[string]string{labelNodePool: "s29a"}
	mark := time.Now()
	createPod(t, "s29a-1", sel, "1500m")
	node := waitPodRunning(t, "s29a-1", long)
	if got := nodeRegion(t, node); got != other {
		t.Errorf("the first pod runs in %s, want %s: %s has no image yet", got, other, cheap)
	}
	for _, c := range callsSince(t, mark, "create") {
		if c.Region == cheap {
			t.Errorf("a launch skipped for its image still called the vendor: %+v", c)
		}
	}
	// Both architectures are copied as one unit; the status shows the copy and
	// then both images ready.
	waitImages(t, "s29a", cheap, "", 3*time.Minute)
	if n := len(objEvents(t, "SCNodeClass", "s29a", vendors.CodeImageCopying)); n < 1 || n > 2 {
		t.Errorf("%d ImageCopying Events, want one per architecture at most", n)
	}

	// The next launch goes to the cheap region, from the copy.
	createPod(t, "s29a-2", sel, "1500m")
	node2 := waitPodRunning(t, "s29a-2", long)
	if got := nodeRegion(t, node2); got != cheap {
		t.Errorf("the second pod runs in %s, want %s: the copy is available", got, cheap)
	}
	if n := len(copiesOf(t, cheap, release)); n != 2 {
		t.Errorf("%d copies in %s, want one per architecture", n, cheap)
	}

	// Nothing uses the copy any more: it goes after the idle TTL (1m).
	deletePodAndWaitNode(t, "s29a-2", node2)
	eventually(t, 5*time.Minute, 2*time.Second, "the idle copies deleted", func() (bool, string) {
		return len(copiesOf(t, cheap, release)) == 0, fmt.Sprint(copiesOf(t, cheap, release))
	})
	waitImages(t, "s29a", cheap, vendors.CodeImageCopyOnDemand, 2*time.Minute)

	// A launch after the delete resolves afresh: the cached image of the deleted
	// copy is gone, so the launch skips the region again and a new copy starts.
	mark = time.Now()
	createPod(t, "s29a-3", sel, "1500m")
	node3 := waitPodRunning(t, "s29a-3", long)
	if got := nodeRegion(t, node3); got != other {
		t.Errorf("the pod after the delete runs in %s, want %s: no stale image of %s", got, other, cheap)
	}
	for _, c := range callsSince(t, mark, "create") {
		if c.Region == cheap {
			t.Errorf("a launch from the deleted copy: %+v", c)
		}
	}
	eventually(t, time.Minute, time.Second, "a new copy unit started", func() (bool, string) {
		n := len(copiesOf(t, cheap, release))
		return n == 2, fmt.Sprintf("%d copies", n)
	})
}

// S29b: when the official image appears in a region that has a copy nobody
// launched from, the copy is deleted (rule b) and the region reports the
// official image.
func TestS29PrimaryAppearsAndTheCopyGoes(t *testing.T) {
	const release = "4757.2.8"
	typ, cheap, other := typeWithUniqueCheapestRegion(t, []string{us1, eu1}, 2, keys2(taken)...)
	taken[typ] = true
	setFake(t, copySteering(other))
	createNodeClassWith(t, "s29b", withRelease(release), cheap, other)
	createNodePool(t, "s29b", "s29b", in(labelType, vendor+"."+typ), in(labelCapacity, "on-demand"))
	createPod(t, "s29b-1", map[string]string{labelNodePool: "s29b"}, "1500m")
	waitPodRunning(t, "s29b-1", long)
	waitImages(t, "s29b", cheap, "", 3*time.Minute)
	if n := len(copiesOf(t, cheap, release)); n != 2 {
		t.Fatalf("%d copies in %s, want one per architecture", n, cheap)
	}

	setFake(t, copySteering(other, cheap))
	eventually(t, 5*time.Minute, 2*time.Second, "the copies deleted for the official image", func() (bool, string) {
		return len(copiesOf(t, cheap, release)) == 0, fmt.Sprint(copiesOf(t, cheap, release))
	})
	eventually(t, 2*time.Minute, time.Second, "the official image reported", func() (bool, string) {
		imgs := imageStatuses("s29b", cheap)
		for _, img := range imgs {
			if !img.Ready || !strings.HasPrefix(img.Ref, "ami-official-") {
				return false, fmt.Sprintf("%+v", imgs)
			}
		}
		return len(imgs) == 2, fmt.Sprintf("%+v", imgs)
	})
}

// S29c: the release bump. A node runs from a copy in a region without an
// official image; pinning a new release queues its copy at once, drift replaces
// the node in the same region once the copy is there, and the copy of the old
// release stays while the old node runs.
func TestS29ReleaseBump(t *testing.T) {
	const oldRelease, newRelease = "4757.2.9", "4757.3.1"
	typ, cheap, other := typeWithUniqueCheapestRegion(t, []string{us1, eu1}, 2, keys2(taken)...)
	taken[typ] = true
	setFake(t, copySteering(other))
	// Only the region without an official image: the pod waits for the copy.
	nc := createNodeClassWith(t, "s29c", withRelease(oldRelease), cheap)
	createNodePool(t, "s29c", "s29c", in(labelType, vendor+"."+typ), in(labelCapacity, "on-demand"))
	d := createDeployment(t, "s29c", map[string]string{labelNodePool: "s29c"}, "100m")
	deploymentReady := func(updated bool) (bool, string) {
		got := &appsv1.Deployment{}
		if err := k8s.Get(ctx(), client.ObjectKeyFromObject(d), got); err != nil {
			return false, err.Error()
		}
		return got.Status.ReadyReplicas == 1 && (!updated || got.Status.UpdatedReplicas == 1), fmt.Sprint(got.Status)
	}
	// The first launches fail while the copy is made; the claim that matters is the
	// one behind the running pod.
	eventually(t, long, time.Second, "the deployment running from the copy", func() (bool, string) { return deploymentReady(false) })
	claims := waitClaims(t, "s29c", 1, time.Minute)
	if len(claims) != 1 {
		t.Fatalf("%d NodeClaims behind the running pod, want 1", len(claims))
	}
	old, oldHash := claims[0].Name, claims[0].Annotations[v1alpha1.AnnotationNodeClassHash]
	if n := len(copiesOf(t, cheap, oldRelease)); n != 2 {
		t.Fatalf("%d copies of the old release in %s, want one per architecture", n, cheap)
	}

	eventually(t, time.Minute, time.Second, "the NodeClass update", func() (bool, string) {
		got := &v1alpha1.SCNodeClass{}
		if err := k8s.Get(ctx(), client.ObjectKeyFromObject(nc), got); err != nil {
			return false, err.Error()
		}
		got.Spec.Flatcar.Version = newRelease
		if err := k8s.Update(ctx(), got); err != nil {
			return false, err.Error()
		}
		return true, ""
	})
	// The bump queues the new release at once, not at the next garbage collection or launch.
	eventually(t, 30*time.Second, time.Second, "the copies of the new release started", func() (bool, string) {
		n := len(copiesOf(t, cheap, newRelease))
		return n == 2, fmt.Sprintf("%d copies of %s", n, newRelease)
	})

	// Drift replaces the node once the copy is available; the old copy is kept while
	// the old node runs.
	eventually(t, long, 2*time.Second, "the node replaced from the new copy", func() (bool, string) {
		if len(copiesOf(t, cheap, oldRelease)) == 0 {
			for _, i := range liveInstances(t) {
				if i.NodeClaim == old {
					t.Errorf("the copy of the old release was deleted while %s runs from it", old)
				}
			}
		}
		cs, _ := listClaims("s29c")
		for _, c := range cs {
			if c.Name == old {
				return false, "the old NodeClaim still exists"
			}
		}
		fresh := len(cs) == 1 && cs[0].DeletionTimestamp.IsZero()
		return fresh && len(copiesOf(t, cheap, newRelease)) >= 1, fmt.Sprintf("%d NodeClaims", len(cs))
	})
	eventually(t, long, time.Second, "the deployment running again", func() (bool, string) { return deploymentReady(true) })
	cs, _ := listClaims("s29c")
	if len(cs) != 1 || cs[0].Name == old {
		t.Fatalf("NodeClaims after the replacement: %d, the old one %s must be gone", len(cs), old)
	}
	if h := cs[0].Annotations[v1alpha1.AnnotationNodeClassHash]; h == "" || h == oldHash {
		t.Errorf("the replacement carries the drift hash %q of the old release %q, want the new release's", h, oldHash)
	}
	// With the old node gone nothing uses the old copy: it goes, the new one stays.
	eventually(t, 5*time.Minute, 2*time.Second, "the copy of the old release deleted", func() (bool, string) {
		return len(copiesOf(t, cheap, oldRelease)) == 0, fmt.Sprint(copiesOf(t, cheap, oldRelease))
	})
	// The amd64 copy of the new release runs the pod; the unused arm64 one may be idle already.
	if n := len(copiesOf(t, cheap, newRelease)); n < 1 {
		t.Error("the copy of the new release the pod runs from is gone")
	}
}
