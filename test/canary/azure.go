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
	"github.com/SpareCores/scarpenter/pkg/vendors/azure"
)

// canaryCluster names the driver instance of the canary; no resource carries it.
const canaryCluster = "scarpenter-canary"

// checkAzure is the Azure part: the real driver, over the real SDK, behind the read-only guard.
func checkAzure(ctx context.Context, cfg *config, rep *report) {
	a := newAudit()
	clients, err := azure.NewClients(cfg.subscription)
	if err != nil {
		rep.check("Azure configuration", func() (string, error) { return "", err })
		return
	}
	runAzure(ctx, cfg, rep, readOnlyAzure(clients, a), a)
}

// vendorAzure is the vendor's name in test/e2e-vendors.txt.
const vendorAzure = "azure"

// azureArchs are the architectures of the lane.
var azureArchs = []string{"amd64", "arm64"}

// azureRun is the state the checks of one run share: what an earlier check found (the zones, the
// SKU table) is what a later one reads.
type azureRun struct {
	cfg     *config
	rep     *report
	clients azure.Clients
	drv     *azure.Driver
	audit   *audit
	zones   []string
	skus    map[string]azure.SKU
}

// runAzure runs the checks over clients (the guarded real ones, or the fake's in the tests)
// and ends with the audit of what was called.
func runAzure(ctx context.Context, cfg *config, rep *report, clients azure.Clients, a *audit) {
	drv := azure.New(azure.WithClients(clients), azure.WithSubscription(cfg.subscription))
	if err := drv.Init(ctx, vendors.Config{Log: logr.Discard(), ClusterName: canaryCluster}); err != nil {
		rep.check("Azure driver", func() (string, error) { return "", err })
		return
	}
	r := &azureRun{cfg: cfg, rep: rep, clients: clients, drv: drv, audit: a, skus: map[string]azure.SKU{}}
	r.regions(ctx)
	r.images(ctx)
	r.terms(ctx)
	r.types(ctx)
	r.quota(ctx)
	r.guard()
}

// regions: the locations and the zones the controller's driver facts load.
func (r *azureRun) regions(ctx context.Context) {
	r.rep.check("regions and zones "+r.cfg.region, func() (string, error) {
		regions, err := r.drv.Regions(ctx)
		if err != nil {
			return "", err
		}
		if !slices.ContainsFunc(regions, func(x vendors.Region) bool { return strings.EqualFold(x.Name, r.cfg.region) }) {
			return "", fmt.Errorf("the subscription has no region %s (%d regions listed)", r.cfg.region, len(regions))
		}
		z, err := r.drv.Zones(ctx, r.cfg.region)
		if err != nil {
			return "", err
		}
		for _, x := range z {
			r.zones = append(r.zones, x.Name)
		}
		return fmt.Sprintf("%d regions; zones %s", len(regions), strings.Join(r.zones, " ")), nil
	})
}

func (r *azureRun) request(arch string) vendors.ImageRequest {
	return vendors.ImageRequest{
		Region: r.cfg.region, Arch: arch, Channel: r.cfg.flatcarChannel, Version: r.cfg.flatcarVersion,
	}
}

// images: the Flatcar images the NodeClasses resolve (ImagesReady), terms included.
func (r *azureRun) images(ctx context.Context) {
	for _, arch := range azureArchs {
		name := fmt.Sprintf("Flatcar %s %s %s", r.cfg.flatcarChannel, r.cfg.flatcarVersion, arch)
		r.rep.check(name, func() (string, error) {
			img, err := r.drv.ResolveImage(ctx, r.request(arch))
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("%s (%s)", img.Ref, img.Version), nil
		})
	}
}

// terms: the marketplace terms state of the plan of each image, read on its own: a resolution
// that failed on the terms says so in the row above, this row says which plan and what to run.
func (r *azureRun) terms(ctx context.Context) {
	images := &azure.Images{API: r.clients.Images, Terms: r.clients.Agreements}
	for _, arch := range azureArchs {
		r.rep.check("marketplace terms "+arch, func() (string, error) {
			res, err := images.Resolve(ctx, r.request(arch))
			if err != nil {
				return "", err
			}
			if res.Plan == nil {
				return res.Image.URN() + " has no purchase plan: no terms to accept", nil
			}
			p := res.Plan
			ok, err := r.clients.Agreements.Accepted(ctx, p.Publisher, p.Product, p.Name)
			if err != nil {
				return "", err
			}
			detail := fmt.Sprintf("plan %s:%s:%s accepted=%t", p.Publisher, p.Product, p.Name, ok)
			if !ok {
				return detail, fmt.Errorf(
					"the terms are not accepted: az vm image terms accept --publisher %s --offer %s --plan %s",
					p.Publisher, p.Product, p.Name)
			}
			return detail, nil
		})
	}
}

