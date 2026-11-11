/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package main

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"

	"github.com/SpareCores/scarpenter/pkg/vendors/azure"
	"github.com/SpareCores/scarpenter/pkg/vendors/azure/fake"
)

const azureSub = "00000000-0000-0000-0000-000000000000"

func azureConfig(t *testing.T) *config {
	t.Helper()
	cfg := testConfig(releases(t))
	cfg.vendor, cfg.region, cfg.subscription = "azure", "westeurope", azureSub
	cfg.flatcarVersion = "4757.2.3"
	cfg.types = []string{"azure.Standard_B2ats_v2", "azure.Standard_D2ps_v6", "azure.Standard_D2as_v5"}
	return cfg
}

var testPlan = &armcompute.PurchasePlan{Name: new("stable-gen2"), Product: new(azure.OfferAMD64), Publisher: new(azure.DefaultPublisher)}

// azureAccount is a fake subscription that satisfies the lane: the region and its zones, the
// lane's sizes (B-series without Spot, the others with), the quota, the pinned images and
// their accepted terms.
func azureAccount(t *testing.T) *fake.Subscription {
	t.Helper()
	sub := fake.New(azureSub)
	for _, loc := range []string{"westeurope", "northeurope"} {
		sub.AddLocation(loc, true)
		for _, k := range []fake.SKU{
			{Name: "Standard_B2ats_v2", Family: "standardBasv2Family", VCPUs: 2, NoSpot: true},
			{Name: "Standard_D2ps_v6", Family: "standardDPSv6Family", VCPUs: 2, Arm: true},
			{Name: "Standard_D2as_v5", Family: "standardDasv5Family", VCPUs: 2},
			{Name: "Standard_D64s_v5", Family: "standardDSv5Family", VCPUs: 64},
		} {
			if err := sub.AddSKU(loc, k); err != nil {
				t.Fatal(err)
			}
		}
		for usage, limit := range map[string]int64{
			"cores": 64, "lowPriorityCores": 32, "standardBasv2Family": 32, "standardDPSv6Family": 32,
			"standardDasv5Family": 32, "standardDSv5Family": 64, "StandardPublicIPAddresses": 20,
		} {
			sub.SetLimit(loc, usage, limit)
		}
	}
	sub.AddImage(azure.DefaultPublisher, azure.OfferAMD64, "stable-gen2", "4757.2.3", testPlan)
	sub.AcceptTerms(azure.DefaultPublisher, azure.OfferAMD64, "stable-gen2", true)
	sub.AddImage(azure.DefaultPublisher, azure.OfferARM64, "stable", "4757.2.3", nil)
	return sub
}

func runAzureCanary(t *testing.T, sub *fake.Subscription, cfg *config) (*report, *audit) {
	t.Helper()
	rep := &report{}
	a := newAudit()
	checkArtifacts(context.Background(), cfg, rep)
	runAzure(context.Background(), cfg, rep, readOnlyAzure(fakeClients(sub), a), a)
	return rep, a
}

func TestAzureCanaryPassesOnAHealthyWorld(t *testing.T) {
	rep, a := runAzureCanary(t, azureAccount(t), azureConfig(t))
	var b bytes.Buffer
	rep.write(&b)
	if f := failed(rep); len(f) > 0 {
		t.Fatalf("failed checks %v:\n%s", f, b.String())
	}
	for _, want := range []string{"sysext kubernetes v1.37.1", "regions and zones westeurope", "zones 1 2 3",
		"Flatcar stable 4757.2.3 amd64", "Flatcar stable 4757.2.3 arm64", "marketplace terms amd64", "marketplace terms arm64",
		"accepted=true", "no purchase plan", "instance types offered in westeurope", "Standard_D2as_v5(3 zones,2 vCPU,standardDasv5Family,spot)",
		"Standard_B2ats_v2(3 zones,2 vCPU,standardBasv2Family)", "quota vCPUs and public IPs westeurope", "family standardDPSv6Family",
		"read-only guard", "SKUs.List", "Usages.ComputeUsages", "Agreements.Accepted"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("the table has no %q:\n%s", want, b.String())
		}
	}
	if r := a.refusals(); len(r) > 0 {
		t.Errorf("the canary tried writes: %v", r)
	}
}

