# 12 — GPU nodes (NVIDIA, multi-vendor)

Depends on: 14 (driver contract, Butane parts), 13 (Keeper snapshots for the catalog tests), 06 (per-attempt
rendering), the vendor drivers on main (08 AWS, 16 GCP, 17 Azure, 21 Alibaba, 18 OVH; 19 UpCloud for its
conditional step) and ADR-026 (node memory). Not on 22 (no image work: the driver comes from a sysext, and
ADR-021's rungs do not change) and not on plan 26 (interruption). Read `docs/plans/v1/CONVENTIONS.md` first.
Spec: overview §2 (deferred list), §5.3 (capacity), §5.5 (labels), §8 (bootstrap); ADR-012, ADR-017, ADR-021,
[ADR-025](../../../explanation/DECISIONS.md#adr-025-instance-metadata-reaches-only-the-nodes-own-components-accepted-2026-11-19) (instance metadata), [ADR-026](../../../explanation/DECISIONS.md#adr-026-node-memory-a-capped-default-reservation-and-a-lean-footprint-accepted-2026-11-19); [node bootstrap](../../../reference/node-bootstrap.md),
[labels and offerings](../../../reference/labels-and-offerings.md).

Status: **todo**. Decisions of the maintainer 2026-11-19 are folded in (section 6). Research date 2026-11-19, from Flatcar's documentation and sources (flatcar/scripts tag
`stable-4757.2.3`, bootengine, ue-rs, sysext-bakery), the update server's listings, NVIDIA's release notes and
device plugin README, Karpenter core v1.15.1 and Keeper's server table (active rows, every vendor). No cloud API
was called. Everything marked **[U]** is unverified and is settled by the optional manual spikes (section 7), by real
benchmark runs (section 5) or by the cheap lanes.

## Goal

An NVIDIA GPU instance type that SCarpenter launches comes up with a loaded NVIDIA driver, the NVIDIA container
runtime and the device plugin, with no manual step: `nvidia.com/gpu` is in the type's capacity and on the node,
so a Pod with `runtimeClassName: nvidia` that requests all of the type's GPUs schedules, its NodeClaim
initializes and the Pod sees the GPUs. GPU support is chosen automatically from the catalog's accelerator data;
the driver is Flatcar's prebuilt sysext of the pinned Flatcar release (nothing is compiled on boot). Multi-GPU
and full passthrough from the start, 8-GPU NVSwitch systems included through a Fabric Manager step (D9); vendors in
the order AWS, GCP, Azure, Alibaba Cloud, OVHcloud, then UpCloud if it launches GPU plans. Out of scope: vGPU/GRID
and fractional GPUs, AMD GPUs, MIG, NVLink5 rack systems (GB200/GB300 NVL, arm64), arm64 GPUs, NPUs/FPGAs/TPUs, and a per-vendor pre-baked image (a later fallback, only where the
spikes show that boot is too slow).

## 1. Facts this plan builds on

1. **Driver sysext.** Flatcar builds `nvidia-drivers-<branch>[-open]` with every release, its modules signed
   with the release's kernel key (secure boot works), amd64 only. A name in `/etc/flatcar/enabled-sysext.conf`
   makes the initrd (`initrd-setup-root-after-ignition`) download `<server>/amd64-usr/<version>/flatcar-<name>.gz`
   (a signed update payload, verified against the OS's update key by `download_sysext`) unless
   `/etc/flatcar/sysext/flatcar-<name>-<version>.raw` already exists; a failed download fails the boot.
   `<server>` is `https://update.release.flatcar-linux.net` (302 to a CNCF CDN), overridable by the kernel
   argument `flatcar.release_file_server_url` only [U: no file-based override].
2. **Branches per release** (update server, 2026-11-19): stable 4757.2.3 and beta 4820.1.1 build 535, 550 and
   570 (each also `-open`), no 580; alpha 4865.0.0 adds 580 and 580-open; LTS builds none. Versions in 4757.2.3:
   535.309.01, 550.163.01, 570.211.01. Payloads are 300–450 MB (570-open 376.5 MB, 570 448.2 MB). 535, 550 and
   570 are end of life upstream; 580 is the last branch for Maxwell, Pascal and Volta; Blackwell needs 580-open
   or later; Kepler needs 470 (not built).
3. **Runtime sysext.** `nvidia-runtime-v<toolkit>` from the sysext-bakery (13.8 MB; release assets with
   `SHA256SUMS`, the URL pattern `pkg/artifacts` already uses for the `kubernetes` sysext), versions 1.16.2 to
   1.19.1; 1.16.x breaks with the prebuilt driver sysext. It adds the drop-in `containerd.service.d/10-nvidia.conf`,
   which resets `ExecStart` to `containerd --config /etc/containerd/config-nvidia.toml`; tmpfiles copies that file
   from `/usr/share/flatcar/etc/...` **only if it is absent** (`C`). The file is Flatcar's containerd config
   (version 2) plus a runtime `nvidia` and **`default_runtime_name = "nvidia"`**; `nvidia-container-runtime`
   runs in `mode = "auto"`.
4. **Loader.** Flatcar's `nvidia.service` (in the base image, `Before=containerd.target`) sees the merged sysext,
   loads `nvidia nvidia-modeset nvidia-uvm` (not `nvidia-drm`), creates the device nodes and runs `nvidia-smi`.
   The sysext's modprobe configuration blacklists `nouveau` and `nova_core`, leaves `nvidia-drm` modeset off and
   sets `fbdev=0`. `nvidia-persistenced.service` is shipped but nothing starts it [U].
5. **Fabric Manager** is in no Flatcar sysext: 8- and 16-GPU NVSwitch systems (HGX A100/H100/H200/B200) cannot
   initialize CUDA without it. Its version must equal the driver's exactly (the daemon aborts on a mismatch).
   NVIDIA publishes it as an archive per driver version
   (`developer.download.nvidia.com/compute/nvidia-driver/redist/fabricmanager/linux-x86_64/fabricmanager-linux-x86_64-<driver version>-archive.tar.xz`,
   with the sha256 in the driver release's `redist/redistrib_<driver version>.json`; verified 2026-11-20 for 535.309.01,
   550.163.01, 570.211.01 and 580.178.04, whose downloaded files hash to the manifests) holding `bin/nv-fabricmanager` (dynamic, needs
   only glibc and zlib), `lib/libnvfm`, `etc/fabricmanager.cfg`, the topology files in `share/nvidia/nvswitch/` and, from 570, the start
   script; the distribution packages (`nvidia-fabricmanager-<major>`) wrap the same files.
6. **Karpenter** initializes a NodeClaim only when every resource the claim requested is non-zero in the Node's
   allocatable, so a `nvidia.com/gpu` request needs a registered device plugin. The NVIDIA device plugin
   (v0.20.2) with a non-default `nvidia` runtime needs a RuntimeClass `nvidia` and `runtimeClassName: nvidia` on
   its own Pod.
7. **SCarpenter today.** Labels `accelerator-count` (rounded by `labels.Int`: 0.125 becomes `0`),
   `-memory-mib`, `-manufacturer`, `-model`, `-type` (`pkg/catalog/labels/attributes.go`); no `nvidia.com/gpu`
   capacity (`pkg/catalog/overhead.go`). AWS, Azure and Alibaba launch GPU types without a driver; GCP drops
   `a2 a3 a4 a4x g2 g4` (`pkg/vendors/gcp/quotas.go`), OVH its GPU families (`pkg/vendors/ovh/families.go`),
   UpCloud its `GPU-` plans, Vultr `vcg` and `vdm`. The catalog's `Class` has no Flatcar version.
8. **Catalog data** (active rows, NVIDIA): AWS L4, L40S, A10G, T4, V100, A100, H100, H200, B200, B300, RTX PRO
   4500/6000 (Blackwell), T4G (arm64), fractional L4 (`g6f`, `gr6f`); GCP A100, L4, H100, H200, B200, GB200, GB300,
   RTX PRO 6000 (fractional `g4-standard-6/12/24`); Azure T4, A10 (fractional, and `NV*ads_A10_v5` vGPU), A100,
   H100, H200, V100, M60, RTX PRO 6000 (fractional), GB200/GB300, plus AMD; Alibaba A10, T4, L20, V100, P100, P4,
   34 rows without a model, vGPU `vgn*`/`sgn*`; OVH V100, V100S, RTX 5000, L4, L40S, A10, A100, H100 (1–4 GPUs),
   H200 (8 GPUs); UpCloud L40S, H100, RTX PRO 6000. Hetzner, OCI and Vultr have none active.

## 2. Decisions

D1. **A support table in code, `pkg/nvidia`** (pure, vendor-neutral, no Kubernetes client), in three parts:

- *Models*: Keeper's `accelerator_model` → architecture (`M60` Maxwell; `P4 P40 P100` Pascal; `V100 V100S`
  Volta; `T4 RTX 5000` Turing; `A10 A10G A16 A30 A40 A100` Ampere; `L4 L20 L40 L40S` Ada; `H100 H200` Hopper;
  `B200 B300 GB200 GB300 RTX PRO 4500 RTX PRO 6000` Blackwell; `K80` Kepler).
- *Architectures*: open modules allowed (Turing and later) or required (Blackwell); the branch range (Kepler
  ≤ 470; Maxwell, Pascal, Volta ≤ 580; Blackwell ≥ 580).
- *Releases*: per Flatcar version SCarpenter supports (the pinned stable first, others as they are added): the
  the branches built, the exact driver version of each, the sha256 of each payload (`.gz`) and of each extracted
  `.raw` (for mirrors, D5), the `nvidia-runtime` version with its sha256, and the Fabric Manager archive of each
  driver version with its sha256 (D9).

Selection per (model, count, CPU architecture, Flatcar version): the **highest branch the release builds within
the architecture's range**, flavour `open` for Turing and later and proprietary before. On 4757.2.3 that is
570-open for Turing to Hopper and 570 for Maxwell, Pascal and Volta; Blackwell is unsupported until the pinned
release builds 580-open (decided 2026-11-19: no special code; a NodeClass on alpha gets it from the table). A test over the committed Keeper snapshots fails on an NVIDIA model the table does not
classify, so a new model is a one-line change and never a silent drop.

D2. **What the catalog offers** (unsupported types are dropped, decided 2026-11-19). A type whose `accelerator-type` is GPU is **dropped from the catalog** for a
NodeClass (not offered, never launched) unless all of these hold: the manufacturer is NVIDIA; the model is known;
the GPU count is whole; the CPU is amd64; the NodeClass's Flatcar version has a branch for it (D1); the count is
below the model's NVSwitch threshold, or the release's table has the Fabric Manager archive for its driver (D9); the vendor's driver does not mark it virtual (D10). Dropped types are
counted in a gauge `scarpenter_catalog_dropped_types{nodeclass,vendor,reason}` (reasons `GPUManufacturer`,
`GPUModelUnknown`, `GPUFractional`, `GPUArch`, `GPUNoDriverBranch`, `GPUNVSwitch`, `GPUVirtual`) and the reasons
are listed in the reference. Dropping, not a not-ready offering: an offering that can never become ready would be
retried as unavailable forever, and a node whose GPUs cannot be used is a silent degrade (ADR-017). NPUs, FPGAs,
VPUs and TPUs keep today's behaviour (decided 2026-11-19). No fractional GPU type remains, so `labels.Int`'s rounding
no longer reaches a GPU count; `Int` stays as it is, with a test that no fractional count of a GPU type reaches it.

D3. **Capacity.** Every NVIDIA type the catalog keeps has `nvidia.com/gpu: <count>` in capacity and allocatable
(nothing reserved). Always on: the chart ships the device plugin by default (D6), and an operator who turns it off
runs their own.

D4. **Labels.** Three new well-known type labels, single-valued per type and NodeClass, so Karpenter writes them
on the NodeClaim and core syncs them to the Node at registration (no kubelet `--node-labels`, so NodeRestriction
does not apply, and Pods can select them before a node exists): `scarpenter.sparecores.com/nvidia-driver-branch`
(`570`), `nvidia-driver-version` (`570.211.01`) and `nvidia-driver-flavor` (`open` or `proprietary`);
`DoesNotExist` on every other type. The values are known at render time because the Flatcar version and the
table fix them; the node proves them before it joins: a oneshot `scarpenter-nvidia-check.service` compares
`/proc/driver/nvidia/version` with the version and `nvidia-smi -L` with the GPU count, and the kubelet
`Requires=` it: a node whose check fails never joins (decided 2026-11-19; Karpenter deletes the claim after its
registration timeout and tries again). A node labelled 570.211.01 has that driver loaded. Rejected: writing the version
at boot into an `EnvironmentFile` that feeds the kubelet's `--node-labels` (it works, as `SCARPENTER_NODE_IP_ARGS`
does, but adds nothing the catalog does not know and hides the value from scheduling).

