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
	awsdriver "github.com/SpareCores/scarpenter/pkg/vendors/aws"
	"github.com/SpareCores/scarpenter/pkg/vendors/facts"
)

// The Standard class quota codes (docs/reference/aws-driver.md; the preflight of the lane
// reads the same two).
const (
	standardOnDemand = "L-1216C47A"
	standardSpot     = "L-34B43A08"
)

// checkAWS is the AWS part: the real driver, over the real SDK, behind the read-only guard.
func checkAWS(ctx context.Context, cfg *config, rep *report) {
	a := newAudit()
	base, err := awsdriver.NewClientFactory(ctx, cfg.region)
	if err != nil {
		rep.check("AWS configuration", func() (string, error) { return "", err })
		return
	}
	runAWS(ctx, cfg, rep, readOnly(base, a), a)
}

// runAWS runs the checks over a factory of clients (the guarded real ones, or the fake's in
// the tests) and ends with the audit of what was called.
func runAWS(ctx context.Context, cfg *config, rep *report, factory awsdriver.ClientFactory, a *audit) {
	drv := awsdriver.New(awsdriver.WithClientFactory(factory), awsdriver.WithHomeRegion(cfg.region))
	if err := drv.Init(ctx, vendors.Config{Log: logr.Discard()}); err != nil {
		rep.check("AWS driver", func() (string, error) { return "", err })
		return
	}

	// The regions and zones the controller's driver facts load; the zone IDs are what the
	// offerings are located by.
	var zoneIDs []string
	rep.check("regions and zones "+cfg.region, func() (string, error) {
		f := facts.New(facts.Options{Registry: vendors.NewRegistryFrom(drv)})
		loadErr := f.Load(ctx) // a region the role cannot read is skipped, not an error
		zones, _ := f.Zones(awsdriver.Name)
		z := zones[cfg.region]
		if !slices.Contains(f.Regions(awsdriver.Name), cfg.region) || len(z) == 0 {
			return "", fmt.Errorf("the region %s has no readable zones (%d regions loaded; not enabled, or denied): %w",
				cfg.region, len(f.Regions(awsdriver.Name)), loadErr)
		}
		names := make([]string, 0, len(z))
		for _, x := range z {
			zoneIDs = append(zoneIDs, x.ID)
			names = append(names, x.Name+"/"+x.ID)
		}
		skipped := len(f.SkippedRegions(awsdriver.Name))
		return fmt.Sprintf("%d zones (%s); %d other regions skipped", len(z), strings.Join(names, " "), skipped), nil
	})

	// The Flatcar images the NodeClasses resolve (ImagesReady).
	for _, arch := range []string{archAMD64, archARM64} {
		rep.check(fmt.Sprintf("Flatcar %s %s %s", cfg.flatcarChannel, cfg.flatcarVersion, arch), func() (string, error) {
			img, err := drv.ResolveImage(ctx, vendors.ImageRequest{
				Region: cfg.region, Arch: arch, Channel: cfg.flatcarChannel, Version: cfg.flatcarVersion})
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("%s, root disk %d GiB", img.Ref, img.RootDiskBytes>>30), nil
		})
	}

	// The quota the suite needs (the lane's preflight), read the way the controller reads it.
	rep.check("quota Standard vCPUs "+cfg.region, func() (string, error) {
		snap, err := drv.Quotas(ctx, cfg.region)
		if err != nil {
			return "", err
		}
		limit := map[string]vendors.Bucket{}
		for _, b := range snap.Buckets {
			limit[b.ID] = b
		}
		od, okOD := limit[standardOnDemand]
		spot, okSpot := limit[standardSpot]
		if !okOD || !okSpot {
			return "", fmt.Errorf("service quotas reports no Standard vCPU quota (on-demand %t, spot %t)", okOD, okSpot)
		}
		detail := fmt.Sprintf("on-demand %d of which %d used (need %d), spot %d of which %d used (need %d), %d quota buckets",
			od.Limit, od.Used, cfg.minOnDemand, spot.Limit, spot.Used, cfg.minSpot, len(snap.Buckets))
		var problems []string
		if od.Limit < cfg.minOnDemand {
			problems = append(problems,
				fmt.Sprintf("the on-demand limit %d is below the %d the suite needs", od.Limit, cfg.minOnDemand))
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

	// The instance types of the lane's NodeClasses and NodePools: offered in a zone of the
	// region, and known to the quota mapping (the controller launches only what it can count).
	rep.check("instance types offered in "+cfg.region, func() (string, error) {
		offered, err := drv.LaunchableTypes(ctx, cfg.region)
		if err != nil {
			return "", err
		}
		var found, problems []string
		for _, t := range cfg.types {
			ref := strings.TrimPrefix(t, awsdriver.Name+".")
			zones := 0
			for _, z := range offered[ref] {
				if len(zoneIDs) == 0 || slices.Contains(zoneIDs, z) {
					zones++
				}
			}
			switch {
			case zones == 0:
				problems = append(problems, ref+" is not offered in any zone of "+cfg.region)
			case !consumes(drv, ref):
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

	// The guard's own account of the run: only reads went out, no write was even tried.
	rep.check("read-only guard", func() (string, error) {
		detail := fmt.Sprintf("%d kinds of read calls: %s", len(a.actions()), strings.Join(a.actions(), " "))
		if r := a.refusals(); len(r) > 0 {
			return detail, fmt.Errorf("the checks tried write calls (refused): %s", strings.Join(r, ", "))
		}
		return detail, nil
	})
}

func consumes(drv *awsdriver.Driver, ref string) bool {
	_, ok := drv.Consumption(ref, vendors.CapacityOnDemand)
	return ok
}
