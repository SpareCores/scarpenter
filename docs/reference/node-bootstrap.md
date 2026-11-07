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
  DaemonSets; no cloud-controller-manager is needed.
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

## The user-data

The user-data is a small Ignition config (version 3.4.0) whose
`ignition.config.merge` list holds up to three configs, each a gzip-compressed,
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
2. **network**: [`tailscale`, `direct` or `custom`](#network-templates).
3. **user**, when `spec.butane` is set.

Each part is a Butane config (`variant: flatcar`, version `1.1.0`; `1.0.0` is
accepted in custom and user configs, the experimental version is not), rendered
with Go `text/template` and translated with the Butane library in **strict mode**
(a warning is an error). Ignition's own merge combines the parts, so SCarpenter
does no YAML merging. The templates are embedded
(`pkg/bootstrap/templates/base.bu`, `tailscale.bu`, `direct.bu`).

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
the kubeadm drop-in. `Upholds=` starts the kubelet; nothing enables it.
The `tailscale` sysext likewise ships `tailscaled.service`, started by `Upholds=`.

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
| `.Tailscale.Version`, `.LoginServer`, `.AuthKey`, `.Tags` (comma separated), `.Sysext` | the tailscale settings; set with the `tailscale` template only |
| `.PoweroffAfter`, `.PoweroffAfterSeconds` | `spec.poweroffAfter` as a duration string and in seconds (0: not set) |

## The kubelet

The kubelet configuration is generated, not templated by users (override it with
extra Butane, see below). Fixed: `serverTLSBootstrap: true` and
`rotateCertificates: true`, `cgroupDriver: systemd`, webhook authentication and
authorization with `/etc/kubernetes/pki/ca.crt` as the client CA and anonymous
access off, `resolvConf: /run/systemd/resolve/resolv.conf` (the stub resolver
address does not work inside Pods). From the NodeClass: `maxPods` (default 110),
`systemReserved`, `kubeReserved`, `evictionHard` (unset keeps the kubelet's
defaults; quantities are validated). From the controller: `clusterDomain`
(`--cluster-domain`, default `cluster.local`) and `clusterDNS` (`--cluster-dns`,
unset by default: Pods then use the node's resolver).

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
  plus `--login-server` (Headscale) and `--advertise-tags`, then
  `/opt/scarpenter/bin/wait-for-tailscale-ip` waits up to 120 s for the node's
  tailscale IPv4 and writes `/run/scarpenter/node-ip.env`
  (`SCARPENTER_NODE_IP_ARGS=--node-ip=<ip>`), which the kubelet drop-in reads. A
  second drop-in (`30-tailscale.conf`) makes the kubelet wait for the unit. Nodes
  need only outbound connectivity.
- **`direct`**: nothing but a marker file; the kubelet uses the node's primary
  address (no `--node-ip`).
- **`custom`**: your Butane template (below).

## Custom templates and extra Butane

`spec.network.custom.configMapRef` (template `custom`) and `spec.butane` each name
a ConfigMap key holding a Butane config. Both are Go templates with the variables
above (`{{` in a config that is not a placeholder must be written as
`{{ "{{" }}`), and both must be `variant: flatcar`, version `1.0.0` or `1.1.0`. A
custom template **replaces the network part only**; the base config stays. The
user config is merged last and can override anything. Sysexts you add must be
named like the `extension-release.<name>` file inside the image (the name is the
link `/etc/extensions/<name>.raw`) and may not be called `kubernetes`
(or `tailscale` with that template).

The ConfigMaps are read through the API server at render time and by the NodeClass
controller (about once a minute). Their content, and the resourceVersion of the
auth key Secret (never the key itself), are part of the
[drift hash](#the-nodeclass-controller): changing one drifts the nodes.

## Size budget

AWS accepts 16 KiB of user-data, the smallest limit of the vendors. A
rendering over 16 384 bytes fails with an error naming the compressed size of
each part, and the NodeClass reports it as `ValidationSucceeded=False`
(`TemplateInvalid`) before any launch. The default templates are held to 12 KiB
by golden tests, measured with a CA bundle of RSA-2048 size (1.1 KiB of PEM): about
4.9 KiB with `tailscale`, 3.5 KiB with `direct`. The CA is the largest fixed part and
it does not compress (a 4096-bit CA adds about 0.7 KiB, a rotation bundle of two
CAs about 1 KiB); the NodeClass controller's trial rendering uses the real cluster
CA, so what validates fits at launch. Nodes with extra volumes or local storage get a fourth part, the
[first-boot volume unit](node-settings.md#extra-volumes-and-local-storage) (about
2.5 KiB plus 0.3 KiB per volume: eight volumes and four local disks on the
`tailscale` template measure about 11 KiB); the trial rendering of the NodeClass
controller does not include it, so a NodeClass whose custom snippets take most of
the budget can still fail at launch for a node with many volumes. Large
payloads belong in sysexts, referenced by URL and sha256, not in snippets.
(Compression helps repetitive content only: 20 KiB of random data does not fit.)

## The NodeClass controller

`pkg/controllers/nodeclass` reconciles every SCNodeClass (again after one minute,
after ten seconds while it is not ready). It replaces the stand-in of plan 04. It
writes the status with server-side apply under the field manager
`scarpenter-nodeclass`; the quota status controller writes its own fields under
`scarpenter-quota`, and neither removes the other's.

| Condition | `True` when | Reasons when not |
|---|---|---|
| `ArtifactsReady` | the sysexts of both architectures are resolved | `ReleaseNotFound`, `ResolutionFailed` |
| `ValidationSucceeded` | kubelet quantities parse; the Kubernetes version is within the skew; the referenced ConfigMaps and the Secret exist; a trial rendering for both architectures succeeds within the budget | `InvalidKubeletSettings`, `KubernetesVersionNotSupported`, `ReferenceNotFound`, `ReferenceUnreadable`, `TemplateInvalid` |
| `CatalogReady` | the catalog covers the NodeClass and is not stale | the catalog's: `NoSnapshot`, `ScopePending`, `Stale`, `UnknownClass` |
| `Ready` | all three above | derived |
| `RestartRequired` | (set only when) metric labels were added after start-up | `MetricLabelsNotRegistered` |

Per vendor in `status.vendors.<vendor>`: `VendorReady` (the driver is enabled,
the regions are ones the driver can use, the driver's `ValidateNodeClass`
accepts the block; reasons `VendorNotEnabled`, `UnknownRegions`,
`ValidationFailed`), `ImagesReady` (the driver's `ImageReporter`, when it has one;
a driver without it resolves images at launch and counts as ready), `images`, and
`hash`. A vendor that is not ready does not make the NodeClass not ready; it only
makes its own offerings unavailable, and a `Create` fails with
`NodeClassNotReady` only when **every** vendor the NodeClaim allows is not ready.

The **drift hash** of a vendor is the SHA-256 over the canonical JSON of the
shared fields that change what a node is (`flatcar`, `kubernetes`, `network`,
`networking`, `kubelet`, `butane`, `sysexts`, `poweroffAfter`, `tags`), that
vendor's block without `regions` and `resources`, the contents the spec points at
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
   the tailnet address (100.64.0.0/10), which the vendor does not know: the Node's
   reported addresses stand (verifying the tailnet IP needs the Tailscale API, a
   later hardening); so for `custom`, a driver that reports none and a NodeClaim
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

`config/rbac` (kustomize; the Helm chart of plan 09 templates the same):

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
  runs with this RBAC.

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
  below and the check of the serving certificate's IP addresses against the
  instance's addresses (where the driver reports them, as the AWS driver does)
  instead of the Node's are the controls.
- The AWS driver requires IMDSv2 with a hop limit of 1, so Pods without host
  networking cannot reach the metadata service.
- The tailscale auth key should be **ephemeral, pre-authorized and tagged**, with
  an ACL that limits the tag to what nodes need (the API server, other nodes). In
  v1 one reusable key per NodeClass is used; per-node keys are post-v1.
- Nodes hold no cloud credentials.
- The serving certificate approver's rules are strict on purpose (see above).
- Secret values are never logged; the user-data is not logged.
