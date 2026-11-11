/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package osapi

import "context"

// Compute is the part of Nova the driver calls, per region. List calls return
// every page. Failures are *APIError (or transport errors).
type Compute interface {
	ListFlavors(ctx context.Context) ([]Flavor, error)
	ListServers(ctx context.Context, f ServerFilter) ([]Server, error)
	GetServer(ctx context.Context, id string) (*Server, error)
	CreateServer(ctx context.Context, req CreateServerRequest) (*Server, error)
	DeleteServer(ctx context.Context, id string) error
	ListAvailabilityZones(ctx context.Context) ([]AvailabilityZone, error)
	GetLimits(ctx context.Context) (*Limits, error)
	ListServerGroups(ctx context.Context) ([]ServerGroup, error)
	CreateServerGroup(ctx context.Context, name, policy string) (*ServerGroup, error)
	DeleteServerGroup(ctx context.Context, id string) error
}

// NetworkAPI is the part of Neutron the driver calls, per region.
type NetworkAPI interface {
	ListNetworks(ctx context.Context) ([]Network, error)
	ListSecurityGroups(ctx context.Context) ([]SecurityGroup, error)
	CreateSecurityGroup(ctx context.Context, name, description string) (*SecurityGroup, error)
	CreateSecurityRule(ctx context.Context, groupID string, r SecurityRule) (*SecurityRule, error)
	DeleteSecurityRule(ctx context.Context, ruleID string) error
	DeleteSecurityGroup(ctx context.Context, id string) error
}

// Images is the part of Glance the driver calls (it only reads), per region.
type Images interface {
	ListImages(ctx context.Context, f ImageFilter) ([]Image, error)
}

// Volumes is the part of Cinder the driver calls, per region.
type Volumes interface {
	ListVolumeTypes(ctx context.Context) ([]VolumeType, error)
}

// Clients are the API clients of one region.
type Clients struct {
	Compute Compute
	Network NetworkAPI
	Images  Images
	Volumes Volumes
}

// Provider is the authenticated cloud: the Keystone catalog and the per-region
// clients. The real one wraps a gophercloud provider client; the fake serves an
// in-memory cloud.
type Provider interface {
	// Regions returns the region names of the catalog that have a compute
	// endpoint (the API names, upper case).
	Regions(ctx context.Context) ([]string, error)
	// Clients returns the clients of an API region name; ErrNoEndpoint when the
	// catalog has no endpoint of a needed service there.
	Clients(ctx context.Context, region string) (Clients, error)
}
