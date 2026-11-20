//go:build chart

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package chart

import (
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func devicePlugin(t *testing.T, manifest string) *appsv1.DaemonSet {
	t.Helper()
	for _, o := range decode(t, manifest) {
		if d, ok := o.(*appsv1.DaemonSet); ok {
			return d
		}
	}
	return nil
}

// TestNVIDIADevicePluginDefaults: the device plugin and the RuntimeClass are on
// by default; the DaemonSet runs on driver-labelled nodes only, with the nvidia
// runtime, a digest-pinned image, lean requests and a toleration for every taint.
func TestNVIDIADevicePluginDefaults(t *testing.T) {
	out := mustHelm(t, "-f", filepath.Join("testdata", "values", "default.yaml"))
	if !strings.Contains(out, "kind: RuntimeClass\nmetadata:\n  name: nvidia") || !strings.Contains(out, "handler: nvidia\n") {
		t.Error("the RuntimeClass nvidia is not rendered by default")
	}
	ds := devicePlugin(t, out)
	if ds == nil {
		t.Fatal("the device plugin DaemonSet is not rendered by default")
	}
	spec := ds.Spec.Template.Spec
	if spec.RuntimeClassName == nil || *spec.RuntimeClassName != "nvidia" || spec.PriorityClassName != "system-node-critical" {
		t.Errorf("runtime class %v, priority class %q", spec.RuntimeClassName, spec.PriorityClassName)
	}
	terms := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || len(terms[0].MatchExpressions) != 1 ||
		terms[0].MatchExpressions[0].Key != "scarpenter.sparecores.com/nvidia-driver-branch" ||
		terms[0].MatchExpressions[0].Operator != corev1.NodeSelectorOpExists {
		t.Errorf("node affinity: %+v", terms)
	}
	if len(spec.Tolerations) != 1 || spec.Tolerations[0].Operator != corev1.TolerationOpExists || spec.Tolerations[0].Key != "" {
		t.Errorf("tolerations: %+v", spec.Tolerations)
	}
	c := spec.Containers[0]
	if !strings.HasPrefix(c.Image, "nvcr.io/nvidia/k8s-device-plugin:") || !strings.Contains(c.Image, "@sha256:") {
		t.Errorf("image %q: want NVIDIA's registry, pinned by digest", c.Image)
	}
	if got := c.Resources.Requests.Cpu().String(); got != "10m" {
		t.Errorf("cpu request %s", got)
	}
	if got := c.Resources.Requests.Memory().String(); got != "64Mi" {
		t.Errorf("memory request %s", got)
	}
}

// TestNVIDIAOff: each piece turns off on its own.
func TestNVIDIAOff(t *testing.T) {
	out := mustHelm(t, "-f", filepath.Join("testdata", "values", "nvidia-off.yaml"))
	if strings.Contains(out, "kind: RuntimeClass") || devicePlugin(t, out) != nil {
		t.Error("nvidia-off.yaml still renders the RuntimeClass or the DaemonSet")
	}
	out = mustHelm(t, append([]string{"--set", "nvidia.runtimeClass.create=false"}, required...)...)
	if strings.Contains(out, "kind: RuntimeClass") || devicePlugin(t, out) == nil {
		t.Error("runtimeClass.create=false must drop only the RuntimeClass")
	}
	out = mustHelm(t, append([]string{"--set", "nvidia.devicePlugin.enabled=false"}, required...)...)
	if !strings.Contains(out, "kind: RuntimeClass") || devicePlugin(t, out) != nil {
		t.Error("devicePlugin.enabled=false must drop only the DaemonSet")
	}
}

// TestNVIDIADevicePluginOverrides: image, tolerations and selector come from values;
// without a digest the tag alone is used; a malformed digest is refused.
func TestNVIDIADevicePluginOverrides(t *testing.T) {
	out := mustHelm(t, append([]string{
		"--set", "nvidia.devicePlugin.image.repository=registry.example.com/k8s-device-plugin",
		"--set", "nvidia.devicePlugin.image.digest=",
		"--set", "nvidia.devicePlugin.tolerations[0].key=nvidia.com/gpu",
		"--set", "nvidia.devicePlugin.tolerations[0].operator=Exists",
		"--set", "nvidia.devicePlugin.nodeSelector.topology\\.kubernetes\\.io/region=eu-west-1",
	}, required...)...)
	spec := devicePlugin(t, out).Spec.Template.Spec
	if got := spec.Containers[0].Image; got != "registry.example.com/k8s-device-plugin:v0.20.2" {
		t.Errorf("image %q", got)
	}
	if len(spec.Tolerations) != 1 || spec.Tolerations[0].Key != "nvidia.com/gpu" {
		t.Errorf("tolerations: %+v", spec.Tolerations)
	}
	if spec.NodeSelector["topology.kubernetes.io/region"] != "eu-west-1" || spec.NodeSelector["kubernetes.io/os"] != "linux" {
		t.Errorf("node selector: %+v", spec.NodeSelector)
	}
	if _, err := helm(t, append([]string{"--set", "nvidia.devicePlugin.image.digest=latest"}, required...)...); err == nil {
		t.Error("a digest that is not sha256:<hex> is accepted")
	}
}
