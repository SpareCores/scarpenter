# Install SCarpenter

SCarpenter installs with one Helm chart, `oci://ghcr.io/sparecores/charts/scarpenter`.
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
(`keeper.sparecores.net`: the catalog and, for AWS, the EBS prices from
`/storage_prices`), to the vendor's APIs (for AWS: EC2, Service Quotas and STS), and to the
hosts of the node artifacts (the Flatcar release server and the sysext-bakery
releases on GitHub; point `artifacts.sysextBaseURL` and `artifacts.flatcarBaseURL`
at mirrors if the controller has no such access). Nodes download the same
artifacts themselves; IPv6-only nodes need mirrors, because GitHub has no IPv6.

**Vendor credentials** with the right permissions: [AWS credentials](aws-credentials.md).
On AWS, the account also needs the service-linked role for Spot if you launch Spot
capacity.

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

The images and the chart are signed with cosign (keyless); the release notes of
every version show the `cosign verify` command and attach the SBOMs.

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
  `helm install --skip-crds` leaves them out.
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
| `vendors.aws.credentials.*` | How the controller authenticates to AWS: [AWS credentials](aws-credentials.md). |
| `vendors.aws.enabled` | Turn the AWS driver off or on (default on). |
| `cluster.dns` | The kubelet's `clusterDNS` on nodes. Empty: Helm looks the `kube-dns` Service up at install time, and where it cannot (`helm template`, ArgoCD) the controller reads the DNS Service of `kube-system` itself (label `k8s-app=kube-dns`, else the Service `kube-dns`). If it finds none, SCNodeClasses are not valid (`ValidationSucceeded=False`, reason `ClusterDNSUnknown`, naming this value): set `cluster.dns`, or `cluster.dnsFromService.enabled=false` to give the nodes no cluster DNS (their Pods then use the node's resolver). |
| `cluster.domain` | The kubelet's `clusterDomain` (default `cluster.local`). |
| `cluster.ca.*` | A CA bundle for the nodes other than the `kube-root-ca.crt` ConfigMap of the release namespace (which every namespace has, and which is the right one on kubeadm, k3s and EKS). |
| `kubeletClientCA.*` | [k3s](#k3s-the-kubelet-client-ca). |
| `keeper.url`, `keeper.apiKey.existingSecret` | A different Keeper, or an API key (higher rate limits) from a Secret you create. |
| `bootstrap.tokenTTL` | Lifetime of a node's bootstrap token; at least 20m (core gives a NodeClaim 15 minutes to register). |
| `bootstrap.tailnetPrefixes` | For a Headscale with other prefixes than Tailscale's. |
| `rbac.authKeySecrets` | The tailscale auth key Secrets of other namespaces: the chart creates a Role that reads just that Secret and a RoleBinding there. The key Secret in the release namespace needs nothing. |
| `managedResources.*`, `gc.*`, `quota.*`, `unavailable.*`, `catalog.*` | The tuning of the controller; the defaults suit most clusters. |
| `resources` | Sized from the L3 scale measurement: the controller peaks at about 300 MiB with three regions and about 475 MiB with every AWS region in scope. Keep the memory limit above 700 MiB if you do not restrict regions in the NodeClass. |
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
  Karpenter's own chart (`karpenter-crd`), and **never apply this chart's
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

[AWS credentials](aws-credentials.md) covers the three modes of the chart:
`webIdentity` (the ServiceAccount's token exchanged for an IAM role; the
recommendation), `secret` (static keys in a Secret you create) and `none` (EKS Pod
Identity or the node's instance profile, which the SDK finds itself).

## First NodeClass and NodePool

Nothing launches without an `SCNodeClass` and a Karpenter `NodePool` that
references it. `--set examples.enabled=true` (with the versions and regions you
want: `examples.*`) installs one pair per enabled vendor as a starting point; the
files in `config/samples` show the whole API ([API reference](../reference/api.md)). A
NodeClass pins the Flatcar and Kubernetes versions, so check that the Kubernetes
version is within the skew of your API server. It is ready when `kubectl get
scnodeclass` shows `Ready=True`; otherwise `kubectl describe` names the condition
([status conditions](../reference/controller.md#scnodeclass-status-conditions)).

## Upgrade

```bash
helm upgrade scarpenter oci://ghcr.io/sparecores/charts/scarpenter \
  --version 0.2.0 --namespace scarpenter-system --reuse-values
```

Helm does not upgrade CRDs. Before an upgrade that changes the API (the release
notes say so; `SCNodeClass` is `v1alpha1`), apply the SCNodeClass CRD of the new
version:

```bash
helm pull oci://ghcr.io/sparecores/charts/scarpenter --version 0.2.0 --untar
kubectl apply --server-side -f scarpenter/crds/scarpenter.sparecores.com_scnodeclasses.yaml
```

Apply the two `karpenter.sh_*` files of the chart **only when SCarpenter is the
only Karpenter in the cluster** and its chart created them (`kubectl get crd
nodepools.karpenter.sh -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}'`
prints nothing, and the CRD was not installed by another chart): then do the same
with `scarpenter/crds/karpenter.sh_nodepools.yaml` and `..._nodeclaims.yaml`. On a
cluster that runs another Karpenter they belong to it: applying core v1.15's schema
over a newer one prunes fields, and over its own release's ownership produces
conflicts that `--force-conflicts` would only paper over.

Check `values.yaml` of the new version for values that were added (`--reuse-values`
keeps your old ones and misses new defaults; prefer `-f your-values.yaml`). The
controller keeps no state outside the cluster and the vendor, so a restart loses
nothing: instances are found again by their tags, resources are adopted by their
names and ownership markers. Nodes keep running while the controller is down;
nothing new launches and nothing is cleaned up.

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
2. Delete the SCNodeClasses (`kubectl delete scnodeclass --all`). The controller's
   resource sweep then removes the vendor resources it created for them (networks,
   security groups, placement groups) by their lifecycle, as soon as no instance
   uses them; those with the policy `retain` go when their NodeClass does. Watch
   the Events `ResourceDeleted` and the metric `scarpenter_resources_managed`
   fall to 0 (about one garbage collection interval, two minutes, per dependency
   level; a perUse grace of two minutes applies). Resources of `use` references are
   never touched.
3. Then `helm uninstall scarpenter --namespace scarpenter-system`.

There is **no automatic cleanup on uninstall** (`helm uninstall` removes the
controller at once). A hook that deletes NodePools and NodeClasses would delete
your capacity and workloads' nodes behind your back, and a Helm hook can only wait
for it with an image of its own; the ordered procedure above does the same with
your control. If you uninstall first anyway, the cleanup is manual: instances and
resources carry the tag `scarpenter.sparecores.com/cluster=<cluster name>` (and
resources the names `scarpenter-<cluster>-...`); the bootstrap token Secrets in
`kube-system` carry the label `scarpenter.sparecores.com/nodeclaim`.

What an uninstall leaves behind:

- the **CRDs** (Helm never deletes them) and with them every SCNodeClass and
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
| `SCNodeClass` not `Ready` | `kubectl describe scnodeclass`: `CatalogReady` (Keeper unreachable or no snapshot yet), `ArtifactsReady` (the Flatcar or sysext hosts), `ValidationSucceeded` (a Secret or ConfigMap it references, the Kubernetes skew). |
| Pods stay Pending, no NodeClaim | The NodePool's requirements (vendor, region, capacity type) against the NodeClass's regions; `QuotaAvailable=False` on the NodeClass; core's events on the pod. |
| A node never joins | The instance's serial console. The API endpoint must be reachable from the vendor network, the CNI DaemonSets must run on the node, and the token lives for `bootstrap.tokenTTL`. |
| `kubectl logs` and `exec` fail with 401 on a k3s node | [k3s: the kubelet client CA](#k3s-the-kubelet-client-ca). |
| AWS calls fail with `AccessDenied`, `Missing Region` or `WebIdentityErr` | [AWS credentials](aws-credentials.md#troubleshooting). |
