/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package azure is the foundation of the Azure driver (plan 17): the narrow
// API interfaces over the track-2 SDK, the pure mapping code (names, tags,
// states, the VM request), the error classification, the quota reader and the
// instance operations. The vendors.Driver itself, its registration and the
// resource kinds sit on the same interfaces.
package azure

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armsubscriptions"
)

// The interfaces below are the calls the driver needs, in the shape of the
// plan's vmAPI, nicAPI, ... list, with two deliberate differences from the SDK
// clients: long-running operations are awaited inside the call (the caller
// bounds them with the context's deadline, which is how the 4 minute launch
// wait of plan 17 section 3 works) and pagers are drained into slices. The
// in-memory fake (pkg/vendors/azure/fake) implements exactly these, and sdk.go
// adapts the real clients. Errors are the SDK's: *azcore.ResponseError with the
// ARM error code, which classify reads.

// VMAPI is the virtual machine API of the subscription.
type VMAPI interface {
	// Get reads a VM; instanceView adds $expand=instanceView (power state).
	Get(ctx context.Context, resourceGroup, name string, instanceView bool) (armcompute.VirtualMachine, error)
	// CreateOrUpdate is the idempotent PUT by name; it returns when the
	// long-running operation has ended, or with its error.
	CreateOrUpdate(
		ctx context.Context, resourceGroup, name string, vm armcompute.VirtualMachine,
	) (armcompute.VirtualMachine, error)
	// Delete removes the VM and waits; force is forceDeletion=true.
	Delete(ctx context.Context, resourceGroup, name string, force bool) error
	// ListAll lists every VM of the subscription (the fallback of the tag
	// lookup).
	ListAll(ctx context.Context) ([]armcompute.VirtualMachine, error)
}

// NICAPI is the network interface API.
type NICAPI interface {
	Get(ctx context.Context, resourceGroup, name string) (armnetwork.Interface, error)
	CreateOrUpdate(
		ctx context.Context, resourceGroup, name string, nic armnetwork.Interface,
	) (armnetwork.Interface, error)
	Delete(ctx context.Context, resourceGroup, name string) error
	// ListInGroup lists the NICs of a resource group (the in-use check of an
	// application security group, which Azure does not report back).
	ListInGroup(ctx context.Context, resourceGroup string) ([]armnetwork.Interface, error)
}

// PIPAPI is the public IP address API.
type PIPAPI interface {
	Get(ctx context.Context, resourceGroup, name string) (armnetwork.PublicIPAddress, error)
	CreateOrUpdate(
		ctx context.Context, resourceGroup, name string, pip armnetwork.PublicIPAddress,
	) (armnetwork.PublicIPAddress, error)
	Delete(ctx context.Context, resourceGroup, name string) error
}

// GroupsAPI is the resource group API.
type GroupsAPI interface {
	Get(ctx context.Context, name string) (armresources.ResourceGroup, error)
	CreateOrUpdate(ctx context.Context, name string, rg armresources.ResourceGroup) (armresources.ResourceGroup, error)
	// Delete removes the group with everything in it and waits.
	Delete(ctx context.Context, name string) error
	// List lists every group of the subscription, with its tags.
	List(ctx context.Context) ([]armresources.ResourceGroup, error)
	// Resources lists what a group holds, with tags.
	Resources(ctx context.Context, name string) ([]*armresources.GenericResourceExpanded, error)
}

// VNetAPI is the virtual network API (the subnets are part of the network).
type VNetAPI interface {
	Get(ctx context.Context, resourceGroup, name string) (armnetwork.VirtualNetwork, error)
	CreateOrUpdate(
		ctx context.Context, resourceGroup, name string, vnet armnetwork.VirtualNetwork,
	) (armnetwork.VirtualNetwork, error)
	Delete(ctx context.Context, resourceGroup, name string) error
	// ListAll lists the virtual networks of the subscription, with their tags.
	ListAll(ctx context.Context) ([]armnetwork.VirtualNetwork, error)
}

// NSGAPI is the network security group API.
type NSGAPI interface {
	Get(ctx context.Context, resourceGroup, name string) (armnetwork.SecurityGroup, error)
	CreateOrUpdate(
		ctx context.Context, resourceGroup, name string, nsg armnetwork.SecurityGroup,
	) (armnetwork.SecurityGroup, error)
	Delete(ctx context.Context, resourceGroup, name string) error
	ListAll(ctx context.Context) ([]armnetwork.SecurityGroup, error)
}

// ASGAPI is the application security group API.
type ASGAPI interface {
	Get(ctx context.Context, resourceGroup, name string) (armnetwork.ApplicationSecurityGroup, error)
	CreateOrUpdate(
		ctx context.Context, resourceGroup, name string, asg armnetwork.ApplicationSecurityGroup,
	) (armnetwork.ApplicationSecurityGroup, error)
	Delete(ctx context.Context, resourceGroup, name string) error
	ListAll(ctx context.Context) ([]armnetwork.ApplicationSecurityGroup, error)
}

// PPGAPI is the proximity placement group API.
type PPGAPI interface {
	Get(ctx context.Context, resourceGroup, name string) (armcompute.ProximityPlacementGroup, error)
	CreateOrUpdate(
		ctx context.Context, resourceGroup, name string, ppg armcompute.ProximityPlacementGroup,
	) (armcompute.ProximityPlacementGroup, error)
	Delete(ctx context.Context, resourceGroup, name string) error
	ListAll(ctx context.Context) ([]armcompute.ProximityPlacementGroup, error)
}

// ResourcesAPI lists resources of the subscription by tag. A tag filter returns
// the resources without their tags (id, name, type and location only).
type ResourcesAPI interface {
	// ListByTag is $filter=tagName eq '<name>' and tagValue eq '<value>'.
	ListByTag(ctx context.Context, name, value string) ([]*armresources.GenericResourceExpanded, error)
}

// SKUAPI reads the compute SKU table of a location.
type SKUAPI interface {
	List(ctx context.Context, location string) ([]*armcompute.ResourceSKU, error)
}

// UsageAPI reads the usage and limits of the subscription in a location.
type UsageAPI interface {
	ComputeUsages(ctx context.Context, location string) ([]*armcompute.Usage, error)
	NetworkUsages(ctx context.Context, location string) ([]*armnetwork.Usage, error)
}

// ImageAPI reads marketplace images.
type ImageAPI interface {
	Get(ctx context.Context, location, publisher, offer, sku, version string) (armcompute.VirtualMachineImage, error)
	ListSKUs(ctx context.Context, location, publisher, offer string) ([]string, error)
	// ListVersions lists the versions of a SKU (the name pattern of the images
	// selector).
	ListVersions(ctx context.Context, location, publisher, offer, sku string) ([]string, error)
}

// AgreementAPI reads the marketplace terms state of a plan.
type AgreementAPI interface {
	// Accepted reports whether the subscription accepted the terms.
	Accepted(ctx context.Context, publisher, offer, plan string) (bool, error)
}

// LocationAPI lists the subscription's locations.
type LocationAPI interface {
	List(ctx context.Context) ([]*armsubscriptions.Location, error)
}

// Clients are the API clients of the subscription (the region is a parameter
// of the calls, not of the client).
type Clients struct {
	VMs        VMAPI
	NICs       NICAPI
	PIPs       PIPAPI
	Groups     GroupsAPI
	VNets      VNetAPI
	NSGs       NSGAPI
	ASGs       ASGAPI
	PPGs       PPGAPI
	Resources  ResourcesAPI
	SKUs       SKUAPI
	Usages     UsageAPI
	Images     ImageAPI
	Agreements AgreementAPI
	Locations  LocationAPI
}
