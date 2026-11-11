/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Command sweeper deletes what test clusters of the L5 lane (Azure e2e) left in the
// lane's resource group: virtual machines, network interfaces, public IP addresses,
// managed disks, network security groups, virtual networks and proximity placement
// groups of every cluster named scarpenter-ci-* that is older than -max-age, and the
// resource group itself when it is a disposable, empty one. The e2e workflow runs it for
// its own cluster (-cluster, no age limit) as the last step of the teardown. The selection
// logic is in pkg/vendors/azure/sweep, tested against the in-memory ARM of the Azure
// driver.
//
//	AZURE_SUBSCRIPTION_ID=... AZURE_RESOURCE_GROUP=... go run ./test/e2e-azure/sweeper -dry-run
//
// Credentials come from the driver's chain (the workflows log in with GitHub's OIDC and
// the Azure CLI's session, SCARPENTER_AZURE_DEVELOPMENT=true, or use workload identity).
// The exit status is 1 when a deletion failed, 2 for a usage or setup error.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/SpareCores/scarpenter/pkg/vendors/azure"
	"github.com/SpareCores/scarpenter/pkg/vendors/azure/sweep"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// settings are what the command line and the environment select.
type settings struct {
	subscription string
	opts         sweep.Options
}

// parse reads the flags; the subscription and the resource group default to
// AZURE_SUBSCRIPTION_ID and AZURE_RESOURCE_GROUP.
func parse(args []string, getenv func(string) string, stderr io.Writer) (*settings, error) {
	fs := flag.NewFlagSet("sweeper", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		subscription = fs.String("subscription", getenv("AZURE_SUBSCRIPTION_ID"),
			"subscription ID (default: AZURE_SUBSCRIPTION_ID)")
		group = fs.String("resource-group", getenv("AZURE_RESOURCE_GROUP"),
			"the resource group to sweep (default: AZURE_RESOURCE_GROUP)")
		cluster = fs.String("cluster", "", "sweep this one cluster (exact name), not every cluster of -cluster-prefix")
		prefix  = fs.String("cluster-prefix", sweep.DefaultClusterPrefix,
			"sweep the clusters whose name starts with this")
		maxAge = fs.Duration("max-age", 2*time.Hour, "only resources at least this old; 0: any age, also unknown")
		dryRun = fs.Bool("dry-run", false, "report what would be deleted, delete nothing")
	)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	var missing []string
	if strings.TrimSpace(*subscription) == "" {
		missing = append(missing, "-subscription (AZURE_SUBSCRIPTION_ID)")
	}
	if strings.TrimSpace(*group) == "" {
		missing = append(missing, "-resource-group (AZURE_RESOURCE_GROUP)")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing %s", strings.Join(missing, ", "))
	}
	if *maxAge < 0 {
		return nil, fmt.Errorf("-max-age must not be negative")
	}
	o := sweep.Options{
		ResourceGroup: strings.TrimSpace(*group), Cluster: *cluster, ClusterPrefix: *prefix, MaxAge: *maxAge, DryRun: *dryRun,
	}
	if *cluster != "" {
		o.ClusterPrefix = ""
	}
	return &settings{subscription: strings.TrimSpace(*subscription), opts: o}, nil
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	s, err := parse(args, getenv, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		_, _ = fmt.Fprintln(stderr, "sweeper:", err)
		return 2
	}
	clients, err := azure.NewClients(s.subscription)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sweeper:", err)
		return 2
	}
	development := strings.EqualFold(getenv(azure.EnvDevelopment), "true")
	cred, err := azure.NewCredential(getenv(azure.EnvCredentialsMode), development)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sweeper:", err)
		return 2
	}
	disks, err := sweep.NewSDKDisks(cred, s.subscription)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sweeper:", err)
		return 2
	}
	s.opts.Logf = func(format string, args ...any) { _, _ = fmt.Fprintf(stdout, format+"\n", args...) }
	rep, err := sweep.Run(ctx, clients, disks, s.opts)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sweeper:", err)
		return 2
	}
	_, _ = fmt.Fprintf(stdout, "sweeper: %d deleted, %d would be deleted, %d skipped, %d failed\n",
		rep.Count(sweep.Deleted), rep.Count(sweep.WouldDelete), rep.Count(sweep.Skipped), rep.Count(sweep.Failed))
	if rep.Count(sweep.Failed) > 0 {
		return 1
	}
	return 0
}
