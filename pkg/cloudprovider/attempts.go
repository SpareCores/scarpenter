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
	// maxEventBytes is a safety cap on the message. Events made through the
	// client-go recorder (core/v1, no EventTime) are not length-validated by the
	// apiserver. If core moves to events.k8s.io (mgr.GetEventRecorder), messages
	// over 1024 bytes are rejected and this must change.
	maxEventBytes = 8 << 10
	// classLaunched is the class of the candidate that launched.
	classLaunched = "Launched"
)

// launchAttempt is how one candidate of a Create ended.
type launchAttempt struct {
	region, zone, capacityType, instanceType string
	class, code, message                     string
}

// recordAttempt notes the outcome of a vendor call of the candidate; a nil err
// is the launch that won, which is kept apart so it is always reported.
func (st *launchState) recordAttempt(cand *candidate, class vendors.Class, err error) {
	st.attemptsTotal++
	a := launchAttempt{
		region: cand.region, zone: cand.zone, capacityType: cand.capacityType, instanceType: cand.typeName,
		class: classLaunched,
	}
	if err == nil {
		st.winner = &a
		return
	}
	if len(st.attemptLog) >= maxRecordedAttempts {
		return
	}
	a.class, a.code, a.message = string(class), vendors.CodeOf(err), err.Error()
	if ve, ok := errors.AsType[*vendors.Error](err); ok {
		a.message = ve.Err.Error() // without the "<Class> (<Code>): " prefix, which has its own fields
	}
	st.attemptLog = append(st.attemptLog, a)
}

// formatAttempts renders the attempts of a Create as an Event message: a header
// line, then one line per failed candidate in the order tried:
//
//	launch-attempts v1 tried=<total> shown=<n>[ truncated][ launched=<region>|<zone>|<capacityType>|<instanceType>]
//	<region>|<zone>|<capacityType>|<instanceType>|<class>|<code>|<message>
//
// The winner of a launch that succeeded is in the header only, and counts in
// tried. Failures after the first maxRecordedAttempts are not recorded, and lines
// that do not fit maxEventBytes are dropped from the end; the header says so
// with "truncated".
func formatAttempts(failed []launchAttempt, winner *launchAttempt, total int) string {
	lines := make([]string, len(failed))
	for i, a := range failed {
		lines[i] = strings.Join([]string{
			attemptField(a.region, 0), attemptField(a.zone, 0), attemptField(a.capacityType, 0),
			attemptField(a.instanceType, 0), attemptField(a.class, 0), attemptField(a.code, 0),
			attemptField(a.message, maxAttemptsMessage),
		}, "|")
	}
	launched := ""
	if winner != nil {
		launched = " launched=" + strings.Join([]string{
			attemptField(winner.region, 0), attemptField(winner.zone, 0), attemptField(winner.capacityType, 0),
			attemptField(winner.instanceType, 0),
		}, "|")
	}
	wonCount := 0
	if winner != nil {
		wonCount = 1
	}
	for shown := len(lines); ; shown-- {
		header := fmt.Sprintf("launch-attempts v1 tried=%d shown=%d", total, shown+wonCount)
		if shown+wonCount < total {
			header += " truncated"
		}
		msg := strings.Join(append([]string{header + launched}, lines[:shown]...), "\n")
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
		Message:        formatAttempts(st.attemptLog, st.winner, st.attemptsTotal),
	})
}
