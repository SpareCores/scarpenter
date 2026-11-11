/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/utils/clock"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/conformance"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/fake"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi"
)

const (
	conformanceCluster = "conformance"
	testRegion         = "GRA11"
	testZone           = "nova"
	testType           = "b3-8"
	multiRegion        = "EU-WEST-PAR"
)

var multiZones = []string{"eu-west-par-a", "eu-west-par-b", "eu-west-par-c"}

// snapshotFlavors are the flavors of the launched OVH types of the committed
// snapshot, as Nova lists them (the root disk is the flavor's disk).
func snapshotFlavors(t testing.TB) []osapi.Flavor {
	t.Helper()
	var out []osapi.Flavor
	for _, s := range ovhServers(t) {
		if ok, _ := Launchable(s.APIReference); !ok {
			continue
		}
		var sizes []float64
		for _, st := range s.Storages {
			sizes = append(sizes, st.Size)
		}
		out = append(out, osapi.Flavor{
			Name: s.APIReference, VCPUs: s.VCPUs, RAMMiB: s.MemoryMiB,
			DiskGB: int64(RootDiskGB(s.APIReference, s.StorageSize, sizes)),
		})
	}
	if len(out) == 0 {
		t.Fatal("the snapshot has no launched OVH types")
	}
	return out
}

// world is one fake OVH project and the drivers over it.
type world struct {
	cloud *fake.Cloud
}

func newWorld(t testing.TB) *world {
	t.Helper()
	c := fake.New()
	fl := snapshotFlavors(t)
	c.AddRegion(testRegion, fake.RegionConfig{Flavors: fl})
	c.AddRegion(multiRegion, fake.RegionConfig{Flavors: fl, AZs: multiZones})
	c.AddRegion("SBG5", fake.RegionConfig{NoEndpoint: true})
	c.AddRegion("XYZ1", fake.RegionConfig{Flavors: fl}) // a catalog region the table does not know
	for _, r := range []string{testRegion, multiRegion} {
		c.AddImage(r, osapi.Image{ID: "image", Name: "conformance-boot", VirtualSizeBytes: 13 << 30,
			Properties: map[string]string{"architecture": "x86_64"}})
	}
	return &world{cloud: c}
}

func (w *world) driver(t testing.TB, opts ...Option) *Driver {
	t.Helper()
	opts = append([]Option{
		WithProvider(w.cloud), WithScheduleWait(WaitOptions{Timeout: 5 * time.Second, Interval: time.Millisecond}),
	}, opts...)
	d := New(opts...)
	if err := d.Init(context.Background(), vendors.Config{
		ClusterName: conformanceCluster, Clock: clock.RealClock{}, Log: logr.Discard(),
	}); err != nil {
		t.Fatal(err)
	}
	return d
}

var (
	worldsMu sync.Mutex
	worlds   = map[vendors.Driver]*world{}
)

func worldOf(t *testing.T, d vendors.Driver) *world {
	t.Helper()
	worldsMu.Lock()
	defer worldsMu.Unlock()
	w, ok := worlds[d]
	if !ok {
		t.Fatal("an unknown driver")
	}
	return w
}

