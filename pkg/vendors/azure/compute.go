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
	"maps"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	"k8s.io/utils/clock"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Deadlines (plan 17 section 3): plain calls 45 s; the launch waits for the
// long-running operation up to 4 minutes (a VM still Creating then is returned
// pending and found again by the replay); a delete waits as long.
const (
	callTimeout   = 45 * time.Second
	createTimeout = 4 * time.Minute
	deleteTimeout = 4 * time.Minute
	vmType        = "microsoft.compute/virtualmachines"
)

// Compute is the instance layer of the driver over the API interfaces: launch,
// lookup, list and delete of the VMs of one cluster in one subscription. The
// vendors.Driver methods are thin wrappers over it.
type Compute struct {
	Clients Clients
	// Cluster is the controller's cluster name: the value of the cluster tag
	// every VM carries, and the owner check of delete.
	Cluster string
	Clock   clock.Clock
	// CreateTimeout and DeleteTimeout override the defaults (tests).
	CreateTimeout, DeleteTimeout time.Duration
}

func (c *Compute) createTimeout() time.Duration {
	if c.CreateTimeout > 0 {
		return c.CreateTimeout
	}
	return createTimeout
}

func (c *Compute) deleteTimeout() time.Duration {
	if c.DeleteTimeout > 0 {
		return c.DeleteTimeout
	}
	return deleteTimeout
}

// owned reports whether the VM carries this cluster's tag.
func (c *Compute) owned(vm armcompute.VirtualMachine) bool {
	return LogicalTags(vm.Tags)[vendors.TagCluster] == c.Cluster
}

// Launch is one launch attempt: the replay lookup by the deterministic name,
// the public IP and NIC of the NodeClaim, the VM, the wait, and the cleanup of a
// failed attempt so that the key stays unused.
func (c *Compute) Launch(ctx context.Context, l LaunchSpec) (*vendors.Instance, error) {
	name, rg := l.VMName(), l.Network.ResourceGroup
	ec := errContext{size: l.Size, spot: l.Spot}

	// A VM of this name is the replay: whatever zone, type or capacity type
	// the replay asks for. A Failed one that differs from the request is a
	// leftover of an earlier attempt.
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	existing, err := c.Clients.VMs.Get(cctx, rg, name, true)
	cancel()
	switch {
	case err == nil:
		if !c.owned(existing) {
			return nil, vendors.NewError(vendors.Fatal, codeNameConflict,
				fmt.Errorf("a VM named %s exists in %s without this cluster's tag", name, rg))
		}
		if !isLeftover(existing, l) {
			return c.instance(ctx, existing)
		}
		if err := c.deleteVM(ctx, rg, name); err != nil {
			return nil, err
		}
	case !isNotFound(err):
		return nil, classify(err, ec)
	}

	pipID := ""
	if l.PublicIPv4 {
		cctx, cancel := context.WithTimeout(ctx, callTimeout)
		pip, err := c.Clients.PIPs.CreateOrUpdate(cctx, rg, l.PIPName(), l.PIP())
		cancel()
		if err != nil {
			return nil, classify(err, ec)
		}
		pipID = deref(pip.ID)
	}
	cctx, cancel = context.WithTimeout(ctx, callTimeout)
	nic, err := c.Clients.NICs.CreateOrUpdate(cctx, rg, l.NICName(), l.NIC(pipID))
	cancel()
	if err != nil {
		return nil, classify(err, ec)
	}

	wctx, cancel := context.WithTimeout(ctx, c.createTimeout())
	_, err = c.Clients.VMs.CreateOrUpdate(wctx, rg, name, l.VM(deref(nic.ID)))
	cancel()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			// The wait ran out. A VM still Creating is returned pending: the
			// replay (the lookup by name) finds it.
			if vm, gerr := c.getVM(ctx, rg, name); gerr == nil && StateOf(vm) == vendors.StatePending {
				return c.instance(ctx, vm)
			}
		}
		return nil, c.launchFailed(ctx, rg, name, l, err, ec)
	}
	cctx, cancel = context.WithTimeout(ctx, callTimeout)
	vm, err := c.Clients.VMs.Get(cctx, rg, name, true)
	cancel()
	if err != nil {
		return nil, classify(err, ec)
	}
	return c.instance(ctx, vm)
}

// isLeftover reports whether a Failed VM differs from the launch asked for in
// size or zone: it is a leftover to delete, not the replay.
func isLeftover(vm armcompute.VirtualMachine, l LaunchSpec) bool {
	if vm.Properties == nil || deref(vm.Properties.ProvisioningState) != provFailed {
		return false
	}
	size := ""
	if vm.Properties.HardwareProfile != nil {
		size = string(deref(vm.Properties.HardwareProfile.VMSize))
	}
	return size != l.Size || zoneName(vm.Zones) != zoneName(zonesPtr(l.Zone))
}

