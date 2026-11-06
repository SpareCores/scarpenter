/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package csr approves the kubelet serving certificate requests of SCarpenter's
// nodes (overview section 8.5 step 3). The built-in approver of the
// controller manager only handles client certificates; without a serving
// certificate, kubectl logs and exec fail on control planes that verify the
// kubelet's certificate (k3s). The rules are strict, because an approved
// serving certificate lets its holder impersonate the node's kubelet endpoint:
// a request is approved only when it comes from the node itself, for a node
// that is a NodeClaim of a SCarpenter NodeClass whose Node reports the
// NodeClaim's provider ID, and every name and address it asks for is one the
// Node reports. Anything else is left to other approvers (not ours) or denied
// (ours, but wrong).
package csr

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	certv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
)

// Names of the signer, the identities and the reasons.
const (
	// SignerName is the only signer this controller handles.
	SignerName = certv1.KubeletServingSignerName
	// NodeUserPrefix and NodesGroup identify a kubelet.
	NodeUserPrefix = "system:node:"
	NodesGroup     = "system:nodes"

	ReasonApproved = "ScarpenterApproved"
	ReasonDenied   = "ScarpenterDenied"
	// Event reasons.
	EventApproved = "ServingCertificateApproved"
	EventDenied   = "ServingCertificateDenied"

	// DefaultGrace is how long a request of one of our nodes may wait for its
	// Node to appear and report the addresses the request names (the kubelet
	// creates the request right after it registers); after it the request is
	// denied (addresses that do not match) or left alone (no Node).
	DefaultGrace = 5 * time.Minute
	retryAfter   = 5 * time.Second
)

// Options configures the controller.
type Options struct {
	Client   client.Client
	Recorder events.Recorder
	Clock    clock.Clock
	// Grace defaults to DefaultGrace.
	Grace time.Duration
}

// Controller approves the serving certificate requests of SCarpenter's nodes.
type Controller struct {
	opts Options
}

// New returns the controller; Register adds it to a manager.
func New(opts Options) *Controller {
	if opts.Clock == nil {
		opts.Clock = clock.RealClock{}
	}
	if opts.Grace <= 0 {
		opts.Grace = DefaultGrace
	}
	return &Controller{opts: opts}
}

// Register watches the certificate signing requests with the manager.
func (c *Controller) Register(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("csr").
		// The tests start several managers in one process; controller-runtime
		// keeps the names process-wide.
		WithOptions(controller.Options{SkipNameValidation: new(true)}).
		For(&certv1.CertificateSigningRequest{}).Complete(c)
}

// verdict is the decision about a request.
type verdict int

const (
	ignore  verdict = iota // not ours, or nothing to decide yet
	approve                // every rule holds
	deny                   // ours, and wrong
	wait                   // ours, the Node does not tell enough yet
)

// Reconcile decides one request.
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	r := &certv1.CertificateSigningRequest{}
	if err := c.opts.Client.Get(ctx, req.NamespacedName, r); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if r.Spec.SignerName != SignerName || decided(r) || len(r.Status.Certificate) > 0 || !r.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}
	v, why, err := c.decide(ctx, r)
	if err != nil {
		return reconcile.Result{}, err
	}
	logger := log.FromContext(ctx).WithValues("csr", r.Name, "user", r.Spec.Username)
	switch v {
	case approve:
		logger.Info("approving the kubelet serving certificate request")
		return reconcile.Result{}, c.decideCSR(ctx, r, certv1.CertificateApproved, ReasonApproved, why,
			EventApproved, corev1.EventTypeNormal)
	case deny:
		logger.Info("denying the kubelet serving certificate request", "reason", why)
		return reconcile.Result{}, c.decideCSR(ctx, r, certv1.CertificateDenied, ReasonDenied, why,
			EventDenied, corev1.EventTypeWarning)
	case wait:
		if c.opts.Clock.Since(r.CreationTimestamp.Time) > c.opts.Grace {
			logger.Info("giving up on a kubelet serving certificate request, left undecided", "reason", why)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{RequeueAfter: retryAfter}, nil
	}
	return reconcile.Result{}, nil
}

func decided(r *certv1.CertificateSigningRequest) bool {
	for _, cond := range r.Status.Conditions {
		switch cond.Type {
		case certv1.CertificateApproved, certv1.CertificateDenied, certv1.CertificateFailed:
			return true
		}
	}
	return false
}

