/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package main

import (
	"fmt"
	"strconv"
	"strings"
)

// config is what the lane pins (test/e2e-<vendor>/defaults.sh, exported by it and read
// from the environment here: one source of truth). The base URLs are for tests.
type config struct {
	vendor            string
	region            string
	flatcarChannel    string
	flatcarVersion    string
	kubernetesVersion string
	tailscaleVersion  string
	k3sVersion        string
	// types are the instance types the lane's NodeClasses and NodePools use, with the
	// vendor prefix (aws.t3a.small).
	types []string
	// minOnDemand and minSpot are the vCPUs of quota the suite needs.
	minOnDemand, minSpot int64

	sysextBase string
	k3sBase    string
}

// vendorEnv maps a vendor to the environment variable of its region.
var vendorEnv = map[string]string{"aws": "AWS_REGION"}

func loadConfig(vendor string, getenv func(string) string) (*config, error) {
	c := &config{vendor: vendor}
	var missing []string
	need := func(name string) string {
		v := strings.TrimSpace(getenv(name))
		if v == "" {
			missing = append(missing, name)
		}
		return v
	}
	if regionVar, ok := vendorEnv[vendor]; ok {
		c.region = need(regionVar)
	}
	c.flatcarChannel = need("E2E_FLATCAR_CHANNEL")
	c.flatcarVersion = need("E2E_FLATCAR_VERSION")
	c.kubernetesVersion = need("E2E_KUBERNETES_VERSION")
	c.tailscaleVersion = need("E2E_TAILSCALE_VERSION")
	c.k3sVersion = need("E2E_K3S_VERSION")
	for _, name := range []string{"E2E_TYPE_AMD64", "E2E_TYPE_ARM64", "E2E_TYPE_DB"} {
		c.types = append(c.types, need(name))
	}
	for t := range strings.SplitSeq(need("E2E_TYPES_DESCRIBED"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			c.types = append(c.types, t)
		}
	}
	if t := strings.TrimSpace(getenv("E2E_LOCAL_STORAGE_TYPE")); t != "" {
		c.types = append(c.types, t)
	}
	var err error
	if c.minOnDemand, err = intEnv(getenv, "E2E_MIN_VCPU_OD", &missing); err != nil {
		return nil, err
	}
	if c.minSpot, err = intEnv(getenv, "E2E_MIN_VCPU_SPOT", &missing); err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing %s: the pins come from test/e2e-%s/defaults.sh, run test/canary/run.sh %s",
			strings.Join(missing, ", "), vendor, vendor)
	}
	return c, nil
}

func intEnv(getenv func(string) string, name string, missing *[]string) (int64, error) {
	v := strings.TrimSpace(getenv(name))
	if v == "" {
		*missing = append(*missing, name)
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s=%q is not a number of vCPUs", name, v)
	}
	return n, nil
}
