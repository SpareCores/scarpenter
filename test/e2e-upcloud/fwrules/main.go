/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Command fwrules prints the Public and Utility firewall rules the driver writes for a
// node of the lane (the tailscale network template, no inbound rules in the NodeClass), as
// the JSON the API answers a firewall rule list with. The publicfw scenario compares it
// with the rules it reads from the server, so a change of the driver's list is noticed.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud"
)

func main() {
	rules, err := upcloud.PublicFirewallRules(upcloud.FirewallOptions{Tailscale: true})
	if err != nil {
		fmt.Fprintln(os.Stderr, "fwrules:", err)
		os.Exit(1)
	}
	out := map[string]any{"firewall_rules": map[string]any{"firewall_rule": rules}}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, "fwrules:", err)
		os.Exit(1)
	}
}
