/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

//nolint:lll,goconst,dupl // the fake repeats the SDK's long signatures and ARM texts
package fake

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
)

// maxCustomData is the limit of the decoded custom data (65535 bytes).
const maxCustomData = 65535

// VMs is the virtual machine API.
type VMs struct{ s *Subscription }

// copyVM copies the VM and its properties so that callers never alias the
// stored state.
func copyVM(vm armcompute.VirtualMachine) armcompute.VirtualMachine {
	out := vm
	if vm.Properties != nil {
		p := *vm.Properties
		out.Properties = &p
	}
	return out
}

// view returns the VM as the API shows it: with the instance view when asked.
func (v *vmState) view(instanceView bool) armcompute.VirtualMachine {
	out := copyVM(v.vm)
	if instanceView && val(out.Properties.ProvisioningState) != "Creating" {
		out.Properties.InstanceView = &armcompute.VirtualMachineInstanceView{Statuses: []*armcompute.InstanceViewStatus{
			{Code: new("ProvisioningState/" + strings.ToLower(val(out.Properties.ProvisioningState)))},
			{Code: new("PowerState/" + v.power)},
		}}
	}
	return out
}

// Get reads a VM.
func (a *VMs) Get(ctx context.Context, rg, name string, instanceView bool) (armcompute.VirtualMachine, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "vm.get"); err != nil {
		return armcompute.VirtualMachine{}, err
	}
	if err := s.groupExists(rg); err != nil {
		return armcompute.VirtualMachine{}, err
	}
	v, ok := s.vms[key(rg, name)]
	if !ok {
		return armcompute.VirtualMachine{}, Error(http.StatusNotFound, "ResourceNotFound",
			fmt.Sprintf("The Resource 'Microsoft.Compute/virtualMachines/%s' under resource group '%s' was not found.", name, rg))
	}
	return v.view(instanceView), nil
}

// ListAll lists every VM, without instance views.
func (a *VMs) ListAll(ctx context.Context) ([]armcompute.VirtualMachine, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "vm.list"); err != nil {
		return nil, err
	}
	var out []armcompute.VirtualMachine
	for _, k := range sortedKeys(s.vms) {
		out = append(out, s.vms[k].view(false))
	}
	return out, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := mapKeys(m)
	slices.Sort(out)
	return out
}

// sizeOf returns the VM's size.
func sizeOf(vm armcompute.VirtualMachine) string {
	if vm.Properties == nil || vm.Properties.HardwareProfile == nil {
		return ""
	}
	return string(val(vm.Properties.HardwareProfile.VMSize))
}

func isSpot(vm armcompute.VirtualMachine) bool {
	return vm.Properties != nil && val(vm.Properties.Priority) == armcompute.VirtualMachinePriorityTypesSpot
}

func zoneOf(vm armcompute.VirtualMachine) string {
	if len(vm.Zones) == 0 {
		return ""
	}
	return val(vm.Zones[0])
}

// findSKU returns the SKU row of the size in the location.
func (l *location) findSKU(size string) *armcompute.ResourceSKU {
	for _, r := range l.skus {
		if strings.EqualFold(val(r.Name), size) {
			return r
		}
	}
	return nil
}

func capValue(r *armcompute.ResourceSKU, name string) string {
	for _, c := range r.Capabilities {
		if val(c.Name) == name {
			return val(c.Value)
		}
	}
	return ""
}

func vcpusOf(r *armcompute.ResourceSKU) int64 {
	n, _ := strconv.ParseInt(capValue(r, "vCPUs"), 10, 64)
	return n
}

// usage computes the usage of a name in a location: the foreign usage plus the
// VMs the fake holds (Deleting ones included until they are gone).
func (s *Subscription) usage(l *location, name string) int64 {
	n := l.foreign[name]
	for _, v := range s.vms {
		if !strings.EqualFold(val(v.vm.Location), l.name) {
			continue
		}
		r := l.findSKU(sizeOf(v.vm))
		if r == nil {
			continue
		}
		c := vcpusOf(r)
		switch {
		case isSpot(v.vm) && name == usageSpot:
			n += c
		case !isSpot(v.vm) && (name == usageCores || name == val(r.Family)):
			n += c
		}
	}
	return n
}

