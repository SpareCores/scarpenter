/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package gpupending removes the gpu-pending taint (ADR-031) from the Nodes of
// SCarpenter's NodeClaims: from a GPU node once its own check labelled it
// gpu-ready (a node may label itself but NodeRestriction forbids it to change
// its taints), and at once from a node that needs no driver check. The latter
// are the CPU nodes of a NodePool that lists the taint in startupTaints, which
// core puts on every node of the pool. A GPU node whose check failed (label
// gpu-ready=false) or that still has the taint after Timeout loses its NodeClaim.
package gpupending

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
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

// Timeout is how long a node may keep the taint after its creation before its NodeClaim is
// deleted: a failed check never makes the node Initialized, so core would keep it (at GPU
// prices) until expireAfter.
const Timeout = 30 * time.Minute

// Controller removes the taint from the Nodes that carry the ready label or need no check,
// and deletes the NodeClaim of a node whose check failed or took too long.
type Controller struct {
	client   client.Client
	recorder events.Recorder
	clock    clock.Clock
}

// New returns the controller; Register adds it to a manager.
func New(c client.Client, recorder events.Recorder, clk clock.Clock) *Controller {
	return &Controller{client: c, recorder: recorder, clock: clk}
}

// Register watches Nodes with the manager.
func (c *Controller) Register(mgr ctrl.Manager) error {
	err := ctrl.NewControllerManagedBy(mgr).Named("gpu-pending").
		// The tests start several managers in one process; controller-runtime
		// keeps the names process-wide.
		WithOptions(controller.Options{SkipNameValidation: new(true)}).
		For(&corev1.Node{}).Complete(c)
	if err != nil {
		return fmt.Errorf("registering the GPU pending controller: %w", err)
	}
	return nil
}

// Reconcile decides one node.
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	node := &corev1.Node{}
	if err := c.client.Get(ctx, req.NamespacedName, node); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !strings.HasPrefix(node.Spec.ProviderID, scope.ProviderIDPrefix) {
		return reconcile.Result{}, nil
	}
	// Only the types that boot the NVIDIA driver carry the driver branch label (the
	// kubelet registers with it); the others need no check.
	needsCheck := node.Labels[v1alpha1.LabelNVIDIADriverBranch] != ""
	pending := slices.ContainsFunc(node.Spec.Taints, isPending)
	if needsCheck {
		switch {
		case node.Labels[v1alpha1.LabelGPUReady] == "false":
			return reconcile.Result{}, c.deleteClaim(ctx, node,
				"the GPU check failed on the node (see the journal of scarpenter-nvidia-check.service)")
		case node.Labels[v1alpha1.LabelGPUReady] != "true":
			if !pending {
				return reconcile.Result{}, nil
			}
			if wait := Timeout - c.clock.Since(node.CreationTimestamp.Time); wait > 0 {
				return reconcile.Result{RequeueAfter: wait}, nil
			}
			return reconcile.Result{}, c.deleteClaim(ctx, node,
				fmt.Sprintf("the GPU check did not pass within %s of the node's creation", Timeout))
		}
	}
	if !pending {
		return reconcile.Result{}, nil
	}
	patched := node.DeepCopy()
	patched.Spec.Taints = slices.DeleteFunc(slices.Clone(node.Spec.Taints), isPending)
	// The optimistic lock: the patch replaces the whole taint list, which core and
	// the retention controller also write.
	lock := client.MergeFromWithOptimisticLock{}
	if err := c.client.Patch(ctx, patched, client.MergeFromWithOptions(node, lock)); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	return reconcile.Result{}, nil
}

func isPending(t corev1.Taint) bool { return t.Key == v1alpha1.TaintGPUPending }

// deleteClaim deletes the NodeClaim of the node (core then replaces the node) and says why.
func (c *Controller) deleteClaim(ctx context.Context, node *corev1.Node, reason string) error {
	var claims karpv1.NodeClaimList
	if err := c.client.List(ctx, &claims); err != nil {
		return fmt.Errorf("listing the NodeClaims: %w", err)
	}
	for i := range claims.Items {
		claim := &claims.Items[i]
		if claim.Status.ProviderID != node.Spec.ProviderID || !claim.DeletionTimestamp.IsZero() {
			continue
		}
		log.FromContext(ctx).Info("deleting the NodeClaim of a GPU node that is not ready",
			"node", node.Name, "nodeclaim", claim.Name, "reason", reason)
		if c.recorder != nil {
			c.recorder.Publish(events.Event{
				InvolvedObject: claim, Type: corev1.EventTypeWarning, Reason: "GPUCheckFailed", Message: reason,
				DedupeValues: []string{claim.Name},
			})
		}
		return client.IgnoreNotFound(c.client.Delete(ctx, claim))
	}
	return nil
}