D5. **Driver delivery** (Ignition writes the files with pinned sha256, decided 2026-11-19). Default: Flatcar's own server, fetched by the initrd because of `enabled-sysext.conf`
(one line of user-data; Flatcar checks the payload's signature). Mirror: a controller flag
`--nvidia-driver-base-url` (`SCARPENTER_NVIDIA_DRIVER_BASE_URL`, Helm value), IaC-neutral, with the layout
`<base>/amd64-usr/<version>/flatcar-nvidia-drivers-<branch>[-open].raw.gz`. When it is set, the `nvidia` part
writes the extracted sysext to `/etc/flatcar/sysext/flatcar-nvidia-drivers-<branch>[-open]-<version>.raw` as an
Ignition file (`compression: gzip`, `verification.hash` from the table), so the initrd finds it and downloads
nothing: no kernel argument, so no first-boot reboot (the kernel-argument mirror was rejected). The runtime sysext goes through
`pkg/artifacts` like every sysext (`--sysext-base-url` mirrors it), and its sha256 must also equal the table's
pin. A committed script `hack/nvidia-mirror.sh` (network; not in `make test`) builds the mirror tree of a release:
it downloads the payloads, extracts them with Flatcar's own `download_sysext` (ue-rs, pinned, in a container),
compresses them, writes `SHA256SUMS` and prints the table entries for the release. The compile path of
`nvidia.service` never runs (the sysext is present).

D6. **Device plugin and RuntimeClass in the Helm chart** (the device plugin on by default, the RuntimeClass off by default because k3s and the GPU operator ship one, each switchable, decided 2026-11-20): values `nvidia.runtimeClass.create` (default `false`)
and `nvidia.devicePlugin.enabled` (default `true`); the image pinned by digest, its repository overridable. The
DaemonSet selects nodes with `nvidia-driver-branch` `Exists`, tolerates every taint, runs with
`runtimeClassName: nvidia` and priority `system-node-critical`, and requests 10m CPU and 64Mi memory [U: S6].
Karpenter counts it as DaemonSet overhead only on the GPU types it selects, so non-GPU and `Lean` nodes
(ADR-026) pay nothing; GPU nodes have 16 GiB or more, where 64Mi does not matter. No GPU operator, DCGM exporter
or GPU feature discovery (an operator may add them).

D7. **containerd: `nvidia` as a runtime handler only, `runc` stays the default** (decided 2026-11-19). SCarpenter
keeps the runtime sysext's drop-in and file and changes one line: a oneshot
`scarpenter-nvidia-containerd.service` (`After=systemd-tmpfiles-setup.service`, `Before=containerd.service`,
required by it) sets `default_runtime_name = "runc"` in `/etc/containerd/config-nvidia.toml` and fails unless
that line is there afterwards, so a changed sysext stops the boot (and L4 catches it) instead of silently making
`nvidia` the default. Why: with `nvidia` as the default and `mode = "auto"`, every container whose image sets
`NVIDIA_VISIBLE_DEVICES` (every CUDA base image) gets the GPUs without requesting `nvidia.com/gpu`, which defeats
the device plugin's accounting; with a handler only, a Pod gets GPUs only with `runtimeClassName: nvidia`, which
the benchmark Pods set (and they request all GPUs). SCarpenter owns no other containerd configuration (ADR-026);
the `Lean` drop-in sets environment only and is unaffected. No CDI: the device plugin's default (environment
variable) strategy works through the handler.

