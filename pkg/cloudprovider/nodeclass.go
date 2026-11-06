/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider

import (
	"context"
	"errors"
	"fmt"

	"github.com/awslabs/operatorpkg/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
)

// resolveNodeClass returns the SCNodeClass a NodeClassReference names.
func (c *CloudProvider) resolveNodeClass(
	ctx context.Context, ref *karpv1.NodeClassReference,
) (*v1alpha1.SCNodeClass, error) {
	if ref == nil {
		return nil, apierrors.NewNotFound(v1alpha1.SchemeGroupVersion.WithResource("scnodeclasses").GroupResource(), "")
	}
	nc := &v1alpha1.SCNodeClass{}
	if err := c.opts.Client.Get(ctx, types.NamespacedName{Name: ref.Name}, nc); err != nil {
		return nil, err
	}
	return nc, nil
}

// checkNodeClassReady returns the core error for a NodeClass that cannot
// launch nodes now (overview section 4.1 step 1).
func checkNodeClassReady(nc *v1alpha1.SCNodeClass) error {
	ready := nc.StatusConditions().Get(status.ConditionReady)
	switch {
	case ready == nil || ready.IsUnknown():
		return cloudprovider.NewCreateError(errors.New("the SCNodeClass has no Ready condition yet"),
			"NodeClassReadinessUnknown", "the SCNodeClass is not reconciled yet")
	case ready.IsFalse():
		return cloudprovider.NewNodeClassNotReadyError(errors.New(ready.Message))
	case ready.ObservedGeneration != nc.Generation:
		return cloudprovider.NewNodeClassNotReadyError(
			errors.New("the SCNodeClass status has not been reconciled against the latest spec"))
	}
	return nil
}

// vendorReady reports whether the vendor's part of the NodeClass is ready
// (condition VendorReady in status.vendors.<vendor>), with the reason when not.
func vendorReady(nc *v1alpha1.SCNodeClass, vendor string) (bool, string) {
	vs, ok := nc.Status.Vendors[vendor]
	if !ok {
		return false, "the SCNodeClass has no status for the vendor yet"
	}
	for _, cond := range vs.Conditions {
		if cond.Type == v1alpha1.VendorConditionReady {
			if cond.Status == metav1.ConditionTrue {
				return true, ""
			}
			return false, fmt.Sprintf("%s: %s", cond.Reason, cond.Message)
		}
	}
	return false, "the SCNodeClass has no VendorReady condition for the vendor yet"
}

// readyVendors returns the vendors of the NodeClass that are ready.
func readyVendors(nc *v1alpha1.SCNodeClass) map[string]bool {
	out := map[string]bool{}
	for _, v := range nc.Spec.Vendors.Names() {
		if ok, _ := vendorReady(nc, v); ok {
			out[v] = true
		}
	}
	return out
}