// check validates a VM PUT the way the compute resource provider does, up to
// the allocation. It returns the error of the first failing check.
func (s *Subscription) check(vm armcompute.VirtualMachine, rg string) error {
	p := vm.Properties
	if p == nil || p.HardwareProfile == nil || p.StorageProfile == nil || p.OSProfile == nil {
		return Error(http.StatusBadRequest, "InvalidParameter", "The VM needs hardware, storage and OS profiles.")
	}
	if err := s.groupExists(rg); err != nil {
		return err
	}
	loc := strings.ToLower(val(vm.Location))
	l, ok := s.locations[loc]
	if !ok {
		return Error(http.StatusBadRequest, "InvalidResourceLocation", fmt.Sprintf("The provided location '%s' is not available for resource type 'Microsoft.Compute/virtualMachines'.", val(vm.Location)))
	}
	if cd := val(p.OSProfile.CustomData); cd != "" {
		raw, err := base64.StdEncoding.DecodeString(cd)
		if err != nil {
			return Error(http.StatusBadRequest, "InvalidParameter", "The value of parameter customData is invalid: not Base64.")
		}
		if len(raw) > maxCustomData {
			return Error(http.StatusBadRequest, "InvalidParameter", "Custom data is too large: it cannot exceed 65535 bytes.")
		}
	}
	size := sizeOf(vm)
	sku := l.findSKU(size)
	if sku == nil {
		return Error(http.StatusBadRequest, "InvalidParameter",
			fmt.Sprintf("The requested VM size %s is not available in the current region.", size))
	}
	zone := zoneOf(vm)
	if val(vm.Properties.Priority) == armcompute.VirtualMachinePriorityTypesSpot && !strings.EqualFold(capValue(sku, "LowPriorityCapable"), "true") {
		return Error(http.StatusBadRequest, "AzureSpotIsNotSupportedForThisVMSize",
			fmt.Sprintf("Azure Spot is not supported for the VM size %s.", size))
	}
	for _, r := range sku.Restrictions {
		restricted := val(r.Type) == armcompute.ResourceSKURestrictionsTypeLocation ||
			(val(r.Type) == armcompute.ResourceSKURestrictionsTypeZone && zone != "" && slices.ContainsFunc(r.Values, func(z *string) bool { return val(z) == zone }))
		if restricted {
			// ARM names the zones when the restriction is a zone's.
			where := fmt.Sprintf("location '%s'", val(vm.Location))
			if val(r.Type) == armcompute.ResourceSKURestrictionsTypeZone {
				where += fmt.Sprintf(" zones '%s'", zone)
			}
			return Error(http.StatusBadRequest, "SkuNotAvailable", fmt.Sprintf(
				"The requested VM size for resource 'Following SKUs have failed for Capacity Restrictions: %s' is currently not available in %s.", size, where))
		}
	}
	if zone != "" {
		if len(sku.LocationInfo) == 0 || !slices.ContainsFunc(sku.LocationInfo[0].Zones, func(z *string) bool { return val(z) == zone }) {
			return Error(http.StatusBadRequest, "SkuNotAvailable", fmt.Sprintf(
				"The requested VM size for resource 'Following SKUs have failed for Capacity Restrictions: %s' is currently not available in location '%s' zones '%s'.",
				size, val(vm.Location), zone))
		}
	}
	sp := p.StorageProfile
	if sp.ImageReference != nil {
		if err := s.checkImage(vm, loc); err != nil {
			return err
		}
	}
	if p.NetworkProfile == nil || len(p.NetworkProfile.NetworkInterfaces) == 0 {
		return Error(http.StatusBadRequest, "InvalidParameter", "The VM needs a network interface.")
	}
	for _, ref := range p.NetworkProfile.NetworkInterfaces {
		id, err := arm.ParseResourceID(val(ref.ID))
		if err != nil {
			return Error(http.StatusBadRequest, "InvalidParameter", "Invalid network interface ID.")
		}
		if _, ok := s.nics[key(id.ResourceGroupName, id.Name)]; !ok {
			return Error(http.StatusNotFound, "ResourceNotFound", fmt.Sprintf("Resource /%s was not found.", id.String()))
		}
	}
	return nil
}

