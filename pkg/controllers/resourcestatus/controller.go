/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package resourcestatus checks the vendor resources of every SCNodeClass
// (00a section 9, overview section 6.2): it resolves what `use` references and
// `forbid` kinds need per region, creates the preCreate resources, lists the
// managed ones, and publishes the result in status.vendors.<v>.resources and
// the vendor condition ResourcesReady. A region where a required resource is
// missing or failed has unavailable offerings (the engine is the catalog's
// class gate) with the reason in the condition message; the NodeClass itself
// stays Ready.
//
// The status is written with server-side apply under the field manager
// scarpenter-resources and owns nothing but those two fields. One apply per class
// carries every vendor, and nothing is written while the content is unchanged.
package resourcestatus

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/awslabs/operatorpkg/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/resources"
	"github.com/SpareCores/scarpenter/pkg/unavailable"
)

// The keys of a status entry the controller applies.
const (
	fieldKind   = "kind"
	fieldRegion = "region"
)

// Names of the field manager and the condition reasons.
const (
	FieldManager      = "scarpenter-resources"
	ReasonReady       = "Ready"
	ReasonUnavailable = "ResourcesUnavailable"
	ReasonCheckFailed = "CheckFailed"

	// DefaultInterval is how often the resources are checked.
	DefaultInterval = 30 * time.Second
	eventDedupe     = 10 * time.Minute
	maxMessage      = 1000
)

// Options configures the controller.
type Options struct {
	Client client.Client
	Engine *resources.Engine
	// Unavailable has the marks that only a class sees dropped with the class.
	Unavailable *unavailable.Cache
	// Regions returns the regions the driver can use, for a NodeClass block
	// that lists none.
	Regions  interface{ Regions(vendor string) []string }
	Recorder events.Recorder
	Clock    clock.Clock
	// Interval defaults to DefaultInterval.
	Interval time.Duration
}

// Controller computes the resource status of the SCNodeClasses.
type Controller struct{ opts Options }

// New returns the controller; Register adds it to a manager.
func New(o Options) *Controller {
	if o.Clock == nil {
		o.Clock = clock.RealClock{}
	}
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	return &Controller{opts: o}
}

// Register watches SCNodeClasses with the manager; every reconcile requeues
// itself after the interval.
func (c *Controller) Register(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("resource-status").
		WithOptions(controller.Options{SkipNameValidation: new(true)}).
		For(&v1alpha1.SCNodeClass{}).Complete(c)
}

// forget drops what is kept for a deleted NodeClass.
func (c *Controller) forget(class string) {
	c.opts.Engine.ForgetClass(class)
	if c.opts.Unavailable != nil {
		c.opts.Unavailable.ForgetClass(class)
	}
}

// Reconcile checks and applies the resource status of one NodeClass.
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var nc v1alpha1.SCNodeClass
	if err := c.opts.Client.Get(ctx, req.NamespacedName, &nc); err != nil {
		if apierrors.IsNotFound(err) {
			c.forget(req.Name)
		}
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !nc.DeletionTimestamp.IsZero() {
		c.forget(nc.Name)
		return reconcile.Result{}, nil
	}
	if err := c.applyStatus(ctx, &nc); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: c.opts.Interval}, nil
}

// regionsOf returns the regions to check for the vendor.
func (c *Controller) regionsOf(nc *v1alpha1.SCNodeClass, vendor string) []string {
	regions, _ := nc.Spec.Vendors.Regions(vendor)
	if len(regions) == 0 && c.opts.Regions != nil {
		regions = c.opts.Regions.Regions(vendor)
	}
	out := slices.Clone(regions)
	slices.Sort(out)
	return slices.Compact(out)
}

// applyStatus writes the status of every vendor of the class in one apply, when
// any of them changed: an apply drops the fields this manager owned and left
// out, so one apply per vendor would remove the other vendors' fields. A vendor
// that left the class is left out too, which removes its fields.
func (c *Controller) applyStatus(ctx context.Context, nc *v1alpha1.SCNodeClass) error {
	names := nc.Spec.Vendors.Names()
	vendors := map[string]any{}
	changed := false
	for _, vendor := range names {
		body, differs := c.vendorBody(ctx, nc, vendor)
		if body != nil {
			vendors[vendor] = body
		}
		changed = changed || differs
	}
	for vendor, vs := range nc.Status.Vendors {
		if !slices.Contains(names, vendor) && (len(vs.Resources) > 0 || len(vs.ResourceCounts) > 0 ||
			slices.ContainsFunc(vs.Conditions, isResourcesReady)) {
			changed = true
		}
	}
	if !changed {
		return nil
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "SCNodeClass",
		"metadata":   map[string]any{"name": nc.Name},
		"status":     map[string]any{"vendors": vendors},
	}}
	err := c.opts.Client.SubResource("status").Apply(ctx, client.ApplyConfigurationFromUnstructured(u),
		client.FieldOwner(FieldManager), client.ForceOwnership)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("applying the resource status of %s: %w", nc.Name, err)
	}
	return nil
}

func isResourcesReady(c status.Condition) bool {
	return c.Type == v1alpha1.VendorConditionResourcesReady
}

