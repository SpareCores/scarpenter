/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws

import (
	"context"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// typeInfo is what the driver knows about an instance type from
// DescribeInstanceTypes: the vCPUs the quota counts (DefaultVCpus, never the
// CpuOptions of an instance), and the features profiles depend on.
type typeInfo struct {
	Name       string
	VCPUs      int64
	ENAExpress bool
	Burstable  bool
	Nitro      bool
	// Strategies are the supported placement group strategies.
	Strategies []string
	// LocalNVMe says the type has instance store disks that appear as NVMe
	// devices (the by-id pattern of the local storage hint matches them);
	// LocalDisks is their number.
	LocalNVMe  bool
	LocalDisks int
}

// typeBatch is how many instance types one DescribeInstanceTypes call names.
const typeBatch = 100

// typeTable holds the types the driver has seen. Instance type facts do not
// change, so entries are kept for the life of the process.
type typeTable struct {
	mu     sync.RWMutex
	byName map[string]typeInfo
	// unknown are names DescribeInstanceTypes does not know, until the time.
	unknown map[string]time.Time
	// full says the first region's complete listing was read.
	full bool
	// load serialises the loads.
	load sync.Mutex
}

func newTypeTable() *typeTable {
	return &typeTable{byName: map[string]typeInfo{}, unknown: map[string]time.Time{}}
}

func (t *typeTable) get(name string) (typeInfo, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	ti, ok := t.byName[name]
	return ti, ok
}

func (t *typeTable) isFull() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.full
}

// forget remembers that a type name is unknown until the time.
func (t *typeTable) forget(name string, until time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.unknown[name] = until
}

// unseen returns the names the table neither knows nor has recently found
// unknown.
func (t *typeTable) unseen(names []string, now time.Time) []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []string
	for _, n := range names {
		if _, ok := t.byName[n]; ok {
			continue
		}
		if until, ok := t.unknown[n]; ok && now.Before(until) {
			continue
		}
		out = append(out, n)
	}
	return out
}

func (t *typeTable) add(infos []types.InstanceTypeInfo) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	added := false
	for _, in := range infos {
		ti := typeInfo{Name: string(in.InstanceType), Burstable: in.BurstablePerformanceSupported != nil &&
			*in.BurstablePerformanceSupported}
		if in.VCpuInfo != nil && in.VCpuInfo.DefaultVCpus != nil {
			ti.VCPUs = int64(*in.VCpuInfo.DefaultVCpus)
		}
		if in.NetworkInfo != nil && in.NetworkInfo.EnaSrdSupported != nil {
			ti.ENAExpress = *in.NetworkInfo.EnaSrdSupported
		}
		ti.Nitro = in.Hypervisor == types.InstanceTypeHypervisorNitro
		if in.PlacementGroupInfo != nil {
			for _, s := range in.PlacementGroupInfo.SupportedStrategies {
				ti.Strategies = append(ti.Strategies, string(s))
			}
			slices.Sort(ti.Strategies)
		}
		if is := in.InstanceStorageInfo; is != nil && is.NvmeSupport != types.EphemeralNvmeSupportUnsupported {
			for _, disk := range is.Disks {
				if disk.Count != nil {
					ti.LocalDisks += int(*disk.Count)
				}
			}
			ti.LocalNVMe = ti.LocalDisks > 0
		}
		if prev, ok := t.byName[ti.Name]; !ok || !reflect.DeepEqual(prev, ti) {
			t.byName[ti.Name] = ti
			added = true
		}
	}
	return added
}

// unknownTypeTTL is how long a name DescribeInstanceTypes does not know is not
// asked for again.
const unknownTypeTTL = time.Hour

// describeTypes reads the types by name.
func (d *Driver) describeTypes(ctx context.Context, cl Clients, names []string) ([]types.InstanceTypeInfo, error) {
	in := &ec2.DescribeInstanceTypesInput{}
	for _, n := range names {
		in.InstanceTypes = append(in.InstanceTypes, types.InstanceType(n))
	}
	out, err := cl.EC2.DescribeInstanceTypes(ctx, in)
	if err != nil {
		return nil, err
	}
	return out.InstanceTypes, nil
}

// ensureTypes makes the table know the types: the first call reads the whole
// listing of the region (it is the same in every region for the types they
// share), later calls read what is still missing by name. It bumps the epoch
// of the node options when the table changed, because Supported and the quota
// consumption depend on it.
func (d *Driver) ensureTypes(ctx context.Context, region string, names []string) error {
	t := d.types
	if t.isFull() && len(t.unseen(names, d.clock.Now())) == 0 {
		return nil
	}
	t.load.Lock()
	defer t.load.Unlock()
	cl, err := d.clients(ctx, region)
	if err != nil {
		return err
	}
	changed := false
	if !t.isFull() {
		var token *string
		for {
			out, err := cl.EC2.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{NextToken: token,
				MaxResults: new(int32(typeBatch))})
			if err != nil {
				return classify(err, errContext{})
			}
			changed = t.add(out.InstanceTypes) || changed
			if token = out.NextToken; token == nil {
				break
			}
		}
		t.mu.Lock()
		t.full = true
		t.mu.Unlock()
	}
	now := d.clock.Now()
	missing := t.unseen(names, now)
	for chunk := range slices.Chunk(missing, typeBatch) {
		infos, err := d.describeTypes(ctx, cl, chunk)
		if err != nil {
			if !isCode(err, "InvalidInstanceType") {
				return classify(err, errContext{})
			}
			// One unknown name fails the whole call: ask for the names one at a
			// time, so that the known ones are counted (an unknown type's usage
			// would otherwise be under-counted), and remember the unknown ones for
			// a while instead of asking at every refresh.
			infos = nil
			for _, n := range chunk {
				one, err := d.describeTypes(ctx, cl, []string{n})
				switch {
				case err == nil:
					infos = append(infos, one...)
				case isCode(err, "InvalidInstanceType"):
					t.forget(n, now.Add(unknownTypeTTL))
					d.log.V(1).Info("instance type unknown to the region", "region", region, "type", n)
				default:
					return classify(err, errContext{})
				}
			}
		}
		changed = t.add(infos) || changed
	}
	if changed {
		d.epoch.Add(1)
	}
	return nil
}
