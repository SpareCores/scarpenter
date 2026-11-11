/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package operator

import (
	"context"
	"errors"
	"fmt"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/cloudprovider"
	"github.com/SpareCores/scarpenter/pkg/controllers/quotastatus"
	"github.com/SpareCores/scarpenter/pkg/controllers/retention"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// WithRetentionControllers registers the billing retention controller and its
// status controller (ADR-024, plan 23). They share the cluster state of
// Karpenter core, so it must run after WithCoreControllers; with
// --billing-retention=false nothing is registered.
func (o *Operator) WithRetentionControllers(ctx context.Context, c *Components, cp *cloudprovider.CloudProvider) error {
	if !o.Options.BillingRetention {
		return nil
	}
	if o.clusterState == nil {
		return errors.New("the billing retention controller needs the cluster state: call WithCoreControllers first")
	}
	qs := quotastatus.New(quotastatus.Options{
		Client: o.GetClient(), Catalog: c.Catalog, Tracker: c.Tracker, Clock: o.Clock,
	})
	ret := retention.New(retention.Options{
		Client:     o.GetClient(),
		Reader:     o.GetAPIReader(),
		Cluster:    o.clusterState,
		Rules:      retention.CatalogRules{Catalog: c.Catalog, Registry: c.Registry},
		Starts:     billingStarts{cp: cp},
		Clock:      o.Clock,
		Recorder:   o.EventRecorder,
		Signals:    c.Signals,
		Quota:      quotaScopes{client: o, qs: qs},
		Registerer: metrics.Registry,
	})
	if err := ret.Register(o.Manager); err != nil {
		return err
	}
	if err := retention.NewStatusController(ret, 0).Register(o.Manager); err != nil {
		return fmt.Errorf("registering the billing status controller: %w", err)
	}
	return nil
}

// billingStarts gives the billing start of an instance from the cloud
// provider's description of its NodeClaim.
type billingStarts struct{ cp *cloudprovider.CloudProvider }

func (b billingStarts) BillingStart(ctx context.Context, providerID string) (string, error) {
	nc, err := b.cp.Get(ctx, providerID)
	if err != nil {
		return "", fmt.Errorf("describing %s: %w", providerID, err)
	}
	return nc.Annotations[v1alpha1.AnnotationBillingStart], nil
}

// quotaScopes lists the scopes where every offering of a NodeClass is blocked by
// the quota tracker, which the quota status controller computes per class.
type quotaScopes struct {
	client *Operator
	qs     *quotastatus.Controller
}

func (q quotaScopes) BlockedScopes(ctx context.Context) ([]retention.QuotaScope, error) {
	var classes v1alpha1.SCNodeClassList
	if err := q.client.GetClient().List(ctx, &classes); err != nil {
		return nil, fmt.Errorf("listing the NodeClasses: %w", err)
	}
	seen := map[retention.QuotaScope]bool{}
	var out []retention.QuotaScope
	for i := range classes.Items {
		blocked, err := q.qs.Compute(ctx, classes.Items[i].Name)
		if err != nil {
			return nil, err
		}
		for vendor, list := range blocked {
			for _, b := range list {
				s := retention.QuotaScope{Vendor: vendor, Region: b.Region, CapacityType: b.CapacityType}
				if !seen[s] {
					seen[s] = true
					out = append(out, s)
				}
			}
		}
	}
	return out, nil
}