// vendorBody is the apply body of one vendor, and whether it differs from the
// status as read. A vendor whose regions are not known yet (the driver is not
// loaded) keeps what the status has, and a vendor with nothing yet gets no body.
func (c *Controller) vendorBody(ctx context.Context, nc *v1alpha1.SCNodeClass, vendor string) (map[string]any, bool) {
	current := nc.Status.Vendors[vendor]
	regions := c.regionsOf(nc, vendor)
	if len(regions) == 0 {
		i := slices.IndexFunc(current.Conditions, isResourcesReady)
		if i < 0 {
			return nil, false
		}
		return resourceBody(current.Conditions[i], current.Resources, current.ResourceCounts), false
	}
	rep := c.opts.Engine.Check(ctx, nc, vendor, regions)
	notReady, reasons := rep.NotReady()

	var condStatus metav1.ConditionStatus
	var reason, message string
	switch {
	case len(notReady) > 0:
		parts := make([]string, 0, len(notReady))
		for _, r := range notReady {
			parts = append(parts, r+": "+reasons[r])
		}
		condStatus, reason = metav1.ConditionFalse, ReasonUnavailable
		message = "offerings are unavailable in the regions with missing or failed resources: " + strings.Join(parts, "; ")
	case len(rep.Errors) > 0:
		condStatus, reason = metav1.ConditionUnknown, ReasonCheckFailed
		message = fmt.Sprintf("%v", rep.Errors[0])
	default:
		condStatus, reason = metav1.ConditionTrue, ReasonReady
	}
	if len(message) > maxMessage {
		message = message[:maxMessage-3] + "..."
	}
	for _, region := range notReady {
		c.warn(nc, vendor, region, reasons[region])
	}

	// The condition keeps its transition time while its status holds.
	cond := status.Condition{
		Type: v1alpha1.VendorConditionResourcesReady, Status: condStatus, Reason: reason, Message: message,
		LastTransitionTime: metav1.NewTime(c.opts.Clock.Now()), ObservedGeneration: nc.Generation,
	}
	differs := true
	if i := slices.IndexFunc(current.Conditions, isResourcesReady); i >= 0 {
		old := current.Conditions[i]
		if old.Status == cond.Status {
			cond.LastTransitionTime = old.LastTransitionTime
		}
		differs = old.Status != cond.Status || old.Reason != cond.Reason || old.Message != cond.Message ||
			old.ObservedGeneration != cond.ObservedGeneration
	}
	differs = differs || !sameResources(current.Resources, rep.Resources) ||
		!slices.Equal(current.ResourceCounts, rep.Counts)
	return resourceBody(cond, rep.Resources, rep.Counts), differs
}

func (c *Controller) warn(nc *v1alpha1.SCNodeClass, vendor, region, reason string) {
	if c.opts.Recorder == nil {
		return
	}
	c.opts.Recorder.Publish(events.Event{
		InvolvedObject: nc, Type: corev1.EventTypeWarning, Reason: "ResourcesUnavailable",
		Message:       fmt.Sprintf("the offerings of %s in %s are unavailable: %s", vendor, region, reason),
		DedupeValues:  []string{nc.Name, vendor, region, reason},
		DedupeTimeout: eventDedupe,
	})
}

// resourceBody is the apply body of one vendor.
func resourceBody(
	cond status.Condition, res []v1alpha1.ResourceStatus, counts []v1alpha1.ResourceCount,
) map[string]any {
	list := make([]any, len(res))
	for i, r := range res {
		m := map[string]any{fieldKind: r.Kind, fieldRegion: r.Region, "id": r.ID}
		if r.Name != "" {
			m["name"] = r.Name
		}
		if r.Managed {
			m["managed"] = true
		}
		if r.Source != "" {
			m["source"] = string(r.Source)
		}
		if r.LastUsed != nil {
			m["lastUsed"] = r.LastUsed.UTC().Format(time.RFC3339)
		}
		if r.Message != "" {
			m["message"] = r.Message
		}
		list[i] = m
	}
	countList := make([]any, len(counts))
	for i, n := range counts {
		countList[i] = map[string]any{fieldKind: n.Kind, fieldRegion: n.Region, "count": int64(n.Count)}
	}
	return map[string]any{
		"conditions": []any{map[string]any{
			"type":               v1alpha1.VendorConditionResourcesReady,
			"status":             string(cond.Status),
			"reason":             cond.Reason,
			"message":            cond.Message,
			"lastTransitionTime": cond.LastTransitionTime.UTC().Format(time.RFC3339),
			"observedGeneration": cond.ObservedGeneration,
		}},
		"resources":      list,
		"resourceCounts": countList,
	}
}

// sameResources compares the status as read with the one computed; lastUsed
// moves with every use, so it counts only when it moved by more than a minute
// (an unchanged object is not rewritten).
func sameResources(a, b []v1alpha1.ResourceStatus) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.Kind != y.Kind || x.Region != y.Region || x.ID != y.ID || x.Name != y.Name || x.Managed != y.Managed ||
			x.Source != y.Source || x.Message != y.Message {
			return false
		}
		switch {
		case x.LastUsed == nil && y.LastUsed == nil:
		case x.LastUsed == nil || y.LastUsed == nil:
			return false
		case y.LastUsed.Sub(x.LastUsed.Time).Abs() > time.Minute:
			return false
		}
	}
	return true
}
