# 12 — GPU nodes on AWS (optional)

Depends on: 10, and a GPU spike result (does Flatcar's NVIDIA driver
extension cover the catalog's AWS GPU families, including GRID/vGPU types?).
Read `docs/plans/v1/CONVENTIONS.md` first. Spec: overview §2 (deferred
list), §5.3, §8.2.

## Goal

GPU instance types boot with working NVIDIA drivers and a containerd
`nvidia` runtime, without a device plugin requirement, selectable by the
accelerator labels.

## Scope (to be refined after the spike)

- NodeClass `gpu` block: driver source (Flatcar official NVIDIA extension or
  a sysext URL + sha256), enabled automatically for types with
  `accelerator-manufacturer=NVIDIA`.
- Base template: enable the driver extension, load modules, persistence
  mode, containerd runtime handler `nvidia` (drop-in), a `RuntimeClass` in
  the chart.
- Optional `nvidia.com/gpu` capacity only when the user declares that a
  device plugin runs in the cluster (`gpu.advertiseResources: true`);
  otherwise labels only (overview §5.3).
- L5 scenario on the cheapest GPU type in scope (manual only; cost noted).

## Gates

CONVENTIONS gates 1–4; golden files for GPU renders; the manual L5 GPU
scenario run by the coordinator.
