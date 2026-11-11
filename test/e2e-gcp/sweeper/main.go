/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Command sweeper deletes what test clusters of the L5 lane (GCP e2e) left in a
// project: instances, resource policies, firewall rules, subnetworks and VPC networks
// of every cluster named scarpenter-ci-* that is older than -max-age, in the -regions
// given (the workflows pass the lane's region) or, with -regions all, in every region
// of the project. The e2e workflow runs it for its own cluster (-cluster, no age
// limit) as part of the teardown. The selection logic is in pkg/vendors/gcp/sweep,
// tested against the in-memory Compute API of the GCP driver.
//
//	go run ./test/e2e-gcp/sweeper -project my-project -dry-run
//
// Credentials are Application Default Credentials; the workflows use a federated
// GitHub identity. Every call has a deadline, and so has the whole sweep (-timeout).
// The exit status is 1 when a deletion failed, 2 for a usage or setup error.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/SpareCores/scarpenter/pkg/vendors/gcp"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp/sweep"
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
		regions = flag.String("regions", "", "regions, comma-separated; empty or all: every region of the project")
		project = flag.String("project", firstEnv("GCP_PROJECT", "GOOGLE_CLOUD_PROJECT", "CLOUDSDK_CORE_PROJECT"),
			"the project to sweep (default: $GCP_PROJECT)")
		wait = flag.Duration("wait", 5*time.Minute,
			"how long to wait for one deletion, and for a resource in use to be released")
		callTimeout = flag.Duration("call-timeout", gcp.DefaultCallTimeout, "the deadline of one API call")
		timeout     = flag.Duration("timeout", 25*time.Minute, "the deadline of the whole sweep")
	)
	flag.Parse()

	if *project == "" {
		fmt.Fprintln(os.Stderr, "sweeper: -project (or GCP_PROJECT) is required")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	sdk, err := gcp.NewSDK(ctx, "scarpenter-sweeper")
	if err != nil {
		fmt.Fprintln(os.Stderr, "sweeper:", err)
		return 2
	}
	defer func() { _ = sdk.Close() }()
	api := gcp.WithTimeouts(sdk, *callTimeout, 0)

	o := sweep.Options{
		Cluster: *cluster, ClusterPrefix: *prefix, MaxAge: *maxAge, DryRun: *dryRun, WaitDeleted: *wait,
		Logf: func(format string, args ...any) { fmt.Printf(format+"\n", args...) },
	}
	if *cluster != "" {
		o.ClusterPrefix = ""
	}
	if o.Regions, err = sweep.ParseRegions(*regions); err != nil {
		fmt.Fprintln(os.Stderr, "sweeper:", err)
		return 2
	}
	rep, err := sweep.Run(ctx, api, *project, o)
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

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
