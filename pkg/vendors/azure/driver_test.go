/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

//nolint:prealloc,lll,dupl,gocyclo,goconst // table-heavy tests: small expectations, repeated fixtures, long SDK signatures
package azure

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/azure/fake"
)

func TestInit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cfg := vendors.Config{ClusterName: testCluster, Log: logr.Discard()}

	require.Error(t, New(WithClients(e.clients)).Init(ctx, vendors.Config{Log: logr.Discard()}), "no cluster name")
	t.Setenv(EnvSubscriptionID, "")
	require.ErrorContains(t, New(WithClients(e.clients)).Init(ctx, cfg), EnvSubscriptionID)

	// The subscription comes from the environment, the clients from the
	// credential chain (nothing is called).
	t.Setenv(EnvSubscriptionID, "sub-env")
	t.Setenv(EnvCredentialsMode, ModeSecret)
	t.Setenv("AZURE_TENANT_ID", "tenant")
	t.Setenv("AZURE_CLIENT_ID", "client")
	t.Setenv("AZURE_CLIENT_SECRET", "secret")
	d := New()
	require.NoError(t, d.Init(ctx, cfg))
	require.Equal(t, "sub-env", d.subscription)
	require.Equal(t, Vendor, d.Vendor())
	require.NotNil(t, d.clients.VMs)

	t.Setenv(EnvCredentialsMode, "nonsense")
	require.ErrorContains(t, New().Init(ctx, cfg), "credentials mode")

	// A price override must be a price.
	t.Setenv(EnvCredentialsMode, "")
	for _, v := range []string{"abc", "-1", "NaN"} {
		t.Setenv(EnvIPv4Price, v)
		require.ErrorContains(t, New(WithClients(e.clients)).Init(ctx, vendors.Config{
			ClusterName: testCluster, Log: logr.Discard(), KeeperURL: "http://keeper.invalid"}), EnvIPv4Price)
	}
	t.Setenv(EnvIPv4Price, "0.01")
	d = New(WithClients(e.clients), WithCreateTimeout(time.Second))
	require.NoError(t, d.Init(ctx, vendors.Config{ClusterName: testCluster, Log: logr.Discard(), KeeperURL: "http://keeper.invalid"}))
	p, ok := d.prices.IPv4Price("westeurope")
	require.True(t, ok)
	require.InDelta(t, 0.01, p, 1e-9)
	require.Equal(t, time.Second, d.compute.CreateTimeout)
	require.True(t, isTrue("true") && isTrue("1") && !isTrue(""))

	cl, err := NewClients("sub-env")
	require.NoError(t, err)
	require.NotNil(t, cl.VMs)
	t.Setenv(EnvCredentialsMode, "nonsense")
	_, err = NewClients("sub-env")
	require.Error(t, err)
}

func TestRegionsAndZones(t *testing.T) {
	e := newEnv(t)
	e.sub.AddLogicalLocation("eastusstg")
	clk := clocktesting.NewFakeClock(time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC))
	d := e.driver(t)
	d.clock = clk
	ctx := context.Background()

	regions, err := d.Regions(ctx)
	require.NoError(t, err)
	names := []string{}
	for _, r := range regions {
		names = append(names, r.Name)
	}
	require.Equal(t, []string{"northeurope", "westcentralus", "westeurope"}, names, "physical locations only, sorted")

	zones, err := d.Zones(ctx, "westeurope")
	require.NoError(t, err)
	require.Equal(t, []vendors.Zone{{ID: "1", Name: "1"}, {ID: "2", Name: "2"}, {ID: "3", Name: "3"}}, zones)
	zones, err = d.Zones(ctx, "westcentralus")
	require.NoError(t, err)
	require.Equal(t, []vendors.Zone{{ID: ZoneNone, Name: ZoneNone}}, zones, "a region without zones is zone 0")
	_, err = d.Zones(ctx, "mars")
	require.True(t, vendors.IsClass(err, vendors.RegionUnavailable), "%v", err)

	// Locations are cached for an hour.
	calls := e.sub.Calls("locations.list")
	_, _ = d.Regions(ctx)
	require.Equal(t, calls, e.sub.Calls("locations.list"))
	clk.Step(2 * time.Hour)
	_, _ = d.Regions(ctx)
	require.Equal(t, calls+1, e.sub.Calls("locations.list"))

	e.sub.FailNext("locations.list", fake.Error(http.StatusInternalServerError, "InternalServerError", "x"), 1)
	clk.Step(2 * time.Hour)
	_, err = d.Regions(ctx)
	require.True(t, vendors.IsClass(err, vendors.Retryable))
}

