/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// EventLaunchAttempts is published on the NodeClaim once per Create call that
// made a vendor call: how each tried candidate ended.
const EventLaunchAttempts = "LaunchAttempts"

const (
	// maxRecordedAttempts is how many candidates of one Create are recorded.
	maxRecordedAttempts = 50
	// maxAttemptsMessage is the length of a candidate's vendor message in runes.
	maxAttemptsMessage = 40
	// maxEventBytes is the Kubernetes limit of an Event's message.
	maxEventBytes = 1024
	// classLaunched is the class of the candidate that launched.
	classLaunched = "Launched"
)

// launchAttempt is how one candidate of a Create ended.
type launchAttempt struct {
	region, zone, capacityType, instanceType string
	class, code, message                     string
}

// recordAttempt notes the outcome of a vendor call of the candidate; a nil err
// is the launch that won.
func (st *launchState) recordAttempt(cand *candidate, err error) {
	st.attemptsTotal++
	if len(st.attemptLog) >= maxRecordedAttempts {
		return
	}
	a := launchAttempt{
		region: cand.region, zone: cand.zone, capacityType: cand.capacityType, instanceType: cand.typeName,
		class: classLaunched,
	}
	if err != nil {
		a.class, a.code, a.message = string(vendors.ClassOf(err)), vendors.CodeOf(err), err.Error()
		if ve, ok := errors.AsType[*vendors.Error](err); ok {
			a.message = ve.Err.Error() // without the "<Class> (<Code>): " prefix, which has its own fields
		}
	}
	st.attemptLog = append(st.attemptLog, a)
}

// formatAttempts renders the attempts of a Create as an Event message: a header
// line, then one line per candidate in the order tried:
//
//	launch-attempts v1 tried=<total> shown=<n>[ truncated]
//	<region>|<zone>|<capacityType>|<instanceType>|<class>|<code>|<message>
//
// Candidates after the first maxRecordedAttempts are not recorded, and lines
// that do not fit maxEventBytes are dropped from the end; the header says so
// with "truncated".
func formatAttempts(attempts []launchAttempt, total int) string {
	lines := make([]string, len(attempts))
	for i, a := range attempts {
		lines[i] = strings.Join([]string{
			attemptField(a.region, 0), attemptField(a.zone, 0), attemptField(a.capacityType, 0),
			attemptField(a.instanceType, 0), attemptField(a.class, 0), attemptField(a.code, 0),
			attemptField(a.message, maxAttemptsMessage),
		}, "|")
	}
	for shown := len(lines); ; shown-- {
		header := fmt.Sprintf("launch-attempts v1 tried=%d shown=%d", total, shown)
		if shown < total {
			header += " truncated"
		}
		msg := strings.Join(append([]string{header}, lines[:shown]...), "\n")
		if len(msg) <= maxEventBytes || shown == 0 {
			return msg
		}
	}
}

// attemptField makes s fit one field of a line: no separators or line breaks,
// at most limit runes (0 is no limit).
func attemptField(s string, limit int) string {
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "|", "/")), " ")
	if limit > 0 && utf8.RuneCountInString(s) > limit {
		s = string([]rune(s)[:limit])
	}
	return s
}

// publishAttempts publishes the LaunchAttempts event of the Create on the
// NodeClaim. It is not deduplicated: every Create call has its own.
func (c *CloudProvider) publishAttempts(nodeClaim *karpv1.NodeClaim, st *launchState, failed bool) {
	if c.opts.Recorder == nil || st.attemptsTotal == 0 {
		return
	}
	typ := corev1.EventTypeNormal
	if failed {
		typ = corev1.EventTypeWarning
	}
	c.opts.Recorder.Publish(events.Event{
		InvolvedObject: nodeClaim,
		Type:           typ,
		Reason:         EventLaunchAttempts,
		Message:        formatAttempts(st.attemptLog, st.attemptsTotal),
	})
}
