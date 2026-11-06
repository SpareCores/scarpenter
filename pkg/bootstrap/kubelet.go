/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package bootstrap

import (
	"fmt"
	"maps"

	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
)

// Defaults of the kubelet configuration that the NodeClass does not set.
const (
	// DefaultMaxPods is the kubelet's own default.
	DefaultMaxPods = 110
	// DefaultClusterDomain is the kubelet's own default.
	DefaultClusterDomain = "cluster.local"
	// ClusterCAPath is where the cluster CA is written on the node: the
	// kubelet trusts it for the client certificates of the API server.
	ClusterCAPath = "/etc/kubernetes/pki/ca.crt"
)

// KubeletConfiguration returns the KubeletConfiguration (kubelet.config.k8s.io/v1beta1)
// YAML of a node. What is fixed: serverTLSBootstrap (the serving certificates
// come from the csr controller, overview section 8.5), the systemd cgroup
// driver Flatcar uses, webhook authentication and authorization with the
// cluster CA as client CA, rotating client certificates, and systemd-resolved's
// upstream resolv.conf (the stub address 127.0.0.53 does not work inside
// Pods). What the NodeClass sets: maxPods (default 110), systemReserved,
// kubeReserved and evictionHard (unset keeps the kubelet's defaults).
// clusterDNS is only set with --cluster-dns. The output is deterministic.
func KubeletConfiguration(spec v1alpha1.KubeletSpec, clusterDNS []string, clusterDomain string) (string, error) {
	maxPods := int32(DefaultMaxPods)
	if spec.MaxPods != nil {
		maxPods = *spec.MaxPods
	}
	if clusterDomain == "" {
		clusterDomain = DefaultClusterDomain
	}
	cfg := map[string]any{
		"apiVersion":         "kubelet.config.k8s.io/v1beta1",
		"kind":               "KubeletConfiguration",
		"serverTLSBootstrap": true,
		"rotateCertificates": true,
		"cgroupDriver":       "systemd",
		"maxPods":            maxPods,
		"clusterDomain":      clusterDomain,
		"resolvConf":         "/run/systemd/resolve/resolv.conf",
		"authentication": map[string]any{
			"anonymous": map[string]any{"enabled": false},
			"webhook":   map[string]any{"enabled": true},
			"x509":      map[string]any{"clientCAFile": ClusterCAPath},
		},
		"authorization": map[string]any{"mode": "Webhook"},
	}
	if len(clusterDNS) > 0 {
		cfg["clusterDNS"] = clusterDNS
	}
	for key, values := range map[string]map[string]string{
		"systemReserved": spec.SystemReserved,
		"kubeReserved":   spec.KubeReserved,
		"evictionHard":   spec.EvictionHard,
	} {
		if len(values) == 0 {
			continue
		}
		if key != "evictionHard" {
			for name, q := range values {
				if _, err := resource.ParseQuantity(q); err != nil {
					return "", fmt.Errorf("kubelet.%s[%s]: %w", key, name, err)
				}
			}
		}
		cfg[key] = maps.Clone(values)
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("encoding the kubelet configuration: %w", err)
	}
	return string(out), nil
}
