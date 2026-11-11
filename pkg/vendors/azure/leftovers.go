/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Resource types of what a launch makes besides the VM (lower case).
const (
	nicType = "microsoft.network/networkinterfaces"
	pipType = "microsoft.network/publicipaddresses"
)

// CollectLeftovers implements vendors.LeftoverCollector: the network
// interfaces and public IP addresses of this cluster in the region that no VM
// uses and whose NodeClaim does not exist. A launch makes them before its VM and
// deletes them when it fails, but a launch interrupted between the two (a
// controller restart, a delete that failed) leaves them with nothing else to
// collect them: the VM's delete options remove them only with a VM. One without
// a NodeClaim tag is never touched. Managed disks are not collected: inline
// disks carry no tags and go with their VM (deleteOption Delete).
func (d *Driver) CollectLeftovers(
	ctx context.Context, region string, nodeClaimExists func(name string) bool,
) (int, error) {
	return d.compute.CollectLeftovers(ctx, region, nodeClaimExists)
}

// CollectLeftovers is the work of Driver.CollectLeftovers: the NICs first (a
// public IP is in use while a NIC references it), then the public IPs, each read
// again before its delete.
func (c *Compute) CollectLeftovers(
	ctx context.Context, region string, nodeClaimExists func(name string) bool,
) (int, error) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	res, err := c.Clients.Resources.ListByTag(cctx, TagKey(vendors.TagCluster), c.Cluster)
	cancel()
	if err != nil {
		return 0, classify(err, errContext{})
	}
	var nics, pips []*arm.ResourceID
	for _, r := range res {
		if r == nil || !inRegions(deref(r.Location), []string{region}) {
			continue
		}
		id, err := arm.ParseResourceID(deref(r.ID))
		if err != nil {
			continue
		}
		switch strings.ToLower(deref(r.Type)) {
		case nicType:
			nics = append(nics, id)
		case pipType:
			pips = append(pips, id)
		}
	}
	orphan := func(tags map[vendors.TagKey]string) bool {
		claim := tags[vendors.TagNodeClaim]
		return tags[vendors.TagCluster] == c.Cluster && claim != "" && !nodeClaimExists(claim)
	}
	deleted := 0
	var errs []error
	for _, id := range nics {
		cctx, cancel := context.WithTimeout(ctx, callTimeout)
		nic, err := c.Clients.NICs.Get(cctx, id.ResourceGroupName, id.Name)
		cancel()
		switch {
		case isNotFound(err):
			continue
		case err != nil:
			errs = append(errs, fmt.Errorf("reading the network interface %s: %w", id.Name, err))
			continue
		}
		if !orphan(LogicalTags(nic.Tags)) || (nic.Properties != nil && nic.Properties.VirtualMachine != nil) {
			continue
		}
		dctx, cancel := context.WithTimeout(ctx, 2*callTimeout)
		err = c.Clients.NICs.Delete(dctx, id.ResourceGroupName, id.Name)
		cancel()
		switch {
		case err == nil:
			deleted++
		case !isNotFound(err):
			errs = append(errs, fmt.Errorf("deleting the network interface %s: %w", id.Name, err))
		}
	}
	for _, id := range pips {
		cctx, cancel := context.WithTimeout(ctx, callTimeout)
		pip, err := c.Clients.PIPs.Get(cctx, id.ResourceGroupName, id.Name)
		cancel()
		switch {
		case isNotFound(err):
			continue
		case err != nil:
			errs = append(errs, fmt.Errorf("reading the public IP address %s: %w", id.Name, err))
			continue
		}
		if !orphan(LogicalTags(pip.Tags)) || (pip.Properties != nil && pip.Properties.IPConfiguration != nil) {
			continue
		}
		dctx, cancel := context.WithTimeout(ctx, 2*callTimeout)
		err = c.Clients.PIPs.Delete(dctx, id.ResourceGroupName, id.Name)
		cancel()
		switch {
		case err == nil:
			deleted++
		case !isNotFound(err):
			errs = append(errs, fmt.Errorf("deleting the public IP address %s: %w", id.Name, err))
		}
	}
	if len(errs) > 0 {
		return deleted, classify(errors.Join(errs...), errContext{})
	}
	return deleted, nil
}

var _ vendors.LeftoverCollector = (*Driver)(nil)
