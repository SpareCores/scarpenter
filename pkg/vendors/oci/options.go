/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import (
	"context"
	"fmt"

	operatoroptions "github.com/SpareCores/scarpenter/pkg/operator/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func init() {
	vendors.Register(Name, func() vendors.Driver { return New() })
}

// CredentialConfigFromOptions is the controller's identity configuration
// (--oci-auth and the source flags).
func CredentialConfigFromOptions(o *operatoroptions.Options) CredentialConfig {
	return CredentialConfig{
		Mode: o.OCIAuth, SecretDir: o.OCICredentialsDir, ConfigFile: o.OCIConfigFile, Profile: o.OCIConfigProfile,
		TokenFile: o.OCITokenFile, IdentityDomain: o.OCIIdentityDomain, ClientID: o.OCIClientID,
		ClientSecretFile: o.OCIClientSecretFile, Region: o.OCIRegion,
	}
}

// applyContextOptions fills what the driver was not given by an Option from the
// controller's options in the context: the compartment (--oci-compartment) and,
// unless clients are set, the credentials. Init calls it first.
func (d *Driver) applyContextOptions(ctx context.Context) error {
	o := operatoroptions.FromContext(ctx)
	if o == nil {
		return nil
	}
	if d.compartment == "" {
		d.compartment = o.OCICompartment
	}
	if d.api.Compute == nil && d.provider == nil {
		p, err := NewConfigurationProvider(CredentialConfigFromOptions(o))
		if err != nil {
			return fmt.Errorf("the OCI credentials (see docs/how-to-guides/oci-credentials.md): %w", err)
		}
		d.provider = p
	}
	return nil
}
