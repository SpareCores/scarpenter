/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import (
	"context"
	"testing"
	"time"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/api"
	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/fake"
)

var testNow = time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)

// env is a fake with a template in fi-hel1 and a launch spec that fits it.
type env struct {
	t        *testing.T
	f        *fake.Fake
	ctx      context.Context
	template string
	plans    Plans
}

func fakeOpts() fake.Options { return fake.Options{} }

func newEnv(t *testing.T, o fake.Options) *env {
	t.Helper()
	f := fake.New(o)
	ps, err := f.GetPlans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return &env{
		t: t, f: f, ctx: context.Background(), plans: PlansOf(ps),
		template: f.AddTemplate("fi-hel1", "flatcar-stable-4757.2.3-amd64-maxiops", upcloud.StorageTierMaxIOPS, 10,
			map[string]string{"scarpenter-image": "flatcar"}),
	}
}

func (e *env) spec(name, plan string) LaunchSpec {
	e.t.Helper()
	p, err := e.plans.ValidatePlan(plan)
	if err != nil {
		e.t.Fatal(err)
	}
	labels, err := ServerLabels(map[vendors.TagKey]string{
		vendors.TagCluster: "c1", vendors.TagNodeClaim: name, vendors.TagManagedBy: vendors.ManagedByValue,
	}, nil, "uid-"+name, testNow)
	if err != nil {
		e.t.Fatal(err)
	}
	return LaunchSpec{
		Zone: "fi-hel1", Plan: p, Name: name, Template: Template{UUID: e.template, Tier: upcloud.StorageTierMaxIOPS, SizeGB: 10},
		Root:       Root{Tier: upcloud.StorageTierMaxIOPS, SizeGB: p.DefaultRootGB(), Encrypted: true},
		Networking: Networking{PublicIPv4: true}, UserData: []byte(`{"ignition":{"version":"3.4.0"}}`), Labels: labels,
	}
}

// launch builds and creates a server and settles it.
func (e *env) launch(name, plan string) *upcloud.ServerDetails {
	e.t.Helper()
	r, err := BuildCreateServer(e.spec(name, plan))
	if err != nil {
		e.t.Fatal(err)
	}
	d, err := CreateServer(e.ctx, e.f, r)
	if err != nil {
		e.t.Fatal(err)
	}
	e.f.Settle()
	return d
}

// errLostResponse is the error of a create whose response was lost.
func errLostResponse() error { return context.DeadlineExceeded }

func apiRuleset(name string) api.CreateRulesetRequest {
	return api.CreateRulesetRequest{Name: name, Stateful: true, Labels: []upcloud.Label{{Key: LabelUse, Value: "true"}}}
}
