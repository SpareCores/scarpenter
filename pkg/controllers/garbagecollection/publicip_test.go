/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package garbagecollection_test

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	gc "github.com/SpareCores/scarpenter/pkg/controllers/garbagecollection"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// publicIPs returns the public address annotations of the Node and the NodeClaim.
func publicIPs(t *testing.T, e *env) (node, claim map[string]string) {
	t.Helper()
	pick := func(a map[string]string) map[string]string {
		out := map[string]string{}
		for _, k := range []string{v1alpha1.AnnotationPublicIPv4, v1alpha1.AnnotationPublicIPv6} {
			if v, ok := a[k]; ok {
				out[k] = v
			}
		}
		return out
	}
	n := &corev1.Node{}
	if err := e.kube.Get(context.Background(), types.NamespacedName{Name: "node-1"}, n); err != nil {
		t.Fatal(err)
	}
	c := &karpv1.NodeClaim{}
	if err := e.kube.Get(context.Background(), types.NamespacedName{Name: "live"}, c); err != nil {
		t.Fatal(err)
	}
	return pick(n.Annotations), pick(c.Annotations)
}

func liveClaim(nodeName string) *karpv1.NodeClaim {
	c := claim("live")
	c.Status.NodeName = nodeName
	return c
}

func publicInstance(v4, v6 string) *vendors.Instance {
	i := inst("i-1", "r1", "live", 10*time.Minute)
	i.Addresses = []string{"10.0.0.5"}
	i.PublicIPv4, i.PublicIPv6 = v4, v6
	return i
}

func sweep(t *testing.T, e *env) {
	t.Helper()
	if _, err := e.c.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPublicIPAnnotations(t *testing.T) {
	i := publicInstance("203.0.113.9", "2001:db8::9")
	drv := &stubDriver{vendor: "aws", instances: map[string][]*vendors.Instance{"r1": {i}}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Annotations: map[string]string{"keep": "me"}}}
	e := newEnv(t, drv, []client.Object{liveClaim("node-1"), node}, nil)

	sweep(t, e)
	want := map[string]string{v1alpha1.AnnotationPublicIPv4: "203.0.113.9", v1alpha1.AnnotationPublicIPv6: "2001:db8::9"}
	n, c := publicIPs(t, e)
	if !maps.Equal(n, want) || !maps.Equal(c, want) {
		t.Fatalf("node %v, claim %v; want %v on both", n, c, want)
	}
	got := &corev1.Node{}
	_ = e.kube.Get(context.Background(), types.NamespacedName{Name: "node-1"}, got)
	if got.Annotations["keep"] != "me" {
		t.Errorf("annotations %v lost another one", got.Annotations)
	}

	// A changed address is updated; one the instance lost is removed, never emptied.
	i.PublicIPv4, i.PublicIPv6 = "203.0.113.10", ""
	sweep(t, e)
	want = map[string]string{v1alpha1.AnnotationPublicIPv4: "203.0.113.10"}
	if n, c = publicIPs(t, e); !maps.Equal(n, want) || !maps.Equal(c, want) {
		t.Fatalf("after a change: node %v, claim %v; want %v on both", n, c, want)
	}
}

func TestPublicIPOnlyIPv6(t *testing.T) {
	drv := &stubDriver{vendor: "hcloud", instances: map[string][]*vendors.Instance{"r1": {publicInstance("", "2001:db8::9")}}}
	e := newEnv(t, drv, []client.Object{liveClaim("node-1"), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}, nil)
	sweep(t, e)
	want := map[string]string{v1alpha1.AnnotationPublicIPv6: "2001:db8::9"}
	if n, c := publicIPs(t, e); !maps.Equal(n, want) || !maps.Equal(c, want) {
		t.Fatalf("node %v, claim %v; want only the IPv6 annotation", n, c)
	}
}

func TestPublicIPWaitsForTheNode(t *testing.T) {
	drv := &stubDriver{vendor: "aws", instances: map[string][]*vendors.Instance{"r1": {publicInstance("203.0.113.9", "")}}}
	// The NodeClaim has no node name yet; then it names a Node that does not exist.
	e := newEnv(t, drv, []client.Object{liveClaim(""), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}, nil)
	sweep(t, e)
	if n, c := publicIPs(t, e); len(n) != 0 || len(c) != 0 {
		t.Fatalf("node %v, claim %v before the node is registered", n, c)
	}
	e = newEnv(t, drv, []client.Object{liveClaim("node-2")}, nil)
	sweep(t, e) // a missing Node is not an error
}

func TestPublicIPFromGetWhenTheListingHasNoAddresses(t *testing.T) {
	listed := inst("i-1", "r1", "live", 10*time.Minute) // like an OCI listing
	listed.PublicUnknown = true
	full := publicInstance("203.0.113.9", "")
	drv := &stubDriver{
		vendor: "oci", instances: map[string][]*vendors.Instance{"r1": {listed}},
		byRef: map[vendors.InstanceRef]*vendors.Instance{listed.Ref: full},
	}
	e := newEnv(t, drv, []client.Object{liveClaim("node-1"), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}, nil)

	sweep(t, e)
	want := map[string]string{v1alpha1.AnnotationPublicIPv4: "203.0.113.9"}
	if n, c := publicIPs(t, e); !maps.Equal(n, want) || !maps.Equal(c, want) {
		t.Fatalf("node %v, claim %v; want %v from GetInstance", n, c, want)
	}
	// Once published, the listing is enough: no further call, and nothing is removed.
	gets := drv.gets
	sweep(t, e)
	if drv.gets != gets {
		t.Errorf("%d GetInstance calls after the annotations were set", drv.gets-gets)
	}
	if n, _ := publicIPs(t, e); !maps.Equal(n, want) {
		t.Errorf("node %v; want the annotation kept", n)
	}
}

