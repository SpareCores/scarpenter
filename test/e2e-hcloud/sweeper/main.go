/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Command sweeper deletes what test clusters of the L5 lane (Hetzner Cloud e2e) left in the CI
// project: servers, volumes, placement groups, firewalls and unassigned Primary IPs of every
// cluster named scarpenter-ci-* that is older than -max-age. The e2e workflow runs it for its own
// cluster (-cluster, no age limit) as part of the teardown. The selection logic is in
// ./sweep, tested against the in-memory API of the Hetzner driver. The Flatcar snapshots are
// never listed.
//
//	HCLOUD_TOKEN=... go run ./test/e2e-hcloud/sweeper -dry-run
//
// The token is the CI project's (HCLOUD_TOKEN). The exit status is 1 when a deletion failed.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/SpareCores/scarpenter/pkg/vendors/hcloud"
	"github.com/SpareCores/scarpenter/test/e2e-hcloud/sweeper/sweep"
)

func main() {
	os.Exit(run())
}

// ips lists the Primary IPs with the SDK (the driver's API has no list call).
type ips struct{ c *hc.Client }

func (i ips) PrimaryIPs(ctx context.Context) ([]*hc.PrimaryIP, error) {
	return i.c.PrimaryIP.All(ctx)
}

func run() int {
	var (
		cluster = flag.String("cluster", "", "sweep this one cluster (exact name), not the clusters of -cluster-prefix")
		prefix  = flag.String("cluster-prefix", sweep.DefaultClusterPrefix, "sweep the clusters whose name starts with this")
		maxAge  = flag.Duration("max-age", 2*time.Hour, "only resources at least this old; 0: any age")
		dryRun  = flag.Bool("dry-run", false, "report what would be deleted, delete nothing")
		wait    = flag.Duration("wait", 5*time.Minute, "how long to wait for deleted servers to be gone first")
	)
	flag.Parse()

	token := os.Getenv("HCLOUD_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "sweeper: HCLOUD_TOKEN is not set")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	api := hcloud.NewClient(hcloud.ClientConfig{
		Token: token, Endpoint: os.Getenv("HCLOUD_ENDPOINT"), Version: "e2e-sweeper",
	})
	opts := []hc.ClientOption{hc.WithToken(token), hc.WithApplication("scarpenter-sweeper", "e2e")}
	if e := os.Getenv("HCLOUD_ENDPOINT"); e != "" {
		opts = append(opts, hc.WithEndpoint(e))
	}
	o := sweep.Options{
		Cluster: *cluster, ClusterPrefix: *prefix, MaxAge: *maxAge, DryRun: *dryRun, WaitDeleted: *wait,
		Logf: func(format string, args ...any) { fmt.Printf(format+"\n", args...) },
	}
	if *cluster != "" {
		o.ClusterPrefix = ""
	}
	rep, err := sweep.Run(ctx, api, ips{hc.NewClient(opts...)}, o)
	fmt.Printf("sweeper: %d deleted, %d would be deleted, %d skipped, %d failed\n",
		rep.Count(sweep.Deleted), rep.Count(sweep.WouldDelete), rep.Count(sweep.Skipped), rep.Count(sweep.Failed))
	if err != nil {
		fmt.Fprintln(os.Stderr, "sweeper:", err)
		return 1
	}
	if rep.Count(sweep.Failed) > 0 {
		return 1
	}
	return 0
}
