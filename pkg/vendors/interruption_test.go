/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vendors_test

import (
	"testing"
	"time"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

type windowDriver struct {
	vendors.Driver
	w vendors.NoticeWindow
}

func (d windowDriver) NoticeWindow() vendors.NoticeWindow { return d.w }

func TestNoticeWindowOf(t *testing.T) {
	for name, tc := range map[string]struct {
		d    vendors.Driver
		want vendors.NoticeWindow
	}{
		"a driver without the capability": {plainDriver{}, vendors.DefaultNoticeWindow},
		"a declared window": {
			windowDriver{w: vendors.NoticeWindow{MinNotice: time.Minute, MaxNotice: 2 * time.Minute}},
			vendors.NoticeWindow{MinNotice: time.Minute, MaxNotice: 2 * time.Minute},
		},
		"a window with no minimum":        {windowDriver{}, vendors.DefaultNoticeWindow},
		"a window whose maximum is below": {windowDriver{w: vendors.NoticeWindow{MinNotice: time.Minute, MaxNotice: time.Second}}, vendors.DefaultNoticeWindow},
	} {
		t.Run(name, func(t *testing.T) {
			if got := vendors.NoticeWindowOf(tc.d); got != tc.want {
				t.Errorf("NoticeWindowOf = %+v, want %+v", got, tc.want)
			}
		})
	}
}
