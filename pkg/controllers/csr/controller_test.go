/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package csr_test

import (
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	certv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	awsapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/controllers/csr"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

var (
	envOnce sync.Once
	envCfg  *rest.Config
	envStop func() error
	envErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if envStop != nil {
		_ = envStop()
	}
	os.Exit(code)
}

type recorder struct {
	mu   sync.Mutex
	list []events.Event
}

func (r *recorder) Publish(evts ...events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.list = append(r.list, evts...)
}

type env struct {
	t     *testing.T
	cfg   *rest.Config
	c     client.Client
	clock *clocktesting.FakeClock
	ctrl  *csr.Controller
	rec   *recorder
}

// seq makes names unique across the tests, which share one API server.
var seq atomic.Int64

func newEnv(t *testing.T) *env {
	t.Helper()
	envOnce.Do(func() {
		envCfg, envStop, envErr = testutil.StartEnvWith(testutil.EnvOptions{CRDs: testutil.KarpenterCRDs()})
	})
	if envErr != nil {
		t.Fatal(envErr)
	}
	// Core registers its types in client-go's global scheme.
	scheme := clientgoscheme.Scheme
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(envCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, cfg: envCfg, c: c, clock: clocktesting.NewFakeClock(time.Now()), rec: &recorder{}}
	e.ctrl = csr.New(csr.Options{Client: c, Recorder: e.rec, Clock: e.clock, Grace: 5 * time.Minute})
	return e
}

// node creates a NodeClaim of a SCNodeClass and (when addresses is not nil) the
// Node of it, with the unique name it returns.
type nodeOpts struct {
	providerID     string // the NodeClaim's
	nodeProviderID string // the Node's; defaults to providerID
	noNode         bool
	class          string // kind of the NodeClaim's class; default SCNodeClass
	group          string
	addresses      []corev1.NodeAddress
	instanceRef    string // the NodeClaim's instance-ref annotation
	className      string // the SCNodeClass of the NodeClaim; default "default"
}

// class creates the SCNodeClass with the network template, once.
func (e *env) class(name string, template v1alpha1.NetworkTemplateName) {
	e.t.Helper()
	nc := &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.SCNodeClassSpec{
			Flatcar: v1alpha1.FlatcarSpec{Version: "4757.2.3"}, Kubernetes: v1alpha1.KubernetesSpec{Version: "v1.37.1"},
			Network: v1alpha1.NetworkSpec{Template: template}, Vendors: v1alpha1.VendorBlocks{AWS: &awsapi.Settings{}},
		},
	}
	switch template {
	case v1alpha1.NetworkTemplateTailscale:
		nc.Spec.Network.Tailscale = &v1alpha1.TailscaleSpec{
			Version:          "v1.80.0",
			AuthKeySecretRef: v1alpha1.SecretKeyRef{Name: "ts", Namespace: "default", Key: "key"},
		}
	case v1alpha1.NetworkTemplateCustom:
		nc.Spec.Network.Custom = &v1alpha1.CustomNetworkSpec{
			ConfigMapRef: v1alpha1.ConfigMapKeyRef{Name: "tpl", Namespace: "default", Key: "key"},
		}
	}
	if err := e.c.Create(e.t.Context(), nc); err != nil && !apierrors.IsAlreadyExists(err) {
		e.t.Fatal(err)
	}
}

func (e *env) node(o nodeOpts) string {
	e.t.Helper()
	n := seq.Add(1)
	name := fmt.Sprintf("node-%d", n)
	if o.class == "" {
		o.class = "SCNodeClass"
	}
	if o.group == "" {
		o.group = v1alpha1.Group
	}
	claim := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: karpv1.NodeClaimSpec{
			NodeClassRef: &karpv1.NodeClassReference{Group: o.group, Kind: o.class, Name: cmp.Or(o.className, "default")},
			Requirements: []karpv1.NodeSelectorRequirementWithMinValues{
				{Key: "kubernetes.io/arch", Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64"}},
			},
		},
	}
	if o.instanceRef != "" {
		claim.Annotations = map[string]string{v1alpha1.AnnotationInstanceRef: o.instanceRef}
	}
	if err := e.c.Create(e.t.Context(), claim); err != nil {
		e.t.Fatal(err)
	}
	if o.providerID != "" {
		claim.Status.ProviderID = o.providerID
		if err := e.c.Status().Update(e.t.Context(), claim); err != nil {
			e.t.Fatal(err)
		}
	}
	if o.noNode {
		return name
	}
	if o.nodeProviderID == "" {
		o.nodeProviderID = o.providerID
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.NodeSpec{ProviderID: o.nodeProviderID}}
	if err := e.c.Create(e.t.Context(), node); err != nil {
		e.t.Fatal(err)
	}
	node.Status.Addresses = o.addresses
	if err := e.c.Status().Update(e.t.Context(), node); err != nil {
		e.t.Fatal(err)
	}
	return name
}

