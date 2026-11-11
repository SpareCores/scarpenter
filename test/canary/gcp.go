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
	"regexp"
	"slices"
	"strings"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/go-logr/logr"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/facts"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp"
)

// The quota metrics the lane's preflight reads (docs/how-to-guides/testing.md#the-l5-lane): CPUS for
// the on-demand nodes, and PREEMPTIBLE_CPUS for the Spot ones once the region has any.
const (
	// vendorGCP is the vendor of this file in vendorChecks and the lane's variables.
	vendorGCP = "gcp"

	gcpOnDemandMetric = "CPUS"
	gcpSpotMetric     = "PREEMPTIBLE_CPUS"
	gcpAllRegions     = "CPUS_ALL_REGIONS"
	// gcpCheckTimeout bounds one check; every API call has its own deadline below it
	// (gcp.WithTimeouts in checkGCP), and the whole canary one more (-timeout).
	gcpCheckTimeout = 3 * time.Minute
)

// gcpGVNICSeries are the machine series that cannot boot from an image without the GVNIC guest
// OS feature (plan 16 section 1: C3, C4, C4A, N4, A3 and later). The lane's db type is a c3.
var gcpGVNICSeries = []string{
	"c3", "c3d", "c4", "c4a", "c4d", "n4", "n4a", "n4d", "a3", "a4", "a4x", "h3", "h4d", "m4", "x4", "z3",
}

// gcpBelow is the problem of a limit under what the suite needs.
func gcpBelow(metric string, limit, need int64) string {
	return fmt.Sprintf("the %s limit %d is below the %d the suite needs", metric, limit, need)
}

var gcpImageRefRe = regexp.MustCompile(`projects/([^/]+)/global/images/([^/]+)$`)

// checkGCP is the GCP part: the real driver, over the real SDK with a deadline on every call,
// behind the read-only guard.
func checkGCP(ctx context.Context, cfg *config, rep *report) {
	a := newAudit()
	sdk, err := gcp.NewSDK(ctx, "scarpenter-canary")
	if err != nil {
		rep.check("GCP configuration", func() (string, error) { return "", err })
		return
	}
	defer func() { _ = sdk.Close() }()
	runGCP(ctx, cfg, rep, gcpReadOnlyCompute(gcp.WithTimeouts(sdk, 0, 0), a), a)
}

// gcpCheck is one run of the checks over a driver: what the checks share.
type gcpCheck struct {
	ctx context.Context
	cfg *config
	rep *report
	api gcp.ComputeAPI
	drv *gcp.Driver
	a   *audit
	// types are the lane's machine types; zoneIDs the numeric IDs of the region's zones (set by
	// the facts check); buckets the region's quota buckets by ID (set by the quota check).
	types   []string
	zoneIDs []string
	buckets map[string]vendors.Bucket
}

// runGCP runs the checks over the API (the guarded real one, or the fake's in the tests) and
// ends with the audit of what was called.
func runGCP(ctx context.Context, cfg *config, rep *report, api gcp.ComputeAPI, a *audit) {
	drv := gcp.New(gcp.WithAPI(api), gcp.WithProject(cfg.project))
	if err := drv.Init(ctx, vendors.Config{Log: logr.Discard()}); err != nil {
		rep.check("GCP driver", func() (string, error) { return "", err })
		return
	}
	c := &gcpCheck{ctx: ctx, cfg: cfg, rep: rep, api: api, drv: drv, a: a, types: gcpLaneTypes(cfg)}
	c.facts()
	c.images()
	c.quotas()
	c.machineTypes()
	c.guard()
}

// check is rep.check under its own deadline.
func (c *gcpCheck) check(name string, fn func(ctx context.Context) (string, error)) {
	c.rep.check(name, func() (string, error) {
		ctx, cancel := context.WithTimeout(c.ctx, gcpCheckTimeout)
		defer cancel()
		return fn(ctx)
	})
}

// facts: the regions and zones the controller's driver facts load; the numeric zone IDs are what
// the offerings are located by (Keeper's zone_id of GCP is Google's own).
func (c *gcpCheck) facts() {
	region := c.cfg.region
	c.check("regions and zones "+region, func(ctx context.Context) (string, error) {
		f := facts.New(facts.Options{Registry: vendors.NewRegistryFrom(c.drv)})
		loadErr := f.Load(ctx)
		zones, _ := f.Zones(gcp.Vendor)
		z := zones[region]
		if !slices.Contains(f.Regions(gcp.Vendor), region) || len(z) == 0 {
			return "", fmt.Errorf("the region %s has no readable zones (%d regions loaded; unknown, down, or denied): %w",
				region, len(f.Regions(gcp.Vendor)), loadErr)
		}
		names := make([]string, 0, len(z))
		for _, x := range z {
			c.zoneIDs = append(c.zoneIDs, x.ID)
			names = append(names, x.Name+"/"+x.ID)
		}
		return fmt.Sprintf("%d zones (%s); %d regions in the project",
			len(z), strings.Join(names, " "), len(f.Regions(gcp.Vendor))), nil
	})
}

