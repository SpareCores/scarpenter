# Node bootstrap

What SCarpenter gives a Flatcar node so that it joins the cluster: the
bootstrap token, the rendered Ignition user-data, the artifacts it downloads,
and the approval of its serving certificate. The design is in
[the overview](../plans/v1/wip/00-overview.md#8-node-bootstrap); this page is
what the code does. The package is `pkg/bootstrap` (rendering, tokens),
`pkg/artifacts` (URLs and checksums) and the controllers `nodeclass`,
`bootstraptoken` and `csr` under `pkg/controllers`.

- [Cluster prerequisites](#cluster-prerequisites)
- [How a node joins](#how-a-node-joins)
- [Bootstrap tokens](#bootstrap-tokens)
- [Artifacts](#artifacts)
- [The user-data](#the-user-data)
- [Template variables](#template-variables)
- [The kubelet](#the-kubelet)
- [Network templates](#network-templates)
- [The interruption agent](#the-interruption-agent)
- [Custom templates and extra Butane](#custom-templates-and-extra-butane)
- [Size budget](#size-budget)
- [The NodeClass controller](#the-nodeclass-controller)
- [The serving certificate approver](#the-serving-certificate-approver)
- [RBAC](#rbac)
- [Security notes](#security-notes)

## Cluster prerequisites

- Bootstrap-token authentication enabled on the API server
  (`--enable-bootstrap-token-auth`; k3s has it by default) and the
  `tokencleaner` controller of the controller manager (it removes expired
  tokens).
- The API server reachable from the nodes at `--api-server-endpoint` (an
  `https://` URL) and a CA bundle for it: `--cluster-ca-file`, or the
  `kube-root-ca.crt` ConfigMap of the controller's namespace.
- A CNI and kube-proxy (or a replacement) that run on the remote nodes as
  DaemonSets; no cloud-controller-manager is needed. They reach the API server by
  its address (`KUBERNETES_SERVICE_HOST` of the DaemonSet), because the ClusterIP
  needs the proxy itself.
- On **k3s**, which signs the API server's client certificate for the kubelet
  with its own client CA (`<data-dir>/server/tls/client-ca.crt`) and not with the
  CA of `kube-root-ca.crt` (its server CA): `--kubelet-client-ca-file` (see
  [the kubelet](#the-kubelet)). The L4 lane verified that a k3s server needs no
  other flag than the ones of a production k3s for remote workers
  (`--flannel-backend=none`, `--disable-kube-proxy`, `--disable-network-policy`,
  `--egress-selector-mode=disabled`): it enables bootstrap-token authentication
  and the `tokencleaner` controller by default and its `kube-root-ca.crt` holds
  the CA nodes need (the server CA that signs the API server's serving
  certificate).
- The [RBAC](#rbac) for the join.
- With the `tailscale` template: a tailnet (or Headscale) the control plane is
  on, and an auth key Secret. With `direct`: nodes and control plane reach each
  other directly.
- The kubelet version (`spec.kubernetes.version`) at most three minors older
  than the API server and never newer; the NodeClass controller enforces it.

## How a node joins

1. `Create` mints a [bootstrap token](#bootstrap-tokens), runs the driver's
   `Prepare` for the launch attempt, renders the user-data of that attempt (the
   architecture, the offering, the type's kube-reserved and the volumes' device
   hints) and gives it to the driver. A NodeClaim's attempts each render their own
   user-data, since plan 06.
2. The node boots Flatcar. Ignition downloads the pinned sysexts (sha256
   verified), writes the files and units; `systemd-sysext` activates the
   `kubernetes` sysext, whose `kubelet.service` starts.
3. The kubelet authenticates with the token as `system:bootstrap:<token-id>`
   (group `system:bootstrappers:scarpenter`) and creates a client certificate
   request, which the controller manager's `csrapproving` controller approves
   because of the [RBAC](#rbac).
4. The kubelet registers the Node with `--provider-id`, `--hostname-override` and
   the taints (`karpenter.sh/unregistered:NoExecute`, the NodeClaim's taints and
   startup taints). Core matches the Node to the NodeClaim by provider ID, syncs
   labels and taints and removes the unregistered taint.
5. With `serverTLSBootstrap` the kubelet requests a serving certificate, which
   the [approver](#the-serving-certificate-approver) approves.
6. The `bootstraptoken` controller deletes the token when the NodeClaim is
   `Registered`.

## Bootstrap tokens

`Create` writes, per NodeClaim, a Secret `bootstrap-token-<token-id>` of type
`bootstrap.kubernetes.io/token` in `kube-system` (`bootstrap.Minter`):

| Key / label | Value |
|---|---|
| `token-id` | 6 characters of `[a-z0-9]`, from `crypto/rand` |
| `token-secret` | 16 characters of `[a-z0-9]`, from `crypto/rand` |
| `expiration` | now + `--bootstrap-token-ttl` (RFC 3339) |
| `usage-bootstrap-authentication` | `true` |
| `auth-extra-groups` | `system:bootstrappers:scarpenter` |
| label `scarpenter.sparecores.com/nodeclaim` | the NodeClaim's name |
| label `scarpenter.sparecores.com/nodeclass` | the NodeClaim's NodeClass (for Events) |

Minting is idempotent per NodeClaim: a retried `Create` gets the token that an
earlier attempt may have put on an instance (the `Retryable` path keeps it),
never a rotated one. An expired token that still exists (the token cleaner is
lazy) is replaced. `--bootstrap-token-ttl` must be at least 20 minutes: core
gives a NodeClaim 15 minutes to register, and the token must outlive that and the
retries within it.

A token is deleted:

| When | By | Event on the NodeClaim |
|---|---|---|
| the NodeClaim is `Registered` | `bootstraptoken` controller | `BootstrapTokenDeleted`: `NodeClaimRegistered` |
| the NodeClaim is being deleted | `bootstraptoken` controller | `BootstrapTokenDeleted`: `NodeClaimDeleting` |
| the NodeClaim is gone | `bootstraptoken` controller | `BootstrapTokenDeleted`: `NodeClaimNotFound` |
| the NodeClaim never launched, or the instance is gone | `Create` / `Delete` of the CloudProvider | none |
| no NodeClaim exists after the grace period | the [garbage collector](controller.md#garbage-collection) | `GarbageCollected` on the NodeClass |
| it expired | core's token cleaner | none |

## Artifacts

`--sysext-base-url` and `--flatcar-base-url` (mirrors; empty is upstream)
configure `pkg/artifacts`:

- Sysext image: `<sysextBase>/<name>-<version>/<name>-<version>-<x86-64|arm64>.raw`,
  for example `kubernetes-v1.37.1-x86-64.raw`. Its sha256 comes from
  `<sysextBase>/<name>-<version>/SHA256SUMS` of the same release. The default
  base is `https://github.com/flatcar/sysext-bakery/releases/download`.
- Flatcar release file: `<flatcarBase>/<arch>-usr/<version>/<file>` with the
  default base `https://<channel>.release.flatcar-linux.net` (a mirror base may
  contain `{channel}`). For the qemu driver; AWS uses official AMIs.

The NodeClass controller fetches each `SHA256SUMS` (no GitHub API, no rate
limits), caches it by URL for an hour (a failure for a minute) and writes the
resolved URL and digest of both architectures to `status.resolved`. Rendering
never fetches anything. If the release server fails while the status already
holds the digests of the pinned version, they are kept and `ArtifactsReady` stays
`True` (its message says refreshing failed); a version that was never resolved,
or is not published (`ReleaseNotFound`), makes `ArtifactsReady` `False`.

**Limitations.** The default sysext and NVIDIA driver URLs (GitHub, `ghcr.io`, the Flatcar release server) have no IPv6, so nodes
without IPv4 need a mirror ([driver contract](driver-contract.md#reaching-the-bootstrap-artifacts)); long term the URLs should point at dual-stack hosts or a mirror.

## The user-data

The user-data is a small Ignition config (version 3.4.0) whose
`ignition.config.merge` list holds the configs below, each a gzip-compressed,
base64-encoded `data:` URL (`compression: gzip`), in this order (later parts
override earlier ones):

1. **base**, always: `/etc/hostname`, the cluster CA (`/etc/kubernetes/pki/ca.crt`),
   the bootstrap kubeconfig (`/etc/kubernetes/bootstrap-kubelet.conf`, mode 0600, which
   references the CA file instead of embedding the CA a second time),
   the [kubelet configuration](#the-kubelet) (`/var/lib/kubelet/config.yaml`), the
   `kubernetes` sysext (a file below `/opt/extensions/kubernetes/` with its sha256,
   linked as `/etc/extensions/kubernetes.raw`) and the sysexts of `spec.sysexts`
   for the node's architecture, the kubelet drop-in, masked `update-engine` and
   `locksmithd`, and with `spec.poweroffAfter` a timer
   (`OnBootSec=<seconds>`) that runs `systemctl poweroff --force`.
2. **lean**, with the [`Lean` footprint](node-settings.md#memory-footprint) only: memory
   limits of the kubelet, containerd and (with the tailscale template) tailscaled, a
   volatile journal and masked units a worker does not need.
3. **volumes**, for nodes with extra volumes or local storage (the
   [first-boot volume unit](node-settings.md#extra-volumes-and-local-storage)).
4. **nvidia**, for a launch of a supported NVIDIA GPU type only (see [NVIDIA GPU nodes](#nvidia-gpu-nodes)):
   the driver sysext, the `nvidia-runtime` sysext, containerd's `nvidia` handler, the module settings
   and the driver check the kubelet requires.
5. **vendor fragments**, one part per fragment the vendor's driver contributes, among them the
   [instance metadata access](#instance-metadata-access) part (`vendor-metadata-access`), unless the
   NodeClass says `Open` or the vendor has no metadata service.
6. **network**: [`tailscale`, `direct` or `custom`](#network-templates).
7. **user**, when `spec.butane` is set.

Each part is a Butane config (`variant: flatcar`, version `1.1.0`; `1.0.0` is
accepted in custom and user configs, the experimental version is not), rendered
with Go `text/template` and translated with the Butane library in **strict mode**
(a warning is an error). Ignition's own merge combines the parts, so SCarpenter
does no YAML merging. The templates are embedded
(`pkg/bootstrap/templates/base.bu`, `lean.bu`, `nvidia.bu`, `tailscale.bu`, `direct.bu`).

**Kubernetes sysext contract.** The `kubernetes` sysext of the sysext-bakery
(inspected for v1.37.1, 2026-11-06) ships `/usr/bin/kubelet`, `kubeadm`,
`kubectl`, the CNI plugins, a `kubelet.service` (`ExecStart=/usr/bin/kubelet`,
`Restart=always`), the kubeadm drop-in `kubelet.service.d/10-kubeadm.conf`
(it sets `--bootstrap-kubeconfig=/etc/kubernetes/bootstrap-kubelet.conf`,
`--kubeconfig=/etc/kubernetes/kubelet.conf`, `--config=/var/lib/kubelet/config.yaml`,
sources `/var/lib/kubelet/kubeadm-flags.env` and `/etc/sysconfig/kubelet`, copies
the CNI plugins to `/opt/cni/bin` before the start), and
`multi-user.target.d/10-kubelet-service.conf` with `Upholds=kubelet.service`.
SCarpenter therefore writes the files at those paths and adds the drop-in
`/etc/systemd/system/kubelet.service.d/20-scarpenter.conf`, which resets
`ExecStart` and spells out the full command line (the three paths above plus
`--provider-id`, `--hostname-override`, `--register-with-taints` and, with the
tailscale template, `--node-ip`), so the unit does not depend on the variables of
the kubeadm drop-in. It also resets `ExecStartPre`, `EnvironmentFile` and
`ConditionPathExists` (the kubelet unit of Azure Container Linux sets its own) and
writes the sysext's `ExecStartPre` steps back verbatim (the CNI plugins into
`/opt/cni/bin`, the version file, the volume plugin directory). `Upholds=` starts the kubelet; nothing enables it.
The `tailscale` sysext likewise ships `tailscaled.service`, started by `Upholds=`.

## NVIDIA GPU nodes

A launch of a supported NVIDIA type ([GPU types](labels-and-offerings.md#gpu-types)) adds the
`nvidia` part (`pkg/bootstrap/templates/nvidia.bu`); the catalog's driver table
(`pkg/nvidia`) chooses the driver of the type, and the NodeClass's Flatcar release
decides which builds exist. The part is not rendered for any other node, and
nothing is compiled on a node: the driver is Flatcar's prebuilt sysext of the
release (its modules are signed with the release's kernel key). The part holds:

- `/etc/flatcar/enabled-sysext.conf` with the driver sysext (`nvidia-drivers-<branch>[-open]`):
  the initrd downloads `<server>/amd64-usr/<version>/flatcar-<name>.gz` from Flatcar's release
  server, a signed payload that it verifies against the OS's update key, unless the image is
  already in `/etc/flatcar/sysext/`. A failed download fails the boot.
- With `--nvidia-driver-base-url` (a mirror), Ignition writes the extracted image to
  `/etc/flatcar/sysext/flatcar-<name>-<version>.raw` from
  `<base>/amd64-usr/<version>/flatcar-<name>.raw.gz` (`compression: gzip`, pinned by the sha256 of
  the extracted image, which Ignition checks over the decompressed bytes), so the initrd downloads
  nothing and there is no kernel argument and no extra reboot. `hack/nvidia-mirror.sh <version> <dir>`
  builds that tree (network and docker; it checks every payload against Flatcar's published sha256
  and its signature, and prints the table's pins). The mirror URL is not part of the drift hash.
- The `nvidia-runtime` sysext of the sysext-bakery (NVIDIA container toolkit and a containerd
  config), pinned by sha256 like the kubernetes sysext (`--sysext-base-url` mirrors it); the version
  and digest are the table's, and it replaces nothing of the NodeClass's `status.resolved`.
- containerd with `nvidia` as a runtime handler only, `runc` stays the default: the runtime
  sysext's config makes `nvidia` the default, so `scarpenter-nvidia-containerd.service`
  (before `containerd.service`, after tmpfiles has copied the file) sets
  `default_runtime_name = "runc"` and fails unless that line is there afterwards. A Pod gets GPUs
  only with `runtimeClassName: nvidia`.
  The runtime sysext's drop-in starts containerd with `--config /etc/containerd/config-nvidia.toml`,
  so on a GPU node a user-supplied `/etc/containerd/config.toml` is ignored (it is not read at all).
  Adding containerd settings on GPU nodes is not supported yet: the config has no drop-in directory
  that SCarpenter uses, and a file of your own at the `config-nvidia.toml` path would have to be a
  full copy of the sysext's file, which the unit above then edits and checks.
- `/etc/modprobe.d/00-scarpenter-nvidia.conf`: `nouveau` and `nova_core` blacklisted, `nvidia-drm`
  without modeset and never loaded (a headless node needs no DRM), no dynamic power management (the
  name sorts before the driver sysext's own `10-flatcar-nvidia-drivers-*.conf`, because modprobe uses
  the first `install` line of a module);
  `scarpenter-nvidia-prepare.service` unloads `nouveau` before `nvidia.service` when the initrd
  loaded it; `Upholds=nvidia-persistenced.service` on `multi-user.target` (persistence mode).
- `scarpenter-nvidia-check.service`, enabled on its own: the kubelet does not wait for it or for the driver
  ([ADR-031](../explanation/DECISIONS.md#adr-031-gpu-nodes-register-before-the-check-and-fabric-manager-work-are-done-accepted-2026-11-22)),
  so the check, Fabric Manager and a later on-node build do not count against Karpenter's 15 minute registration
  timeout. The driver sysext download does: it stays in the initrd (or in Ignition with a mirror), see the first two items. The node registers with the taint `scarpenter.sparecores.com/gpu-pending:NoSchedule`
  (`--register-with-taints`, added to the NodeClaim's taints on every launch of a NVIDIA type). The check:
  the loaded driver (`/proc/driver/nvidia/version`) must be the version of the type's `nvidia-driver-version`
  label and `nvidia-smi -L` must list every GPU of the type (retried for 30 s). On success it labels the Node
  `scarpenter.sparecores.com/gpu-ready=true` with `kubectl` and the kubelet's credentials (retried for 5 minutes
  until the kubelet has registered the Node), and the controller removes the taint. A node cannot remove
  its own taint (NodeRestriction), hence the label. A node that fails keeps the taint, so nothing schedules
  there: the check labels the Node `gpu-ready=false`, and the controller deletes its NodeClaim at once (and when the taint is
  still there 30 minutes after the Node's creation), with an Event `GPUCheckFailed`; the unit's journal line says why.
  **A NodePool that can launch GPU types (a mixed CPU and GPU pool too) must list the taint in `spec.template.spec.startupTaints`**, because Karpenter
  takes startup taints only from the NodePool: with it the scheduler plans pods onto the node and `Initialized` waits for the
  taint's removal; the controller clears it at once from the pool's nodes that need no check (no `nvidia-driver-branch` label), because core puts a startup taint on every node of the pool; without it the scheduler sees a permanent taint and may launch extra nodes for the same pods, so the NVIDIA types are not
  offered to a NodePool that lacks it. Adding it changes the NodePool's hash, so its nodes drift and are replaced.
  `Ready` is the kubelet's and does not depend on the driver. `Initialized` also needs `nvidia.com/gpu`
  registered by the device plugin. DaemonSets that the node needs for Ready (the CNI) must tolerate `NoSchedule` taints.

- **NVSwitch systems** (8 or more GPUs of A100, H100 and H200 HGX models) additionally get Fabric
  Manager, which no Flatcar sysext ships and without which CUDA does not initialize on them. NVIDIA's
  Fabric Manager archive of the driver's exact version (the daemon aborts on a mismatch) is pinned by
  sha256 in the table; `scarpenter-nvidia-fabricmanager.service` downloads it after the boot (so the download does
  not count against the registration timeout) to `/opt/nvidia-fabricmanager/` from NVIDIA's driver
  redistributables (`developer.download.nvidia.com/.../redist/fabricmanager/linux-x86_64/`), or from
  `<--nvidia-driver-base-url>/fabricmanager/<driver version>/` with a mirror, and verifies the hash with `sha256sum -c` against a pin file that Ignition writes. The unit (after
  `nvidia.service`, started by the check unit that wants it) unpacks the archive at every start, sets its
  `fabricmanager.cfg` to run in the foreground, to log to syslog (the journal) and to take the topology
  and database files from the archive, and runs `nv-fabricmanager -c` (`Restart=on-failure`). The check
  unit then also waits (10 minutes) for Fabric Manager's success line (`Successfully configured all the
  available ...`, which names NVSwitches on A100 and GPUs and NVSwitches on H100; read from the journal
  of the unit's current run), so the device plugin never advertises GPUs that cannot initialize. `hack/nvidia-mirror.sh <version> <dir> fabricmanager-<driver version>`
  copies an archive into the mirror tree and prints its pin. Smaller slices of the same hardware (four
  GPUs and fewer) and 8-GPU V100 systems (NVLink without NVSwitch) get none of this. B200 and B300 (NVLink5)
  are not offered: NVIDIA's start script also runs the NVLink subnet manager (`nvlsm`) there, which needs
  the `ib_umad` module and `ibstat`, and Flatcar has neither.

The part is about 2 KiB of the user-data, and Fabric Manager adds about 0.6 KiB (the worst case of a GPU
node is 14053 of the 15360 bytes of the budget). Its largest variant (the mirror, 16 GPUs, Fabric Manager)
is part of the NodeClass controller's trial rendering for amd64, and a unit test holds the worst case of a
GPU node (everything else on) to 15 KiB, which leaves a user's Butane at least 1 KiB.

The chart ships the device plugin and, on request (`nvidia.runtimeClass.create`), the RuntimeClass `nvidia`, whose handler is the runtime this part
registers ([run GPU workloads](../how-to-guides/run-gpu-workloads.md)). The part is rendered for every launch of a
supported NVIDIA type: the catalog advertises `nvidia.com/gpu` and the driver labels (`nvidia.NodeBootstrap`,
a constant of `pkg/nvidia`), and the cloud provider rebuilds the selection from them.

## Template variables

The variables of `.NodeName` etc. are the fields of `bootstrap.TemplateData`; a
template that uses a variable that does not exist fails (strict). Functions: `q`
(quote as a YAML string), `indent N` (pad every line, for block scalars), `b64`
(base64).

| Variable | Meaning |
|---|---|
| `.NodeName` | the node name, the NodeClaim's name |
| `.ProviderID` | `scarpenter://<vendor>/<region>/<nodeclaim>` |
| `.APIServerEndpoint` | `--api-server-endpoint` |
| `.ClusterCA` | the PEM bundle of the cluster CA |
| `.BootstrapToken` | `<token-id>.<token-secret>` |
| `.Arch` | `amd64` or `arm64` |
| `.Vendor`, `.Region` | the offering the node is launched into |
| `.Zone`, `.InstanceType` | the offering of the launch attempt (the user-data is rendered per attempt, since plan 06); empty in the trial rendering of the NodeClass controller |
| `.Labels` | the NodeClaim's labels (a map), for templates that need them; not passed to the kubelet |
| `.KubeletConfig` | the `KubeletConfiguration` YAML |
| `.KubeletArgs` | `--provider-id`, `--hostname-override` and `--register-with-taints` |
| `.Sysexts` | the kubernetes sysext and `spec.sysexts` of the architecture: `.Name`, `.URL`, `.SHA256`, `.Path`, `.Link` |
| `.Tailscale.Version`, `.LoginServer`, `.AuthKey`, `.Tags` (comma separated), `.AcceptRoutes`, `.Sysext` | the tailscale settings; set with the `tailscale` template only |
| `.NVIDIA.Version`, `.Count`, `.Sysext`, `.Runtime` (a sysext), `.Driver` (`.Path`, `.URL`, `.SHA256`; nil without a mirror), `.CheckScript` | the NVIDIA driver of the node; `.NVIDIA` is nil (and the `nvidia` part not rendered) on a node without a GPU driver |
| `.NodeIPCIDR` | the IPv4 range of the private network the node joins, from the driver; empty without one (the `direct` template turns it into `--node-ip`, see [network templates](#network-templates)) |
| `.Lean` | whether `spec.kubelet.footprint` is `Lean` |
| `.PoweroffAfter`, `.PoweroffAfterSeconds` | `spec.poweroffAfter` as a duration string and in seconds (0: not set) |

## The kubelet

The kubelet configuration is generated, not templated by users (override it with
extra Butane, see below). Fixed: `serverTLSBootstrap: true` and
`rotateCertificates: true`, `cgroupDriver: systemd`, webhook authentication and
authorization with `/etc/kubernetes/pki/ca.crt` as the client CA (or, with
`--kubelet-client-ca-file`, the CA written to `/etc/kubernetes/pki/kubelet-client-ca.crt`:
the API server's client certificate for `kubectl logs` and `exec` must verify
against it) and anonymous access off, `resolvConf: /run/systemd/resolve/resolv.conf` (the stub resolver
address does not work inside Pods). On a spot launch also `shutdownGracePeriod: 25s` and `shutdownGracePeriodCriticalPods: 5s`
([graceful node shutdown](#the-interruption-agent)). From the NodeClass: `maxPods` (default 110),
`systemReserved`, `kubeReserved`, `evictionHard` (unset keeps the kubelet's
defaults; quantities are validated); with `footprint: Lean` the defaults of these
and a few more settings are the [preset's](node-settings.md#memory-footprint). From the controller: `clusterDomain`
(`--cluster-domain`, default `cluster.local`) and `clusterDNS` (`--cluster-dns`; when it is not set the controller reads the cluster DNS Service itself, and `--cluster-dns=none` leaves it unset: Pods then use the node's resolver).

The catalog assumes the AWS provider's kube-reserved formula for scheduling
(which depends on the instance's vCPUs and `maxPods`). Since plan 06 the user-data
is rendered per launch attempt and its `kubeReserved` is exactly the reservation
the catalog computed for the chosen instance type (the formula, with the keys of
`spec.kubelet.kubeReserved` replacing single resources), so the node's allocatable
matches the scheduler's. The trial rendering of the NodeClass controller has no
instance type and keeps `spec.kubelet.kubeReserved` only.

## Network templates

[Choose a node network template](../how-to-guides/choose-a-network-template.md)
compares them. What each renders:

- **`tailscale`**: the `tailscale` sysext (pinned, sha256) and the auth key in
  `/etc/scarpenter/tailscale-authkey` (mode 0600). The unit
  `scarpenter-tailscale-up.service` (oneshot, before the kubelet) runs
  `tailscale up --auth-key=file:... --hostname=<nodeclaim> --accept-dns=false`
  plus `--login-server` (Headscale), `--accept-routes` (`acceptRoutes: true`,
  so the node uses the subnet routes advertised into the tailnet) and `--advertise-tags`, plus the flags a vendor's fragment sets in `SCARPENTER_TAILSCALE_UP_ARGS` (Alibaba Cloud: `--netfilter-mode=nodivert`), then
  `/opt/scarpenter/bin/wait-for-tailscale-ip` waits up to 120 s for the node's
  tailscale IPv4 and writes `/run/scarpenter/node-ip.env`
  (`SCARPENTER_NODE_IP_ARGS=--node-ip=<ip>`), which the kubelet drop-in reads. A
  second drop-in (`30-tailscale.conf`) makes the kubelet wait for the unit. Nodes
  need only outbound connectivity.
- **`direct`**: a marker file; the kubelet uses the node's primary address (no
  `--node-ip`), unless the driver reports a private network range for the launch
  (`Prepared.NodeIPCIDR`, see below).
- **`custom`**: your Butane template (below).

### Per-node tailnet keys

`network.tailscale.keySource` is `Static` (the default when unset: every node
joins with the key of `authKeySecretRef`) or `PerNode`: the controller mints one
key per node when it launches it. A per-node key is single use, pre-authorised,
tagged with the NodeClass's `tags`, **ephemeral** (always, there is no field) and
expires after `--tailnet-key-ttl` (1 h). It is in that node's user-data only, is
revoked after the node registered, and the node's tailnet device is deleted with
its NodeClaim. A key read after the node joined is worthless; a key read before it
joined admits one device with the NodeClass's tags for at most the expiry.

The rules of a `PerNode` NodeClass:

- `authKeySecretRef` must be absent (it is required with `Static`), and `tags`
  must not be empty: Tailscale requires tags on keys an OAuth client mints, and
  tagged devices have no node key expiry.
- The backend, its URL and its credentials belong to the **controller**, never to
  a NodeClass (`--tailnet-backend`, `--tailnet-api-url`,
  `--tailnet-credentials-dir`, [controller reference](controller.md)): a NodeClass
  editor must not be able to send the controller's credentials to a URL of their
  choosing. The NodeClass is `ValidationSucceeded=False`
  (`TailnetKeysNotConfigured`) when the controller has no backend (`none`) or no
  credentials directory, or when `loginServer` does not match the backend: empty
  for `tailscale`, equal to `--tailnet-api-url` for `headscale` (scheme, host,
  port and path; case of scheme and host, a default port and a trailing slash are
  ignored; a URL with user information, a query, a fragment or a host that is not
  ASCII never matches). Validation calls no API; bad credentials
  show at the first launch.
- Headscale 0.28 or newer only. A Headscale API key is not scoped: it can
  administer the whole server, so it lives in one Secret that is mounted into the
  controller only. A Tailscale OAuth client needs the scopes `auth_keys` (write)
  and `devices:core` (write) and the NodeClass's tags.
- Switching a NodeClass from `Static` to `PerNode` drifts its nodes once; writing
  `keySource: Static` changes nothing. The trial rendering uses a 128-character
  placeholder key, so the key needs no room beyond the [size budget](#size-budget)
  as it is.

A `PerNode` NodeClass validates, but the launch path does not mint keys yet:
`Create` refuses it with `NodeClassNotReady` until it does.

### The node IP on a private network

A node whose private network is a second interface next to the public one
(Hetzner private networks, UpCloud SDN) has its default route on the
public interface, so a kubelet without `--node-ip` registers the public address
and node-to-node and control-plane traffic leaves the private network. For
these, the driver puts the IPv4 range of the network the launch attaches into
`Prepared.NodeIPCIDR`, provided the network gives the interface its address by
DHCP (Flatcar runs DHCP on every interface; nothing else configures it) ([driver contract](driver-contract.md)); the core passes it
to the renderer as a hint of the launch, "the node IP is the node's local address
inside this CIDR". It derives from the resolved resources (brought, discovered or
created network), never from the spec, so it does not change the NodeClass hash:
nodes launched before keep what they have, nodes launched after get the unit.

With the `direct` template and a range, the user-data adds:

- `/opt/scarpenter/bin/scarpenter-node-ip` (`templates/scarpenter-node-ip.sh`), which waits
  up to 120 s for a global IPv4 address of the node inside the range (DHCP of the
  private interface can take a moment after `network-online.target`) and writes
  `/run/scarpenter/node-ip.env` (`SCARPENTER_NODE_IP_ARGS=--node-ip=<ip>`), the
  same file the `tailscale` template writes. If several addresses are inside the range the
  first one `ip` lists wins.
- `scarpenter-node-ip.service` (oneshot, `Before=kubelet.service`, started with
  `TimeoutStartSec=150`) and the kubelet drop-in `30-node-ip.conf` (`Requires=` and
  `After=` the unit). **If no address shows up the unit fails and the kubelet does
  not start** (the journal says `no IPv4 address inside <range>` and lists the
  addresses), so the node never registers with the wrong IP; Karpenter's
  registration timeout then replaces it.

It costs about 1.7 KiB of user-data (the script and the unit; the `direct` default
is 3.2 KiB without them, 4.9 KiB with). Only some vendors use it, but `Validate`
sizes its trial rendering with a range, so that a `direct` NodeClass close to the
[budget](#size-budget) is rejected at validation and not when a node launches on
such a vendor. IPv6 ranges are not
handled: the range must be an IPv4 prefix, else the rendering fails. The
`tailscale` template ignores the range (the tailnet address is the node IP), and a
`custom` template can read it as `.NodeIPCIDR` (empty without one).

Vendors whose instance has one interface with a private primary address and a
NAT'ed public one (AWS, GCP, Azure, OCI, Alibaba Cloud) need no hint: the kubelet
already registers the private address that the subnet gave the node, and they
supply none. OVH does not support private networks in v1. A Vultr VPC has no DHCP; a boot unit gives its interface the address from the metadata service, and the driver supplies no range;
an UpCloud network brought without DHCP gets none either. Their nodes
register the public address.

## Instance metadata access

The vendor's instance metadata service serves the node's user-data, which holds the one-hour bootstrap
token and, with the `tailscale` template, the Tailscale auth key, and on some vendors credentials of the
instance. A Pod that can reach it can read them. `spec.instanceMetadata.access` decides who can:

```yaml
spec:
  instanceMetadata:
    access: Restricted   # the default when instanceMetadata is unset; HostOnly or Open open it further
```

| `access` | Host processes | hostNetwork Pods | Pods in the Pod network |
|---|---|---|---|
| `Restricted` (default) | only the listed node components | dropped | dropped |
| `HostOnly` | allowed | allowed | dropped |
| `Open` | allowed | allowed | allowed |

With **`Restricted`** only the listed node components reach the metadata addresses; every other host process
and every Pod, in the Pod network and in the host network, is dropped. The metadata service stays on at the
cloud: Ignition reads the user-data in the initramfs, before the node's own rules exist, and no vendor API
can switch the service off and on again without a window in which the node cannot boot or read its data.
**`HostOnly`** is the same fragment with the `OUTPUT` allowlist replaced by one rule per address that drops packets without a
local source address (`-m addrtype ! --src-type LOCAL`), next to the `FORWARD` drop: every host process and every hostNetwork Pod
reaches the addresses, and Pods in the Pod network do not. It needs no Pod CIDR and no CNI interface names: a Pod's packets
are routed through the host, where `FORWARD` sees them (kube-proxy's DNAT happens before it). kube-proxy in IPVS mode sends
the Pod traffic for a Service endpoint through `OUTPUT` instead, with the Pod's source address, which the `OUTPUT` rule drops.
Use it for components deployed with hostNetwork that need the metadata service (a vendor's CSI node plugin or cloud
controller) without opening it to the Pod network. In `HostOnly` host processes and
hostNetwork Pods can read the user-data, the bootstrap token and the Tailscale auth key included, as anything privileged on
the node can already. Keep hostNetwork out of other namespaces (Pod Security `baseline` forbids it) and use
single-use or ephemeral Tailscale keys.
**`Open`** adds nothing to the node's configuration and leaves the service reachable by everything, for a
NodeClass whose workloads need it, such as a benchmark that reads the instance's identity or a vendor's CSI
driver or cloud controller that runs as a Pod and takes its credentials from the metadata service. On AWS a consumer in the
Pod network also needs `vendors.aws.metadataOptions.httpPutResponseHopLimit: 2`: with the default hop limit of 1 the
IMDSv2 reply does not reach a Pod, in `Open` too. On AWS `HostOnly` is rejected together with `httpTokens: optional` or a hop
limit above 1, so that the second layer (IMDSv2, hop limit 1) is never overridden silently.

It applies to nodes launched after it exists. `instanceMetadata` leaves the
[drift hash](#the-nodeclass-controller) as it was while it is unset, so upgrading SCarpenter drifts no node:
running nodes keep the (open) metadata service they were launched with until the NodeClass sets
`access: Restricted`, `HostOnly` or `Open`; changing the mode of a NodeClass changes the hash and replaces its nodes like any other
change of the hash. A NodeClass
that sets the deprecated `spec.bootstrap.hardening` drifts no node either: the field has no effect, stays in the hash as
before, and the controller publishes a `DeprecatedHardening` Event for it ([ADR-025](../explanation/DECISIONS.md)).

### What is restricted

Each driver declares its vendor's metadata addresses and the units of its image that need them (the optional
`InstanceMetadata` capability, [driver contract](driver-contract.md#metadata-endpoints)); the core builds one
Butane fragment from that. Only TCP connections the node opens (the `SYN`) are dropped, and not to port 53:
several of these addresses also serve DNS and NTP (GCP, OCI, Azure's WireServer), which must keep working, and
replies to connections that come from them (Azure's load balancer health probes come from `168.63.129.16`) must
pass. UDP is not touched; no vendor serves its metadata over UDP. Addresses of other services, such as AWS's DNS
(`169.254.169.253`) and NTP (`169.254.169.123`), are not listed.

| Vendor | Addresses | Allowed units besides Afterburn's |
|---|---|---|
| `aws` | `169.254.169.254`, `fd00:ec2::254` | `amazon-ssm-agent.service` |
| `gcp` | `169.254.169.254`, `fd20:ce::254` | `oem-gce.service` |
| `azure` | `169.254.169.254`, `168.63.129.16` (the WireServer) | `waagent.service` |
| `oci` | `169.254.169.254`, `fd00:c1::a9fe:a9fe` | none |
| `alicloud` | `100.100.100.200` | none |
| `hcloud` | `169.254.169.254`, `fe80::a9fe:a9fe` | none |
| `upcloud` | `169.254.169.254` | none |
| `vultr` | `169.254.169.254` | `scarpenter-vpc-interface.service` |
| `ovh` | `169.254.169.254`, `fe80::a9fe:a9fe` | none |
| `qemu` | no metadata service, nothing is added | none |

Afterburn's `coreos-metadata.service` and `coreos-metadata-sshkeys@.service` are allowed on every vendor.
The unit names are those of the Flatcar release's OEM images (`oem-ami`, `oem-gce`, `oem-azure`). The IPv6
addresses of OCI and the link-local address of Hetzner and OpenStack are those Afterburn and cloud-init use;
if a vendor's service is reachable under an address that is not listed (Azure hands out the WireServer's
address by DHCP and `168.63.129.16` is its fixed value), it is not restricted. Alibaba's `100.100.0.0/16` holds
other platform services and is left alone.

### How it works

- The allowed units run in the systemd slice `scarpenter-metadata.slice` (a `Slice=` drop-in each, so their
  cgroup is below `scarpenter.slice/scarpenter-metadata.slice`). The slice stays active while it is empty.
- `scarpenter-metadata-firewall.service`, a oneshot ordered before `network-pre.target`, loads iptables rules
  (iptables-nft, the `security` table, for IPv4 and IPv6) from `/etc/scarpenter/metadata-iptables.rules` and
  `/etc/scarpenter/metadata-ip6tables.rules` with `/opt/scarpenter/bin/scarpenter-metadata-firewall`. In
  `OUTPUT`, a TCP connection to the addresses is dropped unless the socket's cgroup is below the slice
  (`-m cgroup --path`): that covers host processes, Pods in the host network and what the kubelet or containerd
  fetch for a Pod (an HTTP probe or an image at a metadata address). In `FORWARD`, which Pods in the Pod network
  use, it is dropped for everyone; the CNI's masquerade comes later, in `POSTROUTING`. The `security` table runs after `filter`: a drop in one base chain is final, and an accept in another
  (kube-proxy and the CNIs use `filter`) does not override it.
- The kubelet requires the unit, so a node whose rules did not load does not join. The allowed units want the
  unit and start after it.
- Ignition runs in the initramfs and is not affected. `iptables -m cgroup` is used because the `socket cgroupv2`
  expression of nftables needs `CONFIG_NFT_SOCKET`, which Flatcar's kernel does not build.
- The rules are never removed. Stopping the unit or the slice leaves them in place, and a slice that is stopped
  and started again is a new cgroup that the loaded rules no longer match, so even the allowed units would be
  dropped (fail closed). Stopping the slice stops the unit (and with it the kubelet); the next allowed unit that
  starts starts the unit again, which loads the rules for the new cgroup first, and `systemctl restart
  scarpenter-metadata.slice` does the same. Restarting the unit restarts the kubelet, and after a stop the
  kubelet needs `systemctl start kubelet`.

### On a node

```bash
systemctl status scarpenter-metadata.slice scarpenter-metadata-firewall.service
sudo iptables-nft -t security -vnL SCARPENTER-IMDS-OUTPUT     # the packet counters show what is dropped
sudo ip6tables-nft -t security -vnL SCARPENTER-IMDS-OUTPUT
sudo iptables-nft -t security -vnL SCARPENTER-IMDS-FORWARD
systemctl show -p Slice,ControlGroup <unit>                    # is a unit in the slice?
sudo systemd-run --slice=scarpenter-metadata.slice --pipe --wait curl -s http://<address>/   # allowed
```

A request to a metadata address that hangs and then times out comes from a process outside the slice.

### What it cannot stop

- Root on the host, and a privileged Pod with `NET_ADMIN` (it edits the rules) or `CAP_SYS_ADMIN` (it joins the
  allowed cgroup). Keep such Pods out of the cluster, or in a NodeClass with `Open` for what needs it.
- Anything in the allowed slice, such as the sessions of the SSM agent on AWS.
- A Pod in the host network with `NET_RAW` (in containerd's default capabilities), which can send packets below
  the IP layer past `OUTPUT`: drop the capability in the Pod's security context. A Pod in the Pod network with it
  still goes through `FORWARD`.
- A metadata service that is reachable under an address that is not in the table.
- A CNI that routes Pod traffic without netfilter (eBPF host routing: Cilium's BPF host routing, Calico's eBPF dataplane)
  does not pass `FORWARD`. In `Restricted` and `HostOnly` that is the rule keeping Pods away, so on AWS only the hop limit of 1 then protects them;
  add an egress policy of the CNI that denies the metadata addresses.
- Pods on secondary networks (macvlan, ipvlan, SR-IOV through Multus), and CNIs that bridge Pods at layer 2 onto the node network,
  bypass the host's routing, so `FORWARD` never sees their traffic.
- In `HostOnly`, a hostNetwork proxy (an egress gateway, an HTTP proxy) can relay a Pod's request to the metadata service.
- On OVH the config drive, enabled by default, holds a copy of the user-data as a block device that privileged hostPath Pods read in every mode.
- Components in the cluster that need the metadata service: a vendor's CSI node plugin or cloud controller
  deployed with hostNetwork belongs on a NodeClass with `access: HostOnly`; one in the Pod network, vendor workload
  identity or a benchmark tool that reads the instance's identity needs `access: Open`.

The fragment counts in the [size budget](#size-budget) (about 2 KiB of the user-data) and in the trial rendering of
the NodeClass controller. `spec.butane` and a custom template merge after it and can override it.

## The interruption agent

A **spot** launch whose driver implements [`InterruptionNotices`](driver-contract.md#interruption-notices-the-node-agent) also gets a small
node agent ([plan 26](../plans/v1/done/26-interruption.md) section 2, [ADR-027](../explanation/DECISIONS.md)). It is a shell script, rendered
into the user-data, so there is nothing to build or host. With the metadata restriction it is part of that fragment (`metadata-access`: one
part of the user-data, which saves a gzip header and a base64 wrapper); without it it is the fragment `interruption-agent`.

- `/opt/scarpenter/bin/scarpenter-interruption-agent`: the vendor's `scarpenter_check` function in a generic loop. It runs the check every
  interval; on a notice (exit 0) it reduces the printed reason and deadline to letters, digits and `:.+-`, waits for the kubelet's client
  certificate, and patches **its own Node** (`PATCH /api/v1/nodes/<name>`, JSON merge patch, `curl`) with the annotations
  `scarpenter.sparecores.com/interruption`, `interruption-deadline` and `interruption-source=instance-metadata`
  ([labels](labels.md#interruption-annotations)), retrying until it gets a 2xx. A refused or failing patch (the Node not registered yet, or a
  permanent 403) is logged once (`annotating the Node failed, retrying`) and retried with a backoff of up to 8 s. A restart first reads the Node:
  with the annotation present it does not patch again. It logs one line per state change to the journal (`journalctl -u scarpenter-interruption`).
  The interruption controller clamps the deadline to the vendor's notice window; the agent does not.
- `scarpenter-interruption.service`: `Slice=scarpenter-metadata.slice`, `Restart=always`, `Environment=` with `NODE_NAME` and `API_SERVER` (the
  template variables `NodeName` and `APIServerEndpoint`), started at boot after `network-online.target` (and after
  `scarpenter-metadata-firewall.service` when the metadata restriction is rendered), before the kubelet has a certificate: a vendor that needs an
  early first metadata call (Azure's opt-in) gets it. With `instanceMetadata.access: Open` the slice is created implicitly and the agent works the
  same.

**Credential.** The agent authenticates with the kubelet's client certificate (`/var/lib/kubelet/pki/kubelet-client-current.pem`, certificate and
key in one file) and the cluster CA. NodeRestriction limits that identity to its own Node and does not forbid annotations, so a node can only annotate
itself; a node that lies can only delete itself and mark its own type and zone unavailable for one TTL. L4 scenario Q16 checks both directions on
k3s: its own Node is patched (200), another Node is refused (403).

**Graceful node shutdown.** Spot launches also get `shutdownGracePeriod: 25s` and `shutdownGracePeriodCriticalPods: 5s` in the kubelet
configuration, so an ACPI shutdown without a usable notice (a missed one, GCP with no notice) still sends the Pods SIGTERM. Q17 checks on Flatcar
that the kubelet holds a delay lock of logind and that a Pod gets SIGTERM on `systemctl poweroff`. On-demand launches have neither.

**Size.** The agent adds about 1.2 KiB to the user-data of a spot node (the script with a 240 byte check is 1.3 KiB; it is compressed with the
rest of its part). So `SpotAgentBytes` (1.5 KiB) is added to the 12 KiB budget of the golden worst case for spot nodes (12.9 KiB direct, 12.6 KiB
tailscale, with a check as long as AWS's IMDSv2 one). The largest case, which AWS's p4d and p5 spot types can reach (16 GPUs with Fabric Manager,
the mirror, two volumes, 24 local disks, the restriction and the agent) is 16105 bytes with `direct` and 15753 bytes with `tailscale` of the 16384
limit with AWS's real check (the size tests render it), and a test holds it 256 bytes under it. That leaves 279 bytes: room for a check of about AWS's
length and little more. Two safety nets: a launch whose
user-data still exceeds the limit is refused for that offering alone (`InvalidOffering`, code `UserDataTooLarge`: the offering is marked
unavailable and an `OfferingRejected` Event carries the sizes; the type's on-demand offering and the other types launch), and the NodeClass's trial
rendering, which renders the agent without the NVIDIA part and the NVIDIA part without the agent as hard checks, also renders the combination
and reports a failure only as a Warning Event `UserDataTooLarge` on the NodeClass: the NodeClass stays valid.

Azure's check is the longest so far (the agent script is 1.5 KiB, at the limit; `TestNoticeCheckSize`). The Azure Spot worst case of the golden
parameters (the metadata restriction with two addresses and the agent) is 13 061 bytes with `direct` and 12 709 bytes with `tailscale`
(`TestWorstCaseAzureSpotUserDataStaysInTheBudget`), against Azure's custom data limit of 64 KiB (65 535 bytes) and the driver's budget of 16 KiB.

## Custom templates and extra Butane

`spec.network.custom.configMapRef` (template `custom`) and `spec.butane` each name
a ConfigMap key holding a Butane config. Both are Go templates with the variables
above (`{{` in a config that is not a placeholder must be written as
`{{ "{{" }}`), and both must be `variant: flatcar`, version `1.0.0` or `1.1.0`. A
custom template **replaces the network part only**; the base config stays. The
user config is merged last and can override anything. Sysexts you add must be
named like the `extension-release.<name>` file inside the image (the name is the
link `/etc/extensions/<name>.raw`) and may not be called `kubernetes`
(or `tailscale` with that template, or `nvidia-runtime` on a GPU node).

The ConfigMaps are read through the API server at render time and by the NodeClass
controller (about once a minute). Their content, and the resourceVersion of the
auth key Secret (never the key itself), are part of the
[drift hash](#the-nodeclass-controller): changing one drifts the nodes.

## Size budget

AWS accepts 16 KiB of user-data, the smallest limit of the vendors. A
rendering over 16 384 bytes fails with an error naming the compressed size of
each part, and the NodeClass reports it as `ValidationSucceeded=False`
(`TemplateInvalid`) before any launch. The default templates are held to 12 KiB
by golden tests (15 KiB for a [GPU node](#nvidia-gpu-nodes)), measured with a CA bundle of RSA-2048 size (1.1 KiB of PEM). A
plain rendering is 4.5 KiB with `tailscale` and 3.2 KiB with `direct`; with
everything on (the node IP unit, two volumes, 24 local disks with one mount each,
the Lean footprint, the metadata restriction) it is 11.0 KiB with `tailscale` and
11.3 KiB with `direct`, and a unit test holds that worst case to the 12 KiB. A spot
node of a vendor with a notice check adds the [interruption agent](#the-interruption-agent)
(about 1.2 KiB; 12.6 and 12.9 KiB in the worst case, held to 12 KiB plus 1.5 KiB).
The CA is the largest fixed part and
it does not compress (a 4096-bit CA adds about 0.7 KiB, a rotation bundle of two
CAs about 1 KiB). The NodeClass controller's trial rendering uses the real cluster
CA, a node IP range, and the largest storage setup of each vendor, so what
validates fits at launch.

Two measures keep the user-data small. Each part is gzip-compressed on its own, and
the scripts it installs are embedded as percent-encoded text without their comment
lines, because gzip compresses that to a third, while the base64 data URL Butane
prefers for a script compresses 30 % worse. Large payloads belong in sysexts,
referenced by URL and sha256, not in snippets. (Compression helps repetitive
content only: 20 KiB of random data does not fit.)

## The NodeClass controller

`pkg/controllers/nodeclass` reconciles every SCNodeClass (again after one minute,
after ten seconds while it is not ready). It replaces the stand-in of plan 04. It
writes the status with server-side apply under the field manager
`scarpenter-nodeclass`; the quota status controller writes its own fields under
`scarpenter-quota`, and neither removes the other's.

| Condition | `True` when | Reasons when not |
|---|---|---|
| `ArtifactsReady` | the sysexts of both architectures are resolved | `ReleaseNotFound`, `ResolutionFailed` |
| `ValidationSucceeded` | kubelet quantities parse; the Kubernetes version is within the skew; a `PerNode` NodeClass matches the controller's tailnet backend; the referenced ConfigMaps and the Secret exist; a trial rendering for both architectures succeeds within the budget | `InvalidKubeletSettings`, `KubernetesVersionNotSupported`, `TailnetKeysNotConfigured`, `ReferenceNotFound`, `ReferenceUnreadable`, `TemplateInvalid` |
| `CatalogReady` | the catalog covers the NodeClass and is not stale | the catalog's: `NoSnapshot`, `ScopePending`, `Stale`, `UnknownClass` |
| `Ready` | all three above | derived |
| `RestartRequired` | (set only when) metric labels were added after start-up | `MetricLabelsNotRegistered` |

Per vendor in `status.vendors.<vendor>`: `VendorReady` (the driver is enabled,
at least one named region can be used, the driver's `ValidateNodeClass`
accepts the block; reasons `VendorNotEnabled`, `UnknownRegions`, `NoUsableRegions`,
`ValidationFailed`; unusable regions among usable ones show in `RegionsUnavailable`), `ImagesReady` (the driver's `ImageReporter`, when it has one;
a driver without it resolves images at launch and counts as ready; an image that is not the pinned release
makes it `False` with `ImageVersionMismatch` or `ImageVersionUnknown`), `images`, and
`hash`. A vendor that is not ready does not make the NodeClass not ready; it only
makes its own offerings unavailable, and a `Create` fails with
`NodeClassNotReady` only when **every** vendor the NodeClaim allows is not ready.

The **drift hash** of a vendor is the SHA-256 over the canonical JSON of the
shared fields that change what a node is (`flatcar`, `kubernetes`, `network`,
`networking`, `kubelet`, `butane`, `sysexts`, `poweroffAfter`, `tags`), `instanceMetadata` (left out while unset, so the
hash of a NodeClass that does not set it is the one it had before the field existed), that vendor's block without `regions` and `resources`, the contents the spec points at
(custom template, Butane, the auth key Secret's resourceVersion), the resolved
artifact digests (not their URLs: switching to a mirror of the same release does
not drift nodes), and `HashVersion`. When an input cannot be loaded (a reference
that is missing or unreadable, a failing skew check) the previous hash is kept and
`ValidationSucceeded` says why: a transient failure never drifts the nodes. The
hash is published up to a minute after a ConfigMap changes, so a node launched in
that minute gets the new content with the previous hash and drifts once. Another vendor's block, regions, resource
references, metrics and selections do not change it.

Events (on the SCNodeClass, Warning, deduplicated for 10 minutes):
`ArtifactsNotResolved`, `ValidationFailed`, `VendorNotReady`, `RestartRequired`,
`MetricInvalid`.

## The serving certificate approver

`pkg/controllers/csr` approves a `kubernetes.io/kubelet-serving` request only
when all of these hold:

1. the requester is `system:node:<name>` in the group `system:nodes`;
2. `<name>` is a NodeClaim whose NodeClassRef is an SCNodeClass;
3. the request's subject is `CN=system:node:<name>`, `O=system:nodes`, with no
   e-mail or URI SANs, and its usages are digital signature, server auth (and
   key encipherment) only;
4. the Node `<name>` exists and its `providerID` is the NodeClaim's;
5. every DNS SAN is the node name or a hostname/DNS address of the Node, and
   every IP SAN is an IP address of the **instance** where the node's IPs are the
   vendor's: with the `direct` template, and a driver that reports the addresses of
   the NodeClaim's instance (`vendors.Instance.Addresses`, the AWS driver does),
   those are the only addresses a serving certificate may name, because the Node
   object is written by the node, which a leaked bootstrap token does not bind to
   the NodeClaim's machine. With the `tailscale` template the kubelet's node IP is
   the tailnet address, which the vendor does not know: the instance's addresses
   plus the Node's addresses inside the tailnet prefixes (`--tailnet-prefixes`,
   default Tailscale's `100.64.0.0/10` and `fd7a:115c:a1e0::/48`; set it for a
   Headscale with other prefixes) are allowed (verifying one tailnet IP against the
   tailnet needs the Tailscale API, a later hardening); for `custom`, a driver that reports none and a NodeClaim
   without an instance reference. An instance that is gone or terminated allows no
   address (with `direct`).

Requests of other signers, or from other requesters, or for nodes that are not
SCarpenter's, are **left alone** (other approvers may handle them). A request
that is ours but wrong (rules 3 and 4, or names the Node does not report once
five minutes passed) is **denied**. While the Node or its addresses are not there
yet, the request is looked at again every five seconds. Events:
`ServingCertificateApproved`, `ServingCertificateDenied`. Without this, `kubectl
logs` and `exec` fail on control planes that verify serving certificates (k3s).

## RBAC

`config/rbac` (kustomize; the Helm chart renders the same rules, generated from these files and checked against them by `make test-chart`; Karpenter core's permissions are in `config/rbac-core`):

- **The join** (`node-join.yaml`), three ClusterRoleBindings of Kubernetes' own
  roles: `system:node-bootstrapper` for the group `system:bootstrappers:scarpenter`;
  `system:certificates.k8s.io:certificatesigningrequests:nodeclient` for the same
  group (auto-approval of the client certificate request) and `...:selfnodeclient`
  for `system:nodes` (renewals).
- **SCarpenter** (`controller.yaml`): SCNodeClasses and their status; read access to
  NodeClaims, Nodes and certificate signing requests; `update` on
  `certificatesigningrequests/approval` and `approve` on the signer
  `kubernetes.io/kubelet-serving` only; `get` on ConfigMaps in every namespace
  (references name theirs); a Role in `kube-system` for the token Secrets (`get`,
  `list`, `create`, `delete`; RBAC cannot limit `get` and `list` by label, so the
  controller can read every Secret of `kube-system`); and a Role and RoleBinding
  that allow `get` on Secrets in the controller's own namespace, for the tailscale
  auth key. **There is no cluster-wide access to Secrets.** An auth key Secret in
  another namespace needs a Role (`get` on that Secret) and a RoleBinding to the
  controller's ServiceAccount there; the commands are in `config/rbac/controller.yaml`,
  and without them the NodeClass reports `ValidationSucceeded=False`
  (`ReferenceUnreadable`) naming the missing binding. Reads of Secrets and
  ConfigMaps go to the API server directly (no cluster-wide informer). The L3 lane
  runs with this RBAC (installed through the Helm chart). The chart can also create the
  Role and RoleBinding for an auth key Secret in another namespace
  (`rbac.authKeySecrets`).

## Security notes

User-data is readable by anything that can read the instance metadata and by
principals that may describe the instance's attributes. What limits the damage:

- The bootstrap token lives at most `--bootstrap-token-ttl` and is deleted when
  the NodeClaim registers. Be aware what it is: the token is not bound to a node
  name, and the built-in approver signs client certificates for any
  `system:node:<name>`. A holder of a leaked token can, within its lifetime, obtain
  credentials for **any node name**, including an existing node's (and with that
  read what the Node authorizer allows, such as the Secrets of that node's Pods,
  and change that Node's reported addresses). This is inherent to bootstrap tokens
  (ADR-003); the short lifetime, the deletion at registration, the IMDSv2 hop limit
  below and the check of the serving certificate's IP addresses are the controls:
  with the `direct` template against the instance's addresses (where the driver
  reports them, as the AWS driver does) instead of the Node's; with `tailscale`
  the Node's addresses count only inside the tailnet prefixes
  (`--tailnet-prefixes`) next to the instance's, so a leaked token cannot get a
  certificate for a VPC or any other non-tailnet address.
- The AWS driver requires IMDSv2 with a hop limit of 1, so Pods without host
  networking cannot reach the metadata service; every vendor's metadata service is, besides,
  [restricted to the node's own components](#instance-metadata-access) unless the NodeClass says `Open`.
- The tailscale auth key should be **ephemeral, pre-authorized and tagged**, with
  an ACL that limits the tag to what nodes need (the API server, other nodes). In
  v1 one reusable key per NodeClass is used; per-node keys are post-v1.
- Nodes hold no cloud credentials.
- The serving certificate approver's rules are strict on purpose (see above).
- Secret values are never logged; the user-data is not logged.