func TestSKUTableAndOfferings(t *testing.T) {
	e := newEnv(t)
	d := e.driver(t)
	ctx := context.Background()

	types, err := d.LaunchableTypes(ctx, "westeurope")
	require.NoError(t, err)
	require.Equal(t, []string{"1", "2", "3"}, types["Standard_D2s_v5"])
	require.Equal(t, []string{"1", "2"}, types["Standard_D8s_v5"], "zone 3 is restricted for the subscription")
	require.NotContains(t, types, "Standard_A2_v2", "Gen1 only")
	require.NotContains(t, types, "Standard_D2as_v6", "NVMe only")
	require.NotContains(t, types, "Standard_NC6", "restricted in the location")
	z, err := d.LaunchableTypes(ctx, "westcentralus")
	require.NoError(t, err)
	require.Equal(t, []string{ZoneNone}, z["Standard_D2s_v5"])

	// The table is cached and feeds the options and the quota mapping.
	calls := e.sub.Calls("skus.list")
	_, err = d.LaunchableTypes(ctx, "westeurope")
	require.NoError(t, err)
	require.Equal(t, calls, e.sub.Calls("skus.list"))
	_, ok := d.skuFacts("Standard_D2s_v5")
	require.True(t, ok)
	cons, known := d.Consumption("Standard_D2s_v5", vendors.CapacityOnDemand)
	require.True(t, known)
	require.Len(t, cons, 3)
	require.NotZero(t, d.NodeOptionsEpoch(), "new facts change the epoch")

	// A region that is denied, or whose sizes are all restricted, is unusable.
	e.sub.FailNext("skus.list", fake.Error(http.StatusForbidden, "AuthorizationFailed", "no"), 1)
	_, err = d.LaunchableTypes(ctx, "northeurope")
	require.True(t, vendors.IsClass(err, vendors.RegionUnavailable), "%v", err)
	e.sub.AddLocation("restricted", true)
	require.NoError(t, e.sub.AddSKU("restricted", fake.SKU{Name: "Standard_D2s_v5", Family: "f", VCPUs: 2, RestrictedLocation: true}))
	_, err = d.LaunchableTypes(ctx, "restricted")
	require.True(t, vendors.IsClass(err, vendors.RegionUnavailable), "%v", err)
}

func TestVendorArchAndBilling(t *testing.T) {
	d := New()
	for arch, want := range map[string]string{"amd64": "x64", "arm64": "Arm64"} {
		got, ok := d.VendorArch(arch)
		require.True(t, ok)
		require.Equal(t, want, got)
	}
	_, ok := d.VendorArch("riscv")
	require.False(t, ok)
	rule, ok := d.BillingRule("westeurope", "DSv5")
	require.True(t, ok)
	require.Equal(t, time.Minute, *rule.Increment)
	require.Nil(t, rule.Minimum, "no documented minimum: nothing worth holding a node for")
	require.True(t, rule.Verified)
	require.NotEmpty(t, rule.Source)
	require.Equal(t, "scarpenter-use", d.TagKey(vendors.TagUse))
	require.Equal(t, "scarpenter-cluster", d.TagKey(vendors.TagCluster))
}

// launchReq is a launch request of the driver tests, with the resources Prepare
// would have made.
func (e *env) launchReq(t *testing.T, name string) vendors.CreateRequest {
	t.Helper()
	req, _ := request{nodeClaim: name}.build(t)
	req.Image = vendors.Image{Ref: ImageRef{Publisher: DefaultPublisher, Offer: OfferAMD64, SKU: "stable-gen2", Version: "4547.1.0"}.URN()}
	return req
}

