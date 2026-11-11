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

	"github.com/SpareCores/scarpenter/internal/version"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud"
	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/api"
)

// vendorUpCloud is the vendor ID of the UpCloud lane.
const vendorUpCloud = "upcloud"

// checkUpCloud is the UpCloud part: the real driver, over the real API with the token in
// UPCLOUD_TOKEN, behind the read-only guard.
func checkUpCloud(ctx context.Context, cfg *config, rep *report) {
	c, err := api.NewFromEnv("scarpenter-canary/" + version.Version)
	if err != nil {
		rep.check("UpCloud API token", func() (string, error) { return "", err })
		return
	}
	a := newAudit()
	runUpCloud(ctx, cfg, rep, readOnlyUpCloud(c, a), a)
}

// runUpCloud runs the checks over an API (the guarded real one, or the fake's in the tests)
// and ends with the audit of what was called.
func runUpCloud(ctx context.Context, cfg *config, rep *report, a api.API, au *audit) {
	// The token: UpCloud's expire (up to a year, no scopes) and nothing else tells us.
	rep.check("API token", func() (string, error) {
		acct, err := a.GetAccount(ctx)
		if err != nil {
			return "", fmt.Errorf("the token does not work (expired? rotate UPCLOUD_TOKEN, "+
				"docs/how-to-guides/testing-and-releasing.md): %w", upcloud.Classify(err))
		}
		return "account " + acct.UserName, nil
	})

	drv := upcloud.New(upcloud.WithAPI(a))
	if err := drv.Init(ctx, vendors.Config{Log: logr.Discard()}); err != nil {
		rep.check("UpCloud driver", func() (string, error) { return "", err })
		return
	}

	// UpCloud's regions are its zones: the zone is usable and has the dummy zone of the
	// same ID the offerings are located by.
	rep.check("regions and zones "+cfg.region, func() (string, error) {
		regions, err := drv.Regions(ctx)
		if err != nil {
			return "", err
		}
		if !slices.ContainsFunc(regions, func(r vendors.Region) bool { return r.Name == cfg.region }) {
			return "", fmt.Errorf("the zone %s is not among the %d zones the account can use", cfg.region, len(regions))
		}
		zones, err := drv.Zones(ctx, cfg.region)
		if err != nil {
			return "", err
		}
		if len(zones) != 1 || zones[0].ID != cfg.region {
			return "", fmt.Errorf("the zone %s has the zones %v, want its one dummy zone", cfg.region, zones)
		}
		return fmt.Sprintf("%d zones in the account", len(regions)), nil
	})

	// The Flatcar template the NodeClasses resolve (ImagesReady): private, online, labelled
	// with the pinned release. UpCloud has one architecture.
	rep.check(fmt.Sprintf("Flatcar %s %s amd64 template", cfg.flatcarChannel, cfg.flatcarVersion), func() (string, error) {
		img, err := drv.ResolveImage(ctx, vendors.ImageRequest{
			Region: cfg.region, Arch: "amd64", Channel: cfg.flatcarChannel, Version: cfg.flatcarVersion})
		if err != nil {
			return "", fmt.Errorf("%w (import the template of the images repository, "+
				"test/e2e-upcloud/defaults.sh lists its labels)", err)
		}
		return fmt.Sprintf("%s, root disk %d GiB", img.Ref, img.RootDiskBytes>>30), nil
	})

	// The account limits the suite needs (the lane's preflight), read the way the controller
	// reads them: account-wide buckets.
	rep.check("account limits cores and memory", func() (string, error) {
		snap, err := drv.GlobalQuotas(ctx)
		if err != nil {
			return "", err
		}
		limit := map[string]vendors.Bucket{}
		for _, b := range snap.Buckets {
			limit[b.ID] = b
		}
		cores, okC := limit[upcloud.BucketCores]
		mem, okM := limit[upcloud.BucketMemory]
		if !okC || !okM {
			return "", fmt.Errorf("the account reports no positive limit for cores (%t) or memory (%t): "+
				"a limit of 0 is unknown, "+
				"and the lane's sub-account needs both set (the cost guard)", okC, okM)
		}
		detail := fmt.Sprintf("cores %d of which %d used (need %d), memory %d MB of which %d used (need %d), %d buckets",
			cores.Limit, cores.Used, cfg.minCores, mem.Limit, mem.Used, cfg.minMemoryMB, len(snap.Buckets))
		var problems []string
		if cores.Limit < cfg.minCores {
			problems = append(problems, fmt.Sprintf("the cores limit %d is below the %d the suite needs",
				cores.Limit, cfg.minCores))
		}
		if mem.Limit < cfg.minMemoryMB {
			problems = append(problems, fmt.Sprintf("the memory limit %d MB is below the %d MB the suite needs",
				mem.Limit, cfg.minMemoryMB))
		}
		if len(problems) > 0 {
			return detail, errors.New(strings.Join(problems, "; "))
		}
		return detail, nil
	})

	// The plans of the lane's NodeClasses and NodePools: offered in the zone, and known to
	// the quota mapping (the controller launches only what it can count).
	rep.check("plans offered in "+cfg.region, func() (string, error) {
		offered, err := drv.LaunchableTypes(ctx, cfg.region)
		if err != nil {
			return "", err
		}
		var found, problems []string
		for _, t := range cfg.types {
			ref := strings.TrimPrefix(t, upcloud.Vendor+".")
			switch {
			case !slices.Contains(offered[ref], cfg.region):
				problems = append(problems, ref+" is not offered in "+cfg.region)
			default:
				if _, ok := drv.Consumption(ref, vendors.CapacityOnDemand); !ok {
					problems = append(problems, ref+" has no quota mapping")
					continue
				}
				found = append(found, ref)
			}
		}
		detail := fmt.Sprintf("%d plans in the zone; the lane's: %s", len(offered), strings.Join(found, " "))
		if len(problems) > 0 {
			return detail, errors.New(strings.Join(problems, "; "))
		}
		return detail, nil
	})

	// The guard's own account of the run: only reads went out, no write was even tried.
	rep.check("read-only guard", func() (string, error) {
		detail := fmt.Sprintf("%d kinds of read calls: %s", len(au.actions()), strings.Join(au.actions(), " "))
		if r := au.refusals(); len(r) > 0 {
			return detail, fmt.Errorf("the checks tried write calls (refused): %s", strings.Join(r, ", "))
		}
		return detail, nil
	})
}