D8. **Node-side pieces: one Butane part `nvidia`**, merged after `volumes` and before the vendor fragments,
rendered only for a launch attempt of an NVIDIA type (the user-data is rendered per attempt since plan 06; the
NodeClass controller's trial rendering includes its largest variant, so the 16 KiB budget is checked; estimate
1.5 KiB). It holds: `enabled-sysext.conf`; the runtime sysext file and link; D5's file when a mirror is set; D7's
unit; `/etc/modprobe.d/00-scarpenter-nvidia.conf` (`blacklist nouveau`, `blacklist nova_core`,
`options nvidia-drm modeset=0 fbdev=0`, `install nvidia-drm /bin/true`, since a headless compute node needs no
DRM and this also prevents the udev reload loop the benchmark's earlier setup hit [U: the loop on Flatcar];
`options nvidia NVreg_DynamicPowerManagement=0x00`); `scarpenter-nvidia-prepare.service`
(`Before=nvidia.service`; unbinds and unloads `nouveau` if the initrd loaded it); `Upholds=nvidia-persistenced.service`
in a `multi-user.target` drop-in (persistence mode, so container starts do not reinitialize the GPUs); D4's check
unit and the kubelet drop-in `30-nvidia.conf` (`Requires=` and `After=` it); on NVSwitch types also D9's Fabric
Manager file and unit. Drift: the part is a function of the
Flatcar version (in the hash) and the table; the mirror URL is not hashed (like artifact URLs). GPU nodes that run
without a driver today keep it until they are replaced (new nodes only, as in ADR-025).