func (s *Subscription) checkImage(vm armcompute.VirtualMachine, loc string) error {
	ref := vm.Properties.StorageProfile.ImageReference
	k := strings.ToLower(strings.Join([]string{val(ref.Publisher), val(ref.Offer), val(ref.SKU), val(ref.Version)}, ":"))
	im, ok := s.images[k]
	if !ok || im.missingIn[loc] {
		return Error(http.StatusBadRequest, "PlatformImageNotFound", fmt.Sprintf("The platform image '%s' is not available.", k))
	}
	if im.plan == nil {
		return nil
	}
	if vm.Plan == nil || val(vm.Plan.Name) != val(im.plan.Name) {
		return Error(http.StatusBadRequest, "VMMarketplaceInvalidInput", "Creating a virtual machine from Marketplace image requires Plan information in the request.")
	}
	if !s.agreement[strings.ToLower(val(im.plan.Publisher)+"/"+val(im.plan.Product)+"/"+val(im.plan.Name))] {
		return Error(http.StatusBadRequest, "MarketplacePurchaseEligibilityFailed", "Marketplace purchase eligibility check returned errors: the legal terms were not accepted.")
	}
	return nil
}

// CreateOrUpdate is the PUT by name: idempotent, and a long-running operation
// the fake completes inside the call.
func (a *VMs) CreateOrUpdate(ctx context.Context, rg, name string, vm armcompute.VirtualMachine) (armcompute.VirtualMachine, error) {
	s := a.s
	s.mu.Lock()
	if err := s.begin(ctx, "vm.put"); err != nil {
		s.mu.Unlock()
		return armcompute.VirtualMachine{}, err
	}
	k := key(rg, name)
	existing, exists := s.vms[k]
	if exists && val(existing.vm.Properties.ProvisioningState) != "Failed" {
		// A PUT of an existing VM updates it in place: another zone is not
		// allowed, the rest is accepted.
		if zoneOf(vm) != zoneOf(existing.vm) {
			s.mu.Unlock()
			return armcompute.VirtualMachine{}, Error(http.StatusBadRequest, "PropertyChangeNotAllowed", "Changing property 'zones' is not allowed.")
		}
		existing.vm.Tags = vm.Tags
		out := existing.view(false)
		s.mu.Unlock()
		return out, nil
	}
	if err := s.check(vm, rg); err != nil {
		s.mu.Unlock()
		return armcompute.VirtualMachine{}, err
	}
	if exists {
		s.removeVM(k)
	}
	loc := s.locations[strings.ToLower(val(vm.Location))]
	size, zone, spot := sizeOf(vm), zoneOf(vm), isSpot(vm)
	sku := loc.findSKU(size)

	// The allocation happens inside the long-running operation: quota first,
	// then capacity.
	allocErr := s.allocate(loc, sku, size, zone, spot)

	stored := copyVM(vm)
	stored.ID = new(s.id(rg, "Microsoft.Compute", "virtualMachines", name))
	stored.Name = new(name)
	stored.Type = new("Microsoft.Compute/virtualMachines")
	stored.Properties.VMID = new(fmt.Sprintf("vm-%s", strings.ToLower(name)))
	stored.Properties.TimeCreated = new(s.Now())
	st := &vmState{vm: stored, rg: rg, power: "running"}
	if allocErr != nil {
		if s.leaveFailed {
			stored.Properties.ProvisioningState = new("Failed")
			st.vm = stored
			s.vms[k] = st
			s.registerDisks(rg, name, stored)
		}
		s.mu.Unlock()
		return armcompute.VirtualMachine{}, allocErr
	}
	stored.Properties.ProvisioningState = new("Creating")
	s.vms[k] = s.attach(st, rg, name)
	if s.hold {
		s.mu.Unlock()
		<-ctx.Done()
		return armcompute.VirtualMachine{}, ctx.Err()
	}
	st.vm.Properties.ProvisioningState = new("Succeeded")
	out := st.view(false)
	s.mu.Unlock()
	return out, nil
}

// attach registers the disks of a VM that is being created and returns it.
func (s *Subscription) attach(st *vmState, rg, name string) *vmState {
	s.registerDisks(rg, name, st.vm)
	return st
}

func (s *Subscription) registerDisks(rg, vmName string, vm armcompute.VirtualMachine) {
	sp := vm.Properties.StorageProfile
	if sp.OSDisk != nil && sp.OSDisk.DiffDiskSettings == nil {
		s.disks[key(rg, val(sp.OSDisk.Name))] = vmName
	}
	for _, d := range sp.DataDisks {
		s.disks[key(rg, val(d.Name))] = vmName
	}
}

