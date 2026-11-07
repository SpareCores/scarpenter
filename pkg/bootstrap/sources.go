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
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

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

	dnsMu   sync.Mutex
	dns     []string
	dnsTime time.Time
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

// DNSNamespace and DNSServiceLabel find the cluster DNS Service: kubeadm, k3s, EKS
// and most distributions label it k8s-app=kube-dns whatever its name (CoreDNS
// is named kube-dns or coredns, or carries a release prefix); the Service named
// DNSServiceName is the fallback.
const (
	DNSNamespace    = "kube-system"
	DNSServiceLabel = "k8s-app"
	DNSServiceValue = "kube-dns"
	DNSServiceName  = "kube-dns"
)

// ClusterDNS returns the ClusterIPs of the cluster DNS Service of kube-system
// (the one labelled k8s-app=kube-dns, else the one named kube-dns). It fails when
// there is none, naming how to give the address.
func (k *KubeReferences) ClusterDNS(ctx context.Context) ([]string, error) {
	clk := k.Clock
	if clk == nil {
		clk = clock.RealClock{}
	}
	k.dnsMu.Lock()
	defer k.dnsMu.Unlock()
	if len(k.dns) > 0 && clk.Since(k.dnsTime) < caCacheTTL {
		return k.dns, nil
	}
	var svcs corev1.ServiceList
	if err := k.Reader.List(ctx, &svcs, client.InNamespace(DNSNamespace),
		client.MatchingLabels{DNSServiceLabel: DNSServiceValue}); err != nil {
		return nil, fmt.Errorf("listing the cluster DNS Service in %s: %w", DNSNamespace, err)
	}
	// Several Services may carry the label (NodeLocal DNSCache's kube-dns-upstream
	// does): the one named kube-dns first, then by name, so that the choice never
	// depends on the order of the list. A Service without a ClusterIP (headless)
	// is skipped; the Service named kube-dns is the fallback when none of the
	// labelled ones has an address.
	candidates := svcs.Items
	slices.SortFunc(candidates, func(a, b corev1.Service) int {
		if ka, kb := a.Name == DNSServiceName, b.Name == DNSServiceName; ka != kb {
			if ka {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	ips := serviceIPs(candidates)
	if len(ips) == 0 {
		var svc corev1.Service
		err := k.Reader.Get(ctx, types.NamespacedName{Namespace: DNSNamespace, Name: DNSServiceName}, &svc)
		if client.IgnoreNotFound(err) != nil {
			return nil, fmt.Errorf("reading the Service %s/%s: %w", DNSNamespace, DNSServiceName, err)
		}
		if err == nil {
			ips = serviceIPs([]corev1.Service{svc})
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no cluster DNS Service found in %s (labelled %s=%s, or named %s): set --cluster-dns "+
			"(Helm value cluster.dns) to its ClusterIP, or --cluster-dns=none (cluster.dns: [none]) "+
			"to leave the nodes' clusterDNS unset", DNSNamespace, DNSServiceLabel, DNSServiceValue, DNSServiceName)
	}
	k.dns, k.dnsTime = ips, clk.Now()
	log.FromContext(ctx).Info("found the cluster DNS Service", "namespace", DNSNamespace, "clusterDNS", ips)
	return ips, nil
}

// serviceIPs are the ClusterIPs of the first Service that has any.
func serviceIPs(svcs []corev1.Service) []string {
	for _, svc := range svcs {
		var ips []string
		for _, ip := range svc.Spec.ClusterIPs {
			if ip != "" && ip != corev1.ClusterIPNone {
				ips = append(ips, ip)
			}
		}
		if len(ips) == 0 && svc.Spec.ClusterIP != "" && svc.Spec.ClusterIP != corev1.ClusterIPNone {
			ips = append(ips, svc.Spec.ClusterIP)
		}
		if len(ips) > 0 {
			return ips
		}
	}
	return nil
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
