/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp_test

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp"
)

// methodPermissions maps every method of gcp.ComputeAPI to the IAM permissions its
// call needs. The optional is the managed-network role: only the calls of the
// resources SCarpenter creates itself.
var methodPermissions = map[string]struct {
	required, optional []string
}{
	"InsertInstance":          {required: []string{"compute.instances.create"}},
	"DeleteInstance":          {required: []string{"compute.instances.delete"}},
	"GetInstance":             {required: []string{"compute.instances.get"}},
	"ListInstances":           {required: []string{"compute.instances.list"}},
	"AggregatedListInstances": {required: []string{"compute.instances.list"}},
	"ListZones":               {required: []string{"compute.zones.list"}},
	"GetRegion":               {required: []string{"compute.regions.get"}},
	"ListRegions":             {required: []string{"compute.regions.list"}},
	"ListMachineTypes":        {required: []string{"compute.machineTypes.list"}},
	"GetProject":              {required: []string{"compute.projects.get"}},
	"GetImage":                {required: []string{"compute.images.get"}},
	"GetImageFromFamily":      {required: []string{"compute.images.getFromFamily"}},
	"ListImages":              {required: []string{"compute.images.list"}},
	"GetSubnetwork":           {required: []string{"compute.subnetworks.get"}},
	"ListSubnetworks":         {required: []string{"compute.subnetworks.list"}},
	"InsertSubnetwork":        {optional: []string{"compute.subnetworks.create"}},
	"DeleteSubnetwork":        {optional: []string{"compute.subnetworks.delete"}},
	"GetNetwork":              {required: []string{"compute.networks.get"}},
	"ListNetworks":            {required: []string{"compute.networks.list"}},
	"InsertNetwork":           {optional: []string{"compute.networks.create", "compute.networks.updatePolicy"}},
	"DeleteNetwork":           {optional: []string{"compute.networks.delete"}},
	"ListFirewalls":           {required: []string{"compute.firewalls.list"}},
	"InsertFirewall":          {optional: []string{"compute.firewalls.create", "compute.networks.updatePolicy"}},
	"DeleteFirewall":          {optional: []string{"compute.firewalls.delete", "compute.networks.updatePolicy"}},
	"GetResourcePolicy":       {required: []string{"compute.resourcePolicies.get"}},
	"ListResourcePolicies":    {required: []string{"compute.resourcePolicies.list"}},
	"InsertResourcePolicy":    {required: []string{"compute.resourcePolicies.create"}},
	"DeleteResourcePolicy":    {required: []string{"compute.resourcePolicies.delete"}},
	"WaitZoneOperation":       {required: []string{"compute.zoneOperations.get"}},
	"GetZoneOperation":        {required: []string{"compute.zoneOperations.get"}},
	"WaitRegionOperation":     {required: []string{"compute.regionOperations.get"}},
	"GetRegionOperation":      {required: []string{"compute.regionOperations.get"}},
	"WaitGlobalOperation":     {required: []string{"compute.globalOperations.get"}},
	"GetGlobalOperation":      {required: []string{"compute.globalOperations.get"}},
}

// impliedByInsert are the permissions Compute checks on the other resources an
// instances.insert, instances.delete or a firewall change touches: the boot and data
// disks, the subnetwork and its external address, the network, the image, and the
// metadata, labels, tags, service account and scheduling it sets.
var impliedByInsert = []string{
	"compute.disks.create", "compute.disks.delete", "compute.disks.get", "compute.disks.list", "compute.disks.setLabels",
	"compute.disks.use", "compute.images.useReadOnly", "compute.instances.setLabels", "compute.instances.setMetadata",
	"compute.instances.setScheduling", "compute.instances.setServiceAccount", "compute.instances.setTags",
	"compute.machineTypes.get", "compute.networks.use", "compute.resourcePolicies.use", "compute.subnetworks.use",
	"compute.subnetworks.useExternalIp", "compute.zones.get",
}

type roleFile struct {
	Title               string   `json:"title"`
	Description         string   `json:"description"`
	Stage               string   `json:"stage"`
	IncludedPermissions []string `json:"includedPermissions"`
}

func readRole(t *testing.T, name string) roleFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(testutil.RepoRoot(), "docs", "reference", name))
	if err != nil {
		t.Fatal(err)
	}
	var r roleFile
	if err := yaml.UnmarshalStrict(raw, &r); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	// A custom role has a title of at most 100 characters and at most 3000 permissions.
	if r.Title == "" || len(r.Title) > 100 || r.Description == "" || r.Stage != "GA" || len(r.IncludedPermissions) == 0 {
		t.Fatalf("%s: %+v", name, r)
	}
	if !slices.IsSorted(r.IncludedPermissions) {
		t.Errorf("%s: the permissions are not sorted", name)
	}
	return r
}

// TestRoleFilesMatchTheDriver: every method of the Compute API the driver uses has its
// permission in the roles, and the roles hold nothing else.
func TestRoleFilesMatchTheDriver(t *testing.T) {
	api := reflect.TypeFor[gcp.ComputeAPI]()
	for i := range api.NumMethod() {
		if _, ok := methodPermissions[api.Method(i).Name]; !ok {
			t.Errorf("the API method %s has no permission in the table of this test", api.Method(i).Name)
		}
	}
	for m := range methodPermissions {
		if _, ok := api.MethodByName(m); !ok {
			t.Errorf("the table names %s, which is not an API method", m)
		}
	}
	main, managed := readRole(t, "gcp-iam-role.yaml"), readRole(t, "gcp-iam-role-managed.yaml")
	wantMain, wantManaged := map[string]bool{}, map[string]bool{}
	for _, p := range methodPermissions {
		for _, x := range p.required {
			wantMain[x] = true
		}
		for _, x := range p.optional {
			wantManaged[x] = true
		}
	}
	for _, x := range impliedByInsert {
		wantMain[x] = true
	}
	for _, x := range slices.Sorted(maps.Keys(wantMain)) {
		if !slices.Contains(main.IncludedPermissions, x) {
			t.Errorf("the role lacks %s", x)
		}
	}
	for _, x := range main.IncludedPermissions {
		if !wantMain[x] {
			t.Errorf("the role holds %s, which the driver does not need", x)
		}
	}
	for _, x := range slices.Sorted(maps.Keys(wantManaged)) {
		if !slices.Contains(managed.IncludedPermissions, x) {
			t.Errorf("the managed role lacks %s", x)
		}
	}
	for _, x := range managed.IncludedPermissions {
		if !wantManaged[x] {
			t.Errorf("the managed role holds %s, which the driver does not need", x)
		}
		if slices.Contains(main.IncludedPermissions, x) {
			t.Errorf("%s is in both roles", x)
		}
	}
	// Nothing that changes IAM or reads secrets.
	for _, x := range append(slices.Clone(main.IncludedPermissions), managed.IncludedPermissions...) {
		switch x {
		case "compute.instances.setIamPolicy", "compute.images.setIamPolicy", "compute.projects.setCommonInstanceMetadata":
			t.Errorf("the roles hold %s", x)
		}
	}
}
