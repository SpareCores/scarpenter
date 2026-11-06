/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package bootstraptoken deletes the bootstrap token of a NodeClaim once the
// token has done its job or can no longer be used (overview section 8.1): when
// the NodeClaim is Registered (the kubelet has joined and holds its own
// credentials), when it is being deleted, and when it is gone. Core's token
// cleaner removes expired tokens as a backstop, and the garbage collector
// removes tokens whose NodeClaim never showed up.
package bootstraptoken

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/bootstrap"
)

// Controller deletes the tokens of registered and missing NodeClaims.
type Controller struct {
	client client.Reader
	tokens *bootstrap.Minter
}

// New returns the controller; Register adds it to a manager. Reader reads the
// NodeClaims.
func New(reader client.Reader, tokens *bootstrap.Minter) *Controller {
	return &Controller{client: reader, tokens: tokens}
}

// Register watches NodeClaims with the manager. Only events that can end a
// token's life are reconciled: creation (a restart finds the registered ones),
// the transition to Registered, deletion.
func (c *Controller) Register(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("bootstrap-token").
		// The tests start several managers in one process; controller-runtime
		// keeps the names process-wide.
		WithOptions(controller.Options{SkipNameValidation: new(true)}).
		For(&karpv1.NodeClaim{}, builder.WithPredicates(predicates())).Complete(c)
}

func predicates() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return ours(e.Object) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return ours(e.Object) },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			if !ours(e.ObjectNew) {
				return false
			}
			was, now := registered(e.ObjectOld), registered(e.ObjectNew)
			return (now && !was) || !e.ObjectNew.GetDeletionTimestamp().IsZero() && e.ObjectOld.GetDeletionTimestamp().IsZero()
		},
	}
}

func ours(o client.Object) bool {
	nc, ok := o.(*karpv1.NodeClaim)
	if !ok {
		return false
	}
	ref := nc.Spec.NodeClassRef
	return ref != nil && ref.Group == v1alpha1.Group && ref.Kind == "SCNodeClass"
}

func registered(o client.Object) bool {
	nc, ok := o.(*karpv1.NodeClaim)
	return ok && nc.StatusConditions().Get(karpv1.ConditionTypeRegistered).IsTrue()
}

// Reconcile deletes the token when its NodeClaim is registered, deleting or gone.
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	nc := &karpv1.NodeClaim{}
	err := c.client.Get(ctx, types.NamespacedName{Name: req.Name}, nc)
	switch {
	case apierrors.IsNotFound(err):
		_, err = c.tokens.DeleteFor(ctx, req.Name, bootstrap.ReasonNodeClaimGone, nil)
	case err != nil:
		return reconcile.Result{}, fmt.Errorf("reading the NodeClaim %s: %w", req.Name, err)
	case !nc.DeletionTimestamp.IsZero():
		_, err = c.tokens.DeleteFor(ctx, req.Name, bootstrap.ReasonNodeClaimDeleting, nc)
	case registered(nc):
		_, err = c.tokens.DeleteFor(ctx, req.Name, bootstrap.ReasonRegistered, nc)
	}
	return reconcile.Result{}, err
}
