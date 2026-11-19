//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider_test

import (
	"context"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	fakedriver "github.com/SpareCores/scarpenter/pkg/vendors/fake"
)

// privateDriver is a driver whose launch joins a private network.
type privateDriver struct {
	*fakedriver.Driver
	cidr string
}

func (d *privateDriver) Prepare(ctx context.Context, req vendors.PrepareRequest) (*vendors.Prepared, error) {
	p, err := d.Driver.Prepare(ctx, req)
	if p != nil {
		p.NodeIPCIDR = d.cidr
	}
	return p, err
}

// The range of the private network the driver prepared reaches the render
// request; a driver that prepares none leaves it empty.
func TestCreateGivesTheRendererTheNodeIPRange(t *testing.T) {
	for name, cidr := range map[string]string{"private": "10.20.0.0/16", "none": ""} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, envOptions{
				wrap: func(d *fakedriver.Driver) vendors.Driver { return &privateDriver{Driver: d, cidr: cidr} },
			})
			if _, err := e.cp.Create(t.Context(), e.newClaim("claim-nodeip-"+name, []string{"aws.m7i.large"})); err != nil {
				t.Fatal(err)
			}
			if got := e.renderer.last().NodeIPCIDR; got != cidr {
				t.Errorf("NodeIPCIDR = %q, want %q", got, cidr)
			}
		})
	}
}
