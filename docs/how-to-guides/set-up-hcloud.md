# Set up Hetzner Cloud

The `hcloud` driver launches Hetzner Cloud servers in one project, from Flatcar snapshots
that you keep in that project, and joins them to your cluster. Each location (`fsn1`,
`nbg1`, `hel1`, `ash`, `hil`, `sin`) is a region and its own zone. Every server is
on-demand, billed per started hour. Details: the
[Hetzner Cloud driver reference](../reference/hcloud-driver.md).

**Preview.** The driver is implemented and covered by unit and offline tests, but it has not run against the real cloud yet: expect rough edges and report them.

## What you need

- A Hetzner Cloud project used only by SCarpenter's nodes. The API token gives full
  control of its project, and SCarpenter scopes itself only by its own labels.
- Project limits worth checking in the Console: servers, Primary IPs, volumes, firewalls,
  placement groups and snapshots. The API cannot read them, so note the server limit for
  [step 3](#3-enable-the-driver).
- The nodes reach, over their public address: the API server endpoint of the chart
  (`cluster.apiServerEndpoint`, or over the tailnet with the `tailscale` template), and the
  sysext host (`github.com` and its release downloads, unless you set a mirror with
  `--sysext-base-url`). Those hosts have no IPv6, so keep the default public IPv4.

## 1. The Flatcar image

Hetzner offers no Flatcar image and has no image import API, so the driver boots
**snapshots in your project**: one per architecture and Flatcar release, global in the
project (not per location). It finds the snapshot for an architecture by three labels and
needs exactly one match:

| Label | Value |
|---|---|
| `os` | `flatcar` |
| `flatcar-channel` | the NodeClass's `flatcar.channel` (default `stable`) |
| `flatcar-version` | the NodeClass's `flatcar.version`, for example `4757.2.3` |

**The driver makes them for you** (ADR-030): when a NodeClass pins a release that has no
snapshot, the leader boots a temporary server in rescue mode, writes Flatcar's Hetzner
image, snapshots it with these labels and deletes the server: a few minutes and one
started hour of a small server per architecture. While it runs, the NodeClass's
`ImagesReady` says `ImageBuilding`. It needs outbound TCP 22 from the controller to the
temporary server ([details](../reference/hcloud-driver.md#building-the-snapshot)).

To make them yourself instead, set `vendors.hcloud.buildImages: false` in the chart and use
the community tool [`hcloud-upload-image`](https://github.com/apricote/hcloud-upload-image),
which does the same:

```sh
export HCLOUD_TOKEN=...   # a read-write token of the project
VERSION=4757.2.3 CHANNEL=stable
for arch in amd64 arm64; do
  case "$arch" in amd64) a=x86 dir=amd64-usr ;; arm64) a=arm dir=arm64-usr ;; esac
  hcloud-upload-image upload \
    --image-url "https://${CHANNEL}.release.flatcar-linux.net/${dir}/${VERSION}/flatcar_production_hetzner_image.bin.bz2" \
    --architecture "$a" --compression bz2 \
    --labels "os=flatcar,flatcar-channel=${CHANNEL},flatcar-version=${VERSION}" \
    --description "Flatcar ${CHANNEL} ${VERSION} ${arch}"
done
hcloud image list --type snapshot --selector os=flatcar --output columns=id,architecture,labels
```

Repeat it for every version a NodeClass pins, and keep the snapshots. An `x86` snapshot
boots only x86 server types, an `arm` one only `cax*` types; skip an architecture you do
not use. To find snapshots by other labels or IDs, set `spec.vendors.hcloud.resources.images.use`
([resource selectors](../reference/resource-selectors.md#hcloud)); whatever selects it, the
image's `flatcar-channel` and `flatcar-version` labels must still name the pinned release.

## 2. Credentials

Hetzner has no workload identity: the only mode is a project API token in a Secret. In
the Console, open the project, **Security**, **API tokens**, and create a **Read & Write**
token for SCarpenter alone, separate from your IaC's, so you can rotate it on its own.
There is no narrower permission. Store it under the key `HCLOUD_TOKEN`:

```sh
kubectl -n scarpenter-system create secret generic hcloud-credentials \
  --from-literal=HCLOUD_TOKEN=<token>
```

## 3. Enable the driver

```yaml
vendors:
  hcloud:
    enabled: true
    credentials:
      secret:
        existingSecret: hcloud-credentials   # required
        key: HCLOUD_TOKEN                    # the default
    serverLimit: 10      # optional: the project's server limit from the Console (0: unknown)
    ipv4Price: ""        # optional: USD per hour of a Primary IPv4; empty uses Keeper's price
```

The chart sets `--vendors=hcloud` and `HCLOUD_TOKEN` from the Secret. With `serverLimit`
the controller knows the headroom before a launch; without it a full project shows up
only as a failed launch (`QuotaExceeded`).

## 4. A first NodeClass and NodePool

[`docs/examples/hcloud-minimal.yaml`](../examples/hcloud-minimal.yaml) is the smallest
NodeClass: three locations, a public IPv4, a deny-inbound firewall that SCarpenter creates
per location, and the `tailscale` template, which needs only outbound traffic
([its auth key Secret](choose-a-network-template.md#with-tailscale)). With `direct`
instead, add `spec.networking.inbound.rules` for the kubelet port (10250) from the control
plane and for your CNI between nodes. Then a NodePool:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: hcloud
spec:
  template:
    spec:
      nodeClassRef: {group: scarpenter.sparecores.com, kind: SCNodeClass, name: hcloud-minimal}
      requirements:
        - {key: scarpenter.sparecores.com/vendor, operator: In, values: [hcloud]}
        - {key: kubernetes.io/arch, operator: In, values: [amd64]}
  limits: {cpu: "16"}
```

Volumes, placement groups, brought networks: [node settings](../reference/hcloud-driver.md#node-settings).

## 5. Check that it works

```sh
kubectl get scnodeclass hcloud-minimal
kubectl get scnodeclass hcloud-minimal -o jsonpath='{.status.vendors.hcloud}' | jq
```

`Ready` must be `True`, and under `status.vendors.hcloud.conditions`: `VendorReady`
(the vendor is enabled, its locations exist, the block is valid), `ImagesReady` (a snapshot per architecture;
`status.vendors.hcloud.images` names it per location) and `ResourcesReady`. Then start a
Pod that needs a new node:

```sh
kubectl run hcloud-test --image=registry.k8s.io/pause:3.10 \
  --overrides='{"spec":{"nodeSelector":{"scarpenter.sparecores.com/vendor":"hcloud"}}}'
kubectl get nodeclaims -w     # a NodeClaim, then a Node, within a few minutes
kubectl delete pod hcloud-test   # the empty node goes after consolidation
```

Launch errors appear in `kubectl describe nodeclaim`, in the Events of the NodeClass and
in the controller's log (`kubectl -n scarpenter-system logs deployment/scarpenter`).

## Limits

- On-demand only (no spot), no Floating IPs or load balancers, no private-only servers.
- SCarpenter does not create private networks (`networks.create` is refused). A brought
  network needs `networking.isolation` `nodeclass` or `none`, because Hetzner firewalls
  filter only the public interface.
- The root disk is the server type's: a profile cannot change it. Data volumes are 10 GB to
  10 TB, at most 16 per server.
- A spread placement group holds at most 10 servers.
- One project per controller. Billing retention stays off until the billing rule is verified.

More in [the reference](../reference/hcloud-driver.md#deferred).

## Troubleshooting

| Symptom | Fix |
|---|---|
| The controller exits with `HCLOUD_TOKEN is not set` | Set `vendors.hcloud.credentials.secret.existingSecret` to a Secret with the key `HCLOUD_TOKEN`. |
| Calls fail with `unauthorized` or `forbidden` | The token is revoked or read-only: create a Read & Write token, update the Secret, restart the controller. |
| `ImagesReady` false, `ImageNotFound` | No snapshot carries the three labels for the pinned release and architecture: run step 1 for that version. |
| `ImageAmbiguous` | Two snapshots carry the same labels for one architecture: delete the older one. |
| `ImageVersionMismatch` or `ImageVersionUnknown` | The selected snapshot's `flatcar-*` labels do not name the pinned release: fix the labels or the selector. |
| `VendorReady` false, `networking.isolation is run` | A brought network needs `networking.isolation: nodeclass` or `none`. |
| `QuotaExceeded` (`resource_limit_exceeded`) | A project limit is reached: raise it in the Console, or set `serverLimit`. |
| `InsufficientCapacity`, `PlacementFull` | The spread placement group already has 10 servers. |
| With `direct`, `kubectl logs` or `exec` time out, or Pods on two nodes cannot talk | The deny-inbound firewall blocks them: add inbound rules, or use `tailscale`. |
