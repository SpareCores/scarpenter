/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package nodeclass_test

import (
	"context"
	"strings"
	"testing"

	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/artifacts"
	"github.com/SpareCores/scarpenter/pkg/bootstrap"
	"github.com/SpareCores/scarpenter/pkg/controllers/nodeclass"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// withTailnet replaces the controller of the environment with one that has the
// tailnet backend.
func (e *env) withTailnet(t nodeclass.TailnetOptions) {
	e.ctrl = nodeclass.New(nodeclass.Options{
		Client: e.c, Catalog: e.cat, Registry: vendors.NewRegistryFrom(e.drv),
		Artifacts: artifacts.New(artifacts.Config{SysextBase: e.server}),
		Renderer:  bootstrap.NewRenderer(bootstrap.RendererOptions{Refs: e.refs, APIServerEndpoint: "https://api.example.com:6443"}),
		Refs:      e.refs, ServerVersion: func(context.Context) (string, error) { return e.version, nil }, Recorder: e.rec,
		Tailnet: t,
	})
}

func perNode(loginServer string, tags ...string) func(*v1alpha1.SCNodeClass) {
	return func(nc *v1alpha1.SCNodeClass) {
		nc.Spec.Network = v1alpha1.NetworkSpec{Template: v1alpha1.NetworkTemplateTailscale, Tailscale: &v1alpha1.TailscaleSpec{
			Version: "v1.98.10", LoginServer: loginServer, KeySource: v1alpha1.TailnetKeyPerNode, Tags: tags,
		}}
	}
}

// A PerNode NodeClass is valid only against a backend that matches it, and needs
// no Secret: the trial rendering carries a placeholder key.
func TestPerNodeTailnetKeys(t *testing.T) {
	headscale := nodeclass.TailnetOptions{
		Backend: nodeclass.TailnetBackendHeadscale, APIURL: "https://headscale.example.com", CredentialsDir: "/creds",
	}
	for name, tc := range map[string]struct {
		tailnet     nodeclass.TailnetOptions
		loginServer string
		wantValid   bool
		wantMessage string
	}{
		"headscale-same-url":              {headscale, "https://headscale.example.com", true, ""},
		"headscale-trailing-slash":        {headscale, "https://headscale.example.com/", true, ""},
		"headscale-default-port-case":     {headscale, "https://Headscale.Example.com:443", true, ""},
		"headscale-another-host":          {headscale, "https://evil.example.com", false, "does not match"},
		"headscale-another-scheme":        {headscale, "http://headscale.example.com", false, "does not match"},
		"headscale-another-port":          {headscale, "https://headscale.example.com:8443", false, "does not match"},
		"headscale-a-path":                {headscale, "https://headscale.example.com/x", false, "does not match"},
		"headscale-userinfo":              {headscale, "https://user@headscale.example.com", false, "does not match"},
		"headscale-query":                 {headscale, "https://headscale.example.com?next=x", false, "does not match"},
		"headscale-fragment":              {headscale, "https://headscale.example.com#x", false, "does not match"},
		"headscale-non-ascii-host":        {headscale, "https://headscale.example.co\u212a", false, "does not match"},
		"headscale-trailing-dot":          {headscale, "https://headscale.example.com.", false, "does not match"},
		"headscale-saas-login-server":     {headscale, "", false, "does not match"},
		"headscale-no-credentials":        {nodeclass.TailnetOptions{Backend: "headscale", APIURL: headscale.APIURL}, "https://headscale.example.com", false, "credentials"},
		"tailscale-empty-login-server":    {nodeclass.TailnetOptions{Backend: "tailscale", APIURL: "https://api.tailscale.com", CredentialsDir: "/c"}, "", true, ""},
		"tailscale-a-login-server":        {nodeclass.TailnetOptions{Backend: "tailscale", APIURL: "https://api.tailscale.com", CredentialsDir: "/c"}, "https://headscale.example.com", false, "does not match"},
		"no-backend":                      {nodeclass.TailnetOptions{}, "", false, "--tailnet-backend none"},
		"backend-none-with-a-credentials": {nodeclass.TailnetOptions{Backend: "none", CredentialsDir: "/c"}, "", false, "--tailnet-backend none"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.withTailnet(tc.tailnet)
			e.create(e.newNodeClass(perNode(tc.loginServer, "tag:bench")))
			nc := e.reconcile(e.name)
			if tc.wantValid {
				wantCond(t, nc, v1alpha1.ConditionTypeValidationSucceeded, metav1.ConditionTrue, nodeclass.ReasonValid)
				wantCond(t, nc, status.ConditionReady, metav1.ConditionTrue, "")
				if nc.Status.Vendors["aws"].Hash == "" {
					t.Error("a valid PerNode NodeClass has no drift hash")
				}
				return
			}
			wantCond(t, nc, v1alpha1.ConditionTypeValidationSucceeded, metav1.ConditionFalse, nodeclass.ReasonTailnetKeysNotConfigured)
			wantCond(t, nc, status.ConditionReady, metav1.ConditionFalse, "")
			if c := cond(nc, v1alpha1.ConditionTypeValidationSucceeded); c == nil || !strings.Contains(c.Message, tc.wantMessage) {
				t.Errorf("ValidationSucceeded = %+v, want a message with %q", c, tc.wantMessage)
			}
		})
	}
}

// A Static NodeClass is not affected by the backend, and the controller makes no
// API call for either.
func TestStaticTailnetKeysIgnoreTheBackend(t *testing.T) {
	e := newEnv(t)
	e.refs.set("secret", "scarpenter", "ts-node-key", "authkey", "tskey-auth-EXAMPLE", "100")
	e.create(e.newNodeClass(func(nc *v1alpha1.SCNodeClass) {
		nc.Spec.Network = v1alpha1.NetworkSpec{Template: v1alpha1.NetworkTemplateTailscale, Tailscale: &v1alpha1.TailscaleSpec{
			Version: "v1.98.10", KeySource: v1alpha1.TailnetKeyStatic,
			AuthKeySecretRef: &v1alpha1.SecretKeyRef{Name: "ts-node-key", Namespace: "scarpenter", Key: "authkey"},
		}}
	}))
	wantCond(t, e.reconcile(e.name), status.ConditionReady, metav1.ConditionTrue, "")
}
