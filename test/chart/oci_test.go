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
)

func TestOCICredentialModes(t *testing.T) {
	const compartment = "ocid1.compartment.oc1..aaaa"
	render := func(t *testing.T, args ...string) (*appsv1.Deployment, []string) {
		t.Helper()
		base := []string{"--set", "cluster.name=c", "--set", "cluster.apiServerEndpoint=https://a.example.com",
			"--set", "vendors.aws.enabled=false", "--set", "vendors.oci.enabled=true", "--set", "vendors.oci.compartment=" + compartment}
		d := deployment(t, mustHelm(t, append(base, args...)...))
		return d, d.Spec.Template.Spec.Containers[0].Args
	}
	t.Run("apiKey", func(t *testing.T) {
		d, args := render(t, "--set", "vendors.oci.credentials.apiKey.existingSecret=oci-key")
		for _, want := range []string{"--vendors=oci", "--oci-compartment=" + compartment, "--oci-auth=apiKey",
			"--oci-credentials-dir=/etc/scarpenter/oci-key"} {
			if !slices.Contains(args, want) {
				t.Errorf("args %v lack %s", args, want)
			}
		}
		ok := false
		for _, v := range d.Spec.Template.Spec.Volumes {
			ok = ok || (v.Secret != nil && v.Secret.SecretName == "oci-key")
		}
		if !ok {
			t.Error("the key Secret is not mounted")
		}
	})
	t.Run("workloadIdentityFederation", func(t *testing.T) {
		d, args := render(t, "--set", "vendors.oci.credentials.mode=workloadIdentityFederation",
			"--set", "vendors.oci.credentials.workloadIdentityFederation.region=eu-frankfurt-1",
			"--set", "vendors.oci.credentials.workloadIdentityFederation.identityDomainURL=https://idcs-x.identity.oraclecloud.com",
			"--set", "vendors.oci.credentials.workloadIdentityFederation.clientId=cid",
			"--set", "vendors.oci.credentials.workloadIdentityFederation.audience=aud",
			"--set", "vendors.oci.credentials.workloadIdentityFederation.clientSecret.existingSecret=sec")
		for _, want := range []string{"--oci-auth=workloadIdentityFederation", "--oci-token-file=/var/run/secrets/oci/token",
			"--oci-client-secret-file=/etc/scarpenter/oci-client/client-secret", "--oci-client-id=cid", "--oci-region=eu-frankfurt-1"} {
			if !slices.Contains(args, want) {
				t.Errorf("args %v lack %s", args, want)
			}
		}
		found := false
		for _, v := range d.Spec.Template.Spec.Volumes {
			found = found || (v.Projected != nil && v.Projected.Sources[0].ServiceAccountToken.Audience == "aud")
		}
		if !found {
			t.Error("no projected token volume with the audience")
		}
	})
	t.Run("none", func(t *testing.T) {
		d, args := render(t, "--set", "vendors.oci.credentials.mode=none")
		for _, a := range args {
			if strings.HasPrefix(a, "--oci-credentials-dir") || strings.HasPrefix(a, "--oci-token-file") {
				t.Errorf("credentials wired in mode none: %s", a)
			}
		}
		if !slices.Contains(args, "--oci-auth=apiKey") {
			t.Errorf("args %v", args)
		}
		for _, v := range d.Spec.Template.Spec.Volumes {
			if strings.HasPrefix(v.Name, "oci-") {
				t.Errorf("volume %s in mode none", v.Name)
			}
		}
	})
	t.Run("off by default", func(t *testing.T) {
		d := deployment(t, mustHelm(t, "--set", "cluster.name=c", "--set", "cluster.apiServerEndpoint=https://a.example.com",
			"--set", "vendors.aws.credentials.mode=none"))
		for _, a := range d.Spec.Template.Spec.Containers[0].Args {
			if strings.HasPrefix(a, "--oci-") {
				t.Errorf("OCI wired while disabled: %s", a)
			}
		}
	})
}

func TestOCIValuesAreValidated(t *testing.T) {
	base := []string{"--set", "cluster.name=c", "--set", "cluster.apiServerEndpoint=https://a.example.com",
		"--set", "vendors.aws.enabled=false", "--set", "vendors.oci.enabled=true"}
	const comp = "vendors.oci.compartment=ocid1.compartment.oc1..aaaa"
	for name, tc := range map[string]struct {
		set  []string
		want string
	}{
		"no compartment":        {[]string{"vendors.oci.credentials.mode=none"}, "compartment is required"},
		"a bad compartment":     {[]string{"vendors.oci.credentials.mode=none", "vendors.oci.compartment=my-compartment"}, "compartment"},
		"no key secret":         {[]string{comp}, "existingSecret is required"},
		"a bad mode":            {[]string{comp, "vendors.oci.credentials.mode=instancePrincipal"}, "mode"},
		"federation incomplete": {[]string{comp, "vendors.oci.credentials.mode=workloadIdentityFederation"}, "are required with credentials mode workloadIdentityFederation"},
		"a bad region":          {[]string{comp, "vendors.oci.credentials.mode=none", "vendors.oci.credentials.workloadIdentityFederation.region=Frankfurt"}, "region"},
		"a short token":         {[]string{comp, "vendors.oci.credentials.mode=none", "vendors.oci.credentials.workloadIdentityFederation.expirationSeconds=60"}, "expirationSeconds"},
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