func TestCreateInstanceChecks(t *testing.T) {
	e := newEnv(t)
	d := e.driver(t)
	ctx := context.Background()
	_, err := d.LaunchableTypes(ctx, testRegion)
	require.NoError(t, err)

	req := e.launchReq(t, "good")
	inst, err := d.CreateInstance(ctx, req)
	require.NoError(t, err)
	require.Equal(t, vendors.StateRunning, inst.State)
	require.Equal(t, "1", inst.Zone)

	for name, mutate := range map[string]func(*vendors.CreateRequest){
		"not a URN": func(r *vendors.CreateRequest) { r.Image.Ref = "image" },
		"user-data": func(r *vendors.CreateRequest) { r.UserData = make([]byte, MaxUserData+1) },
		"a missing URN": func(r *vendors.CreateRequest) {
			r.Image.Ref = "kinvolk:flatcar-container-linux-corevm-amd64:stable-gen2:1.0.0"
		},
	} {
		r := e.launchReq(t, "bad-"+strings.ReplaceAll(name, " ", "-"))
		mutate(&r)
		_, err := d.CreateInstance(ctx, r)
		require.Equal(t, vendors.Fatal, vendors.ClassOf(err), "%s: %v", name, err)
	}

	r := e.launchReq(t, "zone")
	r.Zone = "4"
	_, err = d.CreateInstance(ctx, r)
	require.True(t, vendors.IsClass(err, vendors.ZoneUnusable), "%v", err)
	r.Region = "mars"
	_, err = d.CreateInstance(ctx, r)
	require.True(t, vendors.IsClass(err, vendors.RegionUnavailable), "%v", err)

	// An image with terms the subscription has not accepted is never launched.
	plan := &ImagePlan{Name: "stable-gen2", Product: OfferAMD64, Publisher: DefaultPublisher}
	e.sub.AddImage(DefaultPublisher, OfferAMD64, "stable-gen2", "4999.0.0", planOf(plan))
	r = e.launchReq(t, "terms")
	r.Image.Ref = ImageRef{Publisher: DefaultPublisher, Offer: OfferAMD64, SKU: "stable-gen2", Version: "4999.0.0"}.URN()
	_, err = d.CreateInstance(ctx, r)
	require.Equal(t, CodeMarketplaceTermsNotAccepted, vendors.CodeOf(err), "%v", err)
	e.sub.AcceptTerms(DefaultPublisher, OfferAMD64, "stable-gen2", true)
	_, err = d.CreateInstance(ctx, r)
	require.NoError(t, err)

	// Transient errors of the SKU read and the image lookup are Retryable.
	e.sub.FailNext("skus.list", fake.Error(http.StatusServiceUnavailable, "ServerBusy", "x"), 1)
	d2 := e.driver(t)
	_, err = d2.CreateInstance(ctx, e.launchReq(t, "busy"))
	require.True(t, vendors.IsClass(err, vendors.Retryable), "%v", err)
}

func TestQuotasWithCounted(t *testing.T) {
	e := newEnv(t)
	d := e.driver(t)
	ctx := context.Background()
	snap, err := d.Quotas(ctx, testRegion)
	require.NoError(t, err)
	require.Empty(t, snap.Counted)
	inst, err := d.CreateInstance(ctx, e.launchReq(t, "counted"))
	require.NoError(t, err)
	snap, err = d.Quotas(ctx, testRegion)
	require.NoError(t, err)
	require.Equal(t, []vendors.InstanceRef{inst.Ref}, snap.Counted)
	g, err := d.GlobalQuotas(ctx)
	require.NoError(t, err)
	require.Empty(t, g.Buckets)

	// A failing tag listing leaves the reservations to expire.
	e.sub.FailNext("resources.list", fake.Error(http.StatusInternalServerError, "InternalServerError", "x"), 1)
	snap, err = d.Quotas(ctx, testRegion)
	require.NoError(t, err)
	require.Empty(t, snap.Counted)
	e.sub.FailNext("usages.compute", fake.Error(http.StatusInternalServerError, "InternalServerError", "x"), 1)
	_, err = d.Quotas(ctx, testRegion)
	require.Error(t, err)
}

