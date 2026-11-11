//go:build chart

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package chart

import (
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestGCPCredentialModes(t *testing.T) {
	env := func(t *testing.T, args ...string) (map[string]corev1.EnvVar, *appsv1.Deployment) {
		t.Helper()
		d := deployment(t, mustHelm(t, args...))
		m := map[string]corev1.EnvVar{}
		for _, e := range d.Spec.Template.Spec.Containers[0].Env {
			m[e.Name] = e
		}
		return m, d
	}
	base := []string{"--set", "cluster.name=c", "--set", "cluster.apiServerEndpoint=https://a.example.com",
		"--set", "vendors.aws.enabled=false", "--set", "vendors.gcp.enabled=true", "--set", "vendors.gcp.project=my-project"}
	audience := "//iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/p/providers/q"

	t.Run("workloadIdentity", func(t *testing.T) {
		m, d := env(t, append(slices.Clone(base), "--set", "vendors.gcp.credentials.workloadIdentity.configMap=wif",
			"--set", "vendors.gcp.credentials.workloadIdentity.audience="+audience)...)
		if got := m["GOOGLE_APPLICATION_CREDENTIALS"].Value; got != "/etc/scarpenter/gcp/credential-configuration.json" {
			t.Errorf("GOOGLE_APPLICATION_CREDENTIALS = %q", got)
		}
		found := false
		for _, v := range d.Spec.Template.Spec.Volumes {
			if v.Projected != nil && v.Projected.Sources[0].ServiceAccountToken.Audience == audience {
				found = true
			}
		}
		if !found {
			t.Error("no projected token volume with the provider's audience")
		}
		if !slices.Contains(d.Spec.Template.Spec.Containers[0].Args, "--gcp-project=my-project") {
			t.Errorf("args = %v", d.Spec.Template.Spec.Containers[0].Args)
		}
		if !slices.Contains(d.Spec.Template.Spec.Containers[0].Args, "--vendors=gcp") {
			t.Errorf("args = %v", d.Spec.Template.Spec.Containers[0].Args)
		}
		if _, ok := m["AWS_REGION"]; ok {
			t.Error("AWS wired with the AWS vendor off")
		}
	})
	t.Run("secret", func(t *testing.T) {
		m, d := env(t, append(slices.Clone(base), "--set", "vendors.gcp.credentials.mode=secret",
			"--set", "vendors.gcp.credentials.secret.existingSecret=gcp-key")...)
		if got := m["GOOGLE_APPLICATION_CREDENTIALS"].Value; got != "/etc/scarpenter/gcp-key/key.json" {
			t.Errorf("GOOGLE_APPLICATION_CREDENTIALS = %q", got)
		}
		ok := false
		for _, v := range d.Spec.Template.Spec.Volumes {
			if v.Secret != nil && v.Secret.SecretName == "gcp-key" {
				ok = true
			}
		}
		if !ok {
			t.Error("the key Secret is not mounted")
		}
	})
	t.Run("none", func(t *testing.T) {
		m, _ := env(t, append(slices.Clone(base), "--set", "vendors.gcp.credentials.mode=none")...)
		if _, ok := m["GOOGLE_APPLICATION_CREDENTIALS"]; ok {
			t.Error("credentials wired in mode none")
		}
	})
	t.Run("price", func(t *testing.T) {
		m, _ := env(t, append(slices.Clone(base), "--set", "vendors.gcp.credentials.mode=none", "--set", "vendors.gcp.ipv4Price=0.01")...)
		if m["SCARPENTER_GCP_IPV4_PRICE"].Value != "0.01" {
			t.Errorf("price = %+v", m["SCARPENTER_GCP_IPV4_PRICE"])
		}
	})
	t.Run("off by default", func(t *testing.T) {
		m, _ := env(t, "--set", "cluster.name=c", "--set", "cluster.apiServerEndpoint=https://a.example.com",
			"--set", "vendors.aws.credentials.mode=none")
		if _, ok := m["GOOGLE_APPLICATION_CREDENTIALS"]; ok {
			t.Error("GCP wired while disabled")
		}
	})
}

func TestGCPValuesAreValidated(t *testing.T) {
	base := []string{"--set", "cluster.name=c", "--set", "cluster.apiServerEndpoint=https://a.example.com",
		"--set", "vendors.aws.enabled=false", "--set", "vendors.gcp.enabled=true"}
	for name, tc := range map[string]struct {
		set  []string
		want string
	}{
		"no ConfigMap":     {[]string{"vendors.gcp.credentials.workloadIdentity.audience=//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/q"}, "configMap is required"},
		"no audience":      {[]string{"vendors.gcp.credentials.workloadIdentity.configMap=wif"}, "audience is required"},
		"a bad audience":   {[]string{"vendors.gcp.credentials.workloadIdentity.audience=sts.example.com"}, "audience"},
		"a bad mode":       {[]string{"vendors.gcp.credentials.mode=keys"}, "mode"},
		"a bad project":    {[]string{"vendors.gcp.credentials.mode=none", "vendors.gcp.project=A"}, "project"},
		"no secret":        {[]string{"vendors.gcp.credentials.mode=secret"}, "existingSecret is required"},
		"a negative price": {[]string{"vendors.gcp.credentials.mode=none", "vendors.gcp.ipv4Price=-1"}, "ipv4Price"},
		"a short token":    {[]string{"vendors.gcp.credentials.mode=none", "vendors.gcp.credentials.workloadIdentity.expirationSeconds=60"}, "expirationSeconds"},
	} {
		args := slices.Clone(base)
		for _, s := range tc.set {
			args = append(args, "--set", s)
		}
		if _, err := helm(t, args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error with %q", name, err, tc.want)
		}
	}
}
