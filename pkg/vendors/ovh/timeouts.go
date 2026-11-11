/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"context"
	"time"

	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi"
)

// DefaultCallTimeout is the deadline of one OpenStack call.
const DefaultCallTimeout = 60 * time.Second

// withTimeouts wraps the clients of a region so that every call has its own
// deadline: a hung Nova must not hold a reconcile for ever. A server create is
// one call; the wait for the scheduler is the driver's, in its own loop.
func withTimeouts(c osapi.Clients, d time.Duration) osapi.Clients {
	if d <= 0 {
		return c
	}
	return osapi.Clients{
		Compute: timedCompute{c.Compute, d}, Network: timedNetwork{c.Network, d},
		Images: timedImages{c.Images, d}, Volumes: timedVolumes{c.Volumes, d},
	}
}

func bound(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}

type timedCompute struct {
	osapi.Compute
	d time.Duration
}

func (t timedCompute) ListFlavors(ctx context.Context) ([]osapi.Flavor, error) {
	ctx, cancel := bound(ctx, t.d)
	defer cancel()
	return t.Compute.ListFlavors(ctx)
}

func (t timedCompute) ListServers(ctx context.Context, f osapi.ServerFilter) ([]osapi.Server, error) {
	ctx, cancel := bound(ctx, 3*t.d) // a full listing is several pages
	defer cancel()
	return t.Compute.ListServers(ctx, f)
}

func (t timedCompute) GetServer(ctx context.Context, id string) (*osapi.Server, error) {
	ctx, cancel := bound(ctx, t.d)
	defer cancel()
	return t.Compute.GetServer(ctx, id)
}

func (t timedCompute) CreateServer(ctx context.Context, r osapi.CreateServerRequest) (*osapi.Server, error) {
	ctx, cancel := bound(ctx, t.d)
	defer cancel()
	return t.Compute.CreateServer(ctx, r)
}

func (t timedCompute) DeleteServer(ctx context.Context, id string) error {
	ctx, cancel := bound(ctx, t.d)
	defer cancel()
	return t.Compute.DeleteServer(ctx, id)
}

func (t timedCompute) ListAvailabilityZones(ctx context.Context) ([]osapi.AvailabilityZone, error) {
	ctx, cancel := bound(ctx, t.d)
	defer cancel()
	return t.Compute.ListAvailabilityZones(ctx)
}

func (t timedCompute) GetLimits(ctx context.Context) (*osapi.Limits, error) {
	ctx, cancel := bound(ctx, t.d)
	defer cancel()
	return t.Compute.GetLimits(ctx)
}

func (t timedCompute) ListServerGroups(ctx context.Context) ([]osapi.ServerGroup, error) {
	ctx, cancel := bound(ctx, t.d)
	defer cancel()
	return t.Compute.ListServerGroups(ctx)
}

func (t timedCompute) CreateServerGroup(ctx context.Context, name, policy string) (*osapi.ServerGroup, error) {
	ctx, cancel := bound(ctx, t.d)
	defer cancel()
	return t.Compute.CreateServerGroup(ctx, name, policy)
}

func (t timedCompute) DeleteServerGroup(ctx context.Context, id string) error {
	ctx, cancel := bound(ctx, t.d)
	defer cancel()
	return t.Compute.DeleteServerGroup(ctx, id)
}

type timedNetwork struct {
	osapi.NetworkAPI
	d time.Duration
}

func (t timedNetwork) ListNetworks(ctx context.Context) ([]osapi.Network, error) {
	ctx, cancel := bound(ctx, t.d)
	defer cancel()
	return t.NetworkAPI.ListNetworks(ctx)
}

func (t timedNetwork) ListSecurityGroups(ctx context.Context) ([]osapi.SecurityGroup, error) {
	ctx, cancel := bound(ctx, 3*t.d)
	defer cancel()
	return t.NetworkAPI.ListSecurityGroups(ctx)
}

func (t timedNetwork) CreateSecurityGroup(ctx context.Context, name, description string) (*osapi.SecurityGroup, error) {
	ctx, cancel := bound(ctx, t.d)
	defer cancel()
	return t.NetworkAPI.CreateSecurityGroup(ctx, name, description)
}

func (t timedNetwork) CreateSecurityRule(
	ctx context.Context, groupID string, r osapi.SecurityRule,
) (*osapi.SecurityRule, error) {
	ctx, cancel := bound(ctx, t.d)
	defer cancel()
	return t.NetworkAPI.CreateSecurityRule(ctx, groupID, r)
}

func (t timedNetwork) DeleteSecurityRule(ctx context.Context, id string) error {
	ctx, cancel := bound(ctx, t.d)
	defer cancel()
	return t.NetworkAPI.DeleteSecurityRule(ctx, id)
}

func (t timedNetwork) DeleteSecurityGroup(ctx context.Context, id string) error {
	ctx, cancel := bound(ctx, t.d)
	defer cancel()
	return t.NetworkAPI.DeleteSecurityGroup(ctx, id)
}

type timedImages struct {
	osapi.Images
	d time.Duration
}

func (t timedImages) ListImages(ctx context.Context, f osapi.ImageFilter) ([]osapi.Image, error) {
	ctx, cancel := bound(ctx, 3*t.d)
	defer cancel()
	return t.Images.ListImages(ctx, f)
}

type timedVolumes struct {
	osapi.Volumes
	d time.Duration
}

func (t timedVolumes) ListVolumeTypes(ctx context.Context) ([]osapi.VolumeType, error) {
	ctx, cancel := bound(ctx, t.d)
	defer cancel()
	return t.Volumes.ListVolumeTypes(ctx)
}
