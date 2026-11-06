/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package catalogsync keeps the catalog's view of the SCNodeClasses current: on
// any change of an SCNodeClass it hands the catalog all of them, and the
// catalog refreshes when the scope (vendors, regions, metrics, selections)
// changed (overview section 5.2). It only reads SCNodeClasses; the status
// conditions (CatalogReady, RestartRequired) are set by the NodeClass
// controller from catalog.Status and catalog.RestartRequired.
package catalogsync

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog"
)

// Controller feeds the SCNodeClasses to the catalog.
type Controller struct {
	reader  client.Reader
	catalog *catalog.Catalog
}

// New returns the controller; Register adds it to a manager.
func New(reader client.Reader, c *catalog.Catalog) *Controller {
	return &Controller{reader: reader, catalog: c}
}

// Register watches SCNodeClasses with the manager.
func (c *Controller) Register(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("catalog-sync").For(&v1alpha1.SCNodeClass{}).Complete(c)
}

// Reconcile hands the catalog every SCNodeClass, whichever one changed: the
// catalog needs the union of all of them. A class whose kubelet settings do not
// parse is left out (and logged) until it is fixed.
func (c *Controller) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	var list v1alpha1.SCNodeClassList
	if err := c.reader.List(ctx, &list); err != nil {
		return reconcile.Result{}, fmt.Errorf("listing SCNodeClasses: %w", err)
	}
	classes := make([]catalog.Class, 0, len(list.Items))
	for i := range list.Items {
		nc := &list.Items[i]
		if !nc.DeletionTimestamp.IsZero() {
			continue
		}
		class, err := catalog.ClassFromNodeClass(nc)
		if err != nil {
			log.FromContext(ctx).Error(err, "ignoring an SCNodeClass with invalid kubelet settings", "nodeclass", nc.Name)
			continue
		}
		classes = append(classes, class)
	}
	c.catalog.SetClasses(classes)
	return reconcile.Result{}, nil
}
