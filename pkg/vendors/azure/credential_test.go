/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewCredential(t *testing.T) {
	_, err := NewCredential("magic", false)
	require.Error(t, err)

	// The managed identity member needs no configuration, so the chain exists
	// even where workload identity is not configured.
	c, err := NewCredential(ModeWorkloadIdentity, false)
	require.NoError(t, err)
	require.NotNil(t, c)

	f := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(f, []byte("jwt"), 0o600))
	t.Setenv("AZURE_CLIENT_ID", "00000000-0000-0000-0000-000000000000")
	t.Setenv("AZURE_TENANT_ID", "00000000-0000-0000-0000-000000000001")
	t.Setenv("AZURE_FEDERATED_TOKEN_FILE", f)
	c, err = NewCredential("", true)
	require.NoError(t, err)
	require.NotNil(t, c)

	t.Setenv("AZURE_CLIENT_SECRET", "not-a-real-secret")
	c, err = NewCredential(ModeSecret, false)
	require.NoError(t, err)
	require.NotNil(t, c)
}
