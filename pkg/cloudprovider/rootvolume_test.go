//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider_test

import (
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
)

const gib = int64(1) << 30

func withRootMax(size string) func(*v1alpha1.SCNodeClass) {
	return func(nc *v1alpha1.SCNodeClass) {
		nc.Spec.RootVolume = &v1alpha1.RootVolumeSpec{MaxSize: resource.MustParse(size)}
	}
}

func requesting(storage string) claimOption {
	return func(nc *karpv1.NodeClaim) {
		nc.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse(storage)}
	}
}

// ephemeralOf returns the ephemeral storage capacity and allocatable of a NodeClaim, in GiB.
func ephemeralOf(nc *karpv1.NodeClaim) (capacity, allocatable int64) {
	c, a := nc.Status.Capacity[corev1.ResourceEphemeralStorage], nc.Status.Allocatable[corev1.ResourceEphemeralStorage]
	return c.Value() / gib, a.Value() / gib
}

// TestRootVolumeGrowsToTheRequests: the catalog advertises maxSize, a NodeClaim
// that requests more than the configured 64 GiB launches with a disk that covers
// it (plus the 1 GiB the kubelet reserves), and its status carries that size,
// not the advertised one.
func TestRootVolumeGrowsToTheRequests(t *testing.T) {
	e := newEnv(t, envOptions{transform: withRootMax("1Ti")})
	it, ok := e.cat.Type(className, testType)
	if !ok {
		t.Fatal("no type")
	}
	if c := it.Capacity[corev1.ResourceEphemeralStorage]; c.Value() != 1024*gib {
		t.Errorf("advertised ephemeral-storage = %s, want the 1Ti maxSize", c.String())
	}

	created, err := e.cp.Create(t.Context(), e.newClaim("big", []string{testType}, requesting("600Gi")))
	if err != nil {
		t.Fatal(err)
	}
	if c, a := ephemeralOf(created); c != 601 || a != 600 {
		t.Errorf("a 600Gi request: capacity %d GiB, allocatable %d GiB, want 601 and 600", c, a)
	}
	// The launch is priced with the grown disk; the catalog's price stays that of the default one.
	price, err := strconv.ParseFloat(created.Annotations[v1alpha1.AnnotationPrice], 64)
	if err != nil {
		t.Fatal(err)
	}
	if catalogUSD := e.catalogPrice(created).USD; price <= catalogUSD {
		t.Errorf("launch price %v is not above the catalog's %v: the grown disk is not priced", price, catalogUSD)
	}
}

func TestRootVolumeKeepsTheConfiguredSizeForSmallRequests(t *testing.T) {
	e := newEnv(t, envOptions{transform: withRootMax("1Ti")})
	for name, opts := range map[string][]claimOption{"below": {requesting("10Gi")}, "none": nil} {
		created, err := e.cp.Create(t.Context(), e.newClaim("small-"+name, []string{testType}, opts...))
		if err != nil {
			t.Fatal(err)
		}
		if c, a := ephemeralOf(created); c != 64 || a != 63 {
			t.Errorf("%s: capacity %d GiB, allocatable %d GiB, want the configured 64 and 63", name, c, a)
		}
	}
}

// TestRootVolumeStopsAtMaxSize: a request the maxSize cannot cover fits no type,
// and one that just fits gets the whole maxSize.
func TestRootVolumeStopsAtMaxSize(t *testing.T) {
	e := newEnv(t, envOptions{transform: withRootMax("256Gi")})
	created, err := e.cp.Create(t.Context(), e.newClaim("edge", []string{testType}, requesting("255Gi")))
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := ephemeralOf(created); c != 256 {
		t.Errorf("capacity %d GiB, want 256", c)
	}
	if _, err := e.cp.Create(t.Context(), e.newClaim("over", []string{testType}, requesting("256Gi"))); err == nil {
		t.Error("a request above what maxSize allows launched")
	}
}

// TestWithoutRootVolumeNothingChanges: no rootVolume, no advertised growth, and a
// request above the configured disk fits no type.
func TestWithoutRootVolumeNothingChanges(t *testing.T) {
	e := newEnv(t, envOptions{})
	it, _ := e.cat.Type(className, testType)
	if c := it.Capacity[corev1.ResourceEphemeralStorage]; c.Value() != 64*gib {
		t.Errorf("advertised ephemeral-storage = %s, want the configured 64Gi", c.String())
	}
	if _, err := e.cp.Create(t.Context(), e.newClaim("big", []string{testType}, requesting("600Gi"))); err == nil {
		t.Error("a request above the disk launched")
	}
}
