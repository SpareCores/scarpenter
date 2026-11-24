//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider_test

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/vultr/govultr/v3"
	testingclock "k8s.io/utils/clock/testing"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/cloudprovider"
	"github.com/SpareCores/scarpenter/pkg/unavailable"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/vultr"
	vultrfake "github.com/SpareCores/scarpenter/pkg/vendors/vultr/fake"
)

// Core calls Delete again until it says NodeClaimNotFound. A Vultr bare-metal
// server is halted, waited for and only then deleted, one step per call, and the
// DELETE goes out on the first call that sees it stopped, not after the
// terminating grace.
func TestDeleteOfAVultrBareMetalServerStopsItFirstAndDeletesAtOnce(t *testing.T) {
	acct := vultrfake.New()
	acct.StopGets = 2 // the CloudProvider reads the server too
	acct.AddPlan("vbm-4c-32gb", 4, 32768, 240)
	acct.AddRegion("ewr", "vbm-4c-32gb")
	clk := testingclock.NewFakeClock(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	acct.SetClock(clk.Now)
	drv := vultr.New(vultr.WithAPI(acct))
	if err := drv.Init(t.Context(), vendors.Config{
		ClusterName: cluster, Clock: clk, Log: logr.Discard(), RegistrationTimeout: time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	cp := cloudprovider.New(cloudprovider.Options{
		Registry: vendors.NewRegistryFrom(drv), Clock: clk, ClusterName: cluster,
		Unavailable: unavailable.New(clk, unavailable.TTLs{InsufficientCapacity: time.Minute}),
	})
	srv, err := acct.CreateBareMetal(t.Context(), &govultr.BareMetalCreate{
		Region: "ewr", Plan: "vbm-4c-32gb", Tags: []string{
			vendors.DefaultTagKey(vendors.TagCluster) + "=" + cluster,
			vendors.DefaultTagKey(vendors.TagNodeClaim) + "=claim-bm",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	acct.Advance()
	claim := &karpv1.NodeClaim{}
	claim.Name = "claim-bm"
	claim.Status.ProviderID = vultr.ProviderID("ewr", "claim-bm")
	claim.Annotations = map[string]string{v1alpha1.AnnotationInstanceRef: "ewr/bm:" + srv.ID}

	// Call 1 halts, call 2 still sees the server running: both fail, retryably,
	// and nothing is deleted.
	for i := 1; i <= 2; i++ {
		err := cp.Delete(t.Context(), claim)
		if err == nil || karpcloudprovider.IsNodeClaimNotFoundError(err) {
			t.Fatalf("Delete #%d = %v, want a retryable error", i, err)
		}
		if acct.Calls("DeleteBareMetal") != 0 {
			t.Fatalf("Delete #%d sent the DELETE", i)
		}
		clk.Step(10 * time.Second)
	}
	if acct.Calls("HaltBareMetal") != 1 {
		t.Fatalf("%d halts, want 1", acct.Calls("HaltBareMetal"))
	}
	// Call 3 sees it stopped (terminating to the CloudProvider) and deletes it,
	// seconds after the stop, far inside the terminating grace.
	if err := cp.Delete(t.Context(), claim); err != nil {
		t.Fatalf("Delete #3 = %v, want nil", err)
	}
	if acct.Calls("DeleteBareMetal") != 1 || len(acct.BareMetals()) != 0 {
		t.Fatalf("%d deletes, %d servers left", acct.Calls("DeleteBareMetal"), len(acct.BareMetals()))
	}
	// Only now is the NodeClaim released.
	clk.Step(10 * time.Second)
	if err := cp.Delete(t.Context(), claim); !karpcloudprovider.IsNodeClaimNotFoundError(err) {
		t.Fatalf("Delete #4 = %v, want NodeClaimNotFound", err)
	}
	if len(acct.DeletedWhileRunning) != 0 {
		t.Errorf("deleted while running: %v", acct.DeletedWhileRunning)
	}
}
