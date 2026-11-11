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
	"os"
	"slices"
	"strings"

	"github.com/go-logr/logr"
	"github.com/vultr/govultr/v3"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/vultr"
)

// vendorVultr is the vendor ID of the lane (a line of test/e2e-vendors.txt once the account is ready).
const vendorVultr = "vultr"

// checkVultr is the Vultr part: the real driver, over the real API client, behind the
// read-only guard. The key comes from VULTR_API_KEY (a second sub-user, read access
// only; its IP allowlist must admit the runner).
func checkVultr(ctx context.Context, cfg *config, rep *report) {
	a := newAudit()
	client, err := vultr.NewClient(vultr.ClientConfig{APIKey: os.Getenv(vultr.APIKeyEnv)})
	if err != nil {
		rep.check("Vultr client", func() (string, error) { return "", err })
		return
	}
	runVultr(ctx, cfg, rep, readOnlyVultr(client, a), a)
}

// runVultr runs the checks over an API (the guarded real one, or the fake in the tests) and
// ends with the audit of what was called.
func runVultr(ctx context.Context, cfg *config, rep *report, api vultr.API, a *audit) {
	drv := vultr.New(vultr.WithAPI(api))
	if err := drv.Init(ctx, vendors.Config{ClusterName: "canary", Log: logr.Discard()}); err != nil {
		rep.check("Vultr driver", func() (string, error) { return "", err })
		return
	}

	// The key and its ACL: the call the controller's VendorReady makes.
	rep.check("API key", func() (string, error) {
		if _, _, err := api.ListInstances(ctx, &govultr.ListOptions{PerPage: 1}); err != nil {
			return "", fmt.Errorf("the key is refused (the key, its ACL, or the runner's address is not in its "+
				"IP allowlist): %w", err)
		}
		return "instances readable", nil
	})

	rep.check("region "+cfg.region, func() (string, error) {
		regions, err := drv.Regions(ctx)
		if err != nil {
			return "", err
		}
		if !slices.ContainsFunc(regions, func(r vendors.Region) bool { return r.Name == cfg.region }) {
			return "", fmt.Errorf("the region %s is not in the %d regions Vultr lists", cfg.region, len(regions))
		}
		return fmt.Sprintf("%d regions", len(regions)), nil
	})

	// The Flatcar snapshot the NodeClasses resolve (ImagesReady): exactly one, complete.
	rep.check(fmt.Sprintf("Flatcar snapshot %s %s amd64", cfg.flatcarChannel, cfg.flatcarVersion), func() (string, error) {
		img, err := drv.ResolveImage(ctx, vendors.ImageRequest{
			Region: cfg.region, Arch: archAMD64, Channel: cfg.flatcarChannel, Version: cfg.flatcarVersion})
		if err != nil {
			return "", fmt.Errorf("%w (create it from the images repository's URL, docs/how-to-guides/vultr-credentials.md)",
				err)
		}
		return img.Ref, nil
	})

	// The plans of the lane's NodeClasses and NodePools: in stock in the region and launched by
	// the driver.
	rep.check("plans in stock in "+cfg.region, func() (string, error) {
		offered, err := drv.LaunchableTypes(ctx, cfg.region)
		if err != nil {
			return "", err
		}
		var found, problems []string
		for _, t := range cfg.types {
			ref := strings.TrimPrefix(t, vultr.Vendor+".")
			if len(offered[ref]) == 0 {
				problems = append(problems, ref+" is not in stock in "+cfg.region)
				continue
			}
			found = append(found, ref)
		}
		detail := fmt.Sprintf("%d launchable plans in the region; the lane's: %s", len(offered), strings.Join(found, " "))
		if len(problems) > 0 {
			return detail, errors.New(strings.Join(problems, "; "))
		}
		return detail, nil
	})

	rep.check("read-only guard", func() (string, error) {
		detail := fmt.Sprintf("%d kinds of read calls: %s", len(a.actions()), strings.Join(a.actions(), " "))
		if r := a.refusals(); len(r) > 0 {
			return detail, fmt.Errorf("the checks tried write calls (refused): %s", strings.Join(r, ", "))
		}
		return detail, nil
	})
}
