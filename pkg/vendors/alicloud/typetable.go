/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"context"
	"slices"
	"sync"

	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"
	"github.com/alibabacloud-go/tea/dara"
)

// TypeInfo is what the driver knows about an instance type from
// DescribeInstanceTypes: the vCPUs the quota counts, and the facts the
// profiles depend on.
type TypeInfo struct {
	Name      string
	Family    string
	VCPUs     int64
	MemoryMiB int64
	Arch      string // amd64 or arm64
	// NVMe says the type exposes its disks as NVMe devices (8th generation and
	// later), false for virtio-blk.
	NVMe bool
	// DiskQuantity is the number of disks the type can attach.
	DiskQuantity int64
	// LocalDisks is the number of bundled local disks (the i and d families).
	LocalDisks int64
}

// typeBatch is how many instance types one DescribeInstanceTypes call names
// (the API takes 1 to 10).
const typeBatch = 10

// TypeTable holds the types the driver has seen. Instance type facts do not
// change, so entries are kept for the life of the process.
type TypeTable struct {
	mu     sync.RWMutex
	byName map[string]TypeInfo
}

// NewTypeTable returns an empty table.
func NewTypeTable() *TypeTable { return &TypeTable{byName: map[string]TypeInfo{}} }

// Get returns the type's facts.
func (t *TypeTable) Get(name string) (TypeInfo, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	ti, ok := t.byName[name]
	return ti, ok
}

// Put adds or replaces a type (tests, and the loader).
func (t *TypeTable) Put(ti TypeInfo) {
	t.mu.Lock()
	t.byName[ti.Name] = ti
	t.mu.Unlock()
}

// Len is the number of known types.
func (t *TypeTable) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.byName)
}

func (t *TypeTable) unseen(names []string) []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []string
	for _, n := range names {
		if _, ok := t.byName[n]; !ok && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// typeInfoOf converts the SDK's type to TypeInfo. The vCPU count is
// CpuCoreCount (ECS names the number of vCPUs "CpuCoreCount"; Keeper's vcpus
// is the same figure).
func typeInfoOf(it *ecs.DescribeInstanceTypesResponseBodyInstanceTypesInstanceType) TypeInfo {
	name := deref(it.InstanceTypeId)
	family, _ := FamilyOf(name)
	arch := "amd64"
	if deref(it.CpuArchitecture) == "ARM" {
		arch = "arm64"
	}
	return TypeInfo{
		Name: name, Family: family, VCPUs: int64(deref(it.CpuCoreCount)),
		MemoryMiB: int64(float64(deref(it.MemorySize))*1024 + 0.5),
		Arch:      arch, NVMe: deref(it.NvmeSupport) == "required" || deref(it.NvmeSupport) == "supported",
		DiskQuantity: int64(deref(it.DiskQuantity)), LocalDisks: int64(deref(it.LocalStorageAmount)),
	}
}

// LoadTypes reads the facts of the named types of a region that the table does
// not know yet (ten names a call). A name ECS does not return stays unknown.
func (c *Cloud) LoadTypes(ctx context.Context, region string, names []string) error {
	todo := c.Types.unseen(names)
	if len(todo) == 0 {
		return nil
	}
	cl, err := c.Clients(ctx, region)
	if err != nil {
		return err
	}
	for batch := range slices.Chunk(todo, typeBatch) {
		var token *string
		for {
			in := &ecs.DescribeInstanceTypesRequest{InstanceTypes: dara.StringSlice(batch), NextToken: token}
			out, err := call(ctx, c, callTimeout, errContext{},
				func(ctx context.Context) (*ecs.DescribeInstanceTypesResponseBody, error) {
					return cl.ECS.DescribeInstanceTypes(ctx, in)
				})
			if err != nil {
				return err
			}
			if out.InstanceTypes != nil {
				for _, it := range out.InstanceTypes.InstanceType {
					c.Types.Put(typeInfoOf(it))
				}
			}
			if deref(out.NextToken) == "" {
				break
			}
			token = out.NextToken
		}
	}
	return nil
}

// LoadAllTypes reads every instance type of a region (the first region's
// listing fills the table for the process).
func (c *Cloud) LoadAllTypes(ctx context.Context, region string) error {
	cl, err := c.Clients(ctx, region)
	if err != nil {
		return err
	}
	var token *string
	for {
		in := &ecs.DescribeInstanceTypesRequest{NextToken: token}
		out, err := call(ctx, c, callTimeout, errContext{},
			func(ctx context.Context) (*ecs.DescribeInstanceTypesResponseBody, error) {
				return cl.ECS.DescribeInstanceTypes(ctx, in)
			})
		if err != nil {
			return err
		}
		if out.InstanceTypes != nil {
			for _, it := range out.InstanceTypes.InstanceType {
				c.Types.Put(typeInfoOf(it))
			}
		}
		if deref(out.NextToken) == "" {
			return nil
		}
		token = out.NextToken
	}
}