// patchCounter counts the patches of Nodes and NodeClaims and fails those of
// NodeClaims while failClaims is set.
type patchCounter struct {
	nodes, claims int
	failClaims    bool
}

func (p *patchCounter) funcs() interceptor.Funcs {
	return interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object,
		patch client.Patch, opts ...client.PatchOption) error {
		switch obj.(type) {
		case *corev1.Node:
			p.nodes++
		case *karpv1.NodeClaim:
			p.claims++
			if p.failClaims {
				return errors.New("the API server is down")
			}
		}
		return c.Patch(ctx, obj, patch, opts...)
	}}
}

// patchedEnv is an env whose client (the one the test reads too) counts patches.
func patchedEnv(t *testing.T, drv *stubDriver, p *patchCounter) *env {
	t.Helper()
	kube := interceptor.NewClient(fake.NewClientBuilder().WithScheme(scheme(t)).
		WithObjects(liveClaim("node-1"), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}).Build(), p.funcs())
	e := newEnv(t, drv, nil, func(o *gc.Options) { o.Client = kube })
	e.kube = kube
	return e
}

func TestPublicIPNoPatchWhenNothingChanged(t *testing.T) {
	drv := &stubDriver{vendor: "aws", instances: map[string][]*vendors.Instance{"r1": {publicInstance("203.0.113.9", "")}}}
	p := &patchCounter{}
	e := patchedEnv(t, drv, p)
	sweep(t, e)
	if p.nodes != 1 || p.claims != 1 {
		t.Fatalf("%d node and %d claim patches on the first sweep, want one each", p.nodes, p.claims)
	}
	sweep(t, e)
	if p.nodes != 1 || p.claims != 1 {
		t.Errorf("%d node and %d claim patches after an unchanged sweep, want no more", p.nodes, p.claims)
	}
}

func TestPublicIPUnknownKeepsTheAnnotations(t *testing.T) {
	i := publicInstance("203.0.113.9", "")
	drv := &stubDriver{vendor: "azure", instances: map[string][]*vendors.Instance{"r1": {i}}}
	e := newEnv(t, drv, []client.Object{liveClaim("node-1"), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}, nil)
	sweep(t, e)
	// A throttled read of the public IP: the instance says nothing about its addresses.
	i.PublicIPv4, i.PublicUnknown = "", true
	drv.byRef = map[vendors.InstanceRef]*vendors.Instance{i.Ref: i} // the read fails again
	sweep(t, e)
	want := map[string]string{v1alpha1.AnnotationPublicIPv4: "203.0.113.9"}
	if n, c := publicIPs(t, e); !maps.Equal(n, want) || !maps.Equal(c, want) {
		t.Fatalf("node %v, claim %v; want the annotations kept", n, c)
	}
}

func TestPublicIPPrivateOnlyIsReadOnce(t *testing.T) {
	listed := inst("i-1", "r1", "live", 10*time.Minute)
	listed.PublicUnknown = true
	read := publicInstance("", "") // no public address
	drv := &stubDriver{
		vendor: "oci", instances: map[string][]*vendors.Instance{"r1": {listed}},
		byRef: map[vendors.InstanceRef]*vendors.Instance{listed.Ref: read},
	}
	e := newEnv(t, drv, []client.Object{liveClaim("node-1"), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}, nil)
	sweep(t, e)
	sweep(t, e)
	sweep(t, e)
	if drv.gets != 1 {
		t.Fatalf("%d GetInstance calls over three sweeps, want 1", drv.gets)
	}
	// Once the instance is gone from the listing, a new instance under the
	// same reference is read again.
	drv.instances["r1"] = nil
	sweep(t, e)
	drv.instances["r1"] = []*vendors.Instance{listed}
	sweep(t, e)
	if drv.gets != 2 {
		t.Errorf("%d GetInstance calls after the instance left and came back, want 2", drv.gets)
	}
}

func TestPublicIPClaimPatchFailsAfterTheNodePatch(t *testing.T) {
	drv := &stubDriver{vendor: "aws", instances: map[string][]*vendors.Instance{"r1": {publicInstance("203.0.113.9", "")}}}
	p := &patchCounter{failClaims: true}
	e := patchedEnv(t, drv, p)
	if _, err := e.c.Sweep(context.Background()); err == nil {
		t.Fatal("a failed NodeClaim patch is not reported")
	}
	want := map[string]string{v1alpha1.AnnotationPublicIPv4: "203.0.113.9"}
	if n, c := publicIPs(t, e); !maps.Equal(n, want) || len(c) != 0 {
		t.Fatalf("node %v, claim %v; want the Node annotated and the NodeClaim not", n, c)
	}
	p.failClaims = false
	sweep(t, e)
	if n, c := publicIPs(t, e); !maps.Equal(n, want) || !maps.Equal(c, want) {
		t.Fatalf("node %v, claim %v; want both after the retry", n, c)
	}
	if p.nodes != 1 {
		t.Errorf("%d Node patches, want the retry to touch only the NodeClaim", p.nodes)
	}
}