// launchFailed handles the error of the create: it is classified, and for a
// class that is not transient the VM of the attempt is deleted (waiting),
// because a failed create leaves the key unused, and so are its NIC and public
// IP, which nothing else would collect (their names do not carry the engine's
// prefix). A transient error leaves them all, which the replay finds or the next
// PUT completes. A delete that fails is Retryable.
func (c *Compute) launchFailed(
	ctx context.Context, rg, name string, l LaunchSpec, err error, ec errContext,
) error {
	cerr := classify(err, ec)
	if ctx.Err() != nil {
		return cerr
	}
	if vendors.IsClass(cerr, vendors.Retryable) {
		return cerr
	}
	if derr := c.deleteVM(ctx, rg, name); derr != nil {
		return vendors.NewError(vendors.Retryable, vendors.CodeOf(derr),
			fmt.Errorf("launch failed (%w) and the VM could not be deleted: %w", cerr, derr))
	}
	c.dropNetwork(ctx, rg, l)
	return cerr
}

// dropNetwork deletes the NIC and the public IP of a launch that failed, best
// effort: what it cannot delete is left to the sweeper, and the next attempt
// creates them again under the same names.
func (c *Compute) dropNetwork(ctx context.Context, rg string, l LaunchSpec) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	_ = c.Clients.NICs.Delete(cctx, rg, l.NICName())
	cancel()
	if !l.PublicIPv4 {
		return
	}
	cctx, cancel = context.WithTimeout(ctx, callTimeout)
	_ = c.Clients.PIPs.Delete(cctx, rg, l.PIPName())
	cancel()
}

// deleteVM force-deletes a VM and waits; a VM that is gone is success.
func (c *Compute) deleteVM(ctx context.Context, rg, name string) error {
	dctx, cancel := context.WithTimeout(ctx, c.deleteTimeout())
	defer cancel()
	if err := c.Clients.VMs.Delete(dctx, rg, name, true); err != nil && !isNotFound(err) {
		return classify(err, errContext{})
	}
	return nil
}

// Get returns the instance of a reference; NotFound for a VM that does not
// exist and for a reference that is not <region>/<group>/<vm>.
func (c *Compute) Get(ctx context.Context, ref vendors.InstanceRef) (*vendors.Instance, error) {
	_, rg, name, ok := ParseInstanceRef(ref)
	if !ok {
		return nil, vendors.Errorf(vendors.NotFound, "instance reference %q is not <region>/<resource group>/<vm>", ref)
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	vm, err := c.Clients.VMs.Get(cctx, rg, name, true)
	cancel()
	if err != nil {
		return nil, classify(err, errContext{})
	}
	return c.instance(ctx, vm)
}

// Find looks an instance up by tags in the region; NotFound when there is none.
func (c *Compute) Find(ctx context.Context, region string, tags map[vendors.TagKey]string) (*vendors.Instance, error) {
	list, err := c.List(ctx, []string{region}, tags)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, vendors.Errorf(vendors.NotFound, "no instance with tags %v in %s", tags, region)
	}
	return list[0], nil
}

// List returns the VMs of the regions that carry all the tags. The ARM tag
// filter takes one tag and returns the resources without their tags, so the
// listing filters by one tag (the cluster's when given), keeps the VMs of the
// regions, reads each (with its instance view) and matches the rest of the tags
// client side. When the tag listing fails the fallback is the list of every VM.
// Without a cluster tag the listing is scoped to this cluster, on both paths.
func (c *Compute) List(
	ctx context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]*vendors.Instance, error) {
	if _, ok := tags[vendors.TagCluster]; !ok {
		scoped := maps.Clone(tags)
		if scoped == nil {
			scoped = map[vendors.TagKey]string{}
		}
		scoped[vendors.TagCluster] = c.Cluster
		tags = scoped
	}
	var candidates []armcompute.VirtualMachine
	byTag, err := c.listByTag(ctx, regions, tags)
	if err != nil {
		cctx, cancel := context.WithTimeout(ctx, callTimeout)
		all, lerr := c.Clients.VMs.ListAll(cctx)
		cancel()
		if lerr != nil {
			return nil, classify(lerr, errContext{})
		}
		for _, vm := range all {
			if inRegions(deref(vm.Location), regions) {
				// The list has no instance view: read the VM.
				if r, err := arm.ParseResourceID(deref(vm.ID)); err == nil {
					if full, err := c.getVM(ctx, r.ResourceGroupName, r.Name); err == nil {
						vm = full
					}
				}
				candidates = append(candidates, vm)
			}
		}
	} else {
		candidates = byTag
	}
	var out []*vendors.Instance
	for _, vm := range candidates {
		if !matches(vm, tags) {
			continue
		}
		inst, err := c.instance(ctx, vm)
		if err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
	return out, nil
}

func (c *Compute) getVM(ctx context.Context, rg, name string) (armcompute.VirtualMachine, error) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	return c.Clients.VMs.Get(cctx, rg, name, true)
}