// images: the Flatcar images the NodeClasses resolve (ImagesReady), by name (they cannot be
// listed). arm64 only when the lane runs it: GCE has no official arm64 Flatcar image.
func (c *gcpCheck) images() {
	cfg := c.cfg
	archs := []string{archAMD64}
	if cfg.arm64 {
		archs = append(archs, archARM64)
	}
	for _, arch := range archs {
		var ref string
		name := fmt.Sprintf("Flatcar %s %s %s", cfg.flatcarChannel, cfg.flatcarVersion, arch)
		c.check(name, func(ctx context.Context) (string, error) {
			req := vendors.ImageRequest{Region: cfg.region, Arch: arch, Channel: cfg.flatcarChannel, Version: cfg.flatcarVersion}
			if arch == archARM64 {
				req.Owner = cfg.arm64ImageProject
			}
			img, err := c.drv.ResolveImage(ctx, req)
			if err != nil {
				return "", err
			}
			ref = img.Ref
			return fmt.Sprintf("%s, root disk %d GiB", img.Ref, img.RootDiskBytes>>30), nil
		})
		// What the image must have for the lane's machine types (the biggest image risk of
		// plan 16 section 1): ready, the architecture, and the guest OS features.
		c.check(name+" guest OS features", func(ctx context.Context) (string, error) {
			return gcpImageFeatures(ctx, c.api, ref, arch, c.types)
		})
	}
	if !cfg.arm64 {
		c.rep.check("Flatcar "+archARM64, func() (string, error) {
			return "not configured (E2E_ARM64 is not true: there is no official Flatcar arm64 image on GCE)", nil
		})
	}
}

// quotas: the quota the suite needs (the lane's preflight), read the way the controller reads
// it: regions.get gives the usage with the limit.
func (c *gcpCheck) quotas() {
	cfg := c.cfg
	c.check("quota vCPUs "+cfg.region, func(ctx context.Context) (string, error) {
		snap, err := c.drv.Quotas(ctx, cfg.region)
		if err != nil {
			return "", err
		}
		c.buckets = map[string]vendors.Bucket{}
		for _, b := range snap.Buckets {
			c.buckets[b.ID] = b
		}
		od, ok := c.buckets[gcpOnDemandMetric]
		if !ok {
			return "", fmt.Errorf("regions.get of %s reports no %s quota (%d buckets)",
				cfg.region, gcpOnDemandMetric, len(c.buckets))
		}
		// Spot VMs run on the standard quota until the region has preemptible quota.
		pre, hasPre := c.buckets[gcpSpotMetric]
		needOD := cfg.minOnDemand
		spotNote := gcpSpotMetric + " absent"
		if hasPre {
			spotNote = fmt.Sprintf("%s %d of which %d used", gcpSpotMetric, pre.Limit, pre.Used)
		}
		var problems []string
		if !hasPre || pre.Limit == 0 {
			needOD += cfg.minSpot
		} else if pre.Limit < cfg.minSpot {
			problems = append(problems, gcpBelow(gcpSpotMetric, pre.Limit, cfg.minSpot))
		}
		var detail strings.Builder
		fmt.Fprintf(&detail, "%s %d of which %d used (need %d), %s (need %d), %d quota buckets",
			gcpOnDemandMetric, od.Limit, od.Used, needOD, spotNote, cfg.minSpot, len(c.buckets))
		if od.Limit < needOD {
			problems = append(problems, gcpBelow(gcpOnDemandMetric, od.Limit, needOD))
		}
		// The project-wide limit, when the project has one.
		g, gerr := c.drv.GlobalQuotas(ctx)
		if gerr != nil {
			problems = append(problems, fmt.Sprintf("the project's quotas (projects.get): %v", gerr))
		}
		if g != nil {
			for _, b := range g.Buckets {
				if b.ID != gcpAllRegions {
					continue
				}
				fmt.Fprintf(&detail, "; %s %d of which %d used", gcpAllRegions, b.Limit, b.Used)
				if b.Limit < needOD {
					problems = append(problems, gcpBelow(gcpAllRegions, b.Limit, needOD))
				}
			}
		}
		if len(problems) > 0 {
			return detail.String(), errors.New(strings.Join(problems, "; "))
		}
		return detail.String(), nil
	})
}

