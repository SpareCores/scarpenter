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
	// minInstances is the number of servers the suite needs at once, for the vendors whose
	// quota counts instances (E2E_MIN_INSTANCES, optional).
	minInstances int64
	// minCores and minMemoryMB are the account limits the UpCloud suite needs (cores and
	// memory in MB; UpCloud's limits are account-wide, and have no spot).
	minCores, minMemoryMB int64

	// flatcarImageFile is the file the vendor's images are made from, which the canary checks on
	// the Flatcar release server (hcloud: the Hetzner raw image); empty: nothing to check.
	flatcarImageFile string
	// subscription is the Azure subscription ID (AZURE_SUBSCRIPTION_ID); empty for the other vendors.
	subscription string

	sysextBase string
	k3sBase    string
	// flatcarBase is the Flatcar release server, with {channel}; tests replace it.
	flatcarBase string
}

// vendorEnv maps a vendor to the environment variable of its region.
// For hcloud it is the comma-separated list of the lane's locations.
var vendorEnv = map[string]string{
	vendorAlicloud: "ALICLOUD_REGION", vendorAWS: "AWS_REGION", vendorAzure: "AZURE_REGION",
	vendorHCloud: "E2E_HCLOUD_LOCATIONS", vendorOVH: "OS_REGION_NAME",
	vendorUpCloud: "UPCLOUD_ZONE", vendorVultr: "VULTR_REGION",
}

// The vendors of the canary and the architecture its checks name.
const (
	vendorAlicloud = "alicloud"
	vendorAWS      = "aws"
	vendorHCloud   = "hcloud"
	archAMD64      = "amd64"
	archARM64      = "arm64"
)

// typeAMD64 is the variable of the amd64 type every lane pins.
const typeAMD64 = "E2E_TYPE_AMD64"

// typeVars are the variables of the instance types a lane pins, where a vendor's differ from
// AWS's (Vultr has no Arm instances and no database type, OVH no Arm, UpCloud's lane one
// architecture and no database pool); noQuota are the vendors without a
// readable quota (the vCPU pins are then not needed; Hetzner's limits cannot be read).
var (
	typeVars = map[string][]string{
		vendorOVH:     {typeAMD64, "E2E_TYPE_DB"},
		vendorUpCloud: {typeAMD64},
		vendorVultr:   {typeAMD64, "E2E_TYPE_SECOND"},
	}
	noQuota = map[string]bool{vendorHCloud: true, vendorVultr: true}
)

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
	if vendor == vendorAzure {
		c.subscription = need("AZURE_SUBSCRIPTION_ID")
	}
	c.flatcarChannel = need("E2E_FLATCAR_CHANNEL")
	c.flatcarVersion = need("E2E_FLATCAR_VERSION")
	c.kubernetesVersion = need("E2E_KUBERNETES_VERSION")
	c.tailscaleVersion = need("E2E_TAILSCALE_VERSION")
	c.k3sVersion = need("E2E_K3S_VERSION")
	names, ok := typeVars[vendor]
	if !ok {
		names = []string{typeAMD64, "E2E_TYPE_ARM64", "E2E_TYPE_DB"}
	}
	for _, name := range names {
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
	switch {
	case vendor == vendorUpCloud: // account-wide cores and memory, no spot
		if c.minCores, err = intEnv(getenv, "E2E_MIN_CORES", &missing); err != nil {
			return nil, err
		}
		if c.minMemoryMB, err = intEnv(getenv, "E2E_MIN_MEMORY_MB", &missing); err != nil {
			return nil, err
		}
	case !noQuota[vendor]:
		if c.minOnDemand, err = intEnv(getenv, "E2E_MIN_VCPU_OD", &missing); err != nil {
			return nil, err
		}
		if vendor != vendorOVH { // no spot on OVH
			if c.minSpot, err = intEnv(getenv, "E2E_MIN_VCPU_SPOT", &missing); err != nil {
				return nil, err
			}
		}
	}
	if vendor == vendorHCloud {
		c.flatcarImageFile = "flatcar_production_hetzner_image.bin.bz2"
	}
	if v := strings.TrimSpace(getenv("E2E_MIN_INSTANCES")); v != "" {
		n, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil || n < 0 {
			return nil, fmt.Errorf("E2E_MIN_INSTANCES=%q is not a number of instances", v)
		}
		c.minInstances = n
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
