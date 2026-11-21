/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"path/filepath"
	"strings"
	"testing"
)

// With the OIDC variables set, the credential is the OIDC role and its error
// is the one returned, not the instance metadata's.
func TestDefaultCredentialOIDCErrorIsSurfaced(t *testing.T) {
	t.Setenv("ALIBABA_CLOUD_ROLE_ARN", "acs:ram::1:role/r")
	t.Setenv("ALIBABA_CLOUD_OIDC_PROVIDER_ARN", "acs:ram::1:oidc-provider/p")
	t.Setenv("ALIBABA_CLOUD_OIDC_TOKEN_FILE", filepath.Join(t.TempDir(), "missing-token"))
	cred, err := defaultCredential()
	if err != nil {
		t.Fatal(err)
	}
	for range 2 { // the error stays the same on later calls
		_, err = cred.GetCredential()
		if err == nil {
			t.Fatal("want the OIDC token file error")
		}
		if !strings.Contains(err.Error(), "missing-token") || strings.Contains(err.Error(), "100.100.100.200") {
			t.Fatalf("error is not the OIDC one: %v", err)
		}
	}
}
