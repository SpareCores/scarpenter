/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Command sweeper deletes what test clusters of the L5 lane (Vultr e2e) left in an
// account: instances, firewall groups and VPCs of every cluster named scarpenter-ci-*
// that is older than -max-age, in the -regions given (the instances and the VPCs; firewall
// groups are account-wide). The e2e workflow runs it for its own cluster (-cluster, no age
// limit) as part of the teardown. It never touches snapshots: the Flatcar snapshot is
// permanent. The selection logic is in pkg/vendors/vultr/sweep, tested against the
// in-memory Vultr API of the driver.
//
//	VULTR_API_KEY=... go run ./test/e2e-vultr/sweeper -dry-run
//
// The exit status is 1 when a deletion failed.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/SpareCores/scarpenter/pkg/vendors/vultr"
	"github.com/SpareCores/scarpenter/pkg/vendors/vultr/sweep"
)

func main() {
	os.Exit(run())
}

func run() int {
	var (
		cluster = flag.String("cluster", "", "sweep this one cluster (exact name), not every cluster of -cluster-prefix")
		prefix  = flag.String("cluster-prefix", sweep.DefaultClusterPrefix, "sweep the clusters whose name starts with this")
		maxAge  = flag.Duration("max-age", 2*time.Hour, "only objects at least this old; 0: any age, also unknown")
		dryRun  = flag.Bool("dry-run", false, "report what would be deleted, delete nothing")
		regions = flag.String("regions", "", "regions of the instances and VPCs, comma-separated; empty or all: every region")
		wait    = flag.Duration("wait", 5*time.Minute, "wait for deleted instances to be gone before the groups and VPCs")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	api, err := vultr.NewClient(vultr.ClientConfig{APIKey: os.Getenv(vultr.APIKeyEnv)})
	if err != nil {
		fmt.Fprintln(os.Stderr, "sweeper:", err)
		return 2
	}
	o := sweep.Options{
		Cluster: *cluster, ClusterPrefix: *prefix, MaxAge: *maxAge, DryRun: *dryRun, WaitGone: *wait,
		Logf: func(format string, args ...any) { fmt.Printf(format+"\n", args...) },
	}
	if *cluster != "" {
		o.ClusterPrefix = ""
	}
	if r := strings.TrimSpace(*regions); r != "" && r != "all" {
		for x := range strings.SplitSeq(r, ",") {
			if x = strings.TrimSpace(x); x != "" {
				o.Regions = append(o.Regions, x)
			}
		}
	}
	rep, err := sweep.Run(ctx, api, o)
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
