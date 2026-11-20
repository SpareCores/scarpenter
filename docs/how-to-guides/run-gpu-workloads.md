# Run GPU workloads

What the Helm chart ships for NVIDIA GPU nodes, and how a Pod uses it. GPU nodes
themselves (the driver, the labels, `nvidia.com/gpu`) are described in the
[node bootstrap reference](../reference/node-bootstrap.md#nvidia-gpu-nodes); until
the catalog advertises them, GPU types are launched as plain machines and
selected by their `accelerator-*` labels.

## What the chart installs

Both are on by default and independent of each other.

| Value | Object | Purpose |
|---|---|---|
| `nvidia.runtimeClass.create` | RuntimeClass `nvidia` (handler `nvidia`) | The containerd runtime handler that GPU nodes register. `runc` stays the default there, so a Pod gets GPUs only with `runtimeClassName: nvidia`. |
| `nvidia.devicePlugin.enabled` | DaemonSet `<release>-nvidia-device-plugin` | Advertises `nvidia.com/gpu`. Karpenter initializes a GPU node only after the resource is registered. |

The DaemonSet:

- runs only on nodes with the label `scarpenter.sparecores.com/nvidia-driver-branch`
  (set on the NVIDIA types that boot the driver), so other nodes pay nothing;
- uses `runtimeClassName: nvidia`, `priorityClassName: system-node-critical`, and
  requests 10m CPU and 64Mi memory (Karpenter counts this on GPU types only);
- uses NVIDIA's image from `nvcr.io/nvidia/k8s-device-plugin`, pinned by digest
  (`nvidia.devicePlugin.image.repository` points it at a mirror, for example
  where nvcr.io is slow or blocked);
- mounts the kubelet's `/var/lib/kubelet/device-plugins` as a hostPath, which
  Pod Security `baseline` and `restricted` namespaces refuse: install the chart in
  a namespace that enforces `privileged`, or turn the device plugin off;
- uses the environment variable strategy (no CDI), which works through the
  `nvidia` handler.

## Taints

A node with a taint that the device plugin does not tolerate never gets its Pod,
never registers `nvidia.com/gpu`, and never initializes. The default
`nvidia.devicePlugin.tolerations` is therefore `[{operator: Exists}]`: every
taint. The taints GPU NodePools usually carry are `nvidia.com/gpu` (the common
convention) or a key of your own; if you want a narrower rule, set the list, for
example `[{key: nvidia.com/gpu, operator: Exists}]`. A taint that is not in the
list is your responsibility.

## Using another device plugin or the GPU operator

Set `nvidia.runtimeClass.create=false` and `nvidia.devicePlugin.enabled=false`.
Your RuntimeClass must be named `nvidia` with the handler `nvidia` for the
Pods below, and your device plugin must register `nvidia.com/gpu`.

## A GPU Pod

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: gpu-smoke
spec:
  runtimeClassName: nvidia
  restartPolicy: Never
  containers:
    - name: smi
      image: nvcr.io/nvidia/cuda:12.6.0-base-ubuntu24.04
      command: [nvidia-smi]
      resources:
        limits:
          nvidia.com/gpu: 1
```
