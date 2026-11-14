/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ocienv_test

import (
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors/oci"
	"github.com/SpareCores/scarpenter/test/e2e-oci/ocienv"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestAFederationFromTheTokenFile(t *testing.T) {
	c := ocienv.Credentials(env(map[string]string{
		"OCI_TOKEN_FILE": " /run/oidc ", "OCI_IDENTITY_DOMAIN_URL": "https://idcs-x.identity.oraclecloud.com",
		"OCI_CLIENT_ID": "id", "OCI_CLIENT_SECRET_FILE": "/run/secret", "OCI_REGION": "eu-frankfurt-1",
	}))
	if c.Mode != oci.AuthWorkloadIdentityFederation || c.TokenFile != "/run/oidc" || c.ClientID != "id" ||
		c.ClientSecretFile != "/run/secret" || c.Region != "eu-frankfurt-1" || c.IdentityDomain == "" {
		t.Errorf("%+v", c)
	}
	c = ocienv.Credentials(env(map[string]string{"OCI_TOKEN_FILE": "/x", "OCI_REGION": "eu-frankfurt-1", "OCI_HOME_REGION": "us-ashburn-1"}))
	if c.Region != "us-ashburn-1" {
		t.Errorf("the home region does not win: %q", c.Region)
	}
}

func TestAConfigProfileOtherwise(t *testing.T) {
	c := ocienv.Credentials(env(map[string]string{"OCI_CLI_CONFIG_FILE": "/home/x/config", "OCI_CLI_PROFILE": "CI"}))
	if c.Mode != oci.AuthAPIKey || c.ConfigFile != "/home/x/config" || c.Profile != "CI" || c.TokenFile != "" {
		t.Errorf("%+v", c)
	}
	// The credentials build (and the federation's inputs are checked) without a network call.
	if _, err := oci.NewConfigurationProvider(ocienv.Credentials(env(map[string]string{"OCI_TOKEN_FILE": "/x"}))); err == nil ||
		!strings.Contains(err.Error(), "--oci-identity-domain-url") {
		t.Errorf("a federation without its domain: %v", err)
	}
}

func TestNeedNamesWhatIsMissing(t *testing.T) {
	got, err := ocienv.Need(env(map[string]string{"A": "1", "B": " "}), "A", "B", "C")
	if err == nil || got != nil || !strings.Contains(err.Error(), "B, C") {
		t.Errorf("%v %v", got, err)
	}
	got, err = ocienv.Need(env(map[string]string{"A": " 1 "}), "A")
	if err != nil || got["A"] != "1" {
		t.Errorf("%v %v", got, err)
	}
}