func newConformanceDriver(t *testing.T) vendors.Driver {
	t.Helper()
	w := newWorld(t)
	d := w.driver(t)
	// Warm the flavor table the way the quota tracker does at startup.
	for _, r := range []string{testRegion, multiRegion} {
		if _, err := d.LaunchableTypes(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	worldsMu.Lock()
	worlds[d] = w
	worldsMu.Unlock()
	return d
}

// injected are the errors of Nova and Neutron the driver classifies, by class.
var injected = map[vendors.Class]*osapi.APIError{
	vendors.InsufficientCapacity: {Service: "nova", Status: 500, Message: "No valid host was found. There are not enough hosts available."},
	vendors.Unsupported:          {Service: "nova", Status: 400, Message: "Invalid flavorRef provided."},
	vendors.QuotaExceeded:        {Service: "nova", Status: 403, Message: "Quota exceeded for instances: Requested 1, but already used 10 of 10 instances"},
	vendors.ZoneUnusable:         {Service: "neutron", Status: 409, Message: "No more IP addresses available on network ext-net."},
	vendors.Retryable:            {Service: "nova", Status: 503, Message: "Service Unavailable"},
	vendors.Fatal:                {Service: "nova", Status: 400, Message: "Invalid input for field/attribute name."},
}

func conformanceTypes(t *testing.T) (all, unknown []string) {
	t.Helper()
	for _, s := range ovhServers(t) {
		all = append(all, s.APIReference)
		if ok, _ := Launchable(s.APIReference); !ok {
			unknown = append(unknown, s.APIReference)
		} else {
			unknown = append(unknown, s.APIReference+"/"+vendors.CapacitySpot) // OVH has no spot
		}
	}
	return all, unknown
}

var seedImageID atomic.Int32

// seedImage makes a Glance image the way an operator would: private, in the
// project, with the release in its properties and tag, and the architecture.
func seedImage(t *testing.T, d vendors.Driver, region string, spec conformance.ImageSpec) string {
	t.Helper()
	var tags []string
	for _, k := range sortedKeys(spec.Tags) {
		tags = append(tags, k+"="+spec.Tags[k])
	}
	props := map[string]string{"architecture": "x86_64"}
	if spec.Arch == archARM64 {
		props["architecture"] = "aarch64"
	}
	if spec.Version != "" {
		props[propFlatcarChannel], props[propFlatcarVersion] = spec.Channel, spec.Version
		tags = append(tags, fmt.Sprintf("flatcar-%s-%s", spec.Channel, spec.Version))
	}
	n := seedImageID.Add(1)
	return worldOf(t, d).cloud.AddImage(region, osapi.Image{
		Name: fmt.Sprintf("conformance-%d", n), Tags: tags, Properties: props, Created: spec.Created,
		VirtualSizeBytes: 13 << 30,
	})
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}

func conformanceFixture(t *testing.T, kind vendors.ResourceKind, region, zone string) conformance.Fixture {
	all, unknown := conformanceTypes(t)
	return conformance.Fixture{
		New:    newConformanceDriver,
		Region: region, Zone: zone, InstanceType: testType, MaxUserData: MaxUserData,
		InstanceTypes: all, UnknownQuotaTypes: unknown, StoresIdempotencyKey: true,
		Inject: func(t *testing.T, d vendors.Driver, class vendors.Class, req vendors.CreateRequest) {
			t.Helper()
			worldOf(t, d).cloud.InjectError(req.Region, "CreateServer", injected[class], 1)
		},
		PowerOff: func(t *testing.T, d vendors.Driver, ref vendors.InstanceRef) {
			t.Helper()
			worldOf(t, d).cloud.PowerOff(ref.Region(), ref.ID())
		},
		ExhaustQuota: func(t *testing.T, d vendors.Driver, req vendors.CreateRequest) {
			t.Helper()
			worldOf(t, d).cloud.SetLimits(req.Region, osapi.Limits{MaxInstances: 1})
		},
		Profiles: vendors.Profiles{
			vendors.ProfileStorage: {
				"db":    json.RawMessage(`{"data":[{"name":"pgdata","volumeType":"high-speed","size":"100Gi","mountPath":"/var/lib/pgdata","filesystem":"xfs"}]}`),
				"big":   json.RawMessage(`{"root":{"bootFromVolume":{"volumeType":"high-speed-gen2","size":"200Gi"}}}`),
				"local": json.RawMessage(`{"localStorage":{"policy":"mount","mountPath":"/mnt/local"}}`),
			},
			vendors.ProfilePlacement: {
				"spread":    json.RawMessage(`{"serverGroup":{"policy":"anti-affinity"}}`),
				"colocated": json.RawMessage(`{"serverGroup":{"policy":"affinity"}}`),
			},
		},
		ProfileTypes: []vendors.CatalogType{
			{Vendor: Vendor, APIReference: "b3-8", Family: "b3", VCPUs: 2, LocalStorageGB: 50, LocalStorageCount: 1},
			{Vendor: Vendor, APIReference: "i1-45", Family: "i1", VCPUs: 8, LocalStorageGB: 1950, LocalStorageCount: 3},
			{Vendor: Vendor, APIReference: "d2-2", Family: "d2", VCPUs: 1, LocalStorageGB: 25, LocalStorageCount: 1},
		},
		ResourceKind: kind,
		SeedImage:    seedImage,
		Restart: func(t *testing.T, d vendors.Driver) vendors.Driver {
			t.Helper()
			w := worldOf(t, d)
			d2 := w.driver(t)
			worldsMu.Lock()
			worlds[d2] = w
			worldsMu.Unlock()
			return d2
		},
	}
}

// TestConformance runs the shared driver suite over the launched OVH types of the
// committed snapshot, once per managed kind the suite can check: the NodeClass of
// a server group is not recoverable (it has only a name), see TestServerGroups.
func TestConformance(t *testing.T) {
	for _, tc := range []struct {
		kind         vendors.ResourceKind
		region, zone string
	}{
		{KindSecurityGroups, testRegion, testZone},
		{KindRunSecurityGroups, multiRegion, multiZones[0]},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			conformance.Run(t, conformanceFixture(t, tc.kind, tc.region, tc.zone))
		})
	}
}

func TestMarkerRoundTrip(t *testing.T) {
	tags := map[vendors.TagKey]string{
		vendors.TagCluster: "c", vendors.TagNodeClass: "n", vendors.TagManagedBy: "scarpenter", vendors.TagResource: "securityGroups",
	}
	desc, err := encodeMarker(tags)
	if err != nil || !strings.HasPrefix(desc, markerPrefix) {
		t.Fatal(desc, err)
	}
	if got := decodeMarker(desc); !maps.Equal(got, tags) {
		t.Errorf("round trip: %v", got)
	}
	for _, bad := range []string{"", "user group", markerPrefix + "{", markerPrefix + "[]"} {
		if got := decodeMarker(bad); len(got) != 0 {
			t.Errorf("%q decoded to %v", bad, got)
		}
	}
	tags[vendors.TagNodeClass] = strings.Repeat("n", 300)
	if _, err := encodeMarker(tags); vendors.CodeOf(err) != CodeMarkerTooLong {
		t.Errorf("an oversized marker: %v", err)
	}
}
