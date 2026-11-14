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
	"time"

	"github.com/go-logr/logr"
	"github.com/oracle/oci-go-sdk/v65/core"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/oci"
	"github.com/SpareCores/scarpenter/test/e2e-oci/ocienv"
)

const (
	// vendorOCI is the vendor of this file in vendorChecks and the lane's variables.
	vendorOCI = "oci"

	// ociCheckTimeout bounds one check; every API call has its own deadline below it (the
	// SDK adapter's), and the whole canary one more (-timeout).
	ociCheckTimeout = 3 * time.Minute

	ociCoreSuffix   = "-core-count"
	ociMemorySuffix = "-memory-count"
)

// checkOCI is the OCI part: the real driver, over the real SDK adapter with a deadline on every
// call, behind the read-only guard.
func checkOCI(ctx context.Context, cfg *config, rep *report) {
	provider, err := oci.NewConfigurationProvider(ocienv.Credentials(osGetenv))
	if err != nil {
		rep.check("OCI credentials", func() (string, error) { return "", err })
		return
	}
	a := newAudit()
	sdk := oci.NewSDK(provider, "scarpenter-canary")
	runOCI(ctx, cfg, rep, ociReadOnlyClients(oci.Clients{Compute: sdk, Network: sdk, Identity: sdk, Limits: sdk}, a), a)
}

// ociCheck is one run of the checks over a driver: what the checks share.
type ociCheck struct {
	ctx context.Context
	cfg *config
	rep *report
	api oci.Clients
	drv *oci.Driver
	a   *audit
	// types are the lane's instance types (the driver's names, without the vendor prefix).
	types []string
	// buckets are the region's quota buckets by ID (set by the quota check).
	buckets map[string]vendors.Bucket
}

// runOCI runs the checks over the API (the guarded real one, or the fake's in the tests) and
// ends with the audit of what was called.
func runOCI(ctx context.Context, cfg *config, rep *report, api oci.Clients, a *audit) {
	drv := oci.New(oci.WithClients(api), oci.WithCompartment(cfg.compartment),
		oci.WithImageCompartment(cfg.imageCompartment), oci.WithTenancy(cfg.tenancy))
	if err := drv.Init(ctx, vendors.Config{Log: logr.Discard()}); err != nil {
		rep.check("OCI driver", func() (string, error) { return "", err })
		return
	}
	c := &ociCheck{ctx: ctx, cfg: cfg, rep: rep, api: api, drv: drv, a: a, types: ociLaneTypes(cfg)}
	c.facts()
	c.images()
	c.quotas()
	c.shapes()
	c.guard()
}

func (c *ociCheck) check(name string, fn func(ctx context.Context) (string, error)) {
	c.rep.check(name, func() (string, error) {
		ctx, cancel := context.WithTimeout(c.ctx, ociCheckTimeout)
		defer cancel()
		return fn(ctx)
	})
}

// facts: the tenancy's subscriptions and the availability domains of the lane's region (their
// names carry the tenancy's prefix and their numbering is the tenancy's own).
func (c *ociCheck) facts() {
	region := c.cfg.region
	c.check("regions and availability domains "+region, func(ctx context.Context) (string, error) {
		regions, err := c.drv.Regions(ctx)
		if err != nil {
			return "", err
		}
		names := make([]string, 0, len(regions))
		for _, r := range regions {
			names = append(names, r.Name)
		}
		if !slices.Contains(names, region) {
			return "", fmt.Errorf("the tenancy is not subscribed to %s (subscribed: %s)", region, strings.Join(names, " "))
		}
		zones, err := c.drv.Zones(ctx, region)
		if err != nil {
			return "", err
		}
		if len(zones) == 0 {
			return "", fmt.Errorf("the region %s has no availability domain", region)
		}
		var zn []string
		for _, z := range zones {
			zn = append(zn, z.Name+"/"+z.ID)
		}
		return fmt.Sprintf("%d availability domains (%s); subscribed: %s", len(zones), strings.Join(zn, " "),
			strings.Join(names, " ")), nil
	})
}

