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
	"github.com/SpareCores/scarpenter/pkg/vendors/facts"
	ovhdriver "github.com/SpareCores/scarpenter/pkg/vendors/ovh"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi/sdk"
)

// defaultMinInstances is the number of servers the suite needs at once (the database, the
// client, the amd64 probe, the described, brought-resources, drift and poweroff workloads,
// with some margin) when the lane's pin E2E_MIN_INSTANCES is not in the environment.
const defaultMinInstances = 8

// checkOVH is the OVH part: the real driver, over the real SDK, behind the read-only guard.
func checkOVH(ctx context.Context, cfg *config, rep *report) {
	a := newAudit()
	p, err := sdk.FromEnv(ctx, sdk.Options{UserAgent: "scarpenter-canary/" + version.Version})
	if err != nil {
		rep.check("OpenStack login", func() (string, error) { return "", err })
		return
	}
	runOVH(ctx, cfg, rep, readOnlyOVH(p, a), a)
}

// runOVH runs the checks over a provider (the guarded real one, or the fake cloud in the
// tests) and ends with the audit of what was called.
func runOVH(ctx context.Context, cfg *config, rep *report, provider osapi.Provider, a *audit) {
	drv := ovhdriver.New(ovhdriver.WithProvider(provider))
	if err := drv.Init(ctx, vendors.Config{Log: logr.Discard()}); err != nil {
		rep.check("OVH driver", func() (string, error) { return "", err })
		return
	}

	// The login and the catalog's regions: the controller's driver facts load the same.
	var zoneIDs []string
	rep.check("regions and zones "+cfg.region, func() (string, error) {
		f := facts.New(facts.Options{Registry: vendors.NewRegistryFrom(drv)})
		loadErr := f.Load(ctx)
		zones, _ := f.Zones(ovhdriver.Name)
		z := zones[cfg.region]
		if !slices.Contains(f.Regions(ovhdriver.Name), cfg.region) || len(z) == 0 {
			return "", fmt.Errorf("the region %s is not in the project's catalog or has no readable zones (%d regions loaded): %w",
				cfg.region, len(f.Regions(ovhdriver.Name)), loadErr)
		}
		names := make([]string, 0, len(z))
		for _, x := range z {
			zoneIDs = append(zoneIDs, x.ID)
			names = append(names, x.Name+"/"+x.ID)
		}
		return fmt.Sprintf("%d zones (%s); %d regions in the catalog", len(z), strings.Join(names, " "),
			len(f.Regions(ovhdriver.Name))), nil
	})

	// The Ext-Net network the nodes attach to.
	rep.check("Ext-Net network "+cfg.region, func() (string, error) {
		cl, err := provider.Clients(ctx, cfg.region)
		if err != nil {
			return "", err
		}
		nets, err := cl.Network.ListNetworks(ctx)
		if err != nil {
			return "", err
		}
		for _, n := range nets {
			if n.Name == osapi.ExternalNetwork && n.External {
				return n.ID, nil
			}
		}
		return "", fmt.Errorf("no external network named %s among %d networks", osapi.ExternalNetwork, len(nets))
	})

	// The Flatcar image the NodeClasses resolve (ImagesReady): exactly one match.
	rep.check(fmt.Sprintf("Flatcar %s %s amd64", cfg.flatcarChannel, cfg.flatcarVersion), func() (string, error) {
		img, err := drv.ResolveImage(ctx, vendors.ImageRequest{
			Region: cfg.region, Arch: "amd64", Channel: cfg.flatcarChannel, Version: cfg.flatcarVersion})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s (%s %s)", img.Ref, img.Channel, img.Version), nil
	})

	minInstances := cfg.minInstances
	if minInstances == 0 {
		minInstances = defaultMinInstances
	}
	// The quota the suite needs (the lane's preflight), read the way the controller reads it:
	// Nova's absolute limits of the project in the region.
	rep.check("quota cores and instances "+cfg.region, func() (string, error) {
		snap, err := drv.Quotas(ctx, cfg.region)
		if err != nil {
			return "", err
		}
		limit := map[string]vendors.Bucket{}
		for _, b := range snap.Buckets {
			limit[b.ID] = b
		}
		cores, okC := limit[ovhdriver.BucketCores]
		inst, okI := limit[ovhdriver.BucketInstances]
		if !okC || !okI {
			return "", fmt.Errorf("Nova reports no limit of cores (%t) or instances (%t): an unlimited limit has no bucket", okC, okI)
		}
		detail := fmt.Sprintf("cores %d of which %d used (need %d), instances %d of which %d used (need %d), %d buckets",
			cores.Limit, cores.Used, cfg.minOnDemand, inst.Limit, inst.Used, minInstances, len(snap.Buckets))
		var problems []string
		if cores.Limit-cores.Used < cfg.minOnDemand {
			problems = append(problems, fmt.Sprintf("%d cores are free, the suite needs %d", cores.Limit-cores.Used, cfg.minOnDemand))
		}
		if inst.Limit-inst.Used < minInstances {
			problems = append(problems, fmt.Sprintf("%d instances are free, the suite needs %d", inst.Limit-inst.Used, minInstances))
		}
		if len(problems) > 0 {
			return detail, errors.New(strings.Join(problems, "; "))
		}
		return detail, nil
	})

	// The flavors of the lane's NodeClasses and NodePools: offered in the region and known
	// to the quota mapping.
	rep.check("flavors offered in "+cfg.region, func() (string, error) {
		offered, err := drv.LaunchableTypes(ctx, cfg.region)
		if err != nil {
			return "", err
		}
		var found, problems []string
		for _, t := range cfg.types {
			ref := strings.TrimPrefix(t, ovhdriver.Name+".")
			zones := 0
			for _, z := range offered[ref] {
				if len(zoneIDs) == 0 || slices.Contains(zoneIDs, z) {
					zones++
				}
			}
			_, consumed := drv.Consumption(ref, vendors.CapacityOnDemand)
			switch {
			case zones == 0:
				problems = append(problems, ref+" is not offered in "+cfg.region)
			case !consumed:
				problems = append(problems, ref+" has no quota mapping")
			default:
				found = append(found, fmt.Sprintf("%s(%d)", ref, zones))
			}
		}
		detail := fmt.Sprintf("%d launched flavors in the region; the lane's: %s", len(offered), strings.Join(found, " "))
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
