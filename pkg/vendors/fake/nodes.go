//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"context"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// The label and annotation kwok-controller selects the Nodes it manages by
// (kwok/apis/v1alpha1 of Karpenter core).
const (
	kwokKey   = "kwok.x-k8s.io/node"
	kwokValue = "fake"
)

// createNode creates the Node object a kubelet would register, for
// kwok-controller to keep Ready. It carries the registration taint, so core
// registers it like a real node: syncs the NodeClaim's labels and removes the
// taint. An existing Node of the name is kept (a retried launch).
func (d *Driver) createNode(ctx context.Context, req vendors.CreateRequest) error {
	if d.cfg.Client == nil || req.Node.Name == "" {
		return nil
	}
	labels := maps.Clone(req.Node.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[kwokKey] = kwokValue
	labels[corev1.LabelHostname] = req.Node.Name
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: req.Node.Name, Labels: labels, Annotations: map[string]string{kwokKey: kwokValue},
		},
		Spec: corev1.NodeSpec{
			ProviderID: req.Node.ProviderID,
			Taints:     []corev1.Taint{karpv1.UnregisteredNoExecuteTaint},
		},
		Status: corev1.NodeStatus{
			Capacity: req.Node.Capacity, Allocatable: req.Node.Allocatable, Phase: corev1.NodePending,
		},
	}
	if err := d.cfg.Client.Create(ctx, node); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating the node %s: %w", req.Node.Name, err)
	}
	return nil
}

func (d *Driver) deleteNode(ctx context.Context, name string) error {
	if d.cfg.Client == nil || name == "" {
		return nil
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := d.cfg.Client.Delete(ctx, node); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting the node %s: %w", name, err)
	}
	return nil
}