// listByTag lists with the cluster tag and reads the VMs of
// the regions; a VM that vanished between the two calls is skipped.
func (c *Compute) listByTag(
	ctx context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]armcompute.VirtualMachine, error) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	res, err := c.Clients.Resources.ListByTag(cctx, TagKey(vendors.TagCluster), tags[vendors.TagCluster])
	cancel()
	if err != nil {
		return nil, err
	}
	var out []armcompute.VirtualMachine
	for _, r := range res {
		if r == nil || strings.ToLower(deref(r.Type)) != vmType || !inRegions(deref(r.Location), regions) {
			continue
		}
		id, err := arm.ParseResourceID(deref(r.ID))
		if err != nil {
			continue
		}
		vm, err := c.getVM(ctx, id.ResourceGroupName, id.Name)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, classify(err, errContext{})
		}
		out = append(out, vm)
	}
	return out, nil
}

func inRegions(location string, regions []string) bool {
	for _, r := range regions {
		if strings.EqualFold(r, location) {
			return true
		}
	}
	return false
}

// matches reports whether the VM carries all the logical tags.
func matches(vm armcompute.VirtualMachine, tags map[vendors.TagKey]string) bool {
	have := LogicalTags(vm.Tags)
	for k, v := range tags {
		if have[k] != v {
			return false
		}
	}
	return true
}

// Delete removes the VM and, through the delete options, its disks, NIC and
// public IP. It reads the VM first and refuses one without this cluster's tag
// (Fatal); a VM that is gone, or a reference that cannot be parsed, is success.
func (c *Compute) Delete(ctx context.Context, ref vendors.InstanceRef) error {
	_, rg, name, ok := ParseInstanceRef(ref)
	if !ok {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	vm, err := c.Clients.VMs.Get(cctx, rg, name, false)
	cancel()
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return classify(err, errContext{})
	}
	if !c.owned(vm) {
		return vendors.NewError(vendors.Fatal, codeNotOwned,
			fmt.Errorf("VM %s/%s does not carry the tag of cluster %q: refusing to delete it", rg, name, c.Cluster))
	}
	return c.deleteVM(ctx, rg, name)
}

// instance converts a VM to the contract's instance, with its addresses.
func (c *Compute) instance(ctx context.Context, vm armcompute.VirtualMachine) (*vendors.Instance, error) {
	id, err := arm.ParseResourceID(deref(vm.ID))
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, "",
			fmt.Errorf("VM without a valid resource ID %q: %w", deref(vm.ID), err))
	}
	inst := &vendors.Instance{
		Ref:          InstanceRefOf(deref(vm.Location), id.ResourceGroupName, id.Name),
		Region:       deref(vm.Location),
		Zone:         zoneName(vm.Zones),
		CapacityType: vendors.CapacityOnDemand,
		State:        StateOf(vm),
		Tags:         LogicalTags(vm.Tags),
		Addresses:    c.addresses(ctx, vm),
	}
	if p := vm.Properties; p != nil {
		if p.HardwareProfile != nil {
			inst.Type = string(deref(p.HardwareProfile.VMSize))
		}
		if deref(p.Priority) == armcompute.VirtualMachinePriorityTypesSpot {
			inst.CapacityType = vendors.CapacitySpot
		}
		inst.CreatedAt = deref(p.TimeCreated)
	}
	return inst, nil
}

// addresses reads the private address of the VM's primary NIC and the address
// of its public IP; best effort (a missing NIC or address is left out).
func (c *Compute) addresses(ctx context.Context, vm armcompute.VirtualMachine) []string {
	if vm.Properties == nil || vm.Properties.NetworkProfile == nil {
		return nil
	}
	var out []string
	for _, ref := range vm.Properties.NetworkProfile.NetworkInterfaces {
		if ref == nil {
			continue
		}
		id, err := arm.ParseResourceID(deref(ref.ID))
		if err != nil {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, callTimeout)
		nic, err := c.Clients.NICs.Get(cctx, id.ResourceGroupName, id.Name)
		cancel()
		if err != nil || nic.Properties == nil {
			continue
		}
		for _, ipc := range nic.Properties.IPConfigurations {
			if ipc == nil || ipc.Properties == nil {
				continue
			}
			if ip := deref(ipc.Properties.PrivateIPAddress); ip != "" {
				out = append(out, ip)
			}
			if ipc.Properties.PublicIPAddress == nil {
				continue
			}
			pid, err := arm.ParseResourceID(deref(ipc.Properties.PublicIPAddress.ID))
			if err != nil {
				continue
			}
			cctx, cancel := context.WithTimeout(ctx, callTimeout)
			pip, err := c.Clients.PIPs.Get(cctx, pid.ResourceGroupName, pid.Name)
			cancel()
			if err == nil && pip.Properties != nil && deref(pip.Properties.IPAddress) != "" {
				out = append(out, deref(pip.Properties.IPAddress))
			}
		}
	}
	return out
}