type csrOpts struct {
	user   string
	groups []string
	signer string
	cn     string
	org    []string
	dns    []string
	ips    []string
	emails []string
	usages []certv1.KeyUsage
}

// servingCSR returns a request the way a kubelet makes it for the node, with
// what the options change.
func (e *env) csr(name string, o csrOpts) string {
	e.t.Helper()
	n := seq.Add(1)
	if o.user == "" {
		o.user = "system:node:" + name
	}
	if o.groups == nil {
		o.groups = []string{"system:nodes", "system:authenticated"}
	}
	if o.signer == "" {
		o.signer = certv1.KubeletServingSignerName
	}
	if o.cn == "" {
		o.cn = "system:node:" + name
	}
	if o.org == nil {
		o.org = []string{"system:nodes"}
	}
	if o.usages == nil {
		o.usages = []certv1.KeyUsage{certv1.UsageDigitalSignature, certv1.UsageServerAuth}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		e.t.Fatal(err)
	}
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: o.cn, Organization: o.org}, DNSNames: o.dns, EmailAddresses: o.emails}
	for _, ip := range o.ips {
		tmpl.IPAddresses = append(tmpl.IPAddresses, net.ParseIP(ip))
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		e.t.Fatal(err)
	}
	request := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})

	// The API server fills in the requester from the authenticated user: act as
	// the node. system:masters keeps the impersonated user authorised without
	// RBAC objects.
	cfg := rest.CopyConfig(e.cfg)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: o.user, Groups: append(append([]string{}, o.groups...), "system:masters")}
	asNode, err := client.New(cfg, client.Options{Scheme: e.c.Scheme()})
	if err != nil {
		e.t.Fatal(err)
	}
	r := &certv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("csr-%s-%d", name, n)},
		Spec:       certv1.CertificateSigningRequestSpec{Request: request, SignerName: o.signer, Usages: o.usages},
	}
	if err := asNode.Create(e.t.Context(), r); err != nil {
		e.t.Fatal(err)
	}
	return r.Name
}