// types: the SKU read for the lane's types: launchable in a zone of the region, a quota mapping
// for the tiers the controller counts, and which of them can run as Spot.
func (r *azureRun) types(ctx context.Context) {
	r.rep.check("instance types offered in "+r.cfg.region, func() (string, error) {
		offered, err := r.drv.LaunchableTypes(ctx, r.cfg.region)
		if err != nil {
			return "", err
		}
		raw, err := r.clients.SKUs.List(ctx, r.cfg.region)
		if err != nil {
			return "", err
		}
		r.skus = azure.ParseSKUs(raw, r.cfg.region)
		var found, problems []string
		spot := 0
		for _, t := range r.cfg.types {
			ref := strings.TrimPrefix(t, azure.Vendor+".")
			n := 0
			for _, z := range offered[ref] {
				if len(r.zones) == 0 || z == azure.ZoneNone || slices.Contains(r.zones, z) {
					n++
				}
			}
			s, known := r.skus[ref]
			switch {
			case n == 0:
				problems = append(problems, ref+" is not launchable in any zone of "+r.cfg.region)
			case !known:
				problems = append(problems, ref+" is not in the SKU table")
			case !azureConsumes(r.drv, ref, vendors.CapacityOnDemand):
				problems = append(problems, ref+" has no quota mapping")
			case s.LowPriorityCapable && !azureConsumes(r.drv, ref, vendors.CapacitySpot):
				problems = append(problems, ref+" has no Spot quota mapping")
			default:
				tag := ""
				if s.LowPriorityCapable {
					tag = ",spot"
					spot++
				}
				found = append(found, fmt.Sprintf("%s(%d zones,%d vCPU,%s%s)", ref, n, s.VCPUs, s.Family, tag))
			}
		}
		detail := fmt.Sprintf("%d sizes in the region; the lane's: %s", len(offered), strings.Join(found, " "))
		if spot == 0 && len(problems) == 0 {
			problems = append(problems, "no type of the lane can run as Spot (LowPriorityCapable): the Spot scenarios have none")
		}
		if len(problems) > 0 {
			return detail, errors.New(strings.Join(problems, "; "))
		}
		return detail, nil
	})
}

// quota: what the suite needs (the lane's preflight), read the way the controller reads it: the
// regional vCPUs, the families of the lane's types, the Spot vCPUs, the public IPs.
func (r *azureRun) quota(ctx context.Context) {
	r.rep.check("quota vCPUs and public IPs "+r.cfg.region, func() (string, error) {
		snap, err := r.drv.Quotas(ctx, r.cfg.region)
		if err != nil {
			return "", err
		}
		limit := map[string]vendors.Bucket{}
		for _, b := range snap.Buckets {
			limit[b.ID] = b
		}
		var problems []string
		need := func(id string, want int64, what string) string {
			b, ok := limit[id]
			if !ok {
				problems = append(problems, "the usage API reports no "+what+" ("+id+")")
				return what + " missing"
			}
			if b.Limit < want {
				problems = append(problems, fmt.Sprintf("the %s limit %d is below the %d the suite needs", what, b.Limit, want))
			}
			return fmt.Sprintf("%s %d of which %d used (need %d)", what, b.Limit, b.Used, want)
		}
		parts := []string{
			need(azure.BucketRegionalCores, r.cfg.minOnDemand, "regional vCPUs"),
			need(azure.BucketSpotCores, r.cfg.minSpot, "Spot vCPUs"),
			// One public address per node of a scenario; the lane never has more nodes than types.
			need(azure.BucketPublicIPs, int64(len(r.cfg.types)), "public IPs"),
		}
		families := map[string]bool{}
		for _, t := range r.cfg.types {
			if s, ok := r.skus[strings.TrimPrefix(t, azure.Vendor+".")]; ok && s.Family != "" && !families[s.Family] {
				families[s.Family] = true
				parts = append(parts, need(s.Family, r.cfg.minOnDemand, "family "+s.Family))
			}
		}
		detail := strings.Join(parts, ", ")
		if len(problems) > 0 {
			return detail, errors.New(strings.Join(problems, "; "))
		}
		return detail, nil
	})
}

// guard: the guard's own account of the run: only reads went out, no write was even tried.
func (r *azureRun) guard() {
	r.rep.check("read-only guard", func() (string, error) {
		detail := fmt.Sprintf("%d kinds of read calls: %s", len(r.audit.actions()), strings.Join(r.audit.actions(), " "))
		if refused := r.audit.refusals(); len(refused) > 0 {
			return detail, fmt.Errorf("the checks tried write calls (refused): %s", strings.Join(refused, ", "))
		}
		return detail, nil
	})
}

func azureConsumes(drv *azure.Driver, ref, capacity string) bool {
	_, ok := drv.Consumption(ref, capacity)
	return ok
}
