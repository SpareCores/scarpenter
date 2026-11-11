/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Command sweeper deletes what the OVH L5 lane left in the Public Cloud project: servers,
// security groups and server groups of every cluster named scarpenter-ci-* that is older
// than -max-age, in the -regions given (the workflows pass the lane's region, the only one
// activated in the lane's project) or, with -regions all, in every region of the catalog.
// The e2e workflow runs it for its own cluster (-cluster, no age limit) as the last step of
// the teardown. It never touches images. The selection logic is in pkg/vendors/ovh/sweep,
// tested against the driver's in-memory OpenStack.
//
//	go run ./test/e2e-ovh/sweeper -dry-run
//
// Credentials come from the standard OpenStack environment (OS_AUTH_URL, OS_USERNAME,
// OS_PASSWORD, OS_PROJECT_ID, OS_USER_DOMAIN_NAME); the workflows set them from the
// environment's secrets. The exit status is 1 when a deletion failed.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi/sdk"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/sweep"
)

func main() {
	os.Exit(run())
}

func run() int {
	var (
		cluster = flag.String("cluster", "", "sweep this one cluster (exact name), not every cluster of -cluster-prefix")
		prefix  = flag.String("cluster-prefix", sweep.DefaultClusterPrefix, "sweep the clusters whose name starts with this")
		maxAge  = flag.Duration("max-age", 2*time.Hour, "only resources at least this old; 0: any age")
		dryRun  = flag.Bool("dry-run", false, "report what would be deleted, delete nothing")
		regions = flag.String("regions", "", "catalog region names, comma-separated; empty or all: every one")
		wait    = flag.Duration("wait", 5*time.Minute, "per region, how long to wait for deleted servers to be gone")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	p, err := sdk.FromEnv(ctx, sdk.Options{UserAgent: "scarpenter-sweeper"})
	if err != nil {
		fmt.Fprintln(os.Stderr, "sweeper:", err)
		return 2
	}
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
	rep, err := sweep.Run(ctx, p, o)
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
