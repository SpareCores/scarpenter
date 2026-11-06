/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package bootstrap_test

import (
	"context"
	"regexp"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/SpareCores/scarpenter/pkg/bootstrap"
)

type recorder struct {
	mu     sync.Mutex
	events []events.Event
}

func (r *recorder) Publish(evts ...events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, evts...)
}

func (r *recorder) all() []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]events.Event(nil), r.events...)
}

func nodeClaim(name string) *karpv1.NodeClaim {
	nc := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: name}}
	nc.Spec.NodeClassRef = &karpv1.NodeClassReference{Group: "scarpenter.sparecores.com", Kind: "SCNodeClass", Name: "default"}
	return nc
}

func newMinter(t *testing.T, ttl time.Duration) (*bootstrap.Minter, client.Client, *clocktesting.FakeClock, *recorder) {
	t.Helper()
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	clk := clocktesting.NewFakeClock(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	rec := &recorder{}
	return bootstrap.NewMinter(bootstrap.MinterOptions{Client: kube, TTL: ttl, Clock: clk, Recorder: rec}), kube, clk, rec
}

func tokens(t *testing.T, kube client.Client) []corev1.Secret {
	t.Helper()
	var list corev1.SecretList
	if err := kube.List(context.Background(), &list, client.InNamespace("kube-system")); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

func TestMintCreatesTheSecret(t *testing.T) {
	m, kube, clk, _ := newMinter(t, time.Hour)
	token, err := m.Mint(context.Background(), nodeClaim("default-abc12"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[a-z0-9]{6}\.[a-z0-9]{16}$`).MatchString(token) {
		t.Fatalf("token %q is not <6 chars>.<16 chars> of [a-z0-9]", token)
	}
	secrets := tokens(t, kube)
	if len(secrets) != 1 {
		t.Fatalf("%d Secrets, want 1", len(secrets))
	}
	s := secrets[0]
	id := token[:6]
	if s.Name != "bootstrap-token-"+id || s.Namespace != "kube-system" || s.Type != "bootstrap.kubernetes.io/token" {
		t.Errorf("Secret %s/%s type %s", s.Namespace, s.Name, s.Type)
	}
	if s.Labels["scarpenter.sparecores.com/nodeclaim"] != "default-abc12" || s.Labels["scarpenter.sparecores.com/nodeclass"] != "default" {
		t.Errorf("labels = %v", s.Labels)
	}
	want := map[string]string{
		"token-id": id, "token-secret": token[7:], "usage-bootstrap-authentication": "true",
		"auth-extra-groups": "system:bootstrappers:scarpenter",
		"expiration":        clk.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}
	for k, v := range want {
		if got := string(s.Data[k]); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

// A retried Create gets the token of the first attempt: the instance may exist
// and have booted with it.
func TestMintIsIdempotent(t *testing.T) {
	m, kube, clk, _ := newMinter(t, time.Hour)
	first, err := m.Mint(context.Background(), nodeClaim("a"))
	if err != nil {
		t.Fatal(err)
	}
	clk.Step(40 * time.Minute)
	second, err := m.Mint(context.Background(), nodeClaim("a"))
	if err != nil || second != first {
		t.Fatalf("second Mint = %q, %v; want %q", second, err, first)
	}
	if n := len(tokens(t, kube)); n != 1 {
		t.Errorf("%d Secrets after two Mints, want 1", n)
	}
	other, err := m.Mint(context.Background(), nodeClaim("b"))
	if err != nil || other == first {
		t.Fatalf("another NodeClaim got %q, %v", other, err)
	}
	if n := len(tokens(t, kube)); n != 2 {
		t.Errorf("%d Secrets for two NodeClaims, want 2", n)
	}
}

func TestMintReplacesAnExpiredToken(t *testing.T) {
	m, kube, clk, _ := newMinter(t, time.Hour)
	first, err := m.Mint(context.Background(), nodeClaim("a"))
	if err != nil {
		t.Fatal(err)
	}
	clk.Step(61 * time.Minute)
	second, err := m.Mint(context.Background(), nodeClaim("a"))
	if err != nil || second == first {
		t.Fatalf("Mint after expiry = %q, %v; want a new token", second, err)
	}
	if n := len(tokens(t, kube)); n != 1 {
		t.Errorf("%d Secrets, want the expired one replaced", n)
	}
}

func TestDeleteAndEvent(t *testing.T) {
	m, kube, _, rec := newMinter(t, time.Hour)
	if _, err := m.Mint(context.Background(), nodeClaim("a")); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(context.Background(), "missing"); err != nil {
		t.Fatalf("deleting a missing token: %v", err)
	}
	deleted, err := m.DeleteFor(context.Background(), "a", bootstrap.ReasonRegistered, nodeClaim("a"))
	if err != nil || !deleted {
		t.Fatalf("DeleteFor = %v, %v", deleted, err)
	}
	if n := len(tokens(t, kube)); n != 0 {
		t.Errorf("%d Secrets left", n)
	}
	got := rec.all()
	if len(got) != 1 || got[0].Reason != bootstrap.EventTokenDeleted || got[0].Type != corev1.EventTypeNormal {
		t.Fatalf("events = %+v", got)
	}
	if nc, ok := got[0].InvolvedObject.(*karpv1.NodeClaim); !ok || nc.Name != "a" {
		t.Errorf("the Event is on %v, want the NodeClaim a", got[0].InvolvedObject)
	}
	// Gone NodeClaim: the Event still names it. A second delete is silent.
	if _, err := m.Mint(context.Background(), nodeClaim("b")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.DeleteFor(context.Background(), "b", bootstrap.ReasonNodeClaimGone, nil); err != nil {
		t.Fatal(err)
	}
	if deleted, _ := m.DeleteFor(context.Background(), "b", bootstrap.ReasonNodeClaimGone, nil); deleted {
		t.Error("a second delete reported a deletion")
	}
	if n := len(rec.all()); n != 2 {
		t.Errorf("%d events, want 2", n)
	}
	// The plain Delete of the CloudProvider publishes nothing.
	if _, err := m.Mint(context.Background(), nodeClaim("c")); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(context.Background(), "c"); err != nil {
		t.Fatal(err)
	}
	if n := len(rec.all()); n != 2 {
		t.Errorf("Delete published an event (%d events)", n)
	}
}

// A Secret of another type with the label is not ours to touch.
func TestOtherSecretsAreIgnored(t *testing.T) {
	m, kube, _, _ := newMinter(t, time.Hour)
	other := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "unrelated", Namespace: "kube-system", Labels: map[string]string{"scarpenter.sparecores.com/nodeclaim": "a"}}}
	if err := kube.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Mint(context.Background(), nodeClaim("a")); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(other), &corev1.Secret{}); err != nil {
		t.Errorf("the unrelated Secret is gone: %v", err)
	}
}