func (e *env) reconcile(name string) reconcile.Result {
	e.t.Helper()
	res, err := e.ctrl.Reconcile(e.t.Context(), reconcile.Request{NamespacedName: client.ObjectKey{Name: name}})
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

// outcome is approved, denied or untouched.
func (e *env) outcome(name string) string {
	e.t.Helper()
	r := &certv1.CertificateSigningRequest{}
	if err := e.c.Get(e.t.Context(), client.ObjectKey{Name: name}, r); err != nil {
		e.t.Fatal(err)
	}
	for _, c := range r.Status.Conditions {
		switch c.Type {
		case certv1.CertificateApproved:
			return "approved"
		case certv1.CertificateDenied:
			return "denied"
		}
	}
	return "untouched"
}

var goodAddrs = []corev1.NodeAddress{
	{Type: corev1.NodeInternalIP, Address: "10.1.2.3"},
	{Type: corev1.NodeHostName, Address: "ip-10-1-2-3.example.internal"},
	{Type: corev1.NodeInternalDNS, Address: "ip-10-1-2-3.eu.example.internal"},
	{Type: corev1.NodeExternalIP, Address: "203.0.113.7"},
	{Type: corev1.NodeInternalIP, Address: "2001:db8::1"},
}

const goodProviderID = "scarpenter://aws/us-east-1/"

func TestServingCertificateRules(t *testing.T) {
	e := newEnv(t)
	good := func(name string) csrOpts {
		return csrOpts{dns: []string{name, "ip-10-1-2-3.example.internal"}, ips: []string{"10.1.2.3", "203.0.113.7"}}
	}
	tests := []struct {
		name string
		node func(e *env) string
		csr  func(name string) csrOpts
		// after is a time to move the clock to (past the grace period) before
		// reconciling, 0 for none.
		after time.Duration
		want  string
	}{
		{name: "everything right", want: "approved"},
		{name: "an IPv6 SAN written differently from the Node's address", want: "approved", csr: func(n string) csrOpts {
			return csrOpts{dns: []string{n}, ips: []string{"10.1.2.3", "2001:db8:0:0:0:0:0:1"}}
		}},
		{name: "another IPv6 address, after the grace period", want: "denied", after: time.Hour, csr: func(n string) csrOpts {
			return csrOpts{dns: []string{n}, ips: []string{"2001:db8::2"}}
		}},
		{name: "wrong requester: a user, not a node", want: "untouched", csr: func(n string) csrOpts {
			o := good(n)
			o.user = "alice"
			return o
		}},
		{name: "wrong requester: a node without the nodes group", want: "untouched", csr: func(n string) csrOpts {
			o := good(n)
			o.groups = []string{"system:authenticated"}
			return o
		}},
		{name: "another node asks for this node's certificate", want: "untouched", csr: func(n string) csrOpts {
			o := good(n)
			o.user = "system:node:some-other-node"
			return o
		}},
		{name: "unknown node: no NodeClaim", want: "untouched", node: func(e *env) string { return fmt.Sprintf("no-such-nodeclaim-%d", seq.Add(1)) }},
		{name: "NodeClaim of another provider", want: "untouched", node: func(e *env) string {
			return e.node(nodeOpts{providerID: goodProviderID + "x", addresses: goodAddrs, class: "EC2NodeClass", group: "karpenter.k8s.aws"})
		}},
		{name: "provider ID mismatch", want: "denied", node: func(e *env) string {
			return e.node(nodeOpts{providerID: goodProviderID + "x", nodeProviderID: "scarpenter://aws/us-east-1/impostor", addresses: goodAddrs})
		}},
		{name: "node without a provider ID", want: "denied", node: func(e *env) string {
			return e.node(nodeOpts{providerID: goodProviderID + "x", nodeProviderID: "-", addresses: goodAddrs})
		}},
		{name: "extra DNS SAN, still within the grace period", want: "untouched", csr: func(n string) csrOpts {
			o := good(n)
			o.dns = append(o.dns, "kubernetes.default.svc")
			return o
		}},
		{name: "extra DNS SAN, after the grace period", want: "denied", after: time.Hour, csr: func(n string) csrOpts {
			o := good(n)
			o.dns = append(o.dns, "kubernetes.default.svc")
			return o
		}},
		{name: "IP SAN not among the addresses, after the grace period", want: "denied", after: time.Hour, csr: func(n string) csrOpts {
			o := good(n)
			o.ips = append(o.ips, "10.9.9.9")
			return o
		}},
		{name: "IP SAN not among the addresses, within the grace period", want: "untouched", csr: func(n string) csrOpts {
			o := good(n)
			o.ips = append(o.ips, "10.9.9.9")
			return o
		}},
		{name: "a DNS SAN that is an address of another type", want: "denied", after: time.Hour, csr: func(n string) csrOpts {
			o := good(n)
			o.dns = append(o.dns, "10.1.2.3")
			return o
		}},
		{name: "non-serving signer", want: "untouched", csr: func(n string) csrOpts {
			o := good(n)
			o.signer = certv1.KubeAPIServerClientKubeletSignerName
			o.usages = []certv1.KeyUsage{certv1.UsageDigitalSignature, certv1.UsageClientAuth}
			return o
		}},
		{name: "custom signer", want: "untouched", csr: func(n string) csrOpts {
			o := good(n)
			o.signer = "example.com/custom"
			return o
		}},
		{name: "subject of another node", want: "denied", csr: func(n string) csrOpts {
			o := good(n)
			o.cn = "system:node:other"
			return o
		}},
		{name: "wrong organization", want: "denied", csr: func(n string) csrOpts {
			o := good(n)
			o.org = []string{"system:masters"}
			return o
		}},
		{name: "e-mail SAN", want: "denied", csr: func(n string) csrOpts {
			o := good(n)
			o.emails = []string{"admin@example.com"}
			return o
		}},
		{name: "client auth usage", want: "denied", csr: func(n string) csrOpts {
			o := good(n)
			o.usages = []certv1.KeyUsage{certv1.UsageDigitalSignature, certv1.UsageServerAuth, certv1.UsageClientAuth}
			return o
		}},
		{name: "no server auth usage", want: "denied", csr: func(n string) csrOpts {
			o := good(n)
			o.usages = []certv1.KeyUsage{certv1.UsageDigitalSignature}
			return o
		}},
		{name: "the Node has not registered", want: "untouched", node: func(e *env) string {
			return e.node(nodeOpts{providerID: goodProviderID + "x", noNode: true})
		}},
		{name: "the NodeClaim has no provider ID yet", want: "untouched", node: func(e *env) string {
			return e.node(nodeOpts{addresses: goodAddrs, nodeProviderID: goodProviderID + "x"})
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e.t = t
			var name string
			if tc.node != nil {
				name = tc.node(e)
			} else {
				name = e.node(nodeOpts{providerID: goodProviderID + "x", addresses: goodAddrs})
			}
			// The provider ID must be the Node's own, per NodeClaim.
			if tc.csr == nil {
				tc.csr = good
			}
			csrName := e.csr(name, tc.csr(name))
			e.clock.SetTime(time.Now())
			if tc.after > 0 {
				e.clock.SetTime(time.Now().Add(tc.after))
			}
			e.reconcile(csrName)
			if got := e.outcome(csrName); got != tc.want {
				t.Errorf("outcome = %s, want %s", got, tc.want)
			}
			if tc.want == "approved" {
				// Approving twice is a no-op.
				e.reconcile(csrName)
			}
		})
	}
}

// A request that has to wait is looked at again, and decided once the Node
// reports what it asks for.
func TestWaitsForTheNodeAddresses(t *testing.T) {
	e := newEnv(t)
	name := e.node(nodeOpts{providerID: goodProviderID + "w", addresses: nil})
	csrName := e.csr(name, csrOpts{dns: []string{name}, ips: []string{"10.1.2.3"}})
	e.clock.SetTime(time.Now())
	if res := e.reconcile(csrName); res.RequeueAfter <= 0 || e.outcome(csrName) != "untouched" {
		t.Fatalf("result = %+v, outcome %s: want a requeue and no decision", res, e.outcome(csrName))
	}
	node := &corev1.Node{}
	if err := e.c.Get(t.Context(), client.ObjectKey{Name: name}, node); err != nil {
		t.Fatal(err)
	}
	node.Status.Addresses = goodAddrs
	if err := e.c.Status().Update(t.Context(), node); err != nil {
		t.Fatal(err)
	}
	e.reconcile(csrName)
	if got := e.outcome(csrName); got != "approved" {
		t.Errorf("outcome = %s after the Node reported its addresses", got)
	}
}

// What another approver decided is not touched, and a denied request is not
// approved afterwards.
func TestDecidedRequestsAreLeftAlone(t *testing.T) {
	e := newEnv(t)
	name := e.node(nodeOpts{providerID: goodProviderID + "d", addresses: goodAddrs})
	csrName := e.csr(name, csrOpts{dns: []string{name}, ips: []string{"10.1.2.3"}})
	r := &certv1.CertificateSigningRequest{}
	if err := e.c.Get(t.Context(), client.ObjectKey{Name: csrName}, r); err != nil {
		t.Fatal(err)
	}
	r.Status.Conditions = append(r.Status.Conditions, certv1.CertificateSigningRequestCondition{
		Type: certv1.CertificateDenied, Status: corev1.ConditionTrue, Reason: "ByAdmin", LastUpdateTime: metav1.Now()})
	if err := e.c.SubResource("approval").Update(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	e.reconcile(csrName)
	if got := e.outcome(csrName); got != "denied" {
		t.Errorf("outcome = %s, want the admin's denial to stand", got)
	}
}

func TestEventsAndConditionDetails(t *testing.T) {
	e := newEnv(t)
	name := e.node(nodeOpts{providerID: goodProviderID + "e", addresses: goodAddrs})
	ok := e.csr(name, csrOpts{dns: []string{name}, ips: []string{"10.1.2.3"}})
	bad := e.csr(name, csrOpts{dns: []string{name}, cn: "system:node:other"})
	e.reconcile(ok)
	e.reconcile(bad)

	r := &certv1.CertificateSigningRequest{}
	if err := e.c.Get(context.Background(), client.ObjectKey{Name: ok}, r); err != nil {
		t.Fatal(err)
	}
	if c := r.Status.Conditions[0]; c.Reason != csr.ReasonApproved || c.Status != corev1.ConditionTrue {
		t.Errorf("approval condition = %+v", c)
	}
	reasons := make([]string, 0, 2)
	e.rec.mu.Lock()
	for _, ev := range e.rec.list {
		reasons = append(reasons, ev.Reason+"/"+ev.Type)
	}
	e.rec.mu.Unlock()
	want := map[string]bool{csr.EventApproved + "/Normal": false, csr.EventDenied + "/Warning": false}
	for _, r := range reasons {
		if _, ok := want[r]; ok {
			want[r] = true
		}
	}
	for r, seen := range want {
		if !seen {
			t.Errorf("no event %s in %v", r, reasons)
		}
	}
}

// addrDriver is a driver that knows the addresses of its instances.
type addrDriver struct {
	vendors.Driver
	instances map[vendors.InstanceRef]*vendors.Instance
	err       error
}

func (d *addrDriver) Vendor() string { return "aws" }

func (d *addrDriver) GetInstance(_ context.Context, ref vendors.InstanceRef) (*vendors.Instance, error) {
	if d.err != nil {
		return nil, d.err
	}
	if inst, ok := d.instances[ref]; ok {
		return inst, nil
	}
	return nil, vendors.Errorf(vendors.NotFound, "no instance %s", ref)
}

// A leaked bootstrap token is not bound to a node name: the Node object, which the
// holder of the token writes, cannot vouch for an IP address. Where the driver
// reports the instance's addresses, the IP SANs are checked against them.
func TestIPSANsAreCheckedAgainstTheInstancesAddresses(t *testing.T) {
	e := newEnv(t)
	drv := &addrDriver{instances: map[vendors.InstanceRef]*vendors.Instance{
		"us-east-1/i-real":    {Addresses: []string{"10.1.2.3", "203.0.113.7"}},
		"us-east-1/i-noaddrs": {},
		"us-east-1/i-other":   {Addresses: []string{"10.7.7.7"}},
		"us-east-1/i-gone":    {Addresses: []string{"10.1.2.3"}, State: vendors.StateTerminated},
	}}
	e.ctrl = csr.New(csr.Options{Client: e.c, Recorder: e.rec, Clock: e.clock, Grace: 5 * time.Minute, Registry: vendors.NewRegistryFrom(drv)})
	e.class("ips-direct", v1alpha1.NetworkTemplateDirect)
	e.class("ips-tailscale", v1alpha1.NetworkTemplateTailscale)
	e.class("ips-custom", v1alpha1.NetworkTemplateCustom)
	for _, tc := range []struct {
		name  string
		node  nodeOpts
		ips   []string
		after time.Duration
		want  string
		err   error
	}{
		{name: "the addresses of the instance", node: nodeOpts{instanceRef: "us-east-1/i-real", addresses: goodAddrs}, ips: []string{"10.1.2.3", "203.0.113.7"}, want: "approved"},
		{
			// The attack: the Node (the attacker's) claims the victim's address; the
			// instance, which is the vendor's truth, is elsewhere.
			name: "an address only the Node claims, within the grace period", want: "untouched",
			node: nodeOpts{instanceRef: "us-east-1/i-other", addresses: goodAddrs}, ips: []string{"10.1.2.3"},
		},
		{
			name: "an address only the Node claims, after the grace period", want: "denied", after: time.Hour,
			node: nodeOpts{instanceRef: "us-east-1/i-other", addresses: goodAddrs}, ips: []string{"10.1.2.3"},
		},
		{name: "an address of the instance the Node does not report yet", want: "approved",
			node: nodeOpts{instanceRef: "us-east-1/i-real", addresses: goodAddrs[:1]}, ips: []string{"203.0.113.7"}},
		{name: "a request without IP SANs", want: "approved", node: nodeOpts{instanceRef: "us-east-1/i-other", addresses: goodAddrs}},
		{name: "a driver that reports no addresses falls back to the Node's", want: "approved",
			node: nodeOpts{instanceRef: "us-east-1/i-noaddrs", addresses: goodAddrs}, ips: []string{"10.1.2.3"}},
		{name: "a driver that reports no addresses: the Node's still bind", want: "denied", after: time.Hour,
			node: nodeOpts{instanceRef: "us-east-1/i-noaddrs", addresses: goodAddrs}, ips: []string{"10.9.9.9"}},
		{name: "a NodeClaim without an instance reference falls back to the Node's", want: "approved",
			node: nodeOpts{addresses: goodAddrs}, ips: []string{"10.1.2.3"}},
		{name: "an instance that is gone allows no address", want: "denied", after: time.Hour,
			node: nodeOpts{instanceRef: "us-east-1/i-gone", addresses: goodAddrs}, ips: []string{"10.1.2.3"}},
		{name: "a terminated instance still listing its addresses allows none", want: "denied", after: time.Hour,
			node: nodeOpts{instanceRef: "us-east-1/i-gone", addresses: goodAddrs}, ips: []string{"10.1.2.3"}},
		{
			// The default tailscale template starts the kubelet with the tailnet address
			// as node IP: the vendor cannot vouch for it, the Node's addresses stand.
			name: "tailscale: the tailnet IP the Node reports", want: "approved",
			node: nodeOpts{className: "ips-tailscale", instanceRef: "us-east-1/i-real", addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "100.64.1.2"}}},
			ips:  []string{"100.64.1.2"},
		},
		{
			name: "direct: the same IP is not an address of the instance", want: "denied", after: time.Hour,
			node: nodeOpts{instanceRef: "us-east-1/i-real", addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "100.64.1.2"}}},
			ips:  []string{"100.64.1.2"},
		},
		{name: "tailscale: an IP the Node does not report is still denied", want: "denied", after: time.Hour,
			node: nodeOpts{className: "ips-tailscale", instanceRef: "us-east-1/i-real", addresses: goodAddrs}, ips: []string{"100.64.9.9"}},
		{name: "custom: the Node's addresses stand", want: "approved",
			node: nodeOpts{className: "ips-custom", instanceRef: "us-east-1/i-other", addresses: goodAddrs}, ips: []string{"10.1.2.3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e.t = t
			tc.node.providerID = goodProviderID + "z"
			if tc.node.className == "" {
				tc.node.className = "ips-direct"
			}
			name := e.node(tc.node)
			csrName := e.csr(name, csrOpts{dns: []string{name}, ips: tc.ips})
			e.clock.SetTime(time.Now())
			if tc.after > 0 {
				e.clock.SetTime(time.Now().Add(tc.after))
			}
			e.reconcile(csrName)
			if got := e.outcome(csrName); got != tc.want {
				t.Errorf("outcome = %s, want %s", got, tc.want)
			}
		})
	}
	t.Run("a transient driver error is retried", func(t *testing.T) {
		e.t = t
		drv.err = vendors.Errorf(vendors.Retryable, "throttled")
		defer func() { drv.err = nil }()
		name := e.node(nodeOpts{providerID: goodProviderID + "t", className: "ips-direct", instanceRef: "us-east-1/i-real", addresses: goodAddrs})
		csrName := e.csr(name, csrOpts{dns: []string{name}, ips: []string{"10.1.2.3"}})
		if _, err := e.ctrl.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKey{Name: csrName}}); err == nil {
			t.Error("a failing driver did not fail the reconcile")
		}
		if got := e.outcome(csrName); got != "untouched" {
			t.Errorf("outcome = %s", got)
		}
	})
}
