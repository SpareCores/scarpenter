/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package operator wraps Karpenter core's operator: it adds the SCarpenter
// API to the manager's scheme and holds what the controllers share. The wiring
// follows pkg/operator/operator.go of aws/karpenter-provider-aws at commit
// 30c236cafacb3a23fb3a50370d09d71750b56b29 (Apache-2.0), adapted to core
// v1.15.1.
package operator

import (
	"context"
	"fmt"

	"github.com/awslabs/operatorpkg/controller"
	"github.com/samber/lo"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/metrics"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/overlay"
	corecontrollers "sigs.k8s.io/karpenter/pkg/controllers"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	coreoperator "sigs.k8s.io/karpenter/pkg/operator"

	"github.com/SpareCores/scarpenter/internal/version"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/operator/options"
)

// Operator is core's operator plus what the SCarpenter controllers share.
type Operator struct {
	*coreoperator.Operator
	Options *options.Options

	// staticCatalogs are the catalogs of the drivers that bring their own
	// (vendors.CatalogProvider), set by NewComponents.
	staticCatalogs map[string]catalog.Source
}

// NewOperator wraps a core operator. The options were parsed (and validated)
// by core's option injection when the core operator was created.
func NewOperator(ctx context.Context, op *coreoperator.Operator) (context.Context, *Operator) {
	opts := options.FromContext(ctx)
	if opts == nil {
		// Only reachable when the options package was not linked in, which
		// would be a wiring bug: fail like core does for bad configuration.
		panic("scarpenter options are missing from the context")
	}
	// Core registers its own types in client-go's global scheme, which the
	// manager uses.
	lo.Must0(v1alpha1.AddToScheme(scheme.Scheme), "adding the SCarpenter API to the scheme")

	log.FromContext(ctx).WithValues(
		"version", version.Get().Version,
		"cluster-name", opts.ClusterName,
		"vendors", opts.Vendors,
	).Info(fmt.Sprintf("starting %s", version.Get()))
	return ctx, &Operator{Operator: op, Options: opts}
}

// WithCoreControllers decorates the cloud provider the way core expects
// (metrics, then NodeOverlay) and registers core's controllers with it, after
// registering the per-NodeClass label keys with core. Our own
// controllers are registered with WithControllers by later plans.
func (o *Operator) WithCoreControllers(ctx context.Context, cp karpcloudprovider.CloudProvider) *Operator {
	// Before core builds its controllers from the well-known labels (labels.go).
	lo.Must0(o.RegisterLabelKeys(ctx), "registering the label keys of the SCNodeClasses")
	undecorated := metrics.Decorate(cp)
	decorated := overlay.Decorate(undecorated, o.GetClient(), o.InstanceTypeStore)
	clusterState := state.NewCluster(o.Clock, o.GetClient(), decorated)
	return o.WithControllers(ctx, corecontrollers.NewControllers(
		ctx,
		o.Manager,
		o.Clock,
		o.GetClient(),
		o.EventRecorder,
		decorated,
		undecorated,
		clusterState,
		o.InstanceTypeStore,
		o.PredictionStore,
	)...)
}

// WithControllers registers controllers with the manager and returns the
// operator for chaining.
func (o *Operator) WithControllers(ctx context.Context, controllers ...controller.Controller) *Operator {
	o.Operator.WithControllers(ctx, controllers...)
	return o
}

// Start runs the manager until the context is cancelled.
func (o *Operator) Start(ctx context.Context) {
	o.Operator.Start(ctx)
}
