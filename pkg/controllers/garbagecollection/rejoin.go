/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package garbagecollection

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/scope"
)

// Rejoin deletes at once the Node of an instance SCarpenter deleted
// (Options.RecentlyDeleted) when it registers again and no NodeClaim has its
// provider ID: a machine that keeps running through the vendor's teardown does
// that, possibly several times. Any other Node is left to the sweep. It shares
// the collector's metric and runs on the leader only.
type Rejoin struct {
	c *Collector
	// dryRun are the provider IDs counted in dry-run (under the controller's
	// single worker).
	dryRun sets.Set[string]
}

// NewRejoin returns the controller of the collector; Register adds it to a manager.
func (c *Collector) NewRejoin() *Rejoin {
	return &Rejoin{c: c, dryRun: sets.New[string]()}
}

// Register watches Nodes with the manager: creations and updates only.
func (r *Rejoin) Register(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("rejoined-node").
		// The tests start several managers in one process; controller-runtime
		// keeps the names process-wide.
		WithOptions(controller.Options{SkipNameValidation: new(true)}).
		For(&corev1.Node{}, builder.WithPredicates(predicate.Funcs{
			DeleteFunc:  func(event.DeleteEvent) bool { return false },
			GenericFunc: func(event.GenericEvent) bool { return false },
		})).Complete(r)
}

// Reconcile deletes the Node when its instance was recently deleted and it has
// no NodeClaim.
func (r *Rejoin) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	opts := r.c.opts
	var node corev1.Node
	if err := opts.Client.Get(ctx, req.NamespacedName, &node); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	providerID := node.Spec.ProviderID
	if providerID == "" || !node.DeletionTimestamp.IsZero() ||
		opts.RecentlyDeleted == nil || !opts.RecentlyDeleted(providerID) {
		return reconcile.Result{}, nil
	}
	vendor, _, _, err := scope.ParseProviderID(providerID)
	if err != nil {
		return reconcile.Result{}, nil
	}
	var claims karpv1.NodeClaimList
	if err := opts.Client.List(ctx, &claims); err != nil {
		return reconcile.Result{}, fmt.Errorf("listing NodeClaims: %w", err)
	}
	for i := range claims.Items {
		if claims.Items[i].Status.ProviderID == providerID {
			return reconcile.Result{}, nil
		}
	}
	l := log.FromContext(ctx).WithName("garbagecollection").WithValues(
		"node", node.Name, "provider-id", providerID, "reason", ReasonRejoined)
	if opts.DryRun {
		// Nothing is deleted, so the Node is updated again and again: count it once.
		if r.dryRun.Has(providerID) {
			return reconcile.Result{}, nil
		}
		r.dryRun.Insert(providerID)
		l.Info("dry-run: would delete the Node of a deleted instance that registered again")
		r.c.deletions.WithLabelValues(vendor, KindNode, ReasonRejoined, ResultDryRun).Inc()
		return reconcile.Result{}, nil
	}
	// The UID precondition keeps a re-created Node of the same name safe.
	err = opts.Client.Delete(ctx, &node, client.Preconditions{UID: &node.UID})
	switch {
	case apierrors.IsNotFound(err):
		return reconcile.Result{}, nil
	case apierrors.IsConflict(err):
		// The Node was replaced since the read: look at the new one.
		return reconcile.Result{RequeueAfter: time.Second}, nil
	case err != nil:
		r.c.deletions.WithLabelValues(vendor, KindNode, ReasonRejoined, ResultError).Inc()
		return reconcile.Result{}, fmt.Errorf("deleting Node %s: %w", node.Name, err)
	}
	l.Info("deleted the Node of a deleted instance that registered again")
	r.c.deletions.WithLabelValues(vendor, KindNode, ReasonRejoined, ResultDeleted).Inc()
	r.c.event(ctx, "", "deleted the Node %s: its instance was deleted and it registered again (%s)",
		node.Name, ReasonRejoined)
	return reconcile.Result{}, nil
}
