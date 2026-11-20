//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	fakedriver "github.com/SpareCores/scarpenter/pkg/vendors/fake"
)

// unsupportedDriver refuses every launch as Unsupported.
type unsupportedDriver struct {
	*fakedriver.Driver
	preLaunch bool
}

func (d *unsupportedDriver) CreateInstance(context.Context, vendors.CreateRequest) (*vendors.Instance, error) {
	err := error(vendors.NewError(vendors.Unsupported, "ShapeNotInAvailabilityDomain", nil))
	if d.preLaunch {
		err = vendors.AsPreLaunch(err)
	}
	return nil, err
}

// An Unsupported mark from the driver's own pre-launch check lasts minutes; the
// vendor's explicit answer lasts a day. Each mark is logged with its code.
func TestCreateMarksPreLaunchUnsupportedForMinutes(t *testing.T) {
	for _, preLaunch := range []bool{true, false} {
		t.Run(map[bool]string{true: "pre-launch", false: "vendor"}[preLaunch], func(t *testing.T) {
			e := newEnv(t, envOptions{
				attempts: 1,
				wrap:     func(d *fakedriver.Driver) vendors.Driver { return &unsupportedDriver{Driver: d, preLaunch: preLaunch} },
			})
			target := e.offerings("aws.m7i.large")[0]
			var lines []string
			ctx := logr.NewContext(t.Context(), funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{}))
			_, _ = e.cp.Create(ctx, e.newClaim("claim-1", []string{"aws.m7i.large"}))
			if !e.un.IsUnavailable(unavailableOf(target)) {
				t.Fatalf("%+v was not marked unavailable", target)
			}
			var marks []string
			for _, l := range lines {
				if strings.Contains(l, "marked the offering unsupported") {
					marks = append(marks, l)
				}
			}
			if len(marks) != 1 || !strings.Contains(marks[0], `"code"="ShapeNotInAvailabilityDomain"`) {
				t.Errorf("mark lines %q, want one naming the code", marks)
			}
			e.clock.Step(11 * time.Minute)
			if got := e.un.IsUnavailable(unavailableOf(target)); got == preLaunch {
				t.Errorf("unavailable after 11 minutes = %v with preLaunch=%v", got, preLaunch)
			}
		})
	}
}
