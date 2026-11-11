/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package main

import (
	"context"

	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi"
)

// The read-only guard of the OVH canary, the second wall next to the user's read-only
// roles: the driver is given a provider whose clients refuse every call that is not a List
// or a Get before it reaches the SDK. The wrappers implement the narrow osapi interfaces
// method by method (a method added to an interface breaks the build here), and
// guard_test.go checks by reflection that every method is either passed through as a read
// or refused as a write. It shares audit, errReadOnly and isRead with the AWS guard.

// roProvider wraps an osapi.Provider: every client it returns is guarded.
type roProvider struct {
	inner osapi.Provider
	audit *audit
}

var _ osapi.Provider = (*roProvider)(nil)

// Regions implements osapi.Provider (a read of the Keystone catalog).
func (p *roProvider) Regions(ctx context.Context) ([]string, error) {
	p.audit.read("keystone:ListRegions")
	return p.inner.Regions(ctx)
}

// Clients implements osapi.Provider.
func (p *roProvider) Clients(ctx context.Context, region string) (osapi.Clients, error) {
	c, err := p.inner.Clients(ctx, region)
	if err != nil {
		return osapi.Clients{}, err
	}
	return osapi.Clients{
		Compute: &roCompute{c.Compute, p.audit}, Network: &roNetwork{c.Network, p.audit},
		Images: &roImages{c.Images, p.audit}, Volumes: &roVolumes{c.Volumes, p.audit},
	}, nil
}

type roCompute struct {
	inner osapi.Compute
	audit *audit
}

var _ osapi.Compute = (*roCompute)(nil)

func (g *roCompute) ListFlavors(ctx context.Context) ([]osapi.Flavor, error) {
	g.audit.read("nova:ListFlavors")
	return g.inner.ListFlavors(ctx)
}

func (g *roCompute) ListServers(ctx context.Context, f osapi.ServerFilter) ([]osapi.Server, error) {
	g.audit.read("nova:ListServers")
	return g.inner.ListServers(ctx, f)
}

func (g *roCompute) GetServer(ctx context.Context, id string) (*osapi.Server, error) {
	g.audit.read("nova:GetServer")
	return g.inner.GetServer(ctx, id)
}

func (g *roCompute) ListAvailabilityZones(ctx context.Context) ([]osapi.AvailabilityZone, error) {
	g.audit.read("nova:ListAvailabilityZones")
	return g.inner.ListAvailabilityZones(ctx)
}

func (g *roCompute) GetLimits(ctx context.Context) (*osapi.Limits, error) {
	g.audit.read("nova:GetLimits")
	return g.inner.GetLimits(ctx)
}

func (g *roCompute) ListServerGroups(ctx context.Context) ([]osapi.ServerGroup, error) {
	g.audit.read("nova:ListServerGroups")
	return g.inner.ListServerGroups(ctx)
}

func (g *roCompute) CreateServer(context.Context, osapi.CreateServerRequest) (*osapi.Server, error) {
	return nil, g.audit.deny("nova:CreateServer")
}

func (g *roCompute) DeleteServer(context.Context, string) error {
	return g.audit.deny("nova:DeleteServer")
}

func (g *roCompute) CreateServerGroup(context.Context, string, string) (*osapi.ServerGroup, error) {
	return nil, g.audit.deny("nova:CreateServerGroup")
}

func (g *roCompute) DeleteServerGroup(context.Context, string) error {
	return g.audit.deny("nova:DeleteServerGroup")
}

type roNetwork struct {
	inner osapi.NetworkAPI
	audit *audit
}

var _ osapi.NetworkAPI = (*roNetwork)(nil)

func (g *roNetwork) ListNetworks(ctx context.Context) ([]osapi.Network, error) {
	g.audit.read("neutron:ListNetworks")
	return g.inner.ListNetworks(ctx)
}

func (g *roNetwork) ListSecurityGroups(ctx context.Context) ([]osapi.SecurityGroup, error) {
	g.audit.read("neutron:ListSecurityGroups")
	return g.inner.ListSecurityGroups(ctx)
}

func (g *roNetwork) CreateSecurityGroup(context.Context, string, string) (*osapi.SecurityGroup, error) {
	return nil, g.audit.deny("neutron:CreateSecurityGroup")
}

func (g *roNetwork) CreateSecurityRule(context.Context, string, osapi.SecurityRule) (*osapi.SecurityRule, error) {
	return nil, g.audit.deny("neutron:CreateSecurityRule")
}

func (g *roNetwork) DeleteSecurityRule(context.Context, string) error {
	return g.audit.deny("neutron:DeleteSecurityRule")
}

func (g *roNetwork) DeleteSecurityGroup(context.Context, string) error {
	return g.audit.deny("neutron:DeleteSecurityGroup")
}

type roImages struct {
	inner osapi.Images
	audit *audit
}

var _ osapi.Images = (*roImages)(nil)

func (g *roImages) ListImages(ctx context.Context, f osapi.ImageFilter) ([]osapi.Image, error) {
	g.audit.read("glance:ListImages")
	return g.inner.ListImages(ctx, f)
}

type roVolumes struct {
	inner osapi.Volumes
	audit *audit
}

var _ osapi.Volumes = (*roVolumes)(nil)

func (g *roVolumes) ListVolumeTypes(ctx context.Context) ([]osapi.VolumeType, error) {
	g.audit.read("cinder:ListVolumeTypes")
	return g.inner.ListVolumeTypes(ctx)
}

// readOnlyOVH wraps a provider with the guard.
func readOnlyOVH(inner osapi.Provider, a *audit) osapi.Provider { return &roProvider{inner, a} }
