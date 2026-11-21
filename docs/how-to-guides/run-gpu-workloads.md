# Run GPU workloads

What the Helm chart ships for NVIDIA GPU nodes, and how a Pod uses it. GPU nodes
themselves (the driver, the labels, `nvidia.com/gpu`) are described in the
[node bootstrap reference](../reference/node-bootstrap.md#nvidia-gpu-nodes); until
the catalog advertises them, GPU types are launched as plain machines and
selected by their `accelerator-*` labels.

## What the chart installs

The device plugin is on by default, the RuntimeClass is off by default; they are independent of each other.

| Value | Object | Purpose |
|---|---|---|
| `nvidia.runtimeClass.create` (default `false`) | RuntimeClass `nvidia` (handler `nvidia`) | The containerd runtime handler that GPU nodes register. k3s ships a RuntimeClass `nvidia` with the same handler, as does a cluster with the NVIDIA GPU operator, and Helm cannot adopt an existing object: set it to `true` only where the cluster has no RuntimeClass `nvidia`. The device plugin needs one to exist. `runc` stays the default there, so a Pod gets GPUs only with `runtimeClassName: nvidia`. |
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

## The GPU pending taint

A GPU node registers with the taint `scarpenter.sparecores.com/gpu-pending:NoSchedule` and keeps it until its
driver check passed, so that Pods land only on a node whose GPUs work and the driver work does not count against
Karpenter's 15 minute registration timeout. List it in the NodePool, so that Karpenter plans Pods onto the new
node and waits for the taint's removal:

```yaml
spec:
  template:
    spec:
      startupTaints:
        - {key: scarpenter.sparecores.com/gpu-pending, effect: NoSchedule}
```

Without it Karpenter treats the taint as permanent and may launch more nodes than the Pods need. A node whose
check fails keeps the taint and never takes a Pod; Karpenter does not delete it, so put a deadline on the Pods that
wait for it ([node bootstrap](../reference/node-bootstrap.md#nvidia-gpu-nodes)).

## Taints

A node with a taint that the device plugin does not tolerate never gets its Pod,
never registers `nvidia.com/gpu`, and never initializes. The default
`nvidia.devicePlugin.tolerations` is therefore `[{operator: Exists}]`: every
taint. The taints GPU NodePools usually carry are `nvidia.com/gpu` (the common
convention) or a key of your own; if you want a narrower rule, set the list, for
example `[{key: nvidia.com/gpu, operator: Exists}]`. A taint that is not in the
list is your responsibility.

## Using another device plugin or the GPU operator

Leave `nvidia.runtimeClass.create` at `false` and set `nvidia.devicePlugin.enabled=false`.
Your RuntimeClass must be named `nvidia` with the handler `nvidia` for the
Pods below, and your device plugin must register `nvidia.com/gpu`.

## A GPU Pod

A GPU Pod needs both `runtimeClassName: nvidia` and an `nvidia.com/gpu` request:

- With both, the device plugin passes the GPUs it chose in the container's
  `NVIDIA_VISIBLE_DEVICES`, and the `nvidia` handler's hook mounts exactly those
  devices and the driver libraries.
- With the request but without the runtime class, the Pod lands on a GPU node and
  holds the GPUs, but `runc` ignores the variable: the container sees no GPU and no
  driver library, and nothing warns about it.
- With the runtime class but without a request, nothing places the Pod on a GPU
  node (elsewhere its sandbox fails: the handler does not exist there), and on a
  GPU node an image that sets `NVIDIA_VISIBLE_DEVICES=all` (every CUDA base image)
  sees every GPU of the node, outside the device plugin's accounting. Use the
  runtime class only together with a request.

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
