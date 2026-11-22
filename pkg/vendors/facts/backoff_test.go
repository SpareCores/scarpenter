/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package facts

import (
	"testing"
	"time"
)

func TestBackoffDoublesFromTenSecondsToTheCap(t *testing.T) {
	want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second,
		160 * time.Second, 320 * time.Second, 640 * time.Second, 15 * time.Minute, 15 * time.Minute}
	for i, w := range want {
		if got := backoffDelay(i + 1); got != w {
			t.Errorf("attempt %d: %s, want %s", i+1, got, w)
		}
	}
	if got := backoffDelay(1000); got != 15*time.Minute {
		t.Errorf("attempt 1000: %s", got)
	}
}
