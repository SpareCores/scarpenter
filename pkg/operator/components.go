/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package operator

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/SpareCores/scarpenter/pkg/artifacts"
	"github.com/SpareCores/scarpenter/pkg/bootstrap"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/cloudprovider"
	"github.com/SpareCores/scarpenter/pkg/controllers/bootstraptoken"
	"github.com/SpareCores/scarpenter/pkg/controllers/csr"
	"github.com/SpareCores/scarpenter/pkg/controllers/garbagecollection"
	"github.com/SpareCores/scarpenter/pkg/controllers/nodeclass"
	"github.com/SpareCores/scarpenter/pkg/controllers/quotastatus"
	"github.com/SpareCores/scarpenter/pkg/controllers/resourcestatus"
	"github.com/SpareCores/scarpenter/pkg/quota"
	"github.com/SpareCores/scarpenter/pkg/resources"
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
	Resources   *resources.Engine
	Scope       *scope.Resolver
	// Artifacts, Refs, Renderer and Tokens are the node bootstrap (overview
	// section 8): the sysext digests, the references of the NodeClasses, the
	// user-data and the bootstrap tokens.
	Artifacts *artifacts.Resolver
	Refs      *bootstrap.KubeReferences
	Renderer  *bootstrap.Renderer
	Tokens    *bootstrap.Minter

	scopeMu   sync.Mutex
	lastScope map[string][]string
}

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
	if o.staticCatalogs, err = staticCatalogs(reg); err != nil {
		return nil, err
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
	c.Artifacts = artifacts.New(artifacts.Config{
		SysextBase: opts.SysextBaseURL, FlatcarBase: opts.FlatcarBaseURL, Clock: o.Clock,
	})
	// The references live in any namespace and the tokens in kube-system: read
	// them straight from the API server instead of starting a cluster-wide
	// informer on Secrets.
	c.Refs = &bootstrap.KubeReferences{
		Reader: o.GetAPIReader(), CAFile: opts.ClusterCAFile, Namespace: os.Getenv("POD_NAMESPACE"), Clock: o.Clock,
	}
	c.Renderer = bootstrap.NewRenderer(bootstrap.RendererOptions{
		Refs: c.Refs, APIServerEndpoint: opts.APIServerEndpoint,
		ClusterDNS: opts.ClusterDNS, ClusterDomain: opts.ClusterDomain, KubeletClientCAFile: opts.KubeletClientCAFile,
	})
	c.Tokens = bootstrap.NewMinter(bootstrap.MinterOptions{
		Client: o.GetClient(), Reader: o.GetAPIReader(), TTL: opts.BootstrapTokenTTL,
		Clock: o.Clock, Recorder: o.EventRecorder,
	})
	c.Resources = resources.New(resources.Options{
		Registry: reg, Reader: o.GetClient(), Clock: o.Clock, ClusterName: opts.ClusterName,
		DefaultTTL: opts.ResourceDefaultTTL, PerUseGrace: opts.ResourcePerUseGrace, DryRun: opts.GCDryRun,
		Recorder: o.EventRecorder, DeploymentRef: deploymentRef, Unavailable: un,
		Regions:        func(vendor string) []string { return c.regionsInUse(ctx, vendor) },
		AllRegions:     c.Facts.Regions,
		FullSweepEvery: opts.ResourceFullSweepEvery,
		ResolveTTL:     min(opts.ResourceCheckInterval, 30*time.Second),
		Zones: func(vendor, region string) []string {
			zones, _ := c.Facts.Zones(vendor)
			out := make([]string, 0, len(zones[region]))
			for _, z := range zones[region] {
				out = append(out, z.Name)
			}
			return out
		},
		Registerer: metrics.Registry,
	})
	if c.Catalog, err = o.WithCatalog(c.Facts, un, c.Tracker, c.Resources); err != nil {
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
		Resources:         c.Resources,
		Scope:             c.Scope,
		Tokens:            c.Tokens,
		UserData:          c.Renderer,
		Recorder:          o.EventRecorder,
		InstanceTypeStore: o.InstanceTypeStore,
		ClusterName:       o.Options.ClusterName,
		MaxLaunchAttempts: o.Options.MaxLaunchAttempts,
		Clock:             o.Clock,
		Registerer:        metrics.Registry,
	})
}