func TestAzureCanaryReportsWhatMoved(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*fake.Subscription, *config)
		want   []string
	}{
		"the Flatcar version is not published": {
			mutate: func(_ *fake.Subscription, c *config) { c.flatcarVersion = "9999.0.0" },
			want:   []string{"Flatcar stable 9999.0.0 amd64", "Flatcar stable 9999.0.0 arm64"},
		},
		"the terms are not accepted": {
			mutate: func(s *fake.Subscription, _ *config) {
				s.AcceptTerms(azure.DefaultPublisher, azure.OfferAMD64, "stable-gen2", false)
			},
			want: []string{"Flatcar stable 4757.2.3 amd64", "marketplace terms amd64"},
		},
		"the regional quota shrank": {
			mutate: func(s *fake.Subscription, _ *config) { s.SetLimit("westeurope", "cores", 8) },
			want:   []string{"quota vCPUs and public IPs westeurope"},
		},
		"the Spot quota shrank": {
			mutate: func(s *fake.Subscription, _ *config) { s.SetLimit("westeurope", "lowPriorityCores", 2) },
			want:   []string{"quota vCPUs and public IPs westeurope"},
		},
		"the family quota shrank": {
			mutate: func(s *fake.Subscription, _ *config) { s.SetLimit("westeurope", "standardDPSv6Family", 4) },
			want:   []string{"quota vCPUs and public IPs westeurope"},
		},
		"the public IP quota is gone": {
			mutate: func(s *fake.Subscription, _ *config) { s.SetLimit("westeurope", "StandardPublicIPAddresses", 1) },
			want:   []string{"quota vCPUs and public IPs westeurope"},
		},
		"a type is restricted in the region": {
			mutate: func(s *fake.Subscription, _ *config) {
				_ = s.AddSKU("westeurope", fake.SKU{Name: "Standard_D2as_v5", Family: "standardDasv5Family", VCPUs: 2, RestrictedLocation: true})
			},
			want: []string{"instance types offered in westeurope"},
		},
		"a type is unknown": {
			mutate: func(_ *fake.Subscription, c *config) { c.types = append(c.types, "azure.Standard_Nonexistent") },
			want:   []string{"instance types offered in westeurope"},
		},
		"no lane type can run as Spot": {
			mutate: func(_ *fake.Subscription, c *config) { c.types = []string{"azure.Standard_B2ats_v2"} },
			want:   []string{"instance types offered in westeurope"},
		},
		"the region is not in the subscription": {
			mutate: func(_ *fake.Subscription, c *config) { c.region = "mars" },
			want:   []string{"regions and zones mars"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			sub := azureAccount(t)
			cfg := azureConfig(t)
			tc.mutate(sub, cfg)
			rep, _ := runAzureCanary(t, sub, cfg)
			got := failed(rep)
			for _, w := range tc.want {
				if !slices.Contains(got, w) {
					var b bytes.Buffer
					rep.write(&b)
					t.Fatalf("want %q among the failed checks, got %v:\n%s", w, got, b.String())
				}
			}
		})
	}
}

func TestAzureCanaryNeverWrites(t *testing.T) {
	sub := azureAccount(t)
	runAzureCanary(t, sub, azureConfig(t))
	for _, op := range []string{"vm.put", "vm.delete", "nic.put", "nic.delete", "pip.put", "pip.delete", "group.put", "group.delete",
		"vnet.put", "vnet.delete", "nsg.put", "nsg.delete", "asg.put", "asg.delete", "ppg.put", "ppg.delete"} {
		if n := sub.Calls(op); n != 0 {
			t.Errorf("%s was called %d times", op, n)
		}
	}
}

func TestLoadConfigAzure(t *testing.T) {
	env := map[string]string{
		"AZURE_REGION": "westeurope", "AZURE_SUBSCRIPTION_ID": azureSub, "E2E_FLATCAR_CHANNEL": "stable", "E2E_FLATCAR_VERSION": "4757.2.3",
		"E2E_KUBERNETES_VERSION": "v1.37.1", "E2E_TAILSCALE_VERSION": "v1.98.10", "E2E_K3S_VERSION": "v1.37.2+k3s1",
		"E2E_TYPE_AMD64": "azure.Standard_B2ats_v2", "E2E_TYPE_ARM64": "azure.Standard_D2ps_v6", "E2E_TYPE_DB": "azure.Standard_D2as_v5",
		"E2E_TYPES_DESCRIBED": "azure.Standard_B2ats_v2", "E2E_MIN_VCPU_OD": "16", "E2E_MIN_VCPU_SPOT": "4",
	}
	c, err := loadConfig("azure", func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.region != "westeurope" || c.subscription != azureSub || c.minOnDemand != 16 || len(c.types) != 4 {
		t.Errorf("config %+v", c)
	}
	delete(env, "AZURE_SUBSCRIPTION_ID")
	if _, err := loadConfig("azure", func(k string) string { return env[k] }); err == nil || !strings.Contains(err.Error(), "AZURE_SUBSCRIPTION_ID") {
		t.Errorf("a missing subscription: %v", err)
	}
	delete(env, "AZURE_REGION")
	if _, err := loadConfig("azure", func(k string) string { return env[k] }); err == nil || !strings.Contains(err.Error(), "AZURE_REGION") {
		t.Errorf("a missing region: %v", err)
	}
}
