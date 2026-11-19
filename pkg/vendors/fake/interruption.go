//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"time"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// noticeWindows are the lead times of the vendors with spot instances (plan 26
// section 6), so that the fake impersonates the one it was told to. A vendor
// not listed (no spot) gets the default window.
var noticeWindows = map[string]vendors.NoticeWindow{
	"aws":      {MinNotice: 2 * time.Minute, MaxNotice: 2 * time.Minute},
	"gcp":      {MinNotice: 30 * time.Second, MaxNotice: 2*time.Minute + 30*time.Second},
	"azure":    {MinNotice: 30 * time.Second, MaxNotice: 30 * time.Second},
	"oci":      {MinNotice: 2 * time.Minute, MaxNotice: 2 * time.Minute},
	"alicloud": {MinNotice: 5 * time.Minute, MaxNotice: 5 * time.Minute},
}

// NoticeWindow implements vendors.InterruptionWindow: the window of the
// impersonated vendor, unless --fake-min-notice or --fake-max-notice set one.
func (d *Driver) NoticeWindow() vendors.NoticeWindow {
	w, ok := noticeWindows[d.Vendor()]
	if !ok {
		w = vendors.DefaultNoticeWindow
	}
	if d.opts.MinNotice > 0 {
		w.MinNotice = d.opts.MinNotice
	}
	if d.opts.MaxNotice > 0 {
		w.MaxNotice = d.opts.MaxNotice
	}
	return w
}

var _ vendors.InterruptionWindow = (*Driver)(nil)