// decide applies the rules. Order matters: what makes a request not ours
// returns ignore before anything is denied.
func (c *Controller) decide(ctx context.Context, r *certv1.CertificateSigningRequest) (verdict, string, error) {
	// The requester is a kubelet: user system:node:<name>, group system:nodes.
	name, ok := strings.CutPrefix(r.Spec.Username, NodeUserPrefix)
	if !ok || name == "" || !slices.Contains(r.Spec.Groups, NodesGroup) {
		return ignore, "the requester is not a node", nil
	}
	// The node is a NodeClaim of a SCNodeClass.
	claim := &karpv1.NodeClaim{}
	if err := c.opts.Client.Get(ctx, types.NamespacedName{Name: name}, claim); err != nil {
		if apierrors.IsNotFound(err) {
			return ignore, "no NodeClaim has the name of the node", nil
		}
		return ignore, "", fmt.Errorf("reading the NodeClaim %s: %w", name, err)
	}
	if ref := claim.Spec.NodeClassRef; ref == nil || ref.Group != v1alpha1.Group || ref.Kind != "SCNodeClass" {
		return ignore, "the NodeClaim is not of a SCNodeClass", nil
	}

	// From here on the request is ours: what the request itself says must be
	// right.
	req, err := parseRequest(r.Spec.Request)
	if err != nil {
		return deny, fmt.Sprintf("the request does not parse: %v", err), nil
	}
	if why := checkRequest(r, req, name); why != "" {
		return deny, why, nil
	}

	// And what the Node says must agree.
	node := &corev1.Node{}
	if err := c.opts.Client.Get(ctx, types.NamespacedName{Name: name}, node); err != nil {
		if apierrors.IsNotFound(err) {
			return wait, "the Node has not registered yet", nil
		}
		return ignore, "", fmt.Errorf("reading the Node %s: %w", name, err)
	}
	want := claim.Status.ProviderID
	switch {
	case want == "":
		return wait, "the NodeClaim has no provider ID yet", nil
	case node.Spec.ProviderID != want:
		return deny, fmt.Sprintf("the Node's provider ID %q is not the NodeClaim's %q", node.Spec.ProviderID, want), nil
	}
	if extra := unknownNames(req, name, node); len(extra) > 0 {
		why := fmt.Sprintf("the request names %s, which the Node does not report as its own", strings.Join(extra, ", "))
		if c.opts.Clock.Since(r.CreationTimestamp.Time) <= c.opts.Grace {
			// The Node's addresses may still be on their way.
			return wait, why, nil
		}
		return deny, why, nil
	}
	return approve, "the kubelet of a NodeClaim of a SCNodeClass asked for names its Node reports", nil
}

// parseRequest decodes the PEM PKCS#10 request and checks its signature (proof
// that the requester holds the key).
func parseRequest(pemBytes []byte) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("not a PEM CERTIFICATE REQUEST")
	}
	req, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, err
	}
	if err := req.CheckSignature(); err != nil {
		return nil, fmt.Errorf("bad signature: %w", err)
	}
	return req, nil
}

// checkRequest returns what is wrong with the request itself, "" when nothing:
// the subject is the node, the usages are those of a serving certificate and
// there are no other kinds of names.
func checkRequest(r *certv1.CertificateSigningRequest, req *x509.CertificateRequest, node string) string {
	switch {
	case req.Subject.CommonName != NodeUserPrefix+node:
		return fmt.Sprintf("the subject %q is not %q", req.Subject.CommonName, NodeUserPrefix+node)
	case len(req.Subject.Organization) != 1 || req.Subject.Organization[0] != NodesGroup:
		return fmt.Sprintf("the organization %v is not [%s]", req.Subject.Organization, NodesGroup)
	case len(req.EmailAddresses) > 0:
		return "the request has e-mail SANs"
	case len(req.URIs) > 0:
		return "the request has URI SANs"
	case len(r.Spec.Usages) == 0:
		return "the request has no usages"
	}
	// A serving certificate signs (digital signature) and, with an RSA key,
	// encrypts keys; it authenticates a server.
	allowed := []certv1.KeyUsage{certv1.UsageDigitalSignature, certv1.UsageKeyEncipherment, certv1.UsageServerAuth}
	for _, u := range r.Spec.Usages {
		if !slices.Contains(allowed, u) {
			return fmt.Sprintf("the usage %q is not one of a serving certificate", u)
		}
	}
	if !slices.Contains(r.Spec.Usages, certv1.UsageServerAuth) ||
		!slices.Contains(r.Spec.Usages, certv1.UsageDigitalSignature) {
		return "the request does not ask for the usages server auth and digital signature"
	}
	return ""
}

// unknownNames returns the names and addresses in the request that the Node
// does not report: DNS names are the node name or the Node's hostname and DNS
// addresses, IP addresses are the Node's IPs.
func unknownNames(req *x509.CertificateRequest, name string, node *corev1.Node) []string {
	dns := map[string]bool{name: true}
	ips := map[string]bool{}
	for _, a := range node.Status.Addresses {
		switch a.Type {
		case corev1.NodeHostName, corev1.NodeInternalDNS, corev1.NodeExternalDNS:
			dns[a.Address] = true
		case corev1.NodeInternalIP, corev1.NodeExternalIP:
			if ip := net.ParseIP(a.Address); ip != nil {
				ips[ip.String()] = true
			}
		}
	}
	var extra []string
	for _, d := range req.DNSNames {
		if !dns[d] {
			extra = append(extra, "DNS:"+d)
		}
	}
	for _, ip := range req.IPAddresses {
		if !ips[ip.String()] {
			extra = append(extra, "IP:"+ip.String())
		}
	}
	return extra
}

// decideCSR records the decision through the approval subresource.
func (c *Controller) decideCSR(
	ctx context.Context, r *certv1.CertificateSigningRequest, typ certv1.RequestConditionType,
	reason, message, event, eventType string,
) error {
	r.Status.Conditions = append(r.Status.Conditions, certv1.CertificateSigningRequestCondition{
		Type: typ, Status: corev1.ConditionTrue, Reason: reason, Message: truncate(message),
		LastUpdateTime: metav1.NewTime(c.opts.Clock.Now()),
	})
	if err := c.opts.Client.SubResource("approval").Update(ctx, r); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			// Gone, or decided by someone else in the meantime: look again.
			return nil
		}
		return fmt.Errorf("recording the decision on the request %s: %w", r.Name, err)
	}
	if c.opts.Recorder != nil {
		c.opts.Recorder.Publish(events.Event{
			InvolvedObject: r, Type: eventType, Reason: event, Message: truncate(message),
			DedupeValues: []string{r.Name, event},
		})
	}
	return nil
}

func truncate(s string) string {
	const max = 256
	if len(s) > max {
		return s[:max-3] + "..."
	}
	return s
}