// images: the Flatcar image the NodeClasses resolve (ImagesReady): the naming contract of
// docs/how-to-guides/oci-images.md in the image compartment (OCI_IMAGE_COMPARTMENT, default the
// controller's compartment), per architecture (arm64 only
// when the lane runs it), and its shape compatibility entries: every lane type of the
// architecture must be one the image is registered for.
func (c *ociCheck) images() {
	cfg := c.cfg
	archs := []string{oci.ArchAMD64}
	if cfg.arm64 {
		archs = append(archs, oci.ArchARM64)
	}
	for _, arch := range archs {
		var ref string
		name := fmt.Sprintf("Flatcar %s %s %s", cfg.flatcarChannel, cfg.flatcarVersion, arch)
		c.check(name, func(ctx context.Context) (string, error) {
			img, err := c.drv.ResolveImage(ctx, vendors.ImageRequest{
				Region: cfg.region, Arch: arch, Channel: cfg.flatcarChannel, Version: cfg.flatcarVersion})
			if err != nil {
				return "", err
			}
			ref = img.Ref
			return fmt.Sprintf("%s, root volume %d GiB at least", img.Ref, img.RootDiskBytes>>30), nil
		})
		c.check(name+" shape compatibility", func(ctx context.Context) (string, error) {
			return c.compatible(ctx, ref, arch)
		})
	}
	if !cfg.arm64 {
		c.rep.check("Flatcar "+oci.ArchARM64, func() (string, error) {
			return "not configured (E2E_ARM64 is not true: the arm64 image needs a capability schema and Arm shapes added)", nil
		})
	}
}

// compatible reads the shapes the image is registered for and checks the lane's types of the
// architecture against them.
func (c *ociCheck) compatible(ctx context.Context, ref, arch string) (string, error) {
	if ref == "" {
		return "", errors.New("no image was resolved")
	}
	var shapes []string
	var page *string
	for {
		res, err := c.api.Compute.ListImageShapeCompatibilityEntries(ctx, c.cfg.region,
			core.ListImageShapeCompatibilityEntriesRequest{ImageId: &ref, Page: page})
		if err != nil {
			return "", err
		}
		for _, e := range res.Items {
			if e.Shape != nil {
				shapes = append(shapes, *e.Shape)
			}
		}
		if page = res.OpcNextPage; page == nil || *page == "" {
			break
		}
	}
	var found, problems []string
	for _, t := range c.types {
		typ, err := oci.ParseType(t)
		if err != nil || oci.Arm(typ.Shape) != (arch == oci.ArchARM64) {
			continue
		}
		if slices.Contains(shapes, typ.Shape) {
			found = append(found, typ.Shape)
		} else {
			problems = append(problems, "the image is not registered for the shape "+typ.Shape)
		}
	}
	found = slices.Compact(slices.Sorted(slices.Values(found)))
	detail := fmt.Sprintf("%d compatible shapes; the lane's: %s", len(shapes), strings.Join(found, " "))
	if len(problems) > 0 {
		return detail, errors.New(strings.Join(slices.Compact(slices.Sorted(slices.Values(problems))), "; "))
	}
	return detail, nil
}

// quotas: the limits of the compartment's tenancy in the lane's region, read the way the
// controller reads them (GetResourceAvailability per limit name of the region's shapes): the
// buckets of the lane's shape families must hold what the suite needs.
func (c *ociCheck) quotas() {
	cfg := c.cfg
	c.check("quota OCPUs and memory "+cfg.region, func(ctx context.Context) (string, error) {
		snap, err := c.drv.Quotas(ctx, cfg.region)
		if err != nil {
			return "", err
		}
		c.buckets = map[string]vendors.Bucket{}
		for _, b := range snap.Buckets {
			c.buckets[b.ID] = b
		}
		var found, problems []string
		lane := c.laneBuckets()
		if len(lane) == 0 {
			problems = append(problems,
				"no limit of the lane's shapes can be read: the shapes are not offered or the Limits service does not answer")
		}
		for _, id := range lane {
			b, ok := c.buckets[id]
			if !ok {
				problems = append(problems, fmt.Sprintf("the limit %s is not reported by the Limits service in %s", id, cfg.region))
				continue
			}
			need := c.need(id)
			found = append(found, fmt.Sprintf("%s %d of which %d used (need %d)", id, b.Limit, b.Used, need))
			if b.Limit < need {
				problems = append(problems, fmt.Sprintf("the limit %s is %d, below the %d the suite needs", id, b.Limit, need))
			}
		}
		detail := fmt.Sprintf("%s; %d buckets in the region", strings.Join(found, ", "), len(c.buckets))
		if len(problems) > 0 {
			return detail, errors.New(strings.Join(problems, "; "))
		}
		return detail, nil
	})
}

