/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/go-logr/logr"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/alicloud"
)

// checkAlicloud is the Alibaba Cloud part: the real driver, over the real SDK, behind the
// read-only guard. Nothing here has run against a real account yet (the lane is validated
// statically): the first live run settles what the account answers.
func checkAlicloud(ctx context.Context, cfg *config, rep *report) {
	a := newAudit()
	base := alicloud.NewClientFactory(alicloud.ClientOptions{UserAgent: "scarpenter-canary"})
	runAlicloud(ctx, cfg, rep, readOnlyAlicloud(base, a), a)
}

// runAlicloud runs the checks over a factory of clients (the guarded real ones, or the fake's
// in the tests) and ends with the audit of what was called.
func runAlicloud(ctx context.Context, cfg *config, rep *report, factory alicloud.ClientFactory, a *audit) {
	drv := alicloud.New(alicloud.WithClientFactory(factory), alicloud.WithHomeRegion(cfg.region))
	if err := drv.Init(ctx, vendors.Config{Log: logr.Discard()}); err != nil {
		rep.check("Alibaba Cloud driver", func() (string, error) { return "", err })
		return
	}

	// The region and its zones (DescribeRegions, DescribeZones). The zone IDs are the zone
	// names on Alibaba Cloud (ap-southeast-1a), and the offerings are located by them.
	var zoneIDs []string
	rep.check("regions and zones "+cfg.region, func() (string, error) {
		regions, err := drv.Regions(ctx)
		if err != nil {
			return "", err
		}
		if !slices.ContainsFunc(regions, func(r vendors.Region) bool { return r.Name == cfg.region }) {
			return "", fmt.Errorf("the region %s is not among the %d regions of the account (not enabled, or denied)",
				cfg.region, len(regions))
		}
		zones, err := drv.Zones(ctx, cfg.region)
		if err != nil {
			return "", err
		}
		if len(zones) == 0 {
			return "", fmt.Errorf("the region %s has no zones", cfg.region)
		}
		names := make([]string, 0, len(zones))
		for _, z := range zones {
			zoneIDs = append(zoneIDs, z.ID)
			names = append(names, z.Name)
		}
		return fmt.Sprintf("%d zones (%s) of %d regions", len(zones), strings.Join(names, " "), len(regions)), nil
	})

	// The Flatcar images the NodeClasses resolve (ImagesReady): the account's own images (rung 3,
	// owner self), found by the name Flatcar-<channel>-<version>-<arch>, one per architecture.
	for _, arch := range []string{archAMD64, archARM64} {
		rep.check(fmt.Sprintf("Flatcar %s %s %s", cfg.flatcarChannel, cfg.flatcarVersion, arch), func() (string, error) {
			img, err := drv.ResolveImage(ctx, vendors.ImageRequest{
				Region: cfg.region, Arch: arch, Channel: cfg.flatcarChannel, Version: cfg.flatcarVersion})
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("%s, system disk %d GiB (owner %s)",
				img.Ref, img.RootDiskBytes>>30, alicloud.DefaultImageOwner), nil
		})
	}

	// The instance types of the lane's NodeClasses and NodePools: offered with stock in a zone
	// of the region (DescribeAvailableResource, which also reads their vCPUs), and counted by
	// the quota mapping of both capacity types (the controller launches only what it can count).
	rep.check("instance types offered in "+cfg.region, func() (string, error) {
		offered, err := drv.LaunchableTypes(ctx, cfg.region)
		if err != nil {
			return "", err
		}
		var found, problems []string
		for _, t := range cfg.types {
			ref := strings.TrimPrefix(t, alicloud.Vendor+".")
			zones := 0
			for _, z := range offered[ref] {
				if len(zoneIDs) == 0 || slices.Contains(zoneIDs, z) {
					zones++
				}
			}
			switch {
			case zones == 0:
				problems = append(problems, ref+" is not offered in any zone of "+cfg.region)
			case !alicloudConsumes(drv, ref, vendors.CapacityOnDemand) || !alicloudConsumes(drv, ref, vendors.CapacitySpot):
				problems = append(problems, ref+" has no quota mapping")
			default:
				found = append(found, fmt.Sprintf("%s(%d)", ref, zones))
			}
		}
		detail := fmt.Sprintf("%d types in the region; the lane's: %s", len(offered), strings.Join(found, " "))
		if len(problems) > 0 {
			return detail, errors.New(strings.Join(problems, "; "))
		}
		return detail, nil
	})

	// The vCPU quota the suite needs (the lane's preflight), read the way the controller reads
	// it: DescribeAccountAttributes, the limit and the usage of pay-as-you-go and spot.
	rep.check("quota vCPUs "+cfg.region, func() (string, error) {
		snap, err := drv.Quotas(ctx, cfg.region)
		if err != nil {
			return "", err
		}
		limit := map[string]vendors.Bucket{}
		for _, b := range snap.Buckets {
			limit[b.ID] = b
		}
		od, okOD := limit[alicloud.BucketPostpaid]
		spot, okSpot := limit[alicloud.BucketSpot]
		if !okOD || !okSpot {
			return "", fmt.Errorf("DescribeAccountAttributes reports no vCPU limit (pay-as-you-go %t, spot %t)", okOD, okSpot)
		}
		detail := fmt.Sprintf("pay-as-you-go %d of which %d used (need %d), spot %d of which %d used (need %d)",
			od.Limit, od.Used, cfg.minOnDemand, spot.Limit, spot.Used, cfg.minSpot)
		var problems []string
		if od.Limit < cfg.minOnDemand {
			problems = append(problems,
				fmt.Sprintf("the pay-as-you-go limit %d is below the %d the suite needs", od.Limit, cfg.minOnDemand))
		}
		if spot.Limit < cfg.minSpot {
			problems = append(problems,
				fmt.Sprintf("the spot limit %d is below the %d the suite needs", spot.Limit, cfg.minSpot))
		}
		if len(problems) > 0 {
			return detail, errors.New(strings.Join(problems, "; "))
		}
		return detail, nil
	})

	// The guard's own account of the run: only reads went out, no write was even tried.
	rep.check("read-only guard", func() (string, error) {
		detail := fmt.Sprintf("%d kinds of read calls: %s", len(a.actions()), strings.Join(a.actions(), " "))
		if r := a.refusals(); len(r) > 0 {
			return detail, fmt.Errorf("the checks tried write calls (refused): %s", strings.Join(r, ", "))
		}
		return detail, nil
	})
}

func alicloudConsumes(drv *alicloud.Driver, ref, capacity string) bool {
	_, ok := drv.Consumption(ref, capacity)
	return ok
}
