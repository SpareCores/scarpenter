/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package retention_test

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	v1alpha1 "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/billing"
	"github.com/SpareCores/scarpenter/pkg/controllers/retention"
)

func (h *harness) status() ([]v1alpha1.BillingStatus, *metav1.Condition) {
	h.t.Helper()
	if h.sc == nil {
		h.sc = retention.NewStatusController(h.ctrl, 0)
	}
	if _, err := h.sc.Reconcile(h.ctx(), reconcile.Request{NamespacedName: clientKey(h.class)}); err != nil {
		h.t.Fatal(err)
	}
	nc := &v1alpha1.SCNodeClass{}
	h.must(h.c.Get(h.t.Context(), clientKey(h.class), nc))
	for i := range nc.Status.Conditions {
		c := nc.Status.Conditions[i]
		if c.Type == v1alpha1.ConditionTypeBillingRetention {
			return nc.Status.Billing, &metav1.Condition{Type: c.Type, Status: c.Status, Reason: c.Reason, Message: c.Message}
		}
	}
	return nc.Status.Billing, nil
}

// The class of the harness has one vendor block, aws, whose rule the tests set.
func TestStatusReasonsPerVendor(t *testing.T) {
	for name, tc := range map[string]struct {
		billing *v1alpha1.BillingSpec
		rule    billing.Rule
		row     bool
		active  bool
		reason  string
		period  string
		message string
	}{
		"hourly":            {billing: enabled(), rule: rule(hour, hour, true), active: true, reason: "Active", period: "1h0m0s", message: "increments"},
		"minimum only":      {billing: enabled(), rule: rule(time.Second, 10*time.Minute, true), active: true, reason: "Active", period: "10m0s", message: "minimum only"},
		"disabled":          {billing: nil, rule: rule(hour, hour, true), reason: "Disabled"},
		"per second":        {billing: enabled(), rule: rule(time.Second, 0, true), reason: "PerSecondBilling"},
		"minimum too short": {billing: enabled(), rule: rule(time.Second, time.Minute, true), reason: "MinimumTooShort"},
		"unverified":        {billing: enabled(), rule: rule(hour, hour, false), reason: "Unverified"},
		"unsupported shape": {billing: enabled(), rule: rule(hour, 90*time.Minute, true), reason: "UnsupportedShape"},
		"no Keeper data":    {billing: enabled(), rule: rule(hour, hour, true), row: true, reason: "NoKeeperData"},
		"minPeriod under twice the margin": {billing: &v1alpha1.BillingSpec{RetainUntilPeriodEnd: true,
			Margin: &metav1.Duration{Duration: 10 * time.Minute}, MinPeriod: &metav1.Duration{Duration: 15 * time.Minute}},
			rule: rule(hour, hour, true), reason: "InvalidSettings"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, tc.billing)
			h.rules.rules["aws"] = tc.rule
			h.rules.hide = map[string]bool{}
			if tc.reason == "NoKeeperData" {
				h.rules.hide["aws"] = true
			}
			statuses, cond := h.status()
			if len(statuses) != 1 {
				t.Fatalf("statuses = %+v", statuses)
			}
			got := statuses[0]
			if got.Vendor != "aws" || got.Active != tc.active || got.Reason != tc.reason || got.Period != tc.period ||
				(tc.message != "" && got.Message != tc.message) || (!tc.active && got.Message == "") {
				t.Errorf("status %+v, want active=%v reason=%s period=%q", got, tc.active, tc.reason, tc.period)
			}
			if tc.reason != "NoKeeperData" && got.Source != "test" {
				t.Errorf("source %q: the rule's source is shown", got.Source)
			}
			if cond == nil {
				t.Fatal("no BillingRetention condition")
			}
			if (cond.Status == metav1.ConditionTrue) != tc.active {
				t.Errorf("condition %+v for active=%v", cond, tc.active)
			}
			if !strings.Contains(cond.Message, "aws: "+tc.reason) {
				t.Errorf("condition message %q does not name the reason", cond.Message)
			}
		})
	}
}

// The condition counts the nodes left alone, per reason.
func TestStatusCountsTheNodesLeftAlone(t *testing.T) {
	h := newHarness(t, enabled())
	h.rules.rules["aws"] = rule(hour, hour, true)
	h.replacePool(poolOpts{never: true})
	h.addNode(nodeOpts{vendor: "aws"})
	h.addNode(nodeOpts{vendor: "aws"})
	h.addNode(nodeOpts{vendor: "aws", annotations: map[string]string{karpv1.DoNotDisruptAnnotationKey: "true"}})
	_, cond := h.status()
	if cond == nil || !strings.Contains(cond.Message, "nodes left alone: ConsolidationNever=3") {
		t.Errorf("condition %+v: the Never pool comes before the user's annotation, so all three are ConsolidationNever", cond)
	}
	if got := h.metric("scarpenter_billing_skipped_nodes", "vendor", "aws", "nodeclass", h.class, "reason", "ConsolidationNever"); got != 3 {
		t.Errorf("skipped_nodes = %v", got)
	}

	h2 := newHarness(t, enabled())
	h2.rules.rules["aws"] = rule(hour, hour, true)
	h2.addNode(nodeOpts{vendor: "aws", annotations: map[string]string{karpv1.DoNotDisruptAnnotationKey: "true"}})
	held := h2.addNode(nodeOpts{vendor: "aws"})
	held.reconcile()
	_, cond = h2.status()
	if cond == nil || !strings.Contains(cond.Message, "UserDoNotDisrupt=1") {
		t.Errorf("condition %+v", cond)
	}
	if got := h2.metric("scarpenter_billing_retained_nodes", "vendor", "aws", "nodeclass", h2.class); got != 1 {
		t.Errorf("retained_nodes = %v", got)
	}
	h2.clk.Step(30 * time.Second)
	h2.status()
	if got := h2.metric("scarpenter_billing_retained_seconds_total", "vendor", "aws"); got != 30 {
		t.Errorf("retained_seconds_total = %v, want 30", got)
	}
}

// A vendor whose nodes all have no billing start says so.
func TestStatusNoBillingStart(t *testing.T) {
	h := newHarness(t, enabled())
	h.rules.rules["aws"] = rule(hour, hour, true)
	h.addNode(nodeOpts{vendor: "aws", noStart: true})
	statuses, _ := h.status()
	if len(statuses) != 1 || statuses[0].Active || statuses[0].Reason != "NoBillingStart" {
		t.Errorf("%+v", statuses)
	}
}
