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
)

func testAttempt(i int, class string) launchAttempt {
	return launchAttempt{
		region: "us-east-1", zone: fmt.Sprintf("us-east-1%c", 'a'+rune(i%6)), capacityType: "spot",
		instanceType: fmt.Sprintf("aws.m7i.%dxlarge", i), class: class, code: "InsufficientInstanceCapacity",
		message: "We currently do not have sufficient capacity",
	}
}

func TestFormatAttemptsFailure(t *testing.T) {
	got := formatAttempts([]launchAttempt{testAttempt(1, "InsufficientCapacity"), testAttempt(2, "QuotaExceeded")}, 2)
	want := "launch-attempts v1 tried=2 shown=2\n" +
		"us-east-1|us-east-1b|spot|aws.m7i.1xlarge|InsufficientCapacity|InsufficientInstanceCapacity|We currently do not have sufficient capa\n" +
		"us-east-1|us-east-1c|spot|aws.m7i.2xlarge|QuotaExceeded|InsufficientInstanceCapacity|We currently do not have sufficient capa"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestFormatAttemptsSuccessEndsWithTheWinner(t *testing.T) {
	win := testAttempt(2, classLaunched)
	win.code, win.message = "", ""
	got := formatAttempts([]launchAttempt{testAttempt(1, "InsufficientCapacity"), win}, 2)
	lines := strings.Split(got, "\n")
	if len(lines) != 3 || lines[2] != "us-east-1|us-east-1c|spot|aws.m7i.2xlarge|Launched||" {
		t.Errorf("got\n%s", got)
	}
}

func TestFormatAttemptsCleansFields(t *testing.T) {
	a := testAttempt(1, "Fatal")
	a.message = "bad | thing\nsecond  line"
	got := formatAttempts([]launchAttempt{a}, 1)
	if !strings.HasSuffix(got, "|bad / thing second line") || strings.Count(got, "\n") != 1 {
		t.Errorf("got %q", got)
	}
}

func TestFormatAttemptsBounds(t *testing.T) {
	many := make([]launchAttempt, 0, maxRecordedAttempts)
	for i := range maxRecordedAttempts {
		many = append(many, testAttempt(i, "InsufficientCapacity"))
	}
	got := formatAttempts(many, 80)
	if len(got) > maxEventBytes {
		t.Errorf("%d bytes, want at most %d", len(got), maxEventBytes)
	}
	header, _, _ := strings.Cut(got, "\n")
	shown := strings.Count(got, "\n")
	if want := fmt.Sprintf("launch-attempts v1 tried=80 shown=%d truncated", shown); header != want || shown == 0 || shown >= 50 {
		t.Errorf("header %q, %d lines", header, shown)
	}
	// The lines kept are the first ones, whole.
	if !strings.Contains(got, "aws.m7i.0xlarge") || strings.Contains(got, "aws.m7i.49xlarge") {
		t.Errorf("got\n%s", got)
	}
}

func TestFormatAttemptsNotTruncatedWhenTheyFit(t *testing.T) {
	got := formatAttempts([]launchAttempt{testAttempt(1, "Fatal")}, 1)
	if strings.Contains(got, "truncated") {
		t.Errorf("got %q", got)
	}
}