func TestFailedLaunchDropsItsNetwork(t *testing.T) {
	e := newEnv(t)
	d := e.driver(t)
	ctx := context.Background()
	_, err := d.LaunchableTypes(ctx, testRegion)
	require.NoError(t, err)
	e.sub.NoCapacity(testRegion, "Standard_D2s_v5", "1", false)
	_, err = d.CreateInstance(ctx, e.launchReq(t, "nocap"))
	require.True(t, vendors.IsClass(err, vendors.InsufficientCapacity), "%v", err)
	require.Empty(t, e.sub.NICNames(), "the NIC of a failed attempt is deleted")
	require.Empty(t, e.sub.PIPNames(), "and its public IP")
	require.Empty(t, e.sub.VMNames())

	// A transient failure before any VM exists drops them too: the NodeClaim may
	// never come back for them (the next attempt makes them again).
	e.sub.ClearFailures()
	e.sub.FailNext("vm.put", fake.Error(http.StatusServiceUnavailable, "ServerBusy", "x"), 1)
	_, err = d.CreateInstance(ctx, e.launchReq(t, "busy"))
	require.True(t, vendors.IsClass(err, vendors.Retryable), "%v", err)
	require.Empty(t, e.sub.NICNames())
	require.Empty(t, e.sub.PIPNames())
	inst, err := d.CreateInstance(ctx, e.launchReq(t, "busy"))
	require.NoError(t, err)
	require.Len(t, e.sub.NICNames(), 1)

	// A NIC that fails takes the public IP made for it along.
	e.sub.FailNext("nic.put", fake.Error(http.StatusServiceUnavailable, "ServerBusy", "x"), 1)
	_, err = d.CreateInstance(ctx, e.launchReq(t, "nicfail"))
	require.True(t, vendors.IsClass(err, vendors.Retryable), "%v", err)
	require.Len(t, e.sub.PIPNames(), 1, "only the running VM's public IP is left")
	require.NoError(t, d.DeleteInstance(ctx, inst.Ref))
	require.Empty(t, e.sub.NICNames(), "the delete options remove the NIC and the public IP")
}

func TestKeeperPrices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/storage_prices", r.URL.Path)
		require.Equal(t, "azure", r.URL.Query().Get("vendor"))
		_, _ = w.Write([]byte(`[
		 {"vendor_id":"azure","region_id":"westeurope","storage_id":"premium_lrs","unit":"GB/month","price":0.15,"currency":"USD","status":"active"},
		 {"vendor_id":"azure","region_id":"westeurope","storage_id":"standardssd_lrs","unit":"GB/month","price":0.08,"currency":"USD","status":"active"},
		 {"vendor_id":"azure","region_id":"westeurope","storage_id":"standard_lrs","unit":"GB/hour","price":1,"currency":"USD","status":"active"},
		 {"vendor_id":"aws","region_id":"westeurope","storage_id":"x","unit":"GB/month","price":1,"currency":"USD","status":"active"}]`))
	}))
	defer srv.Close()
	k := newKeeperPrices(srv.URL, 0.005, nil)
	loaded := make(chan struct{}, 1)
	k.onLoad = func() { loaded <- struct{}{} }
	_, ok := k.StoragePrice("westeurope", "Premium_LRS")
	require.False(t, ok, "nothing is known before the load")
	require.NoError(t, k.load(context.Background()))
	p, ok := k.StoragePrice("westeurope", "Premium_LRS")
	require.True(t, ok)
	require.InDelta(t, 0.15, p, 1e-9)
	_, ok = k.StoragePrice("westeurope", "Standard_LRS")
	require.False(t, ok, "only USD per GB and month rows count")
	_, ok = k.StoragePrice("northeurope", "Premium_LRS")
	require.False(t, ok)
	ip, ok := k.IPv4Price("westeurope")
	require.True(t, ok)
	require.InDelta(t, 0.005, ip, 1e-9)
	<-loaded

	// kick loads in the background, at most once a minute.
	k2 := newKeeperPrices(srv.URL, 0.005, nil)
	done := make(chan struct{}, 1)
	k2.onLoad = func() { done <- struct{}{} }
	now := time.Now()
	k2.kick(now, func(error) {})
	<-done
	// onLoad runs inside the load, before the background load clears loading: a
	// kick before that is a no-op.
	require.Eventually(t, func() bool {
		k2.mu.RLock()
		defer k2.mu.RUnlock()
		return !k2.loading
	}, 10*time.Second, time.Millisecond)
	k2.kick(now.Add(time.Second), func(error) {})
	k2.kick(now.Add(7*time.Hour), func(error) {})
	<-done

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	defer bad.Close()
	require.ErrorContains(t, newKeeperPrices(bad.URL, 0, nil).load(context.Background()), "HTTP 418")
	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{")) }))
	defer junk.Close()
	require.ErrorContains(t, newKeeperPrices(junk.URL, 0, nil).load(context.Background()), "decoding")
	errs := make(chan error, 1)
	k3 := newKeeperPrices(bad.URL, 0, nil)
	k3.kick(time.Now(), func(err error) { errs <- err })
	require.Error(t, <-errs)

	// The driver loads them on the first read of the regions and in LoadPrices.
	e := newEnv(t)
	d := New(WithClients(e.clients), WithSubscription("sub-1"))
	require.NoError(t, d.Init(context.Background(), vendors.Config{ClusterName: testCluster, Log: logr.Discard(), KeeperURL: srv.URL}))
	require.NoError(t, d.LoadPrices(context.Background()))
	_, ok = d.storagePrice("westeurope", "Premium_LRS")
	require.True(t, ok)
	require.NoError(t, New().LoadPrices(context.Background()), "a no-op without a Keeper URL")
	_, ok = New().storagePrice("westeurope", "x")
	require.False(t, ok)
	_, ok = New().ipv4Price("westeurope")
	require.False(t, ok)
}

