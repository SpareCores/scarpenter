/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package tailnet

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DirTailscaleCredentials reads the OAuth client from the files client-id and client-secret
// of a mounted Secret. It reads at every call, so a rotated Secret is picked up.
func DirTailscaleCredentials(dir string) func() (string, Secret, error) {
	return func() (string, Secret, error) {
		id, err := readCredential(dir, "client-id")
		if err != nil {
			return "", "", err
		}
		secret, err := readCredential(dir, "client-secret")
		return id, Secret(secret), err
	}
}

// DirHeadscaleAPIKey reads the Headscale API key from the file api-key of a mounted Secret,
// at every call.
func DirHeadscaleAPIKey(dir string) func() (Secret, error) {
	return func() (Secret, error) {
		key, err := readCredential(dir, "api-key")
		return Secret(key), err
	}
}

func readCredential(dir, name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", fmt.Errorf("reading credential %s: %w", name, err)
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("credential %s is empty", name)
	}
	return v, nil
}