// allocate runs the quota and capacity checks of the allocation.
func (s *Subscription) allocate(l *location, sku *armcompute.ResourceSKU, size, zone string, spot bool) error {
	n := vcpusOf(sku)
	tiers := []string{usageCores, val(sku.Family)}
	if spot {
		tiers = []string{usageSpot}
	}
	for _, t := range tiers {
		limit, ok := l.limits[t]
		if !ok {
			continue
		}
		if s.usage(l, t)+n > limit {
			name := "Total Regional Cores"
			if t == usageSpot {
				name = "Total Regional Spot Cores"
			} else if t != usageCores {
				name = t + " Cores"
			}
			return Error(http.StatusConflict, "OperationNotAllowed", fmt.Sprintf(
				"Operation could not be completed as it results in exceeding approved %s quota. Location: %s, Current Limit: %d, Current Usage: %d, Additional Required: %d.",
				name, l.name, limit, s.usage(l, t), n))
		}
	}
	for _, r := range s.rules {
		if !strings.EqualFold(r.location, l.name) || r.size != size || (r.zone != "" && r.zone != zone) || (r.spotOnly && !spot) {
			continue
		}
		code := r.code
		if code == "" {
			code = "AllocationFailed"
			if zone != "" {
				code = "ZonalAllocationFailed"
			}
		}
		return Error(http.StatusConflict, code, fmt.Sprintf("Allocation failed. We do not have sufficient capacity for the requested VM size %s in this region.", size))
	}
	return nil
}

// removeVM deletes a VM and what its delete options cascade to. The caller
// holds the lock.
func (s *Subscription) removeVM(k string) {
	v, ok := s.vms[k]
	if !ok {
		return
	}
	delete(s.vms, k)
	// A disk goes with the VM only with deleteOption Delete; the default is
	// Detach, which leaves the disk behind (owned by nobody).
	p := v.vm.Properties
	if sp := p.StorageProfile; sp != nil {
		if sp.OSDisk != nil {
			s.releaseDisk(v.rg, val(sp.OSDisk.Name), val(sp.OSDisk.DeleteOption) == armcompute.DiskDeleteOptionTypesDelete)
		}
		for _, d := range sp.DataDisks {
			if d != nil {
				s.releaseDisk(v.rg, val(d.Name), val(d.DeleteOption) == armcompute.DiskDeleteOptionTypesDelete)
			}
		}
	}
	if p.NetworkProfile == nil {
		return
	}
	for _, ref := range p.NetworkProfile.NetworkInterfaces {
		if ref.Properties == nil || val(ref.Properties.DeleteOption) != armcompute.DeleteOptionsDelete {
			continue
		}
		id, err := arm.ParseResourceID(val(ref.ID))
		if err != nil {
			continue
		}
		nk := key(id.ResourceGroupName, id.Name)
		if nic, ok := s.nics[nk]; ok {
			delete(s.nics, nk)
			if nic.Properties == nil {
				continue
			}
			for _, ipc := range nic.Properties.IPConfigurations {
				if ipc.Properties == nil || ipc.Properties.PublicIPAddress == nil {
					continue
				}
				pid, err := arm.ParseResourceID(val(ipc.Properties.PublicIPAddress.ID))
				if err != nil {
					continue
				}
				pk := key(pid.ResourceGroupName, pid.Name)
				if pip, ok := s.pips[pk]; ok && pip.Properties != nil && val(pip.Properties.DeleteOption) == "Delete" {
					delete(s.pips, pk)
				}
			}
		}
	}
}

// releaseDisk deletes or detaches a disk of a VM that is being deleted.
func (s *Subscription) releaseDisk(rg, name string, del bool) {
	dk := key(rg, name)
	if _, ok := s.disks[dk]; !ok {
		return
	}
	if del {
		delete(s.disks, dk)
		return
	}
	s.disks[dk] = ""
}

// Delete removes a VM. Like ARM, deleting a VM that does not exist succeeds.
func (a *VMs) Delete(ctx context.Context, rg, name string, force bool) error {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "vm.delete"); err != nil {
		return err
	}
	if err := s.groupExists(rg); err != nil {
		return err
	}
	s.removeVM(key(rg, name))
	return nil
}
