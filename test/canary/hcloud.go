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
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/hcloud"
)

// rateLimitFloor is the least RateLimit-Remaining the canary accepts after its calls: the lane
// runs on the same project and the limit is 3600 requests an hour.
const rateLimitFloor = 1000

// checkHCloud is the Hetzner part: the real driver's read paths, over the real client, behind the
// read-only guards. The token is the read-only one of the environment e2e-canary (HCLOUD_TOKEN).
func checkHCloud(ctx context.Context, cfg *config, rep *report) {
	a := newAudit()
	token := strings.TrimSpace(os.Getenv("HCLOUD_TOKEN"))
	if token == "" {
		rep.check("Hetzner token", func() (string, error) { return "", errors.New("HCLOUD_TOKEN is not set") })
		return
	}
	api := hcloud.NewClient(hcloud.ClientConfig{
		Token: token, Endpoint: os.Getenv("HCLOUD_ENDPOINT"), Version: "canary", HTTPClient: guardedHTTPClient(a),
	})
	runHCloud(ctx, cfg, rep, &roAPI{API: api, audit: a}, a)
}

// runHCloud runs the checks over an API (the guarded real one, or the fake in the tests) and ends
// with the audit of what was called.
func runHCloud(ctx context.Context, cfg *config, rep *report, api hcloud.API, a *audit) {
	drv := hcloud.New(hcloud.WithAPI(api), hcloud.WithRegisterer(prometheus.NewRegistry()))
	if err := drv.Init(ctx, vendors.Config{Log: logr.Discard()}); err != nil {
		rep.check("Hetzner driver", func() (string, error) { return "", err })
		return
	}
	locations := strings.Split(cfg.region, ",")

	// GET /locations: the lane's locations exist (the zones are the locations).
	rep.check("locations "+cfg.region, func() (string, error) {
		regions, err := drv.Regions(ctx)
		if err != nil {
			return "", err
		}
		have := map[string]bool{}
		for _, r := range regions {
			have[r.Name] = true
		}
		var missing []string
		for _, l := range locations {
			if !have[l] {
				missing = append(missing, l)
			}
		}
		detail := fmt.Sprintf("%d locations in the project", len(regions))
		if len(missing) > 0 {
			return detail, fmt.Errorf("the lane's location(s) %s do not exist", strings.Join(missing, ", "))
		}
		return detail, nil
	})

	// The Flatcar snapshots the NodeClasses resolve (ImagesReady): exactly one per architecture.
	for _, arch := range []string{archAMD64, archARM64} {
		name := fmt.Sprintf("Flatcar snapshot %s %s %s", cfg.flatcarChannel, cfg.flatcarVersion, arch)
		rep.check(name, func() (string, error) {
			img, err := drv.ResolveImage(ctx, vendors.ImageRequest{
				Region: locations[0], Arch: arch, Channel: cfg.flatcarChannel, Version: cfg.flatcarVersion})
			if err != nil {
				return "", err
			}
			return "image " + img.Ref, nil
		})
	}

	// GET /server_types: the lane's types exist, in a lane location, and are not past their
	// deprecation. A temporarily unavailable entry (out of stock) is noted, not a failure.
	rep.check("server types in "+cfg.region, func() (string, error) {
		types, err := api.ServerTypes(ctx)
		if err != nil {
			return "", err
		}
		var found, problems, stock []string
		now := time.Now()
		for _, t := range cfg.types {
			ref := strings.TrimPrefix(t, hcloud.Name+".")
			var entries []string
			ok := false
			for _, st := range types {
				if st.Name != ref {
					continue
				}
				for _, l := range st.Locations {
					if !contains(locations, l.Location.Name) {
						continue
					}
					if d := l.Deprecation; d != nil && !d.UnavailableAfter.IsZero() && !now.Before(d.UnavailableAfter) {
						problems = append(problems, fmt.Sprintf("%s is past its deprecation in %s", ref, l.Location.Name))
						continue
					}
					ok = true
					entries = append(entries, l.Location.Name)
					if !l.Available {
						stock = append(stock, ref+"@"+l.Location.Name)
					}
				}
			}
			if !ok {
				problems = append(problems, ref+" is not offered in any lane location")
				continue
			}
			found = append(found, fmt.Sprintf("%s(%s)", ref, strings.Join(entries, ",")))
		}
		detail := fmt.Sprintf("%d types in the project; the lane's: %s", len(types), strings.Join(found, " "))
		if len(stock) > 0 {
			detail += "; temporarily unavailable: " + strings.Join(stock, " ")
		}
		if len(problems) > 0 {
			return detail, errors.New(strings.Join(problems, "; "))
		}
		return detail, nil
	})

	// The rate limit left after the calls above (the headers of the last response).
	rep.check("API rate limit", func() (string, error) {
		rl := api.RateLimit()
		if !rl.Known() {
			return "", errors.New("the API sent no RateLimit-* headers")
		}
		detail := fmt.Sprintf("%d of %d requests left", rl.Remaining, rl.Limit)
		if rl.Remaining < rateLimitFloor {
			return detail, fmt.Errorf("fewer than %d requests left: another run is using the project", rateLimitFloor)
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

func contains(l []string, s string) bool { return slices.Contains(l, s) }
