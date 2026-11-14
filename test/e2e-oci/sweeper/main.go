/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Command sweeper deletes what test clusters of the L5 lane (OCI e2e) left in the lane's
// compartment: instances, network security groups, subnets, route tables, internet
// gateways, security lists and VCNs of every cluster named scarpenter-ci-* that is older than
// -max-age, in the -regions given (the workflows pass the lane's region) or, with -regions
// all, in every subscribed region. The e2e workflow runs it for its own cluster (-cluster, no
// age limit) as part of the teardown. The selection logic is in pkg/vendors/oci/sweep, tested
// against the in-memory OCI API of the driver. -boot-volumes also deletes the available boot
// volumes of the compartment that are old enough, tagged or not: only for a compartment
// dedicated to the lane.
//
//	go run ./test/e2e-oci/sweeper -dry-run
//
// The account and the identity come from the environment (test/e2e-oci/ocienv); the
// workflows use the keyless token exchange. Every call has a deadline, and so has the whole
// sweep (-timeout). The exit status is 1 when a deletion failed, 2 for a usage or setup error.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"

	"github.com/SpareCores/scarpenter/pkg/vendors/oci"
	"github.com/SpareCores/scarpenter/pkg/vendors/oci/sweep"
	"github.com/SpareCores/scarpenter/test/e2e-oci/ocienv"
)

func main() {
	os.Exit(run())
}

func run() int {
	var (
		cluster = flag.String("cluster", "", "sweep this one cluster (exact name), not every cluster of -cluster-prefix")
		prefix  = flag.String("cluster-prefix", sweep.DefaultClusterPrefix,
			"sweep the clusters whose name starts with this")
		maxAge  = flag.Duration("max-age", 2*time.Hour, "only resources at least this old; 0: any age, also unknown")
		dryRun  = flag.Bool("dry-run", false, "report what would be deleted, delete nothing")
		regions = flag.String("regions", "", "regions, comma-separated; empty or all: every subscribed region")
		volumes = flag.Bool("boot-volumes", false,
			"also delete available boot volumes of the compartment that are old enough (a dedicated compartment only)")
		wait = flag.Duration("wait", 5*time.Minute,
			"how long to wait for an instance to be terminated, and for a resource in use to be released")
		callTimeout = flag.Duration("call-timeout", oci.DefaultCallTimeout, "the deadline of one API call")
		timeout     = flag.Duration("timeout", 25*time.Minute, "the deadline of the whole sweep")
	)
	flag.Parse()

	vars, err := ocienv.Need(os.Getenv, ocienv.Compartment, ocienv.Tenancy)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sweeper:", err)
		return 2
	}
	provider, err := oci.NewConfigurationProvider(ocienv.Credentials(os.Getenv))
	if err != nil {
		fmt.Fprintln(os.Stderr, "sweeper:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	sdk := oci.NewSDK(provider, "scarpenter-sweeper", oci.WithSDKTimeout(*callTimeout))
	o := sweep.Options{
		Compartment: vars[ocienv.Compartment], Tenancy: vars[ocienv.Tenancy],
		Cluster: *cluster, ClusterPrefix: *prefix, MaxAge: *maxAge, DryRun: *dryRun, WaitDeleted: *wait,
		BootVolumes: *volumes,
		Logf:        func(format string, args ...any) { fmt.Printf(format+"\n", args...) },
	}
	if *cluster != "" {
		o.ClusterPrefix = ""
	}
	if o.Regions, err = sweep.ParseRegions(*regions); err != nil {
		fmt.Fprintln(os.Stderr, "sweeper:", err)
		return 2
	}
	var vol sweep.VolumesAPI
	if *volumes {
		vol = &blockVolumes{provider: provider, timeout: *callTimeout, clients: map[string]*core.BlockstorageClient{}}
	}
	rep, err := sweep.Run(ctx, oci.Clients{Compute: sdk, Network: sdk, Identity: sdk, Limits: sdk}, vol, o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sweeper:", err)
		return 2
	}
	fmt.Printf("sweeper: %d deleted, %d would be deleted, %d skipped, %d failed\n",
		rep.Count(sweep.Deleted), rep.Count(sweep.WouldDelete), rep.Count(sweep.Skipped), rep.Count(sweep.Failed))
	if rep.Count(sweep.Failed) > 0 {
		return 1
	}
	return 0
}

// blockVolumes is the Block Volume service of the boot volume sweep, over the SDK: one client
// per region. Its calls end by the HTTP client's timeout, not by a context deadline: the SDK's
// retry loop races on a context that ends first (see pkg/vendors/oci/sdk.go), so the context
// the SDK sees never ends and the retry policy is off.
type blockVolumes struct {
	provider common.ConfigurationProvider
	timeout  time.Duration
	clients  map[string]*core.BlockstorageClient
}

func (b *blockVolumes) client(region string) (*core.BlockstorageClient, error) {
	if c, ok := b.clients[region]; ok {
		return c, nil
	}
	bc, err := core.NewBlockstorageClientWithConfigurationProvider(b.provider)
	if err != nil {
		return nil, fmt.Errorf("creating the block storage client of %s: %w", region, err)
	}
	c := &bc
	c.SetRegion(region)
	c.UserAgent = "scarpenter-sweeper"
	policy := common.NoRetryPolicy()
	c.Configuration.RetryPolicy = &policy
	if hc, ok := c.HTTPClient.(*http.Client); ok {
		hc.Timeout = b.timeout
	}
	b.clients[region] = c
	return c, nil
}

func (b *blockVolumes) ListBootVolumes(ctx context.Context, region string, req core.ListBootVolumesRequest,
) (core.ListBootVolumesResponse, error) {
	c, err := b.client(region)
	if err != nil {
		return core.ListBootVolumesResponse{}, err
	}
	return c.ListBootVolumes(context.WithoutCancel(ctx), req)
}

func (b *blockVolumes) DeleteBootVolume(ctx context.Context, region string, req core.DeleteBootVolumeRequest,
) (core.DeleteBootVolumeResponse, error) {
	c, err := b.client(region)
	if err != nil {
		return core.DeleteBootVolumeResponse{}, err
	}
	return c.DeleteBootVolume(context.WithoutCancel(ctx), req)
}