D9. **Fabric Manager for NVSwitch types** (decided 2026-11-19: included now, not deferred). **Which types:** a
model's table entry has an NVSwitch threshold (8 GPUs for `A100 H100 H200 B200 B300`: AWS p4d, p4de, p5.48xlarge, p5e,
p5en, p6; GCP a2-highgpu-8g, a2-megagpu-16g, a2-ultragpu-8g, a3-*-8g, a4; Azure ND A100 v4, ND H100 v5, ND H200 v5; OVH
h200 8x). At or above it the `nvidia` part adds Fabric Manager. Smaller slices of the same hardware (a2-highgpu-1g/2g/4g,
a3-highgpu-1g/2g/4g, p5.4xlarge) are offered without it [U: no Fabric Manager on a slice; whether the hypervisor
exposes NVSwitches to a 4-GPU slice]. 8-GPU V100 systems (AWS p3dn, Azure ND40rs_v2) use NVLink without NVSwitch and
are offered without it. NVLink5 systems (B200/B300 HGX) need the NVLink subnet manager (`nvlsm`) too: from 570 NVIDIA's
`nvidia-fabricmanager-start.sh` detects them (an InfiniBand device whose VPD says `SW_MNG`) and then requires the
`ib_umad` module, `ibstat` and `/opt/nvidia/nvlsm/sbin/nvlsm` (a separate archive in the same redistributables tree), and the
archives hold no HGX B200 topology file, only GB200/GB300 ones. Flatcar has neither `ib_umad` nor `ibstat`, so the table has
a per-model flag `needsNVLSM` and B200/B300 stay dropped (`GPUNVSwitch`) until a step ships that. **What is shipped:** NVIDIA's Fabric Manager archive for the **exact driver version** of the selected
branch (the table pins its URL and sha256 per driver version; the flavour does not change the version). A sysext was
rejected: it needs a build, a signing question and a publishing pipeline per driver release, which the archive
needs none of. Ignition writes the archive (an xz tarball, which Ignition cannot decompress, so as it is) with
`verification.hash` (the NVIDIA URL, or `<--nvidia-driver-base-url>/fabricmanager/<version>/` on a mirror;
`hack/nvidia-mirror.sh` copies it), and
`scarpenter-nvidia-fabricmanager.service` (`ExecStartPre` unpacks to `/opt/nvidia-fabricmanager` and copies the shipped
config with `DAEMONIZE=0`, `LOG_USE_SYSLOG=1` (the journal; with syslog the daemon writes no log file) and the topology path of the unpacked tree; `ExecStart` runs
`nv-fabricmanager -c <config>` in the foreground (no `LD_LIBRARY_PATH`: the binary does not link `libnvfm`); `Restart=on-failure`;
`After=nvidia.service`, `Before=scarpenter-nvidia-check.service`, started by the check unit's `Wants=` (`Requires=` would stop the check at every restart of the daemon and hide its message), so before the kubelet and
the device plugin) runs **only on NVSwitch types**. The check unit, on these types, also waits up to 3 minutes for
Fabric Manager's log line `Successfully configured all the available NVSwitches to route GPU NVLink traffic.` in the journal of the unit's current run (present in
the binaries of all four pinned versions) before the kubelet
starts, so the device plugin never advertises GPUs that cannot initialize. (`nvidia-smi -q`'s fabric state exists on
Hopper only; the log line is the same on A100 and H100.) The unit's order against
`nvidia-persistenced` is settled by S9. **Catalog:** until this step lands the NVSwitch types stay dropped
(`GPUNVSwitch`); once it lands they stay in the catalog for every release whose table entry has the Fabric Manager
pin, and `GPUNVSwitch` remains only for a release without it or a model that needs more (`nvlsm`).

D10. **Per-vendor work stays in the drivers** (the core never branches on a vendor): the vGPU types that need a
GRID driver (a new optional capability `vendors.GPUVirtualizer` with `Virtual(apiReference string) bool`), the
removal of the old GPU drops, GPU quota buckets where the vendor has them, and launch parameters (section 4).

D11. **Instance metadata (ADR-025)**: nothing here reads it. The driver payload comes from Flatcar's server or
the mirror in the initrd, the runtime sysext through Ignition, and neither the loader nor the device plugin uses
the metadata service. The `Restricted` default stays; no unit joins the metadata slice.

D12. **Images** stay on each vendor's ADR-021 rung: the sysexts are per board (`amd64-usr`), not per OEM image.
The default root disk (64 GiB on AWS and GCP) holds the extracted sysext (about 1 GB [U]) and CUDA images. A
pre-baked image (the `.raw` and `enabled-sysext.conf` in the image, so the initrd downloads nothing) is a later
fallback per vendor, built by the images repository (plan 22), only if S2 shows that the download dominates boot.

## 3. API, flags, chart, docs

- **No NodeClass field**: GPU support is automatic per type, with no per-NodeClass branch override (decided 2026-11-19). A NodePool requirement
  `accelerator-count DoesNotExist` already keeps GPU types out.
- The catalog's `Class` gets the Flatcar channel and version (they are NodeClass fields, so `class.Hash`
  covers them).
- Flag `--nvidia-driver-base-url`; Helm values `nvidia.driverBaseURL`, `nvidia.runtimeClass.*`,
  `nvidia.devicePlugin.*`.
- Docs, in the step that adds each piece: `labels.md` and `labels-and-offerings.md` (the three labels,
  `nvidia.com/gpu`, the drop reasons, the supported-model table generated from `pkg/nvidia`), `node-bootstrap.md`
  (the `nvidia` part, its units, the Fabric Manager unit, the mirror layout), `controller.md` (flag, gauge), the chart values reference,
  a how-to `run-gpu-workloads.md` (a Pod with `runtimeClassName: nvidia` requesting all GPUs, selection by
  `accelerator-model` and `nvidia-driver-branch`, quotas per vendor, the mirror, how to report a GPU failure), and each vendor's driver
  reference. The coordinator writes the ADR (GPU nodes) from sections 2 and 6, and updates
  overview §2 and §5.3.