// WithComponentControllers registers the controllers of the components: the
// garbage collector, the NodeClass controller and its quota status, the
// bootstrap token cleanup and the serving certificate approver.
func (o *Operator) WithComponentControllers(ctx context.Context, c *Components) error {
	opts := o.Options
	gc := garbagecollection.New(garbagecollection.Options{
		Client:        o.GetClient(),
		Reader:        o.GetAPIReader(),
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
		Sweepers:      []garbagecollection.Sweeper{c.Resources.Sweep},
		Registerer:    metrics.Registry,
	})
	if err := o.Add(gc); err != nil {
		return fmt.Errorf("adding the garbage collector to the manager: %w", err)
	}
	nodeClass := nodeclass.New(nodeclass.Options{
		Client: o.GetClient(), Catalog: c.Catalog, Registry: c.Registry, Artifacts: c.Artifacts, Renderer: c.Renderer,
		Refs: c.Refs, Regions: c.Facts, ImageGate: c.Resources,
		ServerVersion: o.serverVersion, Recorder: o.EventRecorder, Clock: o.Clock,
	})
	if err := nodeClass.Register(o.Manager); err != nil {
		return fmt.Errorf("registering the NodeClass controller: %w", err)
	}
	if err := bootstraptoken.New(o.GetAPIReader(), c.Tokens).Register(o.Manager); err != nil {
		return fmt.Errorf("registering the bootstrap token controller: %w", err)
	}
	var tailnet []netip.Prefix
	for _, p := range opts.TailnetPrefixes {
		tailnet = append(tailnet, netip.MustParsePrefix(p)) // validated with the options
	}
	approver := csr.New(csr.Options{
		Client: o.GetClient(), Recorder: o.EventRecorder, Clock: o.Clock, Registry: c.Registry, TailnetPrefixes: tailnet,
	})
	if err := approver.Register(o.Manager); err != nil {
		return fmt.Errorf("registering the serving certificate controller: %w", err)
	}
	c.checkClusterCA(ctx)
	qs := quotastatus.New(quotastatus.Options{
		Client: o.GetClient(), Catalog: c.Catalog, Tracker: c.Tracker, Recorder: o.EventRecorder, Clock: o.Clock,
	})
	if err := qs.Register(o.Manager); err != nil {
		return fmt.Errorf("registering the quota status controller: %w", err)
	}
	rs := resourcestatus.New(resourcestatus.Options{
		Client: o.GetClient(), Engine: c.Resources, Regions: c.Facts, Recorder: o.EventRecorder, Clock: o.Clock,
		Interval: opts.ResourceCheckInterval,
	})
	if err := rs.Register(o.Manager); err != nil {
		return fmt.Errorf("registering the resource status controller: %w", err)
	}
	return nil
}

// checkClusterCA logs when nodes cannot be given the cluster CA, which would
// fail every launch; the controller keeps running, the condition may be fixed
// without a restart (the ConfigMap is read again).
func (c *Components) checkClusterCA(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, scopeTimeout)
	defer cancel()
	if _, err := c.Refs.ClusterCA(ctx); err != nil {
		log.FromContext(ctx).Error(err, "the cluster CA for the nodes is not available; launching nodes fails until it is")
	}
}

// serverVersion returns the API server's version.
func (o *Operator) serverVersion(context.Context) (string, error) {
	info, err := o.KubernetesInterface.Discovery().ServerVersion()
	if err != nil {
		return "", fmt.Errorf("reading the API server's version: %w", err)
	}
	return info.GitVersion, nil
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
