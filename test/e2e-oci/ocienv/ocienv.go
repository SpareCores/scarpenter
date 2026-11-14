/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package ocienv reads the identity and the account of the OCI lane from the environment, for
// the Go programs of the lane (the sweeper and the canary): the variables the workflows and
// oci-session.sh export, and nothing else. In CI the identity is the keyless token exchange
// (OCI_TOKEN_FILE, the GitHub OIDC token that oci-session.sh keeps fresh); on a developer's
// machine it is an OCI config file profile (OCI_CLI_CONFIG_FILE and OCI_CLI_PROFILE, or
// ~/.oci/config's DEFAULT; session-token profiles work) or the OCI_CLI_* variables.
package ocienv

import (
	"fmt"
	"strings"

	"github.com/SpareCores/scarpenter/pkg/vendors/oci"
)

// The variables.
const (
	// Compartment is the OCID of the lane's compartment (a dedicated one).
	Compartment = "OCI_COMPARTMENT"
	// Tenancy is the OCID of the tenancy.
	Tenancy = "OCI_TENANCY"
	// Region is the region of the lane.
	Region = "OCI_REGION"
	// HomeRegion is the home region of the tenancy (the federation's region); the lane's
	// region when unset.
	HomeRegion = "OCI_HOME_REGION"
	// TokenFile selects the federation: the file with the token to exchange.
	TokenFile = "OCI_TOKEN_FILE"
	// IdentityDomainURL, ClientID and ClientSecretFile are the identity domain and its
	// confidential application.
	IdentityDomainURL = "OCI_IDENTITY_DOMAIN_URL"
	ClientID          = "OCI_CLIENT_ID"
	ClientSecretFile  = "OCI_CLIENT_SECRET_FILE"
	// ConfigFile and ConfigProfile are the OCI CLI's own variables.
	ConfigFile    = "OCI_CLI_CONFIG_FILE"
	ConfigProfile = "OCI_CLI_PROFILE"
)

// Credentials is the driver's credential configuration for the environment: the
// workloadIdentityFederation mode when OCI_TOKEN_FILE is set, else the apiKey mode (a config
// file when OCI_CLI_CONFIG_FILE is set, else the OCI_CLI_* variables).
func Credentials(getenv func(string) string) oci.CredentialConfig {
	c := oci.CredentialConfig{Getenv: getenv}
	if f := strings.TrimSpace(getenv(TokenFile)); f != "" {
		c.Mode = oci.AuthWorkloadIdentityFederation
		c.TokenFile = f
		c.IdentityDomain = strings.TrimSpace(getenv(IdentityDomainURL))
		c.ClientID = strings.TrimSpace(getenv(ClientID))
		c.ClientSecretFile = strings.TrimSpace(getenv(ClientSecretFile))
		c.Region = strings.TrimSpace(getenv(HomeRegion))
		if c.Region == "" {
			c.Region = strings.TrimSpace(getenv(Region))
		}
		return c
	}
	c.Mode = oci.AuthAPIKey
	c.ConfigFile = strings.TrimSpace(getenv(ConfigFile))
	c.Profile = strings.TrimSpace(getenv(ConfigProfile))
	return c
}

// Need returns the values of the variables, or an error naming the ones that are unset.
func Need(getenv func(string) string, names ...string) (map[string]string, error) {
	out := map[string]string{}
	var missing []string
	for _, n := range names {
		v := strings.TrimSpace(getenv(n))
		if v == "" {
			missing = append(missing, n)
		}
		out[n] = v
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("the variable(s) %s must be set (the lane's workflow exports them)",
			strings.Join(missing, ", "))
	}
	return out, nil
}
