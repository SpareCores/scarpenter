//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/controllers/nodeclass"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// StatusFieldManager owns the status fields the dev status controller writes.
// Other writers (the quota status controller) apply their own fields under
// their own managers.
const StatusFieldManager = "scarpenter-dev-status"

// statusInterval re-reconciles every NodeClass: the catalog becomes ready
// without any NodeClass event.
const conditionReady = "Ready"

const statusInterval = 10 * time.Second

// statusReconciler is the development stand-in for the NodeClass controller of
// plan 05: it marks every NodeClass ready as soon as the catalog covers it and
// publishes the per-vendor drift hash. Artifacts are not resolved (the fake
// driver boots nothing), so ArtifactsReady is always true.
type statusReconciler struct {
	client client.Client
	cat    *catalog.Catalog
	reg    *vendors.Registry
	clock  clock.Clock
}

// RegisterStatusController registers the stand-in with the manager. It is only
// for the L3 lane and development; production builds do not contain it.
func RegisterStatusController(mgr ctrl.Manager, cat *catalog.Catalog, reg *vendors.Registry) error {
	r := &statusReconciler{client: mgr.GetClient(), cat: cat, reg: reg, clock: clock.RealClock{}}
	return ctrl.NewControllerManagedBy(mgr).Named("dev-nodeclass-status").For(&v1alpha1.SCNodeClass{}).Complete(r)
}

func (r *statusReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	nc := &v1alpha1.SCNodeClass{}
	if err := r.client.Get(ctx, req.NamespacedName, nc); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !nc.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}
	now := metav1.NewTime(r.clock.Now())

	st := r.cat.Status(nc.Name)
	cond := func(existing []status.Condition, typ string, ok bool, reason, msg string) map[string]any {
		return condition(existing, typ, ok, reason, msg, nc.Generation, now)
	}
	catalogReady := cond(nc.Status.Conditions, v1alpha1.ConditionTypeCatalogReady, st.Ready, st.Reason, st.Message)
	artifacts := cond(nc.Status.Conditions, v1alpha1.ConditionTypeArtifactsReady, true, "NotRequired",
		"the fake driver boots no artifacts")
	ready := cond(nc.Status.Conditions, conditionReady, st.Ready, readyReason(st), st.Message)

	vendorStatus := map[string]any{}
	names := nc.Spec.Vendors.Names()
	for _, v := range names {
		hash, err := nodeclass.Hash(nc, v)
		if err != nil {
			return reconcile.Result{}, err
		}
		enabled := r.reg.Enabled(v)
		reason, msg := conditionReady, ""
		if !enabled {
			reason, msg = "VendorNotEnabled", fmt.Sprintf("no driver serves the vendor %q", v)
		}
		vs := nc.Status.Vendors[v]
		vendorStatus[v] = map[string]any{
			"hash": hash,
			"conditions": []any{
				cond(vs.Conditions, v1alpha1.VendorConditionReady, enabled, reason, msg),
			},
		}
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.SchemeGroupVersion.String(),
		"kind":       "SCNodeClass",
		"metadata":   map[string]any{"name": nc.Name},
		"status": map[string]any{
			"conditions": []any{ready, catalogReady, artifacts},
			"vendors":    vendorStatus,
		},
	}}
	if err := r.client.Status().Apply(ctx, client.ApplyConfigurationFromUnstructured(obj),
		client.FieldOwner(StatusFieldManager), client.ForceOwnership); err != nil {
		return reconcile.Result{}, fmt.Errorf("applying the status of %s: %w", nc.Name, err)
	}
	return reconcile.Result{RequeueAfter: statusInterval}, nil
}

func readyReason(st catalog.Status) string {
	if st.Ready {
		return "Ready"
	}
	return st.Reason
}

// condition builds a condition entry for the apply; the transition time stays
// when status and reason did not change.
func condition(existing []status.Condition, typ string, ok bool, reason, message string,
	generation int64, now metav1.Time) map[string]any {
	state := string(metav1.ConditionFalse)
	if ok {
		state = string(metav1.ConditionTrue)
	}
	if reason == "" {
		reason = typ
		if ok {
			reason = conditionReady
		}
	}
	ltt := now
	for _, c := range existing {
		if c.Type == typ && string(c.Status) == state && c.Reason == reason {
			ltt = c.LastTransitionTime
		}
	}
	return map[string]any{
		"type": typ, "status": state, "reason": reason, "message": message,
		"observedGeneration": generation, "lastTransitionTime": ltt.UTC().Format(time.RFC3339),
	}
}
