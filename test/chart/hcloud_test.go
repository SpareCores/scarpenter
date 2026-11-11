//go:build chart

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package chart

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestHCloudValuesReachTheController: the Hetzner values land in the flag, the
// token Secret reference and the price override; AWS is off.
func TestHCloudValuesReachTheController(t *testing.T) {
	d := deployment(t, mustHelm(t, "-f", filepath.Join("testdata", "values", "hcloud.yaml")))
	c := d.Spec.Template.Spec.Containers[0]
	o, _, _ := parseControllerArgs(t, c.Args)
	if !slices.Equal(o.Vendors, []string{"hcloud"}) || o.HCloudServerLimit != 25 {
		t.Errorf("vendors %v, server limit %d", o.Vendors, o.HCloudServerLimit)
	}
	env := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		env[e.Name] = e
	}
	tok := env["HCLOUD_TOKEN"].ValueFrom
	if tok == nil || tok.SecretKeyRef == nil || tok.SecretKeyRef.Name != "hcloud-token" || tok.SecretKeyRef.Key != "token" {
		t.Errorf("HCLOUD_TOKEN: %+v", env["HCLOUD_TOKEN"])
	}
	if env["SCARPENTER_HCLOUD_IPV4_PRICE"].Value != "0.0008" {
		t.Errorf("the IPv4 price: %+v", env["SCARPENTER_HCLOUD_IPV4_PRICE"])
	}
	if _, ok := env["AWS_REGION"]; ok {
		t.Error("AWS is not wired when it is off")
	}
}

// TestHCloudNeedsItsToken: enabling the driver without the Secret is refused, and
// the vendors list names both drivers when both are on.
func TestHCloudNeedsItsToken(t *testing.T) {
	args := append(slices.Clone(required), "--set", "vendors.hcloud.enabled=true")
	if _, err := helm(t, args...); err == nil || !strings.Contains(err.Error(), "credentials.secret.existingSecret is required") {
		t.Errorf("without a token Secret: %v", err)
	}
	args = append(args, "--set", "vendors.hcloud.credentials.secret.existingSecret=tok")
	d := deployment(t, mustHelm(t, args...))
	o, _, _ := parseControllerArgs(t, d.Spec.Template.Spec.Containers[0].Args)
	if !slices.Equal(o.Vendors, []string{"aws", "hcloud"}) {
		t.Errorf("vendors %v", o.Vendors)
	}
	for _, bad := range []string{"vendors.hcloud.serverLimit=-1", "vendors.hcloud.ipv4Price=-1", "vendors.hcloud.token=x"} {
		if _, err := helm(t, append(slices.Clone(required), "--set", bad)...); err == nil {
			t.Errorf("helm accepted %s", bad)
		}
	}
}
