/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Command sweeper deletes what test clusters of the L5 lane (AWS e2e) left in an
// account: instances, placement groups, security groups and the managed network
// of every cluster named scarpenter-ci-* that is older than -max-age, in every
// enabled region. The e2e workflow runs it for its own cluster (-cluster, no age
// limit) as the last step of the teardown. The selection logic is in
// pkg/vendors/aws/sweep, tested against the in-memory EC2 of the AWS driver.
//
//	go run ./test/e2e-aws/sweeper -dry-run
//
// Credentials come from the default AWS chain; the workflows use GitHub's OIDC
// role. The exit status is 1 when a deletion failed.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	awsdriver "github.com/SpareCores/scarpenter/pkg/vendors/aws"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws/sweep"
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
		regions = flag.String("regions", "", "comma-separated regions (default: every enabled region of the account)")
		home    = flag.String("home-region", envOr("AWS_REGION", "us-east-1"), "region the enabled regions are listed from")
		wait    = flag.Duration("wait", 5*time.Minute, "per region, how long to wait for terminated instances to be gone")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	factory, err := awsdriver.NewClientFactory(ctx, *home)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sweeper:", err)
		return 2
	}
	o := sweep.Options{
		Cluster: *cluster, ClusterPrefix: *prefix, MaxAge: *maxAge, DryRun: *dryRun, WaitTerminated: *wait,
		Logf: func(format string, args ...any) { fmt.Printf(format+"\n", args...) },
	}
	if *cluster != "" {
		o.ClusterPrefix = ""
	}
	if *regions != "" {
		o.Regions = strings.Split(*regions, ",")
	}
	rep, err := sweep.Run(ctx, factory, *home, o)
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

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
