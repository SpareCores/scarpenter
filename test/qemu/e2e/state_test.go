//go:build qemu && scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// instanceRecord is what the scenarios read of the driver's state directory.
type instanceRecord struct {
	ID       string `json:"id"`
	Phase    string `json:"phase"`
	NodeName string `json:"nodeName"`
	PID      int    `json:"pid"`
	MAC      string `json:"mac"`
	dir      string
}

func instanceRecords() []instanceRecord {
	dirs, _ := filepath.Glob(filepath.Join(lane.stateDir, "instances", "*"))
	var out []instanceRecord
	for _, d := range dirs {
		raw, err := os.ReadFile(filepath.Join(d, "instance.json"))
		if err != nil {
			continue
		}
		var rec instanceRecord
		if json.Unmarshal(raw, &rec) == nil {
			rec.dir = d
			out = append(out, rec)
		}
	}
	return out
}

// vmProcesses counts the QEMU processes of the lane's VMs: the instance
// records say what the driver believes, the processes what is true.
func vmProcesses(t *testing.T) int {
	t.Helper()
	out, _ := run(t, "pgrep", "-fc", "qemu-system-.* -name i-")
	n := 0
	for f := range strings.FieldsSeq(out) {
		if _, err := json.Number(f).Int64(); err == nil {
			v, _ := json.Number(f).Int64()
			n = int(v)
		}
	}
	return n
}

// vmAddress returns the address a VM got from the bridge's DHCP server, from
// dnsmasq's lease file by the VM's MAC (the node's own InternalIP is not the
// bridge address with every network template, and the ARP table of the driver only
// knows a VM that has talked to the control plane).
func vmAddress(claim string) string {
	var mac string
	for _, rec := range instanceRecords() {
		if rec.NodeName == claim {
			mac = rec.MAC
		}
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(lane.stateDir), "dnsmasq.leases"))
	if err != nil || mac == "" {
		return ""
	}
	// Each line: expiry, MAC, IP, hostname, client ID.
	for line := range strings.SplitSeq(string(raw), "\n") {
		if f := strings.Fields(line); len(f) >= 3 && strings.EqualFold(f[1], mac) {
			return f[2]
		}
	}
	return ""
}
