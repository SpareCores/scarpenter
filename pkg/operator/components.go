/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package operator

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/cloudprovider"
	"github.com/SpareCores/scarpenter/pkg/controllers/garbagecollection"
	"github.com/SpareCores/scarpenter/pkg/controllers/quotastatus"
	"github.com/SpareCores/scarpenter/pkg/quota"
	"github.com/SpareCores/scarpenter/pkg/scope"
	"github.com/SpareCores/scarpenter/pkg/unavailable"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/facts"
)

// scopeTimeout bounds one resolution of the regions in use.
const scopeTimeout = 30 * time.Second

// Components are the vendor-neutral pieces the SCarpenter controllers share,
// built from the options.
type Components struct {
	Registry    *vendors.Registry
	Unavailable *unavailable.Cache
	Tracker     *quota.Tracker
	Facts       *facts.Facts
	Catalog     *catalog.Catalog
	Scope       *scope.Resolver

	scopeMu   sync.Mutex
	lastScope map[string][]string
}

// DevHooks run when the components are registered; development builds
// (scarpenter_dev) add controllers that stand in for later plans, for example
// the dev NodeClass status controller of the fake driver.
var DevHooks []func(ctx context.Context, o *Operator, c *Components) error

// NewComponents builds the driver registry from --vendors, the
// unavailable-offerings cache, the quota tracker, the driver facts and the
// catalog, and registers the runnables with the manager. The catalog's controller
// feeds the catalog the SCNodeClasses.
func (o *Operator) NewComponents(ctx context.Context) (*Components, error) {
	opts := o.Options
	reg, err := vendors.NewRegistry(ctx, opts.Vendors, vendors.Config{
		ClusterName: opts.ClusterName,
		Client:      o.GetClient(),
		Reader:      o.GetAPIReader(),
		Clock:       o.Clock,
		Log:         log.FromContext(ctx).WithName("vendors"),
		KeeperURL:   opts.KeeperURL,
	})
	if err != nil {
		return nil, fmt.Errorf("building the driver registry: %w", err)
	}
	un := unavailable.New(o.Clock, unavailable.TTLs{
		InsufficientCapacity: opts.UnavailableInsufficientCapacityTTL,
		Unsupported:          opts.UnavailableUnsupportedTTL,
		QuotaExceeded:        opts.UnavailableQuotaExceededTTL,
		ZoneUnusable:         opts.UnavailableZoneUnusableTTL,
	})

	c := &Components{Registry: reg, Unavailable: un}
	// The tracker needs the regions in scope, which need the driver facts, which
	// need the tracker's launchable view: close the loop with the resolver.
	c.Tracker = quota.New(quota.Options{
		Registry:           reg,
		Clock:              o.Clock,
		Unavailable:        un,
		Scope:              func() map[string][]string { return c.allowedRegions(ctx) },
		RefreshInterval:    opts.QuotaRefreshInterval,
		LimitsInterval:     opts.QuotaLimitsRefreshInterval,
		MaxStaleness:       opts.QuotaMaxStaleness,
		LaunchableInterval: opts.QuotaLaunchableInterval,
		Registerer:         metrics.Registry,
	})
	c.Facts = facts.New(facts.Options{Registry: reg, Launchable: c.Tracker, Clock: o.Clock})
	c.Scope = &scope.Resolver{Reader: o.GetClient(), Registry: reg, Regions: c.Facts}
	if c.Catalog, err = o.WithCatalog(c.Facts, un, c.Tracker); err != nil {
		return nil, err
	}
	for _, r := range []interface {
		Start(context.Context) error
		NeedLeaderElection() bool
	}{c.Facts, c.Tracker} {
		if err := o.Add(r); err != nil {
			return nil, fmt.Errorf("adding a runnable to the manager: %w", err)
		}
	}
	return c, nil
}

func (c *Components) allowedRegions(ctx context.Context) map[string][]string {
	ctx, cancel := context.WithTimeout(ctx, scopeTimeout)
	defer cancel()
	regions, err := c.Scope.Allowed(ctx)
	c.scopeMu.Lock()
	defer c.scopeMu.Unlock()
	if err != nil {
		// Keep the last good scope: an empty one would drop the quota view of
		// every region and let blocked offerings reappear.
		log.FromContext(ctx).Error(err, "resolving the regions in scope, keeping the last one")
		return c.lastScope
	}
	c.lastScope = regions
	return regions
}

func (c *Components) regionsInUse(ctx context.Context, vendor string) []string {
	ctx, cancel := context.WithTimeout(ctx, scopeTimeout)
	defer cancel()
	regions, err := c.Scope.InUse(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "resolving the regions in use")
		return nil
	}
	return regions[vendor]
}

// NewCloudProvider returns the CloudProvider over the components.
func (o *Operator) NewCloudProvider(c *Components) *cloudprovider.CloudProvider {
	return cloudprovider.New(cloudprovider.Options{
		Client:            o.GetClient(),
		Catalog:           c.Catalog,
		Registry:          c.Registry,
		Unavailable:       c.Unavailable,
		Quota:             c.Tracker,
		Scope:             c.Scope,
		Recorder:          o.EventRecorder,
		InstanceTypeStore: o.InstanceTypeStore,
		ClusterName:       o.Options.ClusterName,
		MaxLaunchAttempts: o.Options.MaxLaunchAttempts,
		Clock:             o.Clock,
		Registerer:        metrics.Registry,
	})
}

// WithComponentControllers registers the controllers of the components: the
// garbage collector, the NodeClass quota status and the development hooks.
func (o *Operator) WithComponentControllers(ctx context.Context, c *Components) error {
	opts := o.Options
	gc := garbagecollection.New(garbagecollection.Options{
		Client:        o.GetClient(),
		Registry:      c.Registry,
		Clock:         o.Clock,
		ClusterName:   opts.ClusterName,
		Interval:      opts.GCInterval,
		GracePeriod:   opts.GCGracePeriod,
		DryRun:        opts.GCDryRun,
		Recorder:      o.EventRecorder,
		Regions:       func(vendor string) []string { return c.regionsInUse(ctx, vendor) },
		DeploymentRef: deploymentRef,
		OnDeleted:     c.Tracker.NotifyChanged,
		Registerer:    metrics.Registry,
	})
	if err := o.Add(gc); err != nil {
		return fmt.Errorf("adding the garbage collector to the manager: %w", err)
	}
	qs := quotastatus.New(quotastatus.Options{
		Client: o.GetClient(), Catalog: c.Catalog, Tracker: c.Tracker, Recorder: o.EventRecorder, Clock: o.Clock,
	})
	if err := qs.Register(o.Manager); err != nil {
		return fmt.Errorf("registering the quota status controller: %w", err)
	}
	for _, hook := range DevHooks {
		if err := hook(ctx, o, c); err != nil {
			return err
		}
	}
	return nil
}

// deploymentRef names the controller's Deployment for Events that have no
// NodeClass to attach to (the chart sets SCARPENTER_DEPLOYMENT_NAME and
// POD_NAMESPACE); nil when they are not set.
func deploymentRef() *corev1.ObjectReference {
	name, ns := os.Getenv("SCARPENTER_DEPLOYMENT_NAME"), os.Getenv("POD_NAMESPACE")
	if name == "" || ns == "" {
		return nil
	}
	return &corev1.ObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: name, Namespace: ns}
}
