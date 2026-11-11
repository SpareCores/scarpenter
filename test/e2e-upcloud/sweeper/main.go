/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Command sweeper deletes what test clusters of the L5 lane (UpCloud e2e) left in
// the account: servers (stopped, then deleted with their storages), server groups,
// SDN rulesets and networks and unattached disks of every cluster named
// scarpenter-ci-* that is older than -max-age. UpCloud has no regions: one account
// call covers every zone. The e2e workflow runs it for its own cluster (-cluster,
// no age limit) as the last step of the teardown. The selection logic is in
// pkg/vendors/upcloud/sweep, tested against the in-memory UpCloud of the driver.
//
//	go run ./test/e2e-upcloud/sweeper -dry-run
//
// The credential is the API token in UPCLOUD_TOKEN (UpCloud has no workload
// identity). The exit status is 1 when a deletion failed.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/SpareCores/scarpenter/internal/version"
	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/api"
	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/sweep"
)

func main() {
	os.Exit(run())
}

func run() int {
	var (
		cluster = flag.String("cluster", "", "sweep this one cluster (exact name), not every cluster of -cluster-prefix")
		prefix  = flag.String("cluster-prefix", sweep.DefaultClusterPrefix, "sweep the clusters whose name starts with this")
		maxAge  = flag.Duration("max-age", 2*time.Hour, "only resources at least this old; 0: any age, also unknown")
		dryRun  = flag.Bool("dry-run", false, "report what would be deleted, delete nothing")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	c, err := api.NewFromEnv("scarpenter-sweeper/" + version.Version)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sweeper:", err)
		return 2
	}
	o := sweep.Options{
		Cluster: *cluster, ClusterPrefix: *prefix, MaxAge: *maxAge, DryRun: *dryRun,
		Logf: func(format string, args ...any) { fmt.Printf(format+"\n", args...) },
	}
	if *cluster != "" {
		o.ClusterPrefix = ""
	}
	rep, err := sweep.Run(ctx, c, o)
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
