/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws_test

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	opoptions "github.com/SpareCores/scarpenter/pkg/operator/options"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws/fake"
)

func TestRegistered(t *testing.T) {
	if !slices.Contains(vendors.Registered(), "aws") {
		t.Fatalf("the driver is not registered: %v", vendors.Registered())
	}
	if d := aws.New(); d.Vendor() != "aws" {
		t.Errorf("Vendor() = %q, want the Keeper vendor ID", d.Vendor())
	}
}

func TestRegionsAreTheEnabledOnesAndCached(t *testing.T) {
	e := newEnv(t, envOptions{regions: []fake.RegionSpec{
		fake.DefaultRegion("us-east-1", "use1"), fake.DefaultRegion("eu-central-1", "euc1"),
		{Name: "af-south-1", Disabled: true, AZs: fake.DefaultRegion("af-south-1", "afs1").AZs},
	}})
	ctx := context.Background()
	got, err := e.d.Regions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if names := []string{got[0].Name, got[1].Name}; len(got) != 2 || names[0] != "eu-central-1" || names[1] != "us-east-1" {
		t.Errorf("regions %v, want the two enabled ones, sorted (an opt-in region the account has not enabled is not usable)", got)
	}
	if _, err := e.d.Regions(ctx); err != nil || e.acct.Calls("DescribeRegions") != 1 {
		t.Errorf("a second call read the regions again (%d calls)", e.acct.Calls("DescribeRegions"))
	}
	e.clock.Step(61 * time.Minute)
	if _, err := e.d.Regions(ctx); err != nil || e.acct.Calls("DescribeRegions") != 2 {
		t.Errorf("after an hour the regions were not read again (%d calls)", e.acct.Calls("DescribeRegions"))
	}
}

func TestRegionsAreReadFromTheHomeRegion(t *testing.T) {
	var asked []string
	acct := fake.New()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		opt  []aws.Option
		want string
	}{
		{"the default", context.Background(), nil, "us-east-1"},
		{"the flag", opoptions.ToContext(context.Background(), &opoptions.Options{AWSHomeRegion: "eu-central-1"}), nil, "eu-central-1"},
		{"an option", context.Background(), []aws.Option{aws.WithHomeRegion("eu-central-1")}, "eu-central-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked = nil
			d := aws.New(append(tc.opt, aws.WithClientFactory(func(_ context.Context, region string) (aws.Clients, error) {
				asked = append(asked, region)
				return aws.Clients{EC2: acct.EC2(region), Quotas: acct.Quotas(region)}, nil
			}))...)
			if err := d.Init(tc.ctx, vendors.Config{ClusterName: testCluster, Log: logr.Discard()}); err != nil {
				t.Fatal(err)
			}
			if _, err := d.Regions(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(asked) != 1 || asked[0] != tc.want {
				t.Errorf("clients created for %v, want %s", asked, tc.want)
			}
		})
	}
}

