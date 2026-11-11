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
// scarpenter-resources and owns nothing but those two fields.
package resourcestatus

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/resources"
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

// Reconcile checks and applies the resource status of one NodeClass.
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var nc v1alpha1.SCNodeClass
	if err := c.opts.Client.Get(ctx, req.NamespacedName, &nc); err != nil {
		if apierrors.IsNotFound(err) {
			c.opts.Engine.ForgetClass(req.Name)
		}
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !nc.DeletionTimestamp.IsZero() {
		c.opts.Engine.ForgetClass(nc.Name)
		return reconcile.Result{}, nil
	}
	var errs []error
	for _, vendor := range nc.Spec.Vendors.Names() {
		if err := c.vendor(ctx, &nc, vendor); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		log.FromContext(ctx).Error(fmt.Errorf("%v", errs), "applying the resource status")
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

func (c *Controller) vendor(ctx context.Context, nc *v1alpha1.SCNodeClass, vendor string) error {
	regions := c.regionsOf(nc, vendor)
	if len(regions) == 0 {
		return nil // the driver's regions are not loaded yet
	}
	rep := c.opts.Engine.Check(ctx, nc, vendor, regions)
	notReady, reasons := rep.NotReady()

	var status metav1.ConditionStatus
	var reason, message string
	switch {
	case len(notReady) > 0:
		parts := make([]string, 0, len(notReady))
		for _, r := range notReady {
			parts = append(parts, r+": "+reasons[r])
		}
		status, reason = metav1.ConditionFalse, ReasonUnavailable
		message = "offerings are unavailable in the regions with missing or failed resources: " + strings.Join(parts, "; ")
	case len(rep.Errors) > 0:
		status, reason = metav1.ConditionUnknown, ReasonCheckFailed
		message = fmt.Sprintf("%v", rep.Errors[0])
	default:
		status, reason = metav1.ConditionTrue, ReasonReady
	}
	if len(message) > maxMessage {
		message = message[:maxMessage-3] + "..."
	}
	for _, region := range notReady {
		c.warn(nc, vendor, region, reasons[region])
	}
	return c.apply(ctx, nc, vendor, status, reason, message, rep.Resources, rep.Counts)
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

func (c *Controller) apply(
	ctx context.Context, nc *v1alpha1.SCNodeClass, vendor string, status metav1.ConditionStatus, reason, message string,
	res []v1alpha1.ResourceStatus, counts []v1alpha1.ResourceCount,
) error {
	now := metav1.NewTime(c.opts.Clock.Now())
	current := nc.Status.Vendors[vendor]
	changed := true
	for _, cond := range current.Conditions {
		if cond.Type != v1alpha1.VendorConditionResourcesReady {
			continue
		}
		if string(cond.Status) == string(status) {
			now = cond.LastTransitionTime
		}
		changed = string(cond.Status) != string(status) || cond.Reason != reason || cond.Message != message ||
			cond.ObservedGeneration != nc.Generation
	}
	if !changed && sameResources(current.Resources, res) && slices.Equal(current.ResourceCounts, counts) {
		return nil
	}
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
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "SCNodeClass",
		"metadata":   map[string]any{"name": nc.Name},
		"status": map[string]any{"vendors": map[string]any{vendor: map[string]any{
			"conditions": []any{map[string]any{
				"type":               v1alpha1.VendorConditionResourcesReady,
				"status":             string(status),
				"reason":             reason,
				"message":            message,
				"lastTransitionTime": now.UTC().Format(time.RFC3339),
				"observedGeneration": nc.Generation,
			}},
			"resources":      list,
			"resourceCounts": countList,
		}}},
	}}
	err := c.opts.Client.SubResource("status").Apply(ctx, client.ApplyConfigurationFromUnstructured(u),
		client.FieldOwner(FieldManager), client.ForceOwnership)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("applying the resource status of %s/%s: %w", nc.Name, vendor, err)
	}
	return nil
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
