# Set up Vultr

The `vultr` driver launches Vultr Cloud Compute instances that boot a pinned Flatcar release from a
snapshot in your own account. It attaches a firewall group to every node and, unless
`networking.isolation` is `none`, a VPC per region; it creates and owns both, or uses the ones you
name. The controller calls the Vultr API with the API key of one account; the nodes get no cloud
credentials.

**Preview.** The driver has run real nodes, but the real-cloud e2e lane has not run green. Expect rough edges and report them.

## What you need

- A Vultr account and a **dedicated user** for SCarpenter (Account > Users), not your own.
- Low **account limits** (Account > Limits: maximum instances, maximum instance cost). They are the
  only ceiling: a Vultr key cannot be denied plans or regions, and SCarpenter cannot read the limits.
- The **controller's egress address**, for the key's IP allowlist (the cluster's NAT address, or the
  public address of the node the controller runs on).
- From the nodes: the API server at `cluster.apiServerEndpoint` (over Tailscale or publicly), and the
  artifact hosts (the Flatcar release server and the sysext-bakery releases on GitHub, or your
  mirrors), see [install](install.md#what-you-need).

## 1. The Flatcar image

Flatcar publishes no Vultr image and Vultr snapshots cannot be shared between accounts, so **you create
one snapshot per pinned Flatcar version, once per account**. SCarpenter never creates or deletes one.
The source is the Vultr image of the release built by the Spare Cores images repository (Flatcar with
the OEM id `vultr`, artifact `flatcar-vultr-<channel>-<version>-amd64`), at a public URL. A snapshot is
global in the account: one serves every region.

```bash
curl -sS -X POST https://api.vultr.com/v2/snapshots/create-from-url \
  -H "Authorization: Bearer ${VULTR_API_KEY}" -H "Content-Type: application/json" \
  -d '{"url": "<the image URL of the release>", "description": "flatcar-stable-4757.2.3-amd64"}'
```

Or use a snapshot-from-URL resource of your own tooling. Wait until the snapshot's status is
`complete`. Without `resources.images` in the NodeClass the driver picks the one complete snapshot whose
description is exactly `flatcar-<channel>-<version>-amd64`; `resources.images.use` can name it by ID
(`ids: {"*": [<snapshot ID>]}`) or by a description pattern instead
([vultr-bring-your-own.yaml](../examples/vultr-bring-your-own.yaml)). Either way the description must say
the pinned release (`flatcar-<channel>-<version>-amd64`, optionally followed by a space and a note), or
the snapshot is never booted. Details: [Images](../reference/vultr-driver.md#images).

## 2. Credentials

There is one mode: the API key, as a bearer token (`VULTR_API_KEY`). Vultr has no workload identity for
callers, so the key is a long-lived secret: rotate it, and treat whoever can read the Secret or run a Pod
as the controller's ServiceAccount as holding it.

1. As the dedicated user, create an API key (Account > API).
2. Give the user the ACLs to create and delete instances, firewall groups and VPCs and to read snapshots.
   Vultr's ACLs are coarse, and which ones cover snapshots and VPCs is not verified yet: grant the
   narrowest set that works and write it down.
3. Add the controller's egress address to the key's **IP allowlist**. A call from another address is
   refused, and the NodeClass shows `VendorReady=False`.
4. Store the key:

```bash
kubectl -n scarpenter-system create secret generic vultr-api-key --from-literal=apiKey=<the key>
```

## 3. Enable the driver

```yaml
# values.yaml
vendors:
  aws:
    enabled: false           # the chart enables AWS by default; keep it only if you use it
  vultr:
    enabled: true
    credentials:
      secret:
        existingSecret: vultr-api-key
        key: apiKey          # the default
    # availabilityTTL: 5m    # how long a region's stock is cached (the default)
```

The chart adds `vultr` to `--vendors` and sets `VULTR_API_KEY` from the Secret. It refuses to render
without `existingSecret`, and the controller does not start with an empty key. The controller makes no
Vultr call at start-up: the key is checked when a NodeClass is validated.

## 4. A first NodeClass and NodePool

This NodeClass uses the `tailscale` template (create the `ts-node-key` Secret as in
[choose a network template](choose-a-network-template.md#with-tailscale)) and no VPC. SCarpenter creates
a firewall group per region that denies all inbound traffic.

```yaml
apiVersion: scarpenter.sparecores.com/v1alpha1
kind: SCNodeClass
metadata:
  name: vultr
spec:
  flatcar: {channel: stable, version: "4757.2.3"}
  kubernetes: {version: "v1.37.1"}
  network:
    template: tailscale
    tailscale:
      version: "v1.98.10"
      authKeySecretRef: {name: ts-node-key, namespace: scarpenter-system, key: authkey}
      tags: ["tag:k8s-node"]
  networking: {isolation: none}
  vendors:
    vultr:
      regions: [ewr]
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: vultr
spec:
  template:
    spec:
      nodeClassRef: {group: scarpenter.sparecores.com, kind: SCNodeClass, name: vultr}
      requirements:
        - {key: scarpenter.sparecores.com/vendor, operator: In, values: [vultr]}
        - {key: kubernetes.io/arch, operator: In, values: [amd64]}
        - {key: node.kubernetes.io/instance-type, operator: In, values: ["vultr.vc2-1c-2gb", "vultr.vc2-2c-4gb"]}
  limits: {cpu: "8"}
```

Without `networking.isolation` the default is `run`, which on Vultr puts every node of the NodeClass in
a VPC per region that SCarpenter creates. With the `direct` template the firewall group must let the
control plane reach the kubelet (TCP 10250) and your CNI's ports: add them to `networking.inbound`.
More: [vultr-minimal.yaml](../examples/vultr-minimal.yaml),
[vultr-managed.yaml](../examples/vultr-managed.yaml),
[vultr-bring-your-own.yaml](../examples/vultr-bring-your-own.yaml).

## 5. Check that it works

```bash
kubectl get scnodeclass vultr                 # READY True
kubectl get scnodeclass vultr -o jsonpath='{.status.vendors.vultr.conditions}'
kubectl run probe --image=registry.k8s.io/pause:3.10 \
  --overrides='{"spec":{"nodeSelector":{"karpenter.sh/nodepool":"vultr"}}}'
kubectl get nodeclaims,nodes -w
```

The vendor conditions that matter are `VendorReady` (the key), `ImagesReady` (the snapshot) and
`ResourcesReady` (the firewall group and VPC). A failed launch shows in `kubectl describe nodeclaim` and
in the controller's log (`kubectl -n scarpenter-system logs deployment/scarpenter`). Delete the Pod
afterwards; the empty node is removed.

## Limits

- amd64 only: Vultr has no Arm instances.
- On-demand only; no spot or diskless `vx1` plans.
- Bare metal (`vbm`) plans are offered only when Karpenter's registration timeout (`--registration-timeout`) is at least 25
  minutes (a server needs about 15 minutes to register its Node): set the chart value `karpenter.registrationTimeout` to `25m`
  or more to enable them, or none is offered. A bare-metal server joins no VPC; its inbound traffic is filtered on the node by a host firewall
  built from the NodeClass's firewall group
  ([bare metal](../reference/vultr-driver.md#bare-metal)).
- The root disk is the plan's disk and cannot be changed; no block storage volumes.
- `networking.publicIPv4: false` is refused; storage, network and placement profiles with a `vultr`
  block make their offerings unavailable.
- No quota reading (the account limits are console-only) and no billing retention yet.

See the [Vultr driver reference](../reference/vultr-driver.md) for the rest.

## Troubleshooting

| Symptom | Fix |
|---|---|
| The chart fails: `vendors.vultr.credentials.secret.existingSecret is required` | Create the Secret and set `existingSecret`. |
| The Pod is in `CreateContainerConfigError` | The Secret, or its key (`apiKey` by default), does not exist in the controller's namespace. |
| The controller exits: `VULTR_API_KEY is not set` | The key in the Secret is empty. |
| `VendorReady=False`, "the API key ... is refused by Vultr" | Wrong key, a missing ACL, or the controller's egress address is not in the key's allowlist. |
| `ImagesReady=False`, `ImageNotFound` | No complete snapshot is described `flatcar-<channel>-<version>-amd64` (or it is still pending). |
| `ImagesReady=False`, `ImageVersionMismatch` or `ImageVersionUnknown` | The chosen snapshot's description names another release, or none. |
| `ImagesReady=False`, `ImageAmbiguous` | Two complete snapshots share the description: delete one, or name an ID. |
| Nodes get a VPC you did not want | Set `networking.isolation: none`. |
| The node never joins with `direct` | The firewall group blocks the control plane: add inbound rules for the kubelet and the CNI. |