func TestClientsAreCreatedLazilyOncePerRegion(t *testing.T) {
	acct := fake.New()
	var created []string
	d := aws.New(aws.WithClientFactory(func(_ context.Context, region string) (aws.Clients, error) {
		created = append(created, region)
		return aws.Clients{EC2: acct.EC2(region), Quotas: acct.Quotas(region)}, nil
	}))
	if err := d.Init(context.Background(), vendors.Config{ClusterName: testCluster, Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
	if len(created) != 0 {
		t.Fatalf("Init created clients: %v", created)
	}
	for range 3 {
		if _, err := d.Zones(context.Background(), "eu-central-1"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Zones(context.Background(), "us-east-1"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(created, []string{"eu-central-1", "us-east-1"}) {
		t.Errorf("clients created for %v", created)
	}
}

func TestInitWithTheSDKMakesNoCall(t *testing.T) {
	// The real client factory builds the configuration from the default
	// credential chain without resolving credentials or calling AWS.
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	d := aws.New()
	if err := d.Init(context.Background(), vendors.Config{ClusterName: testCluster, Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
}

func TestZonesMapKeeperZoneIDsToTheAccountsNames(t *testing.T) {
	// Zone names are account-specific: this account's us-east-1a is use1-az4.
	azs := []fake.AZ{
		{Name: "us-east-1a", ID: "use1-az4"}, {Name: "us-east-1b", ID: "use1-az6"}, {Name: "us-east-1c", ID: "use1-az1"},
		{Name: "us-east-1-bos-1a", ID: "use1-bos1-az1", Type: "local-zone", OptIn: "not-opted-in"},
		{Name: "us-east-1-nyc-1a", ID: "use1-nyc1-az1", Type: "local-zone", OptIn: "opted-in"},
		{Name: "us-east-1d", ID: "use1-az2", OptIn: "not-opted-in"},
	}
	e := newEnv(t, envOptions{regions: []fake.RegionSpec{{Name: "us-east-1", AZs: azs}}})
	got, err := e.d.Zones(context.Background(), "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	want := []vendors.Zone{{ID: "use1-az4", Name: "us-east-1a"}, {ID: "use1-az6", Name: "us-east-1b"}, {ID: "use1-az1", Name: "us-east-1c"}}
	if !slices.Equal(got, want) {
		t.Errorf("zones %v, want %v: availability zones only, opted in only", got, want)
	}
	// A launch in zone name us-east-1a lands in the zone of ID use1-az4.
	inst, err := e.d.CreateInstance(context.Background(), request("us-east-1", "us-east-1a", testType, vendors.CapacityOnDemand))
	if err != nil || inst.Zone != "us-east-1a" {
		t.Errorf("%+v, %v", inst, err)
	}
	// Offerings are by zone ID, as Keeper has them.
	e.acct.SetInstanceTypes(smallTypes)
	e.acct.SetOffering("us-east-1", "m7i.large", "use1-az4")
	l, err := e.d.LaunchableTypes(context.Background(), "us-east-1")
	if err != nil || !slices.Equal(l["m7i.large"], []string{"use1-az4"}) {
		t.Errorf("launchable %v, %v", l["m7i.large"], err)
	}
	// Zones are cached for an hour.
	calls := e.acct.Calls("DescribeAvailabilityZones")
	if _, err := e.d.Zones(context.Background(), "us-east-1"); err != nil || e.acct.Calls("DescribeAvailabilityZones") != calls {
		t.Error("zones were read again")
	}
	if _, err := e.d.Zones(context.Background(), "mars-north-1"); err == nil {
		t.Error("zones of an unknown region")
	}
}

func TestUnknownZoneIsUnusable(t *testing.T) {
	e := newEnv(t, envOptions{})
	_, err := e.d.CreateInstance(context.Background(), request(testRegion, "us-east-1z", testType, vendors.CapacityOnDemand))
	if !vendors.IsClass(err, vendors.ZoneUnusable) {
		t.Errorf("got %v", err)
	}
}

func TestKeeperPrices(t *testing.T) {
	// The recorded shape of Keeper's anonymous /storage_prices (an excerpt of the
	// real response of 2026-11-07, values only): rows with nested objects.
	raw, err := os.ReadFile("testdata/keeper_storage_prices.json")
	if err != nil {
		t.Fatal(err)
	}
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.RequestURI())
		if r.URL.Path != "/storage_prices" {
			http.NotFound(w, r) // the /table/ipv4_price and /table/storage_price of the first version do not exist
			return
		}
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	acct := fake.New()
	acct.SetInstanceTypes(smallTypes)
	d := aws.New(aws.WithClientFactory(func(_ context.Context, region string) (aws.Clients, error) {
		return aws.Clients{EC2: acct.EC2(region), Quotas: acct.Quotas(region)}, nil
	}))
	if err := d.Init(context.Background(), vendors.Config{ClusterName: testCluster, KeeperURL: srv.URL, Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
	off := vendors.Offering{Region: "us-east-1", APIReference: testType}
	if _, complete := d.PriceDelta(d.Defaults(), off); complete {
		t.Error("the price is complete before any price was loaded")
	}
	epoch := d.NodeOptionsEpoch()
	if err := d.LoadPrices(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.NodeOptionsEpoch() == epoch {
		t.Error("the epoch did not change when the prices arrived: the catalog would not reprice")
	}
	// 64 GiB of gp3 at the recorded 0.08 per GB-month, and the public IPv4
	// address at the driver's constant (Keeper has none).
	got, complete := d.PriceDelta(d.Defaults(), off)
	if want := 64*0.08/730 + aws.DefaultIPv4PricePerHour; !complete || math.Abs(got-want) > 1e-9 {
		t.Errorf("PriceDelta = %v, %v after the load; want %v, complete", got, complete, want)
	}
	// eu-central-1 has gp3 but st1 is per IOPS and sc1 is in EUR: ignored.
	if got, complete := d.PriceDelta(d.Defaults(), vendors.Offering{Region: "eu-central-1"}); !complete || got <= 64*0.09/730 {
		t.Errorf("eu-central-1 gp3: %v, %v", got, complete)
	}
	st1 := resolveSettings(t, d, `{"storage":{"root":{"volumeType":"st1","volumeSize":"125Gi"}}}`, defaultNetworking(), nil, options.Selection{})
	if _, complete := d.PriceDelta(st1, vendors.Offering{Region: "eu-central-1"}); complete {
		t.Error("a row in another unit priced st1")
	}
	if _, complete := d.PriceDelta(d.Defaults(), vendors.Offering{Region: "ap-south-1"}); complete {
		t.Error("a region without rows is complete")
	}
	if len(asked) != 1 || asked[0] != "/storage_prices?vendor=aws&currency=USD&limit=-1" {
		t.Errorf("Keeper was asked %v", asked)
	}
}

func TestIPv4PriceIsConfigurable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("[]")) }))
	defer srv.Close()
	t.Setenv(aws.IPv4PriceEnv, "0.01")
	acct := fake.New()
	d := aws.New(aws.WithClientFactory(func(_ context.Context, region string) (aws.Clients, error) {
		return aws.Clients{EC2: acct.EC2(region), Quotas: acct.Quotas(region)}, nil
	}))
	if err := d.Init(context.Background(), vendors.Config{ClusterName: testCluster, KeeperURL: srv.URL, Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
	// No storage price: incomplete, but the IPv4 part is what the variable says.
	net := common.Networking{PublicIPv4: new(true)}
	s := resolveSettings(t, d, `{"storage":{"root":{"volumeSize":"1Gi"}}}`, net, nil, options.Selection{})
	if got, _ := d.PriceDelta(s, vendors.Offering{Region: "us-east-1"}); math.Abs(got-0.01) > 1e-9 {
		t.Errorf("IPv4 part %v, want 0.01", got)
	}
	t.Setenv(aws.IPv4PriceEnv, "free")
	if err := aws.New().Init(context.Background(), vendors.Config{KeeperURL: srv.URL, Log: logr.Discard()}); err == nil {
		t.Error("an unparsable price was accepted")
	}
}

func TestKeeperPricesFailureKeepsThePriceIncomplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", http.StatusBadGateway) }))
	defer srv.Close()
	acct := fake.New()
	d := aws.New(aws.WithClientFactory(func(_ context.Context, region string) (aws.Clients, error) {
		return aws.Clients{EC2: acct.EC2(region), Quotas: acct.Quotas(region)}, nil
	}))
	if err := d.Init(context.Background(), vendors.Config{ClusterName: testCluster, KeeperURL: srv.URL, Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
	if err := d.LoadPrices(context.Background()); err == nil {
		t.Fatal("a failed load reported success")
	}
	if _, complete := d.PriceDelta(d.Defaults(), vendors.Offering{Region: "us-east-1"}); complete {
		t.Error("a failed price load made the price complete")
	}
}

func TestReadingTheRegionsStartsThePriceLoad(t *testing.T) {
	hit := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit <- r.URL.Path
		_ = json.NewEncoder(w).Encode([]any{})
	}))
	defer srv.Close()
	acct := fake.New()
	d := aws.New(aws.WithClientFactory(func(_ context.Context, region string) (aws.Clients, error) {
		return aws.Clients{EC2: acct.EC2(region), Quotas: acct.Quotas(region)}, nil
	}))
	if err := d.Init(context.Background(), vendors.Config{ClusterName: testCluster, KeeperURL: srv.URL, Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Regions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := <-hit; got != "/storage_prices" {
		t.Errorf("the first Keeper request is %s", got)
	}
}
