/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package operator

import (
	"context"
	"fmt"
	"slices"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog/labels"
)

// RegisterLabelKeys registers the per-NodeClass label keys with Karpenter core
// as well-known labels (00a section 7.2): the cpu-flag-<flag> keys of the closed
// curated set, and metric-<m>, metric-<m>-pct and metric-<m>-per-usd for every
// metric of the SCNodeClasses that exist now. The fixed keys are registered by
// the API package.
//
// Core reads karpv1.WellKnownLabels when its controllers are constructed (node
// metrics take their label names from it) and the set is an unlocked global map,
// so this must run before WithCoreControllers builds them and never afterwards;
// WithCoreControllers calls it first. A metric added later is not selectable
// until a restart: the catalog's RestartRequired reports the keys.
//
// The SCNodeClasses are listed with the API reader, which works before the
// manager's caches have started.
func (o *Operator) RegisterLabelKeys(ctx context.Context) error {
	var list v1alpha1.SCNodeClassList
	if err := o.GetAPIReader().List(ctx, &list); err != nil {
		return fmt.Errorf("listing SCNodeClasses to register their label keys: %w", err)
	}
	var metrics []string
	for i := range list.Items {
		for name := range list.Items[i].Spec.Metrics {
			if !slices.Contains(metrics, name) {
				metrics = append(metrics, name)
			}
		}
	}
	slices.Sort(metrics)
	keys := append(labels.FlagKeys(), labels.MetricLabelKeys(metrics)...)
	added := labels.RegisterKeys(keys...)
	log.FromContext(ctx).Info("registered the label keys of the SCNodeClasses with Karpenter core",
		"classes", len(list.Items), "metrics", metrics, "keys", len(added))
	return nil
}