// laneBuckets are the limit names the lane's types consume (on demand), sorted and unique.
func (c *ociCheck) laneBuckets() []string {
	var ids []string
	for _, t := range c.types {
		cons, ok := c.drv.Consumption(t, vendors.CapacityOnDemand)
		if !ok {
			continue
		}
		for _, k := range cons {
			ids = append(ids, k.Bucket)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// need is what the suite needs of a bucket: the pins for the amd64 families, and for an Arm
// family twice the biggest lane type of it (a replacement runs next to the node it replaces).
func (c *ociCheck) need(id string) int64 {
	cores := strings.HasSuffix(id, ociCoreSuffix)
	var arm int64
	for _, t := range c.types {
		typ, err := oci.ParseType(t)
		if err != nil || !oci.Arm(typ.Shape) || !typ.Sized {
			continue
		}
		cons, ok := c.drv.Consumption(t, vendors.CapacityOnDemand)
		if !ok || !slices.ContainsFunc(cons, func(k vendors.Consumption) bool { return k.Bucket == id }) {
			continue
		}
		n := int64(typ.GB)
		if cores {
			n = int64(typ.OCPUs)
		}
		arm = max(arm, 2*n)
	}
	if arm > 0 {
		return arm
	}
	if cores {
		return c.cfg.minOCPU
	}
	return c.cfg.minMemoryGB
}

// shapes: the lane's types parse, are offered in an availability domain of the region (the
// driver maps a type to its quota buckets only from a shape table it read), and consume buckets
// the quota check found. A spot type has no quota mapping by design (unverified preemptible limits).
func (c *ociCheck) shapes() {
	region := c.cfg.region
	c.check("instance types offered in "+region, func(ctx context.Context) (string, error) {
		// Quotas read the shape tables of the ADs: the tables the mapping reads.
		if _, err := c.drv.Quotas(ctx, region); err != nil {
			return "", err
		}
		var found, problems []string
		for _, t := range c.types {
			if _, err := oci.ParseType(t); err != nil {
				problems = append(problems, t+" does not parse: "+err.Error())
				continue
			}
			cons, mapped := c.drv.Consumption(t, vendors.CapacityOnDemand)
			if !mapped {
				problems = append(problems, t+" is not offered in the region or has no readable limit")
				continue
			}
			found = append(found, t)
			problems = append(problems, c.fits(t, cons)...)
		}
		detail := fmt.Sprintf("the lane's: %s", strings.Join(found, " "))
		if len(problems) > 0 {
			return detail, errors.New(strings.Join(problems, "; "))
		}
		return detail, nil
	})
}

// fits says what is wrong with the buckets a type consumes: not reported, or a limit below one
// instance. Without the buckets (the quota check failed) it says nothing: that check reports it.
func (c *ociCheck) fits(t string, cons []vendors.Consumption) []string {
	if c.buckets == nil {
		return nil
	}
	var problems []string
	for _, k := range cons {
		b, ok := c.buckets[k.Bucket]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("%s consumes %s, which the Limits service does not report", t, k.Bucket))
		case b.Limit < k.Amount:
			problems = append(problems, fmt.Sprintf("%s needs %d of %s, the limit is %d", t, k.Amount, k.Bucket, b.Limit))
		}
	}
	return problems
}

// guard: the guard's own account of the run: only reads went out, no write was even tried.
func (c *ociCheck) guard() {
	c.rep.check("read-only guard", func() (string, error) {
		detail := fmt.Sprintf("%d statements used: %s", len(c.a.actions()), strings.Join(c.a.actions(), "; "))
		if r := c.a.refusals(); len(r) > 0 {
			return detail, fmt.Errorf("the checks tried write calls (refused): %s", strings.Join(r, ", "))
		}
		return detail, nil
	})
}

// ociLaneTypes are the instance types the lane launches, in the driver's names (without the
// vendor prefix): the pins of defaults.sh, without the Arm types when the lane does not run arm64.
func ociLaneTypes(cfg *config) []string {
	out := make([]string, 0, len(cfg.types))
	for _, t := range cfg.types {
		t = oci.APIReference(t)
		if typ, err := oci.ParseType(t); err == nil && oci.Arm(typ.Shape) && !cfg.arm64 {
			continue
		}
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}
