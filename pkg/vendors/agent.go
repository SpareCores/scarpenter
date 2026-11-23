/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vendors

import (
	"fmt"
	"strings"
	"time"
)

// NoticeCheck is how a node finds out that its vendor is about to take it away
// (plan 26 section 2): a shell function the node agent runs.
type NoticeCheck struct {
	// Script is the body of the POSIX sh function scarpenter_check. It prints
	// "<reason> <deadline>" and returns 0 when a notice is present (the reason is
	// SpotInterrupted, the deadline RFC 3339 and may be left out), 1 when there is
	// none and 2 on a transient error (the vendor's endpoint did not answer). It
	// may use curl, and the variable NODE_NAME (the node's name). An empty
	// Script means the driver has no check: the node gets no agent.
	Script string
	// Interval is the pause after a check that found no notice, in whole seconds
	// (rounded up). Zero for a check that blocks until something changes.
	Interval time.Duration
	// NoticeWindow is the lead time the vendor gives: the interruption controller
	// clamps the deadline to it, and a notice without a usable deadline gets
	// MinNotice.
	NoticeWindow
}

// InterruptionNotices is the optional capability of a driver whose vendor
// announces the loss of a spot or preemptible instance in the instance
// metadata. The core renders the node agent with the driver's check on spot
// launches (InterruptionAgent). A driver without it gets no agent.
type InterruptionNotices interface {
	InterruptionNotices() NoticeCheck
}

// The agent's files and unit. The unit runs in the metadata slice (ADR-025), so
// it reaches the instance metadata service when the restriction is rendered.
const (
	agentScriptPath = "/opt/scarpenter/bin/scarpenter-interruption-agent"
	agentUnit       = "scarpenter-interruption.service"
)

// agentLoop is the generic part of the agent. It reports on the node's own Node
// with the kubelet's client certificate (NodeRestriction lets a node change only
// its own Node), once the certificate exists. The reason and the deadline come
// from the vendor's script and are reduced to safe characters before they go
// into the JSON; the interruption controller clamps the deadline into the
// vendor's notice window. A restart finds the annotation and does not patch
// again. A failing patch (the Node not registered yet, or refused for good) is
// logged once and retried with a backoff of up to 8 s. The script is kept
// short: it is part of the user-data budget.
const agentLoop = `scarpenter_check() {
%s
}
c=/var/lib/kubelet/pki/kubelet-client-current.pem
n=$API_SERVER/api/v1/nodes/$NODE_NAME
a=scarpenter.sparecores.com
k() { curl -fs --max-time 10 --cacert /etc/kubernetes/pki/ca.crt --cert $c "$@"; }
while :; do
 o=$(scarpenter_check)
 case $? in
 0)
  set -f
  set -- $o
  r=$(printf %%s "${1:-SpotInterrupted}" | tr -cd A-Za-z0-9)
  d=$(printf %%s "$2" | tr -cd 0-9A-Za-z:.+-)
  s=1
  until [ -r $c ] && { k "$n" | grep -q $a/interruption-deadline ||
   k -o /dev/null -X PATCH -H 'Content-Type: application/merge-patch+json' "$n" -d \
   "{\"metadata\":{\"annotations\":{\"$a/interruption\":\"$r\",\
\"$a/interruption-deadline\":\"$d\",\"$a/interruption-source\":\"instance-metadata\"}}}"; }; do
   [ -r $c ] && [ -z "$e" ] && echo "annotating the Node failed, retrying" && e=1
   sleep $s
   [ $s -lt 8 ] && s=$((s*2))
  done
  echo "$r: node annotated, deadline $d"
  exec sleep infinity ;;
 1) f=; sleep %d ;;
 *) [ -n "$f" ] || echo "the notice check failed"; f=1; sleep 5 ;;
 esac
done
`

// InterruptionAgentScript returns the node agent's script for the check: the
// generic loop around the vendor's function. It reads NODE_NAME and API_SERVER
// (the API server's URL) from the environment.
func InterruptionAgentScript(c NoticeCheck) (string, error) {
	body := strings.Trim(c.Script, "\n")
	switch {
	case strings.TrimSpace(body) == "":
		return "", fmt.Errorf("the notice check has no script")
	case strings.Contains(body, "{{"):
		// The fragment is rendered as a template.
		return "", fmt.Errorf("the notice check contains {{")
	case c.Interval < 0:
		return "", fmt.Errorf("the notice check has a negative interval")
	}
	seconds := int((c.Interval + time.Second - 1) / time.Second)
	return "#!/bin/sh\n" + fmt.Sprintf(agentLoop, body, seconds), nil
}

// agentParts are the entries the agent adds to a Butane config: items of
// storage.files and of systemd.units, indented for those lists.
type agentParts struct{ files, units string }

// agentPartsOf builds the parts of the driver's agent, and false when the driver
// has no notice check. restricted says that the metadata restriction is
// rendered: the agent then starts after its firewall.
func agentPartsOf(d Driver, restricted bool) (agentParts, bool, error) {
	n, ok := d.(InterruptionNotices)
	if !ok {
		return agentParts{}, false, nil
	}
	check := n.InterruptionNotices()
	if strings.TrimSpace(check.Script) == "" {
		return agentParts{}, false, nil
	}
	script, err := InterruptionAgentScript(check)
	if err != nil {
		return agentParts{}, false, fmt.Errorf("%s: %w", d.Vendor(), err)
	}
	order := "network-online.target"
	if restricted {
		order += " " + metadataFirewall
	}
	var f, u strings.Builder
	fmt.Fprintf(&f, "    - path: %s\n      mode: 0755\n      contents:\n        inline: |\n", agentScriptPath)
	for line := range strings.SplitSeq(strings.TrimRight(script, "\n"), "\n") {
		if line == "" {
			f.WriteString("\n")
			continue
		}
		f.WriteString("          " + line + "\n")
	}
	fmt.Fprintf(&u, "    - name: %s\n      enabled: true\n      contents: |\n", agentUnit)
	u.WriteString("        [Unit]\n        Description=Spot interruption notice\n")
	fmt.Fprintf(&u, "        Wants=%s\n        After=%s\n", order, order)
	fmt.Fprintf(&u, "        [Service]\n        Slice=%s\n        ExecStart=%s\n", metadataSlice, agentScriptPath)
	u.WriteString("        Environment=NODE_NAME={{ .NodeName }} API_SERVER={{ .APIServerEndpoint }}\n")
	u.WriteString("        Restart=always\n        RestartSec=1\n        [Install]\n        WantedBy=multi-user.target\n")
	return agentParts{files: f.String(), units: u.String()}, true, nil
}

// InterruptionAgent returns the fragment of the node agent of a spot node (plan
// 26 section 2) as a fragment of its own, and false when the driver has no notice
// check. BootFragmentsOf puts it into the metadata restriction's fragment when
// there is one, which saves a part of the user-data. The unit starts at boot,
// before the kubelet has a certificate (some vendors need an early first metadata
// call), and reports once the certificate exists.
func InterruptionAgent(d Driver, restricted bool) (BootFragment, bool, error) {
	parts, ok, err := agentPartsOf(d, restricted)
	if !ok || err != nil {
		return BootFragment{}, false, err
	}
	return BootFragment{Name: "interruption-agent", Butane: "variant: flatcar\nversion: 1.1.0\nstorage:\n  files:\n" +
		parts.files + "systemd:\n  units:\n" + parts.units}, true, nil
}