func TestStaticPrices(t *testing.T) {
	p := StaticPrices{Storage: map[string]float64{"westeurope/premium_lrs": 0.1}, IPv4: map[string]float64{"westeurope": 0.004}}
	v, ok := p.StoragePrice("westeurope", "Premium_LRS")
	require.True(t, ok)
	require.InDelta(t, 0.1, v, 1e-9)
	_, ok = p.StoragePrice("westeurope", "Standard_LRS")
	require.False(t, ok)
	v, ok = p.IPv4Price("westeurope")
	require.True(t, ok)
	require.InDelta(t, 0.004, v, 1e-9)
	_ = armcompute.PurchasePlan{}
}

func TestCollectLeftovers(t *testing.T) {
	e := newEnv(t)
	d := e.driver(t)
	ctx := context.Background()
	_, err := d.LaunchableTypes(ctx, testRegion)
	require.NoError(t, err)
	live, err := d.CreateInstance(ctx, e.launchReq(t, "live"))
	require.NoError(t, err)
	// Two launches interrupted after their network: one whose NodeClaim is gone, one
	// whose NodeClaim still exists (its launch may be in flight).
	for _, nc := range []string{"gone", "pending"} {
		l := e.spec(t, request{nodeClaim: nc})
		pip, err := e.clients.PIPs.CreateOrUpdate(ctx, testRG, l.PIPName(), l.PIP())
		require.NoError(t, err)
		_, err = e.clients.NICs.CreateOrUpdate(ctx, testRG, l.NICName(), l.NIC(*pip.ID))
		require.NoError(t, err)
	}
	require.Len(t, e.sub.NICNames(), 3)
	exists := func(n string) bool { return n == "live" || n == "pending" }

	// Another region holds none of them.
	n, err := d.CollectLeftovers(ctx, "northeurope", exists)
	require.NoError(t, err)
	require.Zero(t, n)

	n, err = d.CollectLeftovers(ctx, testRegion, exists)
	require.NoError(t, err)
	require.Equal(t, 2, n, "the NIC and the public IP of the gone NodeClaim")
	require.Len(t, e.sub.NICNames(), 2)
	require.Len(t, e.sub.PIPNames(), 2)

	// A VM's NIC and address are never collected, even when its NodeClaim is gone
	// (the instance sweep deletes the VM, its delete options the rest).
	n, err = d.CollectLeftovers(ctx, testRegion, func(string) bool { return false })
	require.NoError(t, err)
	require.Equal(t, 2, n, "only the pending launch's")
	require.Len(t, e.sub.NICNames(), 1)
	require.Len(t, e.sub.PIPNames(), 1)
	_, err = d.GetInstance(ctx, live.Ref)
	require.NoError(t, err)

	// Errors are reported, not hidden.
	e.sub.FailNext("resources.list", fake.Error(http.StatusServiceUnavailable, "ServerBusy", "x"), 1)
	_, err = d.CollectLeftovers(ctx, testRegion, exists)
	require.True(t, vendors.IsClass(err, vendors.Retryable), "%v", err)
}