## 4. Steps (a fresh agent each; CONVENTIONS rules; no live calls; each ends green, no live GPU gate)

**(a) Table and catalog.** `pkg/nvidia` (D1) with table tests (every architecture boundary, every release entry,
the flavour choice, Blackwell unsupported on 4757.2.3, Volta on 570 proprietary); the snapshot classification
test; the `Class` Flatcar fields; the drop rule and gauge (D2); capacity (D3); the labels (D4), registered as
well-known; the `GPUVirtualizer` capability (D10), implemented by the fake driver, whose catalog gains a 4-GPU
NVIDIA type, a fractional one, a virtual one and an 8-GPU A100; an L3 (KWOK) scenario: a Pod requesting
`nvidia.com/gpu: 4` gets the 4-GPU type and its NodeClaim initializes [U: whether KWOK nodes take extended
resources from the type's capacity; if not, the scenario sets them]; the docs. No bootstrap change.

**Done (a).** `pkg/nvidia` (models, architectures, releases 4757.2.3 and 4865.0.0, `Select`, the generated docs
table), `Class` Flatcar fields, drop rule and gauge, `nvidia.com/gpu` capacity, the three labels (registered),
`vendors.GPUVirtualizer` and `catalog.VirtualGPUFacts`, L3 scenario S21, docs. Deviations from the text above: the
fake driver has no catalog of its own (it impersonates a vendor from the Keeper snapshot), so S21 uses the snapshot's
real AWS types (4-GPU types, fractional `g6f`, arm64 `g5g`, 8-GPU `p4d`, Blackwell) and a new steering key
`virtualGPU` makes the fake's `Virtual` report a type; no synthetic types were added. The payload and `.raw`
sha256 pins of D1, the beta release (driver versions unverified) and the older alpha branches are left to step (b);
the table's Fabric Manager list is empty, so NVSwitch types stay dropped until (b2). `G59` (an Alibaba label) is the
one NVIDIA model of the snapshots that is deliberately not classified. Release guard: the const
`nvidia.NodeBootstrap` (false) keeps `nvidia.com/gpu` and the driver labels off, since Karpenter initializes a
NodeClaim only when every requested resource is registered, so a Pod requesting GPUs would get a node that never
initializes (and is never liquidated: core's liveness checks registration only). Supported GPU types are offered
as plain machines and the drops apply; S21 tests both settings. The step that ships the driver and the device
plugin, and Azure's and Alibaba's `GPUVirtualizer` (their vGPU types would fail the driver check and churn), sets it.

**(b) Node bootstrap.** The `nvidia` part (D7, D8) and the mirror path (D5) in `pkg/bootstrap`; the flag;
the runtime sysext through `pkg/artifacts` with the table's pin; the trial-rendering variant; golden files
(upstream and mirror; open and proprietary; with and without `Lean`; both network templates) and the budget test;
`hack/nvidia-mirror.sh` (shellcheck-clean, not run by the gates); the docs.

**Done (b).** The `nvidia` part (`templates/nvidia.bu`, `scarpenter-nvidia-check.sh`), `NVIDIAParams` and
`RenderRequest.NVIDIA`, the flag, the table's pins (below), `hack/nvidia-mirror.sh`, golden files (open and
proprietary, upstream and mirror, both templates, Lean), the worst-case size test and the trial rendering's
largest variant, the docs. Deviations from the text above: (1) the runtime sysext is not resolved into
`status.resolved` (a new field, and hashed digests, would drift every NodeClass); the table pins version and
sha256, the URL comes from `--sysext-base-url`, and Ignition verifies it. (2) The cloud provider rebuilds
the selection from the type's driver labels and `nvidia.com/gpu` capacity (`nvidia.FromLabels`), so the part is
rendered exactly when the catalog advertises the labels: nothing renders it while `nvidia.NodeBootstrap` is
false, and **(b) does not flip it** (D6 and step (a) tie it to the device plugin of (c) and to Azure's and
Alibaba's `GPUVirtualizer` of (g) and (h)); the flip is the last of these. (3) Ignition's hash with
`compression: gzip` is over the decompressed bytes (Ignition's `decompressCopyHashAndVerify`), so the table pins
the extracted `.raw` (`RawSHA256`) and, for the script, the payload (`PayloadSHA256`); the extracted images are
430 to 570 MB and gzip barely shrinks them (squashfs inside). (4) The worst-case budget of a GPU node is
14 KiB (`GPUBudgetBytes`): the part is about 2 KiB, not 1.5. Pins, verified 2026-11-20 against the update server's
`.gz.sha256` files and by extracting each payload with ue-rs `download_sysext` (rev 94d40218, update key
pinned): every build of 4757.2.3 (535, 550, 570, each open and proprietary) and of 4865.0.0's 580 and 580-open;
nvidia-runtime v1.17.9 (sha256 equal in the bakery release's `SHA256SUMS` and the file; its containerd config has
exactly one `default_runtime_name = "nvidia"` line). The driver version in each image's
`extension-release` equals the table (535.309.01, 550.163.01, 570.211.01, 580.178.04). Every driver sysext
ships `/usr/lib/modprobe.d/10-flatcar-nvidia-drivers-535-kmod-sysext.conf` with an `install` line for
`nvidia-drm`, and modprobe uses the first `install` line in file-name order, so the part's file is
`00-scarpenter-nvidia.conf`. A GPU-less QEMU boot of 4757.2.3 with the part (mirror variant, 570-open,
served locally) showed: Ignition writes the 430 MB image from the `.raw.gz` straight to the root disk (a
temporary file next to the target, not RAM) and verifies the hash over the decompressed bytes; the initrd
then downloads nothing and merges both sysexts; containerd runs `config-nvidia.toml` with
`defaultRuntimeName: runc` and the handler `nvidia`; `nvidia.service` succeeds without a GPU; the check fails
after 30 s with its message, the kubelet never starts (multi-user.target's `Upholds=` retries the kubelet,
so the check reruns every 30 s); `nvidia-persistenced` fails without a GPU; modprobe resolves `nvidia-drm` to
`/bin/true`; Flatcar's kernel has no `nouveau` module (the blacklist and the prepare unit are a guard only).
A mirror file with the wrong content fails Ignition's files stage (no retry on a hash mismatch) and the node
stops in the initrd. Not done: the beta release (driver versions unverified). Unverified until S1/S5: that
`Upholds=nvidia-persistenced.service` keeps it running on a GPU.

**(b2) Fabric Manager (D9).** The table's NVSwitch thresholds, per-model flags and Fabric Manager pins (URL and
sha256 per driver version of each release; a test fails on a release entry without them for a model that needs
them); the Ignition file, the unit and the check's fabric assertion in the `nvidia` part for NVSwitch types only; the
mirror path and `hack/nvidia-mirror.sh` (fetches and pins the archive); the drop rule of D2 relaxed accordingly, with
the gauge reason `GPUNVSwitch` kept for the rest; golden files for an NVSwitch type (upstream and mirror) and the
16 KiB budget test with the largest variant; an L3 scenario where the fake catalog's 8-GPU A100 is now offered and
its NodeClaim initializes; the docs. The L4 GPU-less boot (d) also renders an NVSwitch variant and checks that the
unit's file is verified and unpacked and that the check fails with a clear message.

**Done (b2).** `pkg/nvidia`: per-model `needsNVLSM`, `Release.FabricManager` (driver version to archive sha256),
`Selection.FabricManagerSHA256`, `FabricManagerFile`/`FabricManagerURL`/`FabricManagerPin`; `FromLabels` takes the
accelerator-model label (the cloud provider passes it) and so carries `NVSwitch` and the pin, and fails like `Select`
does; A100, H100 and H200 with 8 or more GPUs are offered on every release (pins for 535.309.01, 550.163.01,
570.211.01 and 580.178.04, taken from NVIDIA's `redistrib_<version>.json` and equal to the downloaded files' hashes,
2026-11-20); B200 and B300 stay dropped (`GPUNVSwitch`, they need `nvlsm`: see D9). The `nvidia` part writes the
archive, the unit and the fabric wait of the check (golden files for an H100 from NVIDIA's server and an A100 from a
mirror), `hack/nvidia-mirror.sh` copies and verifies archives (`fabricmanager-<driver version>`), the L3 scenario S21b,
the docs. Deviations: (1) `Wants=`, not `Requires=`, of the unit from the check (see D9); (2) no
`LD_LIBRARY_PATH` (the binary needs only glibc and zlib); (3) the fabric assertion is Fabric Manager's log line in the
journal, not `nvidia-smi -q` (Hopper only); (4) B200/B300 are not offered, the plan expected them to stay in; (5) no
Fabric Manager table flag per model beyond `needsNVLSM`: the threshold was already there. The GPU worst case of the
user-data is 14161 of 14336 bytes. Verified on a GPU-less QEMU boot of Flatcar 4757.2.3 (the rendered unit and file, the
archive served locally): Ignition verifies the 8 MB archive by hash, the unit unpacks it (xz is on the image), the
copied config has the expected settings, `nv-fabricmanager` runs (all libraries resolve) and exits with `failed to
allocate handle (client) to NVIDIA GPU driver` in the journal, the unit restarts every 5 s, and the check, which wants the
unit, fails after 30 s with its driver message (the fabric stage needs a loaded driver). Unverified (no GPU): that Fabric Manager reaches the success line on real HGX hardware, the
time to a ready fabric, the order against `nvidia-persistenced` (S9), whether H200 and the 4757.2.3 `550`/`535`
branches (not selected by any supported model today) need other settings.

**(c) Chart.** The RuntimeClass and the DaemonSet (D6), values and schema, `make test-chart` golden files, the
how-to.

**Done (c).** `nvidia-runtimeclass.yaml` and `nvidia-device-plugin.yaml` in the chart, values and schema
(`nvidia.runtimeClass.create`, `nvidia.devicePlugin.*`), golden files, `test/chart/nvidia_test.go`, the how-to
`run-gpu-workloads.md`, install and node-bootstrap docs. The image is `nvcr.io/nvidia/k8s-device-plugin` v0.20.2,
pinned by the digest of the multi-architecture index (read from the registry 2026-11-20). Tolerations default to
every taint (D6; a GPU node whose plugin cannot run never initializes) and are a value. The plugin uses the
environment variable strategy through the `nvidia` handler (no CDI, as step (b) configured). Not done:
`nvidia.NodeBootstrap` stays false; the flip waits for Azure's and Alibaba's `GPUVirtualizer` ((g), (h)), and for
(b2), the L4 boot of (d) and a real run to settle the device plugin's resources [U: S6].

**(d) L4: boot without a GPU** (cheap, no GPU instance). A qemu scenario launches the fake NVIDIA type on a VM without a GPU, through the
mirror path, against a local HTTP server that serves the release's driver payload (fetched once from Flatcar's
public server, which CONVENTIONS rule 8 allows, and extracted by step (b)'s script). It asserts: both sysexts
are merged; `nvidia.service` ends harmlessly without a GPU [U]; `nouveau` is not loaded; containerd runs with the
handler `nvidia` and the default `runc` (`crictl info`); `scarpenter-nvidia-check` fails with a clear message and
the kubelet does not start; with the check masked through `spec.butane`, the node joins; the boot timeline is
recorded. This is the cheap guard of D7's rewrite against a runtime sysext bump.

**(e) AWS.** `GPUVirtualizer` for `g6f`, `gr6f` (and any later fractional family);
no other launch change (classes G and P already map to their vCPU quotas, `QuotaZero` until an increase;
`InsufficientInstanceCapacity` stays reactive per zone). The shared L5 scenario `gpu` (in `test/lib.sh`, used
by every vendor's lane): **manual-only** (never in `all`, the smoke set, a CI gate or a schedule; the maintainer
runs it by hand when wanted), with a workflow input
`gpu_instance_type` (default: the vendor's cheapest supported type from its `defaults.sh`); it applies a NodePool
and a Pod with `runtimeClassName: nvidia` that requests all GPUs and runs `nvidia-smi` and a CUDA vectorAdd,
checks the labels against the table and the node's capacity, and records the time to `Ready`. Docs: aws-driver.md
(offered on 4757.2.3: g4dn, g5, g6, gr6, g6e, p3dn, p5.4xlarge, and with Fabric Manager p4d, p4de, p5.48xlarge,
p5e, p5en [U]; dropped: g5g arm64, g6f and gr6f virtual, g7, g7e and p6 Blackwell). Lane default `g4dn.xlarge`.

**(f) GCP.** Remove `a2 a3 a4 a4x g2 g4` from `droppedSeries` (the TPU series stay); the core
drops Blackwell and fractional types (8- and 16-GPU types are offered with Fabric Manager). Quotas: per-region buckets `NVIDIA_<model>_GPUS` and
`PREEMPTIBLE_NVIDIA_<model>_GPUS` (unit GPUs) and the global `GPUS_ALL_REGIONS`, consumed per type [U: the
metric names per model, A100 40 and 80 GB]; the series join the confirmed list when a real run confirms them. Launch:
`onHostMaintenance TERMINATE` and `automaticRestart false` already apply to these series; no `guestAccelerators`
(N1 with attached GPUs is not in Keeper's catalog). Lane default `g2-standard-4`.

**(g) Azure.** `GPUVirtualizer`: the `NV*` sizes (A10 v5, v3 M60 and any other GRID-only
series); AMD and fractional sizes fall to the core's rules. The family vCPU quotas exist. Check the SCSI-or-NVMe
rule against `NCasT4_v3`, NC A100 v4 and NC H100 v5. Lane default `Standard_NC4as_T4_v3`.

**(h) Alibaba Cloud.** `GPUVirtualizer`: `vgn*`, `sgn*`, `*-vws*`; rows without a model
fall to the core. Quotas stay vCPU plus reactive (the per-family Quota Center is the driver's open item). Mainland
China regions: document the mirror (D5) and the chart's image repository override (Flatcar's server, GitHub and
nvcr.io may be slow or blocked [U]). Lane default `ecs.gn6i-c4g1.xlarge` (T4).

**(i) OVHcloud.** Launch `t1 t2 a10 a100 h100 h200 l4 l40s rtx5000` (the 8-GPU
h200 gets Fabric Manager); `g1 g2 g3` stay excluded unless the data shows them active; the GPU quota per region becomes a bucket if
the API reports it, otherwise it stays reactive. Lane default: the cheapest `l4` or `rtx5000` flavor of the lane's
region.

**(j) UpCloud** (conditional, the maintainer's go-ahead). Launch the `GPU-` plans (not `GPU-SPOT-`), read GPU
stock (`GetDevicesAvailability`) for the offerings, a GPU count bucket from the account limits. L40S and H100
qualify; RTX PRO 6000 waits for 580-open.

Each vendor step adds the vendor's `gpu` defaults to its lane (for the manual scenario) and ends at the dry run; no
step runs a GPU instance.

## 5. Testing policy and gates (decided 2026-11-19)

**No live GPU test is a gate or part of CI.** That covers every GPU instance, and any live test that costs more than
about 1-2 USD per run and would need frequent runs. GPU behaviour is verified by **real benchmark runs**.
Kept as gates, all cheap: unit and table tests, golden files, the catalog and snapshot tests, the GPU-less VM boot
(L4, step (d)), and the L3 KWOK scenarios.

CONVENTIONS gates 1-4 per step; gate 5 (L4) for (b), (b2) and (d); golden files for every rendering variant and the
16 KiB budget test with the `nvidia` part (and the Fabric Manager variant) on both templates; `make test-chart` for
(c); the snapshot classification test of (a) over every committed snapshot; coverage of `pkg/nvidia` 90 % or more;
`hack/nvidia-mirror.sh` and the lane scripts shellcheck-clean, and `make test-e2e-<vendor>-dryrun` for (e) to (j).
README status `review` after (d) for the core, and per vendor after its step.

**Real-run feedback.** Until a type has a green real run, the vendor reference lists it as "offered, not yet verified".
The benchmark workload that uses GPU nodes reports its failures with the instance type, the Node's three driver
labels and the NodeClaim name (a node whose check failed never joins, so the report is the NodeClaim's registration
timeout plus the check unit's journal line where the vendor offers console logs); the maintainer turns each failure
into a table fix or a plan item. A short log, section 10 **"Verified in real runs"**, records one line per vendor and
type: date, Flatcar version, driver version, result and what was fixed. A vendor reference calls a GPU type verified
when the log has a success line for it; a Fabric Manager type needs its own line. The manual `gpu` scenario and the
spikes of section 7 stay available for the maintainer when a failure needs a closer look; no step needs them to end.

## 6. Decisions of the maintainer (2026-11-19)

Every recommendation was taken for questions 1-9 and 11; question 10 was decided differently (Fabric Manager now).

1. **Device plugin and RuntimeClass.** Decided by the maintainer 2026-11-19: the chart ships both, on by default,
   each can be disabled (operators with the GPU operator turn both off).
2. **containerd.** Decided by the maintainer 2026-11-19: the `nvidia` handler only, `runc` stays the default, by
   rewriting one line of the sysext's file (D7).
3. **Mirror mechanism.** Decided by the maintainer 2026-11-19: Ignition writes the extracted `.raw` with a pinned
   sha256 and an optional mirror (D5); the kernel-argument mechanism is not used.
4. **Unsupported GPU types.** Decided by the maintainer 2026-11-19: dropped, with the reason gauge (D2).
5. **Blackwell (580-open) and alpha.** Decided by the maintainer 2026-11-19: no special code; the table has the alpha
   entry, the how-to says so, and stable moves to 580 for every architecture when it lands.
6. **Pascal and Volta on 570.** Decided by the maintainer 2026-11-19: offered (the label states the branch); they
   move to 580 with the first stable release that has it.
7. **NodeClass override of the branch, or an opt-out.** Decided by the maintainer 2026-11-19: neither now; add
   `spec.nvidia.branch` only if a benchmark needs two branches on one Flatcar release.
8. **A node whose driver check fails.** Decided by the maintainer 2026-11-19: it never joins (the kubelet requires
   the check).
9. **NPUs, FPGAs, VPUs, TPUs.** Decided by the maintainer 2026-11-19: unchanged (launchable as plain machines with
   labels only; GCP's TPU series stay dropped); each gets its own plan if wanted.
10. **Fabric Manager.** Decided by the maintainer 2026-11-19, differently from the recommendation: **include it now**,
    not deferred (D9, step (b2)); the NVSwitch types stay in the catalog once the step lands.
11. **Vendor order.** Decided by the maintainer 2026-11-19: AWS, GCP, Azure, Alibaba Cloud, OVH, then UpCloud.
12. **Testing.** Decided by the maintainer 2026-11-19: live GPU tests, and any live test costing more than about
    1-2 USD per run that would need frequent runs, are not part of the plan's gates or CI; real benchmark runs
    verify GPU behaviour (section 5).

## 7. Spikes (manual-only and optional: the maintainer runs one when a real run needs a closer look; never a gate, never in CI; the cheapest supported type, minutes each; Butane equal to step (b)'s golden file)

S1. On `g4dn.xlarge` (AWS, T4, 4757.2.3): 570-open loads; `nvidia-smi` lists one GPU at 570.211.01; the runtime
    sysext merges; after D7's rewrite `crictl info` shows the default `runc` and the handler `nvidia`; with the
    device plugin, a Pod with `runtimeClassName: nvidia` gets the GPU and a CUDA Pod without it does not.
S2. Boot timeline (`systemd-analyze`, journal timestamps): the initrd's download from Flatcar's server and from a
    mirror in the same region, and boot to `Ready`. Decides whether any vendor needs a pre-baked image (D12).
S3. Ignition: `compression: gzip` with `verification.hash` (over the compressed or the decompressed bytes) for a
    file of about 1 GB; whether `kernel_arguments` reboots on first boot; that the initrd skips the download when
    the `.raw` exists (D5).
S4. Proprietary 570 on the T4 (the Volta path, cheaply); a Volta type once (OVH `t1` or Alibaba `gn6v`).
S5. `nvidia-persistenced` exists and runs with the `Upholds` drop-in; `nouveau` is loaded neither in the initrd
    nor after; no `nvidia-drm` load attempts and no udev reload loop (`udevadm monitor` for 60 s, the CPU time of
    `systemd-udevd`).
S6. The device plugin's memory and CPU on a 1-GPU and a 4-GPU node (D6's requests).
S7. Whether the toolkit's CDI refresh unit runs (it should not matter with D7); `nvidia.service` on a VM without
    a GPU (also step (d)).
S8. Per vendor (GCP, Azure, Alibaba, OVH): S1 on the lane's default type; GCP's GPU quota metric names and
    consumption; Azure's disk controller on the chosen sizes; from an Alibaba mainland region, the reachability of
    Flatcar's server, GitHub and nvcr.io; OVH's GPU quota in the API; on one 2- or 4-GPU slice of an HGX type
    (`a2-highgpu-2g`), that CUDA initializes without Fabric Manager.
S9. Fabric Manager on one 8-GPU NVSwitch type (the costly one; a real benchmark run is the normal way to learn
    this): the archive's contents and current URL, the exact-version rule against 570-open, the unit's order against
    `nvidia-persistenced`, the fabric-ready assertion, whether Blackwell HGX needs `nvlsm`, time to a ready fabric.

## 8. Unverified facts (settled by the spikes or the lanes)

- The initrd takes another server only from the kernel argument (no file-based override); Ignition's kernel
  arguments stage reboots on Flatcar (S3).
- `nvidia-persistenced.service` is present and works; udev does or does not load `nvidia-drm` on Flatcar (S5).
- `mode = "auto"` hands containers without NVIDIA variables to `runc` unchanged (moot with D7; S1).
- KWOK nodes carry extended resources from the type's capacity (step (a)).
- HGX slices below 8 GPUs need no Fabric Manager; the NVSwitch threshold per model (S8); the Fabric Manager archive
  layout, URL and open-module compatibility, and whether HGX B200 needs `nvlsm` (S9).
- GCP's per-model GPU quota metric names; OVH's GPU quota in the API; the disk controller of Azure's GPU sizes
  (S8).
- Reachability of Flatcar's server, GitHub and nvcr.io from Alibaba's mainland regions (S8).
- The device plugin's resource use (S6).
- The driver version per branch in the table is what `nvidia-smi` reports (S1, S4).

## 9. Dependencies

- **On main**: 14, 13, 06 (per-attempt rendering), the vendor drivers of steps (e) to (j), ADR-026's `Lean`
  part.
- **[ADR-025](../../../explanation/DECISIONS.md#adr-025-instance-metadata-reaches-only-the-nodes-own-components-accepted-2026-11-19)** (metadata restriction, on main): independent (D11); the `nvidia` part and the metadata part meet only in the
  kubelet's `Requires=`.
- **Plan 22**: not needed (D12). **Plan 26** (interruption): independent; GPU spot nodes follow it like any other
  node.
- **Outside the repository**: the operator's mirror, if any (D5); GPU quota increases per vendor (needed by real
  runs and any manual spike).

## 10. Verified in real runs

One line per vendor and type: date, Flatcar version, driver version, result, what was fixed. Empty until the first
real benchmark run on a GPU node.
