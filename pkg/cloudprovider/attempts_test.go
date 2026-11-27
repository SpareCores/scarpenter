/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func testAttempt(i int, class string) launchAttempt {
	return launchAttempt{
		region: "us-east-1", zone: fmt.Sprintf("us-east-1%c", 'a'+rune(i%6)), capacityType: "spot",
		instanceType: fmt.Sprintf("aws.m7i.%dxlarge", i), class: class, code: "InsufficientInstanceCapacity",
		message: "We currently do not have sufficient capacity",
	}
}

func TestFormatAttemptsFailure(t *testing.T) {
	got := formatAttempts([]launchAttempt{testAttempt(1, "InsufficientCapacity"), testAttempt(2, "QuotaExceeded")}, nil, 2)
	want := "launch-attempts v1 tried=2 shown=2\n" +
		"us-east-1|us-east-1b|spot|aws.m7i.1xlarge|InsufficientCapacity|InsufficientInstanceCapacity|We currently do not have sufficient capa\n" +
		"us-east-1|us-east-1c|spot|aws.m7i.2xlarge|QuotaExceeded|InsufficientInstanceCapacity|We currently do not have sufficient capa"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestFormatAttemptsSuccessHasTheWinnerInTheHeader(t *testing.T) {
	win := testAttempt(2, classLaunched)
	got := formatAttempts([]launchAttempt{testAttempt(1, "InsufficientCapacity")}, &win, 2)
	header, _, _ := strings.Cut(got, "\n")
	if want := "launch-attempts v1 tried=2 shown=2 launched=us-east-1|us-east-1c|spot|aws.m7i.2xlarge"; header != want ||
		strings.Count(got, "\n") != 1 {
		t.Errorf("got\n%s", got)
	}
}

func TestFormatAttemptsCleansFields(t *testing.T) {
	a := testAttempt(1, "Fatal")
	a.message = "bad | thing\nsecond  line"
	got := formatAttempts([]launchAttempt{a}, nil, 1)
	if !strings.HasSuffix(got, "|bad / thing second line") || strings.Count(got, "\n") != 1 {
		t.Errorf("got %q", got)
	}
}

func TestFormatAttemptsCutsMultibyteTextByRunes(t *testing.T) {
	a := testAttempt(1, "Fatal")
	a.message = strings.Repeat("é", 100)
	got := formatAttempts([]launchAttempt{a}, nil, 1)
	_, line, _ := strings.Cut(got, "\n")
	msg := line[strings.LastIndex(line, "|")+1:]
	if !utf8.ValidString(got) || msg != strings.Repeat("é", maxAttemptsMessage) {
		t.Errorf("message %q", msg)
	}
}

func TestFormatAttemptsDropsLinesPastTheSizeCapButKeepsTheWinner(t *testing.T) {
	failed := make([]launchAttempt, 0, maxRecordedAttempts)
	for i := range maxRecordedAttempts {
		a := testAttempt(i, "InsufficientCapacity")
		a.message = strings.Repeat("x", 500)
		a.code = strings.Repeat("c", 400)
		failed = append(failed, a)
	}
	win := testAttempt(77, classLaunched)
	got := formatAttempts(failed, &win, 80)
	if len(got) > maxEventBytes {
		t.Errorf("%d bytes, want at most %d", len(got), maxEventBytes)
	}
	header, _, _ := strings.Cut(got, "\n")
	lines := strings.Count(got, "\n")
	want := fmt.Sprintf("launch-attempts v1 tried=80 shown=%d truncated launched=us-east-1|us-east-1f|spot|aws.m7i.77xlarge", lines+1)
	if lines == 0 || lines >= maxRecordedAttempts || header != want {
		t.Errorf("header %q, %d lines, want header %q", header, lines, want)
	}
	if !strings.Contains(got, "aws.m7i.0xlarge") || strings.Contains(got, "aws.m7i.49xlarge") {
		t.Errorf("the kept lines are not the first ones")
	}
}

func TestFormatAttemptsNotTruncatedWhenTheyFit(t *testing.T) {
	if got := formatAttempts([]launchAttempt{testAttempt(1, "Fatal")}, nil, 1); strings.Contains(got, "truncated") {
		t.Errorf("got %q", got)
	}
}

func TestRecordAttemptKeepsTheFirstFiftyFailuresAndAlwaysTheWinner(t *testing.T) {
	st := &launchState{}
	cand := &candidate{region: "r", zone: "z", capacityType: "spot", typeName: "aws.m7i.large"}
	for range maxRecordedAttempts + 10 {
		st.recordAttempt(cand, vendors.InsufficientCapacity, vendors.Errorf(vendors.InsufficientCapacity, "full"))
	}
	st.recordAttempt(cand, "", nil)
	if len(st.attemptLog) != maxRecordedAttempts || st.attemptsTotal != maxRecordedAttempts+11 {
		t.Errorf("%d recorded, %d total", len(st.attemptLog), st.attemptsTotal)
	}
	if st.winner == nil || st.winner.class != classLaunched {
		t.Errorf("winner = %+v", st.winner)
	}
}
