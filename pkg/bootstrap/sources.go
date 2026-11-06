/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
)

// RootCAConfigMap is the ConfigMap every namespace has, holding the cluster CA
// the Pods trust (key ca.crt).
const (
	RootCAConfigMap = "kube-root-ca.crt"
	RootCAKey       = "ca.crt"
	caCacheTTL      = 5 * time.Minute
)

// ErrNotFound marks a referenced ConfigMap, Secret or key that does not exist.
var ErrNotFound = errors.New("not found")

// References reads what a SCNodeClass points at: ConfigMap and Secret keys, and
// the cluster CA. The values come with the resourceVersion of the object, which
// the drift hash uses for the Secret (overview section 6.3).
type References interface {
	ConfigMapKey(ctx context.Context, ref v1alpha1.ConfigMapKeyRef) (value, resourceVersion string, err error)
	SecretKey(ctx context.Context, ref v1alpha1.SecretKeyRef) (value, resourceVersion string, err error)
	ClusterCA(ctx context.Context) (string, error)
}

// KubeReferences reads the references from the API server. Reader should be
// uncached (the manager's APIReader): the references live in any namespace, and
// a cached client would start a cluster-wide informer on Secrets.
type KubeReferences struct {
	Reader client.Reader
	// CAFile is --cluster-ca-file; empty reads the kube-root-ca.crt ConfigMap
	// of Namespace.
	CAFile    string
	Namespace string
	Clock     clock.Clock

	mu     sync.Mutex
	ca     string
	caTime time.Time
}

var _ References = (*KubeReferences)(nil)

// ConfigMapKey reads a ConfigMap key.
func (k *KubeReferences) ConfigMapKey(ctx context.Context, ref v1alpha1.ConfigMapKeyRef) (string, string, error) {
	var cm corev1.ConfigMap
	if err := k.Reader.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, &cm); err != nil {
		return "", "", fmt.Errorf("reading the ConfigMap %s/%s: %w", ref.Namespace, ref.Name, wrapNotFound(err))
	}
	if v, ok := cm.Data[ref.Key]; ok {
		return v, cm.ResourceVersion, nil
	}
	if v, ok := cm.BinaryData[ref.Key]; ok {
		return string(v), cm.ResourceVersion, nil
	}
	return "", "", fmt.Errorf("the ConfigMap %s/%s has no key %q: %w", ref.Namespace, ref.Name, ref.Key, ErrNotFound)
}

// SecretKey reads a Secret key.
func (k *KubeReferences) SecretKey(ctx context.Context, ref v1alpha1.SecretKeyRef) (string, string, error) {
	var s corev1.Secret
	if err := k.Reader.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, &s); err != nil {
		if apierrors.IsForbidden(err) {
			return "", "", fmt.Errorf("the controller may not read the Secret %s/%s: grant it with a Role (get on this "+
				"Secret) and a RoleBinding to the controller's ServiceAccount in the namespace %s (see config/rbac): %w",
				ref.Namespace, ref.Name, ref.Namespace, err)
		}
		return "", "", fmt.Errorf("reading the Secret %s/%s: %w", ref.Namespace, ref.Name, wrapNotFound(err))
	}
	if v, ok := s.Data[ref.Key]; ok {
		return strings.TrimSpace(string(v)), s.ResourceVersion, nil
	}
	return "", "", fmt.Errorf("the Secret %s/%s has no key %q: %w", ref.Namespace, ref.Name, ref.Key, ErrNotFound)
}

// ClusterCA returns the PEM CA bundle nodes use to verify the API server.
func (k *KubeReferences) ClusterCA(ctx context.Context) (string, error) {
	if k.CAFile != "" {
		b, err := os.ReadFile(k.CAFile)
		if err != nil {
			return "", fmt.Errorf("reading --cluster-ca-file: %w", err)
		}
		return validCA(string(b), "--cluster-ca-file "+k.CAFile)
	}
	clk := k.Clock
	if clk == nil {
		clk = clock.RealClock{}
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.ca != "" && clk.Since(k.caTime) < caCacheTTL {
		return k.ca, nil
	}
	ns := k.Namespace
	if ns == "" {
		ns = "kube-system"
	}
	var cm corev1.ConfigMap
	if err := k.Reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: RootCAConfigMap}, &cm); err != nil {
		return "", fmt.Errorf("reading the cluster CA from the ConfigMap %s/%s (or set --cluster-ca-file): %w",
			ns, RootCAConfigMap, wrapNotFound(err))
	}
	ca, err := validCA(cm.Data[RootCAKey], fmt.Sprintf("the ConfigMap %s/%s key %s", ns, RootCAConfigMap, RootCAKey))
	if err != nil {
		return "", err
	}
	k.ca, k.caTime = ca, clk.Now()
	return ca, nil
}

func validCA(pem, from string) (string, error) {
	pem = strings.TrimSpace(pem)
	if !strings.HasPrefix(pem, "-----BEGIN CERTIFICATE-----") {
		return "", fmt.Errorf("%s holds no PEM certificate", from)
	}
	return pem + "\n", nil
}

func wrapNotFound(err error) error {
	if client.IgnoreNotFound(err) == nil {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	return err
}

// Inputs are the contents the NodeClass points at, loaded for the rendering and
// for the drift hash.
type Inputs struct {
	// CustomTemplate is the Butane template of network.custom.
	CustomTemplate string
	// UserButane is spec.butane.
	UserButane string
	// AuthKey is the tailscale auth key; AuthKeyVersion the resourceVersion of
	// its Secret (the hash includes the version, never the key).
	AuthKey        string
	AuthKeyVersion string
}

// LoadInputs reads the ConfigMaps and the Secret a NodeClass references.
func LoadInputs(ctx context.Context, refs References, nc *v1alpha1.SCNodeClass) (Inputs, error) {
	var in Inputs
	var err error
	spec := &nc.Spec
	if spec.Network.Template == v1alpha1.NetworkTemplateCustom && spec.Network.Custom != nil {
		if in.CustomTemplate, _, err = refs.ConfigMapKey(ctx, spec.Network.Custom.ConfigMapRef); err != nil {
			return in, fmt.Errorf("network.custom: %w", err)
		}
	}
	if spec.Butane != nil {
		if in.UserButane, _, err = refs.ConfigMapKey(ctx, spec.Butane.ConfigMapRef); err != nil {
			return in, fmt.Errorf("butane: %w", err)
		}
	}
	if spec.Network.Template == v1alpha1.NetworkTemplateTailscale && spec.Network.Tailscale != nil {
		if in.AuthKey, in.AuthKeyVersion, err = refs.SecretKey(ctx, spec.Network.Tailscale.AuthKeySecretRef); err != nil {
			return in, fmt.Errorf("network.tailscale.authKeySecretRef: %w", err)
		}
	}
	return in, nil
}
