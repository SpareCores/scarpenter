/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package offered

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog"
)

// The labels of the ConfigMaps (managed-by is also how they are found at start).
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelClass     = v1alpha1.Group + "/scnodeclass"
	LabelVendor    = v1alpha1.Group + "/vendor"
	managedBy      = "scarpenter"
	// heartbeat is how long a listing may go unwritten: generatedAt tells readers
	// the controller is alive.
	heartbeat = 10 * time.Minute
)

var configMapKind = corev1.SchemeGroupVersion.WithKind("ConfigMap")

// maxNameLength is the longest name of a ConfigMap.
const maxNameLength = 253

// listing is a ConfigMap this controller wrote.
type listing struct {
	class, vendor string
	digest        [sha256.Size]byte
	written       time.Time
	// zonesOmitted is how it was written; refused says it was not written, as it
	// did not fit: it is tried again when its content changes and at the heartbeat.
	zonesOmitted, refused bool
}

// ConfigMapName is the name of the listing of a class and vendor; a name that
// would be too long is cut and made unique with a hash.
func ConfigMapName(class, vendor string) string {
	name := "scarpenter-offered-" + class + "-" + vendor
	if len(name) <= maxNameLength {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return fmt.Sprintf("%s-%x", name[:maxNameLength-9], sum[:4])
}

// publish writes the ConfigMaps of a class that asks for them, and removes
// the ones it no longer asks for (annotation removed, vendor removed). A
// ConfigMap is written only when its content changed.
func (c *Controller) publish(ctx context.Context, nc *v1alpha1.SCNodeClass, report catalog.Report,
	ready map[string]bool) error {
	wanted := map[string]string{} // name -> vendor
	if nc.Annotations[v1alpha1.AnnotationPublishOffered] == "true" {
		if c.opts.Namespace == "" {
			log.FromContext(ctx).Info("not publishing the offered types: the controller's namespace is unknown (POD_NAMESPACE)",
				"nodeclass", nc.Name)
		} else {
			for _, vendor := range nc.Spec.Vendors.Names() {
				wanted[ConfigMapName(nc.Name, vendor)] = vendor
			}
		}
	}
	if c.opts.Namespace == "" {
		return nil
	}
	if err := c.findListings(ctx); err != nil {
		return err
	}
	if err := c.removeUnwanted(ctx, nc.Name, wanted); err != nil {
		return err
	}
	if len(wanted) == 0 {
		return nil
	}
	var errs []error
	for name, vendor := range wanted {
		if !ready[vendor] {
			continue // written once the vendor's facts are loaded and its launchable types settled
		}
		var types []catalog.TypeReport
		for _, t := range report.Types {
			if t.Vendor == vendor {
				types = append(types, t)
			}
		}
		// A vendor whose listing cannot be written does not hold back the others.
		if err := c.writeListing(ctx, nc, name, vendor, types); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// findListings reads, once, the ConfigMaps an earlier run wrote, so that those of
// a class that lost its annotation while the controller was down are removed.
func (c *Controller) findListings(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.found {
		return nil
	}
	var list metav1.PartialObjectMetadataList
	list.SetGroupVersionKind(configMapKind)
	managed := client.MatchingLabels{LabelManagedBy: managedBy}
	err := c.opts.Reader.List(ctx, &list, client.InNamespace(c.opts.Namespace), managed)
	if err != nil {
		return fmt.Errorf("listing the offered types ConfigMaps: %w", err)
	}
	for _, cm := range list.Items {
		for _, ref := range cm.OwnerReferences {
			if ref.Kind == kindSCNodeClass {
				c.listings[cm.Name] = listing{class: ref.Name, vendor: cm.Labels[LabelVendor]}
			}
		}
	}
	c.found = true
	return nil
}

func (c *Controller) removeUnwanted(ctx context.Context, class string, wanted map[string]string) error {
	c.mu.Lock()
	var gone []string
	vendors := map[string]string{}
	for name, l := range c.listings {
		if _, ok := wanted[name]; !ok && l.class == class {
			gone = append(gone, name)
			vendors[name] = l.vendor
		}
	}
	c.mu.Unlock()
	slices.Sort(gone)
	for _, name := range gone {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(configMapKind)
		cm.SetNamespace(c.opts.Namespace)
		cm.SetName(name)
		if err := c.opts.Client.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting the offered types ConfigMap %s: %w", name, err)
		}
		c.mu.Lock()
		delete(c.listings, name)
		c.mu.Unlock()
		c.metrics.forgetListing(class, vendors[name])
	}
	return nil
}

func (c *Controller) writeListing(ctx context.Context, nc *v1alpha1.SCNodeClass, name, vendor string,
	types []catalog.TypeReport) error {
	// The digest leaves out the time: an unchanged listing is written again only
	// as a heartbeat. The owner is part of it, a recreated class needs its
	// ConfigMaps again.
	now := c.opts.Clock.Now()
	l := newListing(nc.Name, vendor, "", types, c.opts.Quotas)
	content, err := json.Marshal(l)
	if err != nil {
		return fmt.Errorf("encoding the listing: %w", err)
	}
	digest := sha256.Sum256(append([]byte(nc.UID), content...))
	c.mu.Lock()
	known := c.listings[name]
	c.mu.Unlock()
	if known.digest == digest && now.Sub(known.written) < heartbeat {
		return nil
	}
	l.GeneratedAt = now.UTC().Format(time.RFC3339)
	data, zonesOmitted, err := encode(l, c.opts.ListingLimit)
	if err != nil {
		// Not a reconcile error, which would be retried at once: the old ConfigMap
		// ages out and the next try is at a change or the heartbeat.
		log.FromContext(ctx).Error(err, "not writing the offered listing", "nodeclass", nc.Name, "vendor", vendor)
		c.metrics.forgetListing(nc.Name, vendor)
		c.mu.Lock()
		c.listings[name] = listing{class: nc.Name, vendor: vendor, digest: digest, written: now, refused: true}
		c.mu.Unlock()
		return nil
	}
	if zonesOmitted {
		logger := log.FromContext(ctx).V(1)
		if !known.zonesOmitted {
			logger = log.FromContext(ctx)
		}
		logger.Error(nil, "the offered listing exceeded the cap, the zones are left out",
			"nodeclass", nc.Name, "vendor", vendor, "bytes", len(data), "cap", c.opts.ListingLimit)
	}
	cm := corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: c.opts.Namespace, Labels: labelsOf(nc.Name, vendor),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: v1alpha1.GroupVersion.String(), Kind: kindSCNodeClass, Name: nc.Name, UID: nc.UID,
			}},
		},
		BinaryData: map[string][]byte{ListingKey: data},
	}
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&cm)
	if err != nil {
		return fmt.Errorf("encoding the offered types ConfigMap %s: %w", name, err)
	}
	if err := c.opts.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{Object: u}),
		client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("writing the offered types ConfigMap %s: %w", name, err)
	}
	c.metrics.recordListing(nc.Name, vendor, len(data))
	c.mu.Lock()
	c.listings[name] = listing{class: nc.Name, vendor: vendor, digest: digest, written: now, zonesOmitted: zonesOmitted}
	c.mu.Unlock()
	return nil
}

// labelsOf are the labels of a listing; the class label is left out when the
// class name is too long for a label value.
func labelsOf(class, vendor string) map[string]string {
	l := map[string]string{LabelManagedBy: managedBy, LabelVendor: vendor}
	if len(class) <= validation.LabelValueMaxLength {
		l[LabelClass] = class
	}
	return l
}
