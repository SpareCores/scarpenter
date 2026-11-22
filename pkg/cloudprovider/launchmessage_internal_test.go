/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider

import (
	"errors"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func TestLaunchMessageAlwaysHasACodeAndADocumentedClass(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{errors.New("boom"), "Fatal (Unknown): boom"},
		{vendors.Errorf(vendors.Fatal, "no code"), "Fatal (Unknown): no code"},
		{vendors.Errorf(vendors.Retryable, "slow"), "Retryable (Unknown): slow"},
		{vendors.NewError(vendors.NotFound, "Gone", errors.New("gone")), "Fatal (Gone): gone"},
		{vendors.NewError(vendors.RegionUnavailable, "", errors.New("denied")), "Fatal (Unknown): denied"},
		{vendors.NewError(vendors.QuotaExceeded, "Q", errors.New("full")), "QuotaExceeded (Q): full"},
	} {
		if got := launchMessage(tc.err); got != tc.want {
			t.Errorf("launchMessage(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