// machineTypes: the machine types of the lane's NodeClasses and NodePools, offered in a zone of
// the region, and, where the driver maps them, with the quota buckets that exist and fit one
// instance (an unmapped series takes the reactive path: not an error, but said).
func (c *gcpCheck) machineTypes() {
	region := c.cfg.region
	c.check("machine types offered in "+region, func(ctx context.Context) (string, error) {
		offered, err := c.drv.LaunchableTypes(ctx, region)
		if err != nil {
			return "", err
		}
		var found, unmapped, problems []string
		for _, t := range c.types {
			ref := gcp.APIReference(t)
			zones := 0
			for _, z := range offered[ref] {
				if len(c.zoneIDs) == 0 || slices.Contains(c.zoneIDs, z) {
					zones++
				}
			}
			if zones == 0 {
				problems = append(problems, ref+" is not offered in any zone of "+region)
				continue
			}
			found = append(found, fmt.Sprintf("%s(%d)", ref, zones))
			cons, mapped := c.drv.Consumption(ref, vendors.CapacityOnDemand)
			if !mapped {
				unmapped = append(unmapped, ref)
				continue
			}
			problems = append(problems, c.fits(ref, cons)...)
		}
		detail := fmt.Sprintf("%d types in the region; the lane's: %s", len(offered), strings.Join(found, " "))
		if len(unmapped) > 0 {
			detail += "; quota mapping unknown (reactive): " + strings.Join(unmapped, " ")
		}
		if len(problems) > 0 {
			return detail, errors.New(strings.Join(problems, "; "))
		}
		return detail, nil
	})
}

// fits says what is wrong with the quota buckets a type consumes: not reported by regions.get, or
// a limit below one instance. Without the buckets (the quota check failed) it says nothing: that
// check reports it.
func (c *gcpCheck) fits(ref string, cons []vendors.Consumption) []string {
	if c.buckets == nil {
		return nil
	}
	var problems []string
	for _, k := range cons {
		b, ok := c.buckets[k.Bucket]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("%s consumes %s, which regions.get does not report", ref, k.Bucket))
		case b.Limit < k.Amount:
			problems = append(problems, fmt.Sprintf("%s needs %d of %s, the limit is %d", ref, k.Amount, k.Bucket, b.Limit))
		}
	}
	return problems
}

// guard: the guard's own account of the run: only reads went out, no write was even tried.
func (c *gcpCheck) guard() {
	c.rep.check("read-only guard", func() (string, error) {
		detail := fmt.Sprintf("%d permissions used: %s", len(c.a.actions()), strings.Join(c.a.actions(), " "))
		if r := c.a.refusals(); len(r) > 0 {
			return detail, fmt.Errorf("the checks tried write calls (refused): %s", strings.Join(r, ", "))
		}
		return detail, nil
	})
}

// gcpLaneTypes are the machine types the lane launches, with the vendor prefix: the pins of
// defaults.sh, without the arm64 type when the lane does not run arm64.
func gcpLaneTypes(cfg *config) []string {
	out := make([]string, 0, len(cfg.types))
	for _, t := range cfg.types {
		if (!cfg.arm64 && t == cfg.armType) || slices.Contains(out, t) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// gcpImageFeatures reads the image the driver resolved (images.get, by the reference it returned)
// and checks what the lane's types need of it.
func gcpImageFeatures(ctx context.Context, api gcp.ComputeAPI, ref, arch string, types []string) (string, error) {
	m := gcpImageRefRe.FindStringSubmatch(ref)
	if m == nil {
		return "", fmt.Errorf("no image was resolved (reference %q)", ref)
	}
	img, err := api.GetImage(ctx, &computepb.GetImageRequest{Project: m[1], Image: m[2]})
	if err != nil {
		return "", err
	}
	var features []string
	for _, f := range img.GetGuestOsFeatures() {
		features = append(features, f.GetType())
	}
	slices.Sort(features)
	detail := fmt.Sprintf("%s, %s, %s, %d GiB, features %s",
		m[2], img.GetStatus(), gcpArchOf(img), img.GetDiskSizeGb(), strings.Join(features, ","))
	var problems []string
	if img.GetStatus() != "READY" {
		problems = append(problems, "the image is "+img.GetStatus()+", not READY")
	}
	if got := gcpArchOf(img); got != arch {
		problems = append(problems, fmt.Sprintf("the image is %s, the lane needs %s", got, arch))
	}
	if arch == archAMD64 && !slices.Contains(features, "GVNIC") {
		var need []string
		for _, t := range types {
			ref := gcp.APIReference(t)
			if slices.Contains(gcpGVNICSeries, gcp.Family(ref)) && !slices.Contains(need, ref) {
				need = append(need, ref)
			}
		}
		if len(need) > 0 {
			problems = append(problems,
				"the image has no GVNIC guest OS feature, which the lane's types need: "+strings.Join(need, " "))
		}
	}
	if len(problems) > 0 {
		return detail, errors.New(strings.Join(problems, "; "))
	}
	return detail, nil
}

// gcpArchOf is the architecture of an image as the lane names it (an image without one is x86_64).
func gcpArchOf(img *computepb.Image) string {
	if img.GetArchitecture() == "ARM64" {
		return archARM64
	}
	return archAMD64
}
