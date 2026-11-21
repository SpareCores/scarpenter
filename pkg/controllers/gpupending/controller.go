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
// core puts on every node of the pool.
package gpupending

import (
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/scope"
)

// Controller removes the taint from the Nodes that carry the ready label or need no check.
type Controller struct {
	client client.Client
}

// New returns the controller; Register adds it to a manager.
func New(c client.Client) *Controller { return &Controller{client: c} }

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

// Reconcile removes the taint of a node whose check passed or that needs none.
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	node := &corev1.Node{}
	if err := c.client.Get(ctx, req.NamespacedName, node); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !strings.HasPrefix(node.Spec.ProviderID, scope.ProviderIDPrefix) {
		return reconcile.Result{}, nil
	}
	// Only the types that boot the NVIDIA driver carry the driver branch label (core
	// syncs it at registration, together with the startup taints); the others need
	// no check.
	needsCheck := node.Labels[v1alpha1.LabelNVIDIADriverBranch] != ""
	if needsCheck && node.Labels[v1alpha1.LabelGPUReady] != "true" {
		return reconcile.Result{}, nil
	}
	kept := slices.DeleteFunc(slices.Clone(node.Spec.Taints), func(t corev1.Taint) bool {
		return t.Key == v1alpha1.TaintGPUPending
	})
	if len(kept) == len(node.Spec.Taints) {
		return reconcile.Result{}, nil
	}
	patched := node.DeepCopy()
	patched.Spec.Taints = kept
	// The optimistic lock: the patch replaces the whole taint list, which core and
	// the retention controller also write.
	lock := client.MergeFromWithOptimisticLock{}
	if err := c.client.Patch(ctx, patched, client.MergeFromWithOptions(node, lock)); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	return reconcile.Result{}, nil
}
