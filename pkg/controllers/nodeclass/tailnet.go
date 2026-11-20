/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package nodeclass

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
)

// Tailnet backends of the controller (--tailnet-backend).
const (
	TailnetBackendNone      = "none"
	TailnetBackendTailscale = "tailscale"
	TailnetBackendHeadscale = "headscale"
)

// TailnetOptions is the controller-wide tailnet backend that mints per-node keys.
// The backend and its credentials are never per NodeClass: a NodeClass editor must
// not be able to send the controller's credentials to a URL of their choosing.
type TailnetOptions struct {
	// Backend is none (the zero value), tailscale or headscale.
	Backend string
	// APIURL is the backend's base URL (--tailnet-api-url).
	APIURL string
	// CredentialsDir is --tailnet-credentials-dir.
	CredentialsDir string
}

// checkTailnetKeys refuses a keySource PerNode NodeClass the controller cannot
// serve: no backend or credentials, a loginServer that is not the backend's, no
// tags. It calls no API: bad credentials show at the first launch.
func (c *Controller) checkTailnetKeys(nc *v1alpha1.SCNodeClass) error {
	ts := nc.Spec.Network.Tailscale
	if !ts.PerNode() {
		return nil
	}
	t := c.opts.Tailnet
	switch {
	case t.Backend == "" || t.Backend == TailnetBackendNone:
		return errors.New("keySource PerNode needs a tailnet backend: the controller runs with --tailnet-backend none")
	case t.Backend != TailnetBackendTailscale && t.Backend != TailnetBackendHeadscale:
		return fmt.Errorf("keySource PerNode: unknown --tailnet-backend %q", t.Backend)
	case t.CredentialsDir == "":
		return fmt.Errorf("keySource PerNode needs the %s credentials: the controller runs without --tailnet-credentials-dir",
			t.Backend)
	case len(ts.Tags) == 0:
		return errors.New("keySource PerNode needs network.tailscale.tags: the keys are tagged")
	}
	return matchLoginServer(t, ts.LoginServer)
}

// matchLoginServer checks that the NodeClass's loginServer is the backend's: empty
// for tailscale; for headscale the same scheme, host and port (and path) as the
// API URL, a trailing slash ignored.
func matchLoginServer(t TailnetOptions, loginServer string) error {
	if t.Backend == TailnetBackendTailscale {
		if loginServer != "" {
			return fmt.Errorf("keySource PerNode: loginServer %q does not match the controller's tailnet backend "+
				"(tailscale: loginServer must be empty)", loginServer)
		}
		return nil
	}
	want, _ := normalizeURL(t.APIURL)
	if got, ok := normalizeURL(loginServer); !ok || got != want {
		return fmt.Errorf("keySource PerNode: loginServer %q does not match the controller's tailnet backend "+
			"(headscale at %s)", loginServer, t.APIURL)
	}
	return nil
}

// normalizeURL reduces an http or https URL to scheme, host, port and path in a
// comparable form: lower-case scheme and host, the default port dropped, no
// trailing slash. It refuses (false) what could make two different servers look
// alike or carry more than an address: no host, a host that is not ASCII, user
// information, a query or a fragment.
func normalizeURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || !isASCII(u.Host) {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" && !(u.Scheme == "http" && port == "80") && !(u.Scheme == "https" && port == "443") {
		host += ":" + port
	}
	return u.Scheme + "://" + host + strings.TrimRight(u.EscapedPath(), "/"), true
}

func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] > 0x7f {
			return false
		}
	}
	return true
}
