/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"errors"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

// Credential modes of the chart value vendors.azure.credentials.mode.
const (
	// ModeWorkloadIdentity is the recommended mode: the federated token file
	// (AZURE_CLIENT_ID, AZURE_TENANT_ID, AZURE_FEDERATED_TOKEN_FILE), then the
	// managed identity of AKS or an Azure VM.
	ModeWorkloadIdentity = "workloadIdentity"
	// ModeSecret reads a client secret or certificate from the environment
	// (AZURE_CLIENT_ID, AZURE_TENANT_ID, AZURE_CLIENT_SECRET or
	// AZURE_CLIENT_CERTIFICATE_PATH).
	ModeSecret = "secret"
)

// NewCredential builds the credential chain of plan 17 section 2: a
// WorkloadIdentityCredential, an EnvironmentCredential in mode secret, a
// ManagedIdentityCredential, and the Azure CLI only under the development
// flag. A member that cannot be configured from the environment is skipped, as
// azidentity's own default chain does; no token is requested here.
func NewCredential(mode string, development bool) (azcore.TokenCredential, error) {
	var chain []azcore.TokenCredential
	var errs []error
	add := func(c azcore.TokenCredential, err error) {
		if err != nil {
			errs = append(errs, err)
			return
		}
		chain = append(chain, c)
	}
	switch mode {
	case ModeWorkloadIdentity, "":
		add(azidentity.NewWorkloadIdentityCredential(nil))
	case ModeSecret:
		add(azidentity.NewEnvironmentCredential(nil))
	default:
		return nil, fmt.Errorf("unknown credentials mode %q (workloadIdentity or secret)", mode)
	}
	add(azidentity.NewManagedIdentityCredential(nil))
	if development {
		add(azidentity.NewAzureCLICredential(nil))
	}
	if len(chain) == 0 {
		return nil, fmt.Errorf("no Azure credential could be configured: %w", errors.Join(errs...))
	}
	return azidentity.NewChainedTokenCredential(chain, nil)
}
