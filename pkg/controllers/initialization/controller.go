/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package initialization deletes the NodeClaims whose node registered but never
// became Initialized (Ready, startup taints gone, resources registered). Core
// has no deadline for that state: its liveness check stops at registration, and
// consolidation, emptiness and drift skip uninitialized nodes, so such a node
// would run, and be billed, until someone deletes it. The deadline is
// spec.launch.initializationTimeout of the NodeClass, counted from the creation
// of the Node.
//
// It does not fight the GPU pending controller: that one deletes a GPU node whose
// driver check has not passed 30 minutes after the node's creation, which is
// earlier than the default timeout of one hour. Both only delete NodeClaims, so a
// class that sets a shorter timeout simply wins the race.
package initialization

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	"github.com/SpareCores/scarpenter/pkg/scope"
)

// DefaultTimeout is the timeout of a class that does not set
// spec.launch.initializationTimeout.
const DefaultTimeout = time.Hour

// MaxRecheck caps the wait between two looks at a claim, so that a changed
// spec.launch.initializationTimeout (the NodeClass is not watched) applies within it.
const MaxRecheck = 10 * time.Minute

// EventTimeout is the Reason of the Warning Events on the NodeClaim and the NodeClass.
const EventTimeout = "InitializationTimeout"

const nodeClassKind = "SCNodeClass"

// Options configures the Controller.
type Options struct {
	Client   client.Client
	Recorder events.Recorder
	Clock    clock.Clock
	// Registerer receives the metric; nil registers none.
	Registerer prometheus.Registerer
}

// Controller deletes the NodeClaims that stay registered but uninitialized for too long.
type Controller struct {
	opts     Options
	timeouts *prometheus.CounterVec
}

// New returns the controller; Register adds it to a manager.
func New(o Options) *Controller {
	if o.Clock == nil {
		o.Clock = clock.RealClock{}
	}
	c := &Controller{opts: o, timeouts: prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "scarpenter_nodeclaims_initialization_timeout_total",
		Help: "NodeClaims deleted because their node did not become Initialized within the initialization timeout.",
	}, []string{"vendor", "nodeclass"})}
	if o.Registerer != nil {
		register(o.Registerer, c.timeouts)
	}
	return c
}

// register adds the collector; one of an earlier instance (a test that builds
// several) is replaced.
func register(reg prometheus.Registerer, col prometheus.Collector) {
	err := reg.Register(col)
	if already, ok := errors.AsType[prometheus.AlreadyRegisteredError](err); ok {
		reg.Unregister(already.ExistingCollector)
		err = reg.Register(col)
	}
	if err != nil {
		panic(fmt.Sprintf("registering the initialization metric: %v", err))
	}
}

// Register watches the NodeClaims with the manager; like every controller of the
// manager it runs on the leader only.
func (c *Controller) Register(mgr ctrl.Manager) error {
	err := ctrl.NewControllerManagedBy(mgr).Named("initialization-timeout").
		// The tests start several managers in one process; controller-runtime
		// keeps the names process-wide.
		WithOptions(controller.Options{SkipNameValidation: new(true)}).
		For(&karpv1.NodeClaim{}).Complete(c)
	if err != nil {
		return fmt.Errorf("registering the initialization timeout controller: %w", err)
	}
	return nil
}

