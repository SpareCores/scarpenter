# Install SCarpenter

SCarpenter installs with one Helm chart, `oci://ghcr.io/sparecores/charts/scarpenter`
(and a second, `scarpenter-crd`, that keeps the CRDs up to date: see [Upgrade](#upgrade)).
This guide lists what the cluster needs first, the install, the values that
matter, upgrades, and what an uninstall leaves behind. The values and flags
one by one are in the [controller reference](../reference/controller.md#helm-values).

- [What you need](#what-you-need)
- [Install](#install)
- [A cluster that already runs Karpenter](#a-cluster-that-already-runs-karpenter)
- [k3s: the kubelet client CA](#k3s-the-kubelet-client-ca)
- [Where the controller runs](#where-the-controller-runs)
- [Credentials for the vendors](#credentials-for-the-vendors)
- [First NodeClass and NodePool](#first-nodeclass-and-nodepool)
- [Upgrade](#upgrade)
- [Uninstall](#uninstall)
- [Troubleshooting](#troubleshooting)

## What you need

**A Kubernetes cluster** (1.31 or newer: the SCNodeClass CRD uses CEL's CIDR functions, which older API servers reject; the chart refuses to install below it; the kubelets on the nodes may be up to
three minors older than the API server and never newer) whose control plane is not
managed by SCarpenter. Its API server must

- accept **bootstrap tokens** (`--enable-bootstrap-token-auth`; k3s has it by
  default, kubeadm too) and run the `tokencleaner` controller of the controller
  manager, which removes expired tokens. SCarpenter gives every node a token, and
  the node's kubelet joins with it ([how a node joins](../reference/node-bootstrap.md#how-a-node-joins));
- be **reachable from the nodes** at an `https://` address, which you pass as
  `cluster.apiServerEndpoint`. The nodes sit in the vendor's network (for example
  an AWS VPC), not in your cluster: `https://kubernetes.default.svc` is no
  address for them. With the `tailscale` template this is the API server's tailnet
  (or public) address; with `direct`, an address routable from the vendor network.
  [Choose a network template](choose-a-network-template.md) explains the options.

**A CNI and kube-proxy (or a replacement) that run on remote nodes** as
DaemonSets, and no cloud-controller-manager (there is none for the nodes
SCarpenter creates). The DaemonSets must reach the API server by its address
(`KUBERNETES_SERVICE_HOST` set to the endpoint), because the Service ClusterIP
needs the proxy that is not running yet. The L4 lane runs this setup with k3s,
flannel and kube-proxy; the flags of a k3s server are in
[node bootstrap](../reference/node-bootstrap.md#cluster-prerequisites).

**Outbound access for the controller** (not for your nodes): HTTPS to the Keeper API
(`keeper.sparecores.net`: `/healthcheck`, the snapshot files of the catalog, the
vendors' storage and IPv4 prices, and `/benchmark_configs`; it needs an [API key](#keeper-api-key)), to the APIs of the vendors you enable (each provider's set-up guide lists them), and to the
hosts of the node artifacts (the Flatcar release server and the sysext-bakery
releases on GitHub; point `artifacts.sysextBaseURL` and `artifacts.flatcarBaseURL`
at mirrors if the controller has no such access). Nodes download the same
artifacts themselves; IPv6-only nodes need mirrors, because GitHub has no IPv6.

**An account at one or more providers**, set up as its guide describes (the Flatcar
image, credentials with the right permissions, the chart values): see
[Credentials for the vendors](#credentials-for-the-vendors).

**Helm** with OCI registry support (3.8 or newer; the chart and its tests are developed with Helm 4.3, and the templates use no Helm 4 feature, but Helm 3 is not tested) and permission to install CRDs and
cluster-scoped RBAC.

## Install

Install into its own namespace. Two values are required: the name of the cluster
(a tag on every vendor resource, and what the IAM policy is scoped to) and the
API endpoint the nodes use.

```bash
helm install scarpenter oci://ghcr.io/sparecores/charts/scarpenter \
  --version 0.1.0 --namespace scarpenter-system --create-namespace \
  --set cluster.name=my-cluster \
  --set cluster.apiServerEndpoint=https://api.example.com:6443 \
  --set vendors.aws.credentials.webIdentity.roleArn=arn:aws:iam::123456789012:role/scarpenter-my-cluster
```

That is the one-step install: the chart's `crds/` directory creates the CRDs, and
**Helm never upgrades them afterwards**. If you want Helm (or Argo CD, or an
infrastructure tool) to keep the CRDs current, install the CRD chart first and skip
the CRDs of the main chart; the same two commands upgrade both later (`scarpenter-crd` exists from 0.3.0, with the
version of the `scarpenter` chart):

```bash
helm upgrade --install scarpenter-crd oci://ghcr.io/sparecores/charts/scarpenter-crd \
  --version 0.3.0 --namespace scarpenter-system --create-namespace
helm upgrade --install scarpenter oci://ghcr.io/sparecores/charts/scarpenter --skip-crds \
  --version 0.3.0 --namespace scarpenter-system ...   # the values of the install above
```

The images and the charts are signed with cosign (keyless); the release notes of
every version show the `cosign verify` commands and attach the SBOMs.

Check the controller:

```bash
kubectl -n scarpenter-system rollout status deployment/scarpenter
kubectl -n scarpenter-system logs deployment/scarpenter
```

It becomes ready when its caches have synced and the CRDs exist. A value that is
wrong (an unparsable duration, a token lifetime under 20 minutes, an HTTP endpoint)
stops the chart at `helm install` with the reason; the controller itself also
refuses to start on any problem of its command line and logs all of them.

### What the chart installs

- **CRDs**, in the chart's `crds/` directory: `SCNodeClass`, and Karpenter core's
  `NodePool` and `NodeClaim` (the version of Karpenter core the controller links,
  see the release notes; `NodeOverlay` and `CapacityBuffer`, behind core feature
  gates that are off, are not shipped). Helm installs them first, **skips CRDs
  that exist** and **never upgrades or deletes them** ([upgrade](#upgrade);
  [a cluster that already runs Karpenter](#a-cluster-that-already-runs-karpenter)).
  `helm install --skip-crds` leaves them out. The chart `scarpenter-crd` has the
  same files as templates, which `helm upgrade` does update (version always equal to
  the `scarpenter` chart; `karpenterCore=false` leaves out Karpenter core's two).
- A **Deployment** with one replica (leader election on, so a second replica is a
  warm standby; set `replicaCount: 2` for it), a read-only root file system, no
  privilege escalation, a non-root user and the `RuntimeDefault` seccomp profile;
  a Service for the metrics, an optional `ServiceMonitor`
  (`serviceMonitor.enabled`) and, with more than one replica, a PodDisruptionBudget.
- **RBAC**, generated from `config/rbac` and `config/rbac-core` of the repository
  (the chart's tests fail when they differ): the permissions of SCarpenter and
  of Karpenter core; a Role in `kube-system` for the bootstrap token Secrets; a
  Role in the release namespace to read the tailscale auth key Secret; the lease
  Role; and the three ClusterRoleBindings that let nodes join. **No ClusterRole
  grants access to Secrets.** What each rule is for is in
  [node bootstrap](../reference/node-bootstrap.md#rbac) and the
  [controller reference](../reference/controller.md#permissions).
- Optionally (`examples.enabled`), an example `SCNodeClass` and `NodePool` per
  enabled vendor.

### Values that matter

| Value | Why |
|---|---|
| `cluster.name`, `cluster.apiServerEndpoint` | Required. |
| `vendors.<vendor>.enabled`, `vendors.<vendor>.credentials.*` | Which drivers run and how each authenticates: the [provider guides](#credentials-for-the-vendors). AWS is on by default; set `vendors.aws.enabled=false` if you do not use it. |
| `cluster.dns` | The kubelet's `clusterDNS` on nodes. Empty (default): the controller reads the DNS Service of `kube-system` itself (label `k8s-app=kube-dns`, the Service named `kube-dns` first; also with `helm template` and ArgoCD). If it finds none, SCNodeClasses are not valid (`ValidationSucceeded=False`, reason `ClusterDNSUnknown`, naming this value): set the IPs, or `[none]` to give the nodes no cluster DNS (their Pods then use the node's resolver). |
| `cluster.domain` | The kubelet's `clusterDomain` (default `cluster.local`). |
| `cluster.ca.*` | A CA bundle for the nodes other than the `kube-root-ca.crt` ConfigMap of the release namespace (which every namespace has, and which is the right one on kubeadm, k3s and EKS). |
| `kubeletClientCA.*` | [k3s](#k3s-the-kubelet-client-ca). |
| `keeper.url`, `keeper.apiKey.existingSecret` | A self-hosted Keeper, and the API key the public Keeper requires, from a Secret you create ([Keeper API key](#keeper-api-key)). |
| `bootstrap.tokenTTL` | Lifetime of a node's bootstrap token; at least 20m (core gives a NodeClaim 15 minutes to register). |
| `bootstrap.tailnetPrefixes` | For a Headscale with other prefixes than Tailscale's. |
| `rbac.authKeySecrets` | The tailscale auth key Secrets of other namespaces: the chart creates a Role that reads just that Secret and a RoleBinding there. The key Secret in the release namespace needs nothing. |
| `managedResources.*`, `gc.*`, `quota.*`, `unavailable.*`, `catalog.*` | The tuning of the controller; the defaults suit most clusters. |
| `resources` | The defaults (request 640Mi, limit 2Gi) are sized from the L3 scale and catalog refresh measurements: AWS with three regions peaks at about 310 MiB, with every region at about 430 MiB, and every region of every vendor at about 1.1 GiB of heap. Scope the NodeClasses to the regions you use, or raise the limit ([memory sizing](../reference/controller.md#memory-sizing)). |
| `extraArgs`, `extraEnv` | Core's other flags and anything else. |

Everything is in `values.yaml` with comments, and `values.schema.json` rejects
unknown keys and malformed values.

## A cluster that already runs Karpenter

Two Karpenters can share a cluster: core only acts on the NodePools and NodeClaims
whose `nodeClassRef` names a node class of its own provider. What you must do:

- **The CRDs are shared and stay the other Karpenter's.** `NodePool` and
  `NodeClaim` are the same CRDs for every provider. `helm install` skips them
  because they exist (and prints a notice), so SCarpenter's chart never changes
  them. They must be **at least the version of Karpenter core that SCarpenter
  links** (v1.15.1 for the 0.1 releases, see the release notes): an older schema
  prunes fields that SCarpenter's core writes. Upgrade them through the other
  Karpenter's own chart (`karpenter-crd`), install `scarpenter-crd` with
  `--set karpenterCore=false` (only the SCNodeClass CRD), and **never apply this chart's
  `karpenter.sh_*` files over them** (the [upgrade](#upgrade) step applies only the
  SCNodeClass CRD there). If you pass `--skip-crds` to be sure, it also leaves out
  the SCNodeClass CRD: apply `crds/scarpenter.sparecores.com_scnodeclasses.yaml` of
  the chart yourself first (see [upgrade](#upgrade)).
- **Keep the NodePools apart.** A pending pod that fits NodePools of both
  providers gets capacity from both. Separate them with taints or requirements
  (`scarpenter.sparecores.com/vendor`, a taint your workloads tolerate).
- **The leader election lease is separate** (`<release>-leader-election`, not
  core's `karpenter-leader-election`), so SCarpenter can run in the namespace of
  another Karpenter.
- **RBAC and webhooks do not overlap**: the chart's objects carry the release name,
  and SCarpenter runs no webhook.

## k3s: the kubelet client CA

The API server presents a client certificate when it calls a kubelet (`kubectl
logs`, `exec`, metrics). **kubeadm and EKS sign it with the cluster CA, which
nodes already trust: set nothing.** k3s signs it with its own client CA
(`<data-dir>/server/tls/client-ca.crt`, not the CA of `kube-root-ca.crt`), and a
controller running in a Pod cannot read that file on the k3s host. Without the
CA, a node joins and runs Pods, but `kubectl logs` and `exec` fail with 401. Give it
to the controller in one of three ways:

```yaml
# 1. From the API server's own configuration: the chart copies key client-ca-file of
#    kube-system/extension-apiserver-authentication (the API server's --client-ca-file:
#    the client CA on k3s) into a ConfigMap, when you run helm install or upgrade.
#    Re-run helm upgrade after the CA is rotated.
kubeletClientCA:
  source: extensionApiserverAuthentication
---
# 2. The PEM itself.
kubeletClientCA:
  source: inline
  inline: |
    -----BEGIN CERTIFICATE-----
    ...
---
# 3. A ConfigMap (or, with source: secret, a Secret) of the release namespace that you keep.
kubeletClientCA:
  source: configMap
  configMap: {name: k3s-client-ca, key: client-ca.crt}
```

Option 1 needs a live cluster (`helm install` or `helm upgrade`, not `helm template`;
the chart fails with the reason otherwise). The rest of what a k3s server needs
for remote workers (`--flannel-backend=none`, `--disable-kube-proxy`,
`--egress-selector-mode=disabled`, your own CNI and kube-proxy DaemonSets) is in
[node bootstrap](../reference/node-bootstrap.md#cluster-prerequisites).

## Where the controller runs

SCarpenter must never run on a node it manages: when core consolidates or expires
such a node, the controller that would provision the replacement goes with it. The
chart's default is a **preference** for nodes without the `karpenter.sh/nodepool`
label, which holds on every cluster that has any such node (control-plane nodes,
a managed node group, your own machines). To make it a rule, require it:

```yaml
affinity:
  nodeAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      nodeSelectorTerms:
        - matchExpressions:
            - {key: karpenter.sh/nodepool, operator: DoesNotExist}
# or pin it to a group of nodes you operate:
nodeSelector: {kubernetes.io/os: linux, node-role.kubernetes.io/control-plane: ""}
tolerations:
  - {key: node-role.kubernetes.io/control-plane, operator: Exists, effect: NoSchedule}
```

With `replicaCount: 2` the second replica is a hot standby (core's leader election,
the lease `<release>-leader-election` in the release namespace); add
`topologySpreadConstraints` to put them on different nodes.

## Credentials for the vendors

Each provider has a set-up guide: the Flatcar image the driver boots, the
credentials and their permissions, the chart values that enable the driver, a first
NodeClass and NodePool, and the checks. Enable only the drivers you use (AWS is on by
default).

| Provider | Guide |
|---|---|
| Amazon Web Services | [Set up AWS](set-up-aws.md) |
| Oracle Cloud Infrastructure | [Set up OCI](set-up-oci.md) |
| OVHcloud Public Cloud | [Set up OVHcloud](set-up-ovh.md) |
| Google Cloud | [Set up Google Cloud](set-up-gcp.md) |
| Microsoft Azure | [Set up Azure](set-up-azure.md) |
| Alibaba Cloud | [Set up Alibaba Cloud](set-up-alicloud.md) |
| Hetzner Cloud | [Set up Hetzner Cloud](set-up-hcloud.md) |
| UpCloud | [Set up UpCloud](set-up-upcloud.md) |
| Vultr | [Set up Vultr](set-up-vultr.md) |

## Keeper API key

SCarpenter reads its catalog (instance types, regions, zones, prices, benchmark
scores) from Keeper's snapshot files. The public Keeper
(`https://keeper.sparecores.net`) requires an API key for them: without one the
catalog stays empty, SCNodeClasses report `CatalogReady=False` with reason
`KeeperAPIKeyRequired`, `KeeperAuthorized=False` (`APIKeyMissing`, or
`APIKeyRejected` when a key was sent and refused) and the controller logs
`Keeper needs an API key`.

1. Request a key: <https://docs.sparecores.com/api/#authentication>.
2. Put it in a Secret and point the chart at it (`key` defaults to `api-key`):

   ```bash
   kubectl -n scarpenter-system create secret generic keeper-api-key \
     --from-file=api-key=<file with the key>
   ```

   ```yaml
   keeper:
     apiKey:
       existingSecret: keeper-api-key
   ```

   Without the chart, pass `--keeper-api-key-file` (or
   `SCARPENTER_KEEPER_API_KEY_FILE`) with the path of a file holding the key.
3. Rotate it by updating the Secret: the file is read at every catalog refresh,
   no restart is needed. The key stays optional at startup.

Keeper is open source (<https://github.com/SpareCores/sc-keeper>) and can be
self-hosted: set `keeper.url` (`--keeper-url`) to it. A self-hosted Keeper may
need no key.

## First NodeClass and NodePool

Nothing launches without an `SCNodeClass` and a Karpenter `NodePool` that
references it. `--set examples.enabled=true` (with the versions and regions you
want: `examples.*`) installs one pair per enabled vendor among AWS, Alibaba Cloud,
OVHcloud and UpCloud as a starting point (for the others, use the guide's example); the
files in `config/samples` show the whole API ([API reference](../reference/api.md)). A
NodeClass pins the Flatcar and Kubernetes versions, so check that the Kubernetes
version is within the skew of your API server. It is ready when `kubectl get
scnodeclass` shows `Ready=True`; otherwise `kubectl describe` names the condition
([status conditions](../reference/controller.md#scnodeclass-status-conditions)).

## Upgrade

From 0.3.0 on, upgrade the **CRDs first, then the controller**: the controller of a new version
writes fields that the old CRD silently prunes.
Up to 0.2.0 there is no CRD chart: apply the CRD of the release as in
[Without Helm for the CRDs](#without-helm-for-the-crds) before upgrading the controller.

```bash
helm upgrade --install scarpenter-crd oci://ghcr.io/sparecores/charts/scarpenter-crd \
  --version 0.3.0 --namespace scarpenter-system
helm upgrade scarpenter oci://ghcr.io/sparecores/charts/scarpenter --skip-crds \
  --version 0.3.0 --namespace scarpenter-system -f your-values.yaml
```

Check `values.yaml` of the new version for values that were added (`--reuse-values`
keeps your old ones and misses new defaults; prefer `-f your-values.yaml`). The
controller keeps no state outside the cluster and the vendor, so a restart loses
nothing: instances are found again by their tags, resources are adopted by their
names and ownership markers. Nodes keep running while the controller is down;
nothing new launches and nothing is cleaned up.

A skipped CRD upgrade does not fail loudly: the API server prunes the fields it
does not know. That happened from 0.1.0 to 0.2.0, where `spec.kubelet.footprint` and
`spec.instanceMetadata` vanished from NodeClasses (check with `kubectl get crd
scnodeclasses.scarpenter.sparecores.com -o yaml | grep -c footprint`, which prints 0 on
a stale CRD).

### Moving CRDs installed by the main chart to `scarpenter-crd`

CRDs that the `crds/` directory of the `scarpenter` chart created belong to no Helm
release, so Helm refuses to take them into `scarpenter-crd`
(`exists and cannot be imported into the current release`). Adopt them once, with
Helm 3.17 or newer:

```bash
helm upgrade --install scarpenter-crd oci://ghcr.io/sparecores/charts/scarpenter-crd \
  --version 0.3.0 --namespace scarpenter-system --take-ownership
```

or with any Helm, by giving the CRDs the metadata Helm checks, before that command
(without `--take-ownership`). Name only the CRDs that exist; skip the two `karpenter.sh`
CRDs if another Karpenter owns them, and install with `--set karpenterCore=false`:

```bash
for crd in scnodeclasses.scarpenter.sparecores.com nodepools.karpenter.sh nodeclaims.karpenter.sh; do
  kubectl label crd "$crd" app.kubernetes.io/managed-by=Helm --overwrite
  kubectl annotate crd "$crd" meta.helm.sh/release-name=scarpenter-crd \
    meta.helm.sh/release-namespace=scarpenter-system --overwrite
done
```

From then on `--skip-crds` on the `scarpenter` chart and the upgrade above are all
there is. The CRDs carry `helm.sh/resource-policy: keep`: `helm uninstall
scarpenter-crd` leaves them, and with them every object of the kinds.

### Without Helm for the CRDs

Apply the CRD of the release with server-side apply (client-side `kubectl apply`
fails: the CRD is too large for the last-applied annotation):

```bash
kubectl apply --server-side -f https://github.com/SpareCores/scarpenter/releases/download/v0.3.0/scnodeclass-crd.yaml
```

`scnodeclass-crd.yaml` holds the SCNodeClass CRD only. Karpenter core's two are in
`crds/` of the `scarpenter` chart package (also a release asset): apply them only when
SCarpenter is the only Karpenter in the cluster and its chart created them. On a
cluster that runs another Karpenter they belong to it, and applying core v1.15's
schema over a newer one prunes fields.

### NodeClasses created under a CRD older than 0.2.0

The CRD of 0.1.0 defaulted `spec.kubelet.maxPods` to `110`, so the API server stored
`maxPods: 110` on every NodeClass created under it, even when you never wrote it. From
0.2.0 an unset `maxPods` means 110, or 10 with `footprint: Lean`; the stored `110` now
looks like your own choice and **overrides the Lean preset**. After the CRD upgrade,
remove it from the NodeClasses where it was only the old default and the Lean
preset should apply:

```bash
kubectl get scnodeclass -o json | jq -r '.items[]
  | select(.spec.kubelet.footprint == "Lean" and .spec.kubelet.maxPods == 110) | .metadata.name'
kubectl patch scnodeclass <name> --type=json -p '[{"op": "remove", "path": "/spec/kubelet/maxPods"}]'
```

Keep a `maxPods: 110` that you did choose. Fields set after the CRD upgrade are not
affected: nothing is defaulted any more.

## Uninstall

**Order matters**, because the controller is what deletes the instances and the
vendor resources it created:

1. Delete **SCarpenter's** NodePools, those whose node class is an SCNodeClass (other
   Karpenters' NodePools are the same kind, so `delete nodepool --all` would drain
   their nodes too), and wait until none of its NodeClaims is left. Core drains the
   nodes and the controller terminates the instances:

   ```bash
   kubectl get nodepools -o json | jq -r '.items[]
     | select(.spec.template.spec.nodeClassRef.group == "scarpenter.sparecores.com")
     | .metadata.name' | xargs -r kubectl delete nodepool
   kubectl get nodeclaims -o json | jq '[.items[]
     | select(.spec.nodeClassRef.group == "scarpenter.sparecores.com")] | length'   # until 0
   ```
2. **Lower the idle lifetime, then delete the SCNodeClasses.** Managed networks and
   security groups (lifecycle `idleTTL`, the default for them) are deleted **only by the
   running controller, after they have been idle for their lifetime**: 24 hours by
   default. Once the last NodeClass is gone, the NodeClass's own setting no longer
   applies; the default of `managedResources.defaultTTL` (`--resource-default-ttl`) does.
   An uninstall right after the NodeClasses would therefore leave them behind, because
   nothing is left to delete them. Lower the default first:

   ```bash
   helm upgrade scarpenter oci://ghcr.io/sparecores/charts/scarpenter --namespace scarpenter-system \
     --reuse-values --set managedResources.defaultTTL=1m      # and your other options, or --version
   kubectl delete scnodeclass --all
   ```

   The sweep then removes what the NodeClasses created (networks, security groups,
   placement groups) in dependency order once no instance uses them; resources with the
   policy `retain` go when their NodeClass does (and are unused). Wait until the
   controller reports them deleted: the Events `ResourceDeleted` and the metric
   `scarpenter_resources_managed` falling to 0 (a garbage collection interval, two minutes,
   per dependency level, plus the per-use grace of two minutes and the lowered idle
   time). Resources of `use` references are never touched.
3. **Verify that nothing is left, then uninstall.** Everything SCarpenter creates carries
   the tag `scarpenter.sparecores.com/cluster=<cluster name>`; with the AWS CLI, per
   region the NodeClasses allowed:

   ```bash
   f="Name=tag:scarpenter.sparecores.com/cluster,Values=my-cluster"
   aws ec2 describe-instances --filters "$f" --query 'Reservations[].Instances[?State.Name!=`terminated`].InstanceId' --output text
   aws ec2 describe-security-groups --filters "$f" --query 'SecurityGroups[].GroupId' --output text
   aws ec2 describe-placement-groups --filters "$f" --query 'PlacementGroups[].GroupName' --output text
   aws ec2 describe-vpcs --filters "$f" --query 'Vpcs[].VpcId' --output text
   aws ec2 describe-subnets --filters "$f" --query 'Subnets[].SubnetId' --output text
   ```

   Every line must be empty (resources you brought with `use` are not tagged by
   SCarpenter, and are yours). Then `helm uninstall scarpenter --namespace scarpenter-system`.
   What a skipped step or an early uninstall leaves can be deleted by that tag, in this order:
   instances (terminate and wait), placement groups, security groups, then a managed
   network's subnets, route table, internet gateway (detach first) and VPC.

There is **no automatic cleanup on uninstall** (`helm uninstall` removes the
controller at once). A hook that deletes NodePools and NodeClasses would delete
your capacity and workloads' nodes behind your back, and a Helm hook can only wait
for it with an image of its own; the ordered procedure above does the same with
your control. If you uninstall first anyway, the cleanup is manual: instances and
resources carry the tag `scarpenter.sparecores.com/cluster=<cluster name>` (and
resources the names `scarpenter-<cluster>-...`); the bootstrap token Secrets in
`kube-system` carry the label `scarpenter.sparecores.com/nodeclaim`.

What an uninstall leaves behind:

- the **CRDs** (Helm never deletes those of `crds/`, and those of `scarpenter-crd` carry `helm.sh/resource-policy: keep`) and with them every SCNodeClass and
  NodePool. Delete them yourself to remove the data:
  `kubectl delete crd scnodeclasses.scarpenter.sparecores.com`;
- **Nodes** whose NodeClaims were not deleted first: the instances keep running
  and billing, and the Node objects stay; the garbage collector that would remove
  them is gone with the controller;
- vendor resources of step 2 if it was skipped, and the examples you installed
  (`examples.enabled`; they carry `helm.sh/resource-policy: keep`, so deleting them
  is steps 1 and 2, not the uninstall);
- the IAM role, OIDC provider, Secrets and Roles you created yourself;
- the `kube-system` bootstrap token Secrets of NodeClaims that were never
  registered expire by themselves (the token cleaner removes them after the TTL).

## Troubleshooting

| Symptom | Look at |
|---|---|
| `helm install` fails with "is required" or a schema error | The message names the value. |
| The controller is not ready | `kubectl -n scarpenter-system logs`: the usual causes are a missing CRD, an unreachable API server, or a flag the controller rejects (all problems are listed at once). |
| `SCNodeClass` not `Ready` | `kubectl describe scnodeclass`: `CatalogReady` (Keeper unreachable or no snapshot yet), `ArtifactsReady` (the Flatcar or sysext hosts), `ValidationSucceeded` (a Secret or ConfigMap it references, the Kubernetes skew), and per vendor under `status.vendors.<vendor>`: `VendorReady` (`ValidationFailed` names what the driver refuses) and `ImagesReady` (the Flatcar image per region). |
| Pods stay Pending, no NodeClaim | The NodePool's requirements (vendor, region, capacity type) against the NodeClass's regions; `QuotaAvailable=False` on the NodeClass; core's events on the pod. |
| A node never joins | The instance's serial console. The API endpoint must be reachable from the vendor network, the CNI DaemonSets must run on the node, and the token lives for `bootstrap.tokenTTL`. |
| `kubectl logs` and `exec` fail with 401 on a k3s node | [k3s: the kubelet client CA](#k3s-the-kubelet-client-ca). |
| A vendor's calls fail with an authentication or permission error | The Troubleshooting section of that provider's guide, for example [Set up AWS](set-up-aws.md#troubleshooting). |