// Reconcile decides one NodeClaim: it is deleted once its deadline has passed,
// otherwise it is looked at again at the deadline.
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	claim := &karpv1.NodeClaim{}
	if err := c.opts.Client.Get(ctx, req.NamespacedName, claim); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	ref := claim.Spec.NodeClassRef
	if ref == nil || ref.Group != v1alpha1.Group || ref.Kind != nodeClassKind || !claim.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}
	// Before registration core's liveness check has the deadline; after
	// initialization there is nothing to wait for.
	conds := claim.StatusConditions()
	if !conds.IsTrue(karpv1.ConditionTypeRegistered) || conds.IsTrue(karpv1.ConditionTypeInitialized) {
		return reconcile.Result{}, nil
	}
	// Core's reboot controller sets Initialized=Unknown while it reboots a node
	// that was initialized: that node is not stuck.
	if conds.IsTrue(karpv1.ConditionTypeRebooting) {
		return reconcile.Result{}, nil
	}
	vendor, _, _, err := scope.ParseProviderID(claim.Status.ProviderID)
	if err != nil {
		return reconcile.Result{}, nil //nolint:nilerr // not an instance of SCarpenter
	}
	class := &v1alpha1.SCNodeClass{}
	if err := c.opts.Client.Get(ctx, types.NamespacedName{Name: ref.Name}, class); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	timeout := DefaultTimeout
	if l := class.Spec.Launch; l != nil && l.InitializationTimeout != nil {
		timeout = l.InitializationTimeout.Duration
	}
	if timeout == 0 {
		return reconcile.Result{RequeueAfter: MaxRecheck}, nil
	}

	node, err := c.node(ctx, claim)
	if err != nil {
		return reconcile.Result{}, err
	}
	since, from := conds.Get(karpv1.ConditionTypeRegistered).LastTransitionTime.Time, "registration"
	if node != nil {
		since, from = node.CreationTimestamp.Time, "the node was created"
	}
	elapsed := c.opts.Clock.Since(since)
	if wait := timeout - elapsed; wait > 0 {
		return reconcile.Result{RequeueAfter: min(wait, MaxRecheck)}, nil
	}

	// A conflict is retried after a second. The precondition keeps a claim that changed since it was read (it may
	// have initialized) from being deleted on stale data.
	err = c.opts.Client.Delete(ctx, claim, client.Preconditions{ResourceVersion: &claim.ResourceVersion})
	switch {
	case apierrors.IsNotFound(err):
		return reconcile.Result{}, nil
	case apierrors.IsConflict(err):
		return reconcile.Result{RequeueAfter: time.Second}, nil
	case err != nil:
		return reconcile.Result{}, fmt.Errorf("deleting NodeClaim %s: %w", claim.Name, err)
	}
	message := fmt.Sprintf("the node did not become Initialized within %s (%s since %s)%s",
		timeout, elapsed.Round(time.Second), from, readyOf(node))
	log.FromContext(ctx).Info("deleted the NodeClaim of a node that never initialized",
		"nodeclaim", claim.Name, "node", claim.Status.NodeName, "nodeclass", class.Name, "reason", message)
	c.publish(claim, message)
	c.publish(class, fmt.Sprintf("NodeClaim %s: %s", claim.Name, message), claim.Name)
	c.timeouts.WithLabelValues(vendor, class.Name).Inc()
	return reconcile.Result{}, nil
}

// node returns the Node of the claim; nil when it is not there (yet).
func (c *Controller) node(ctx context.Context, claim *karpv1.NodeClaim) (*corev1.Node, error) {
	if claim.Status.NodeName == "" {
		return nil, nil
	}
	node := &corev1.Node{}
	err := c.opts.Client.Get(ctx, types.NamespacedName{Name: claim.Status.NodeName}, node)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting Node %s: %w", claim.Status.NodeName, err)
	}
	return node, nil
}

// readyOf describes the Ready condition of the node for the message; empty without a node or condition.
func readyOf(node *corev1.Node) string {
	if node == nil {
		return ""
	}
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return fmt.Sprintf("; Ready=%s, reason %q: %s", cond.Status, cond.Reason, cond.Message)
		}
	}
	return ""
}

func (c *Controller) publish(obj client.Object, message string, dedupe ...string) {
	if c.opts.Recorder == nil {
		return
	}
	c.opts.Recorder.Publish(events.Event{
		InvolvedObject: obj, Type: corev1.EventTypeWarning, Reason: EventTimeout, Message: message,
		DedupeValues: append([]string{obj.GetName()}, dedupe...),
	})
}
