# Set up Vultr credentials

The controller calls the Vultr API (instances, firewall groups, VPCs, snapshots) with the API key of
one account. The nodes it launches get **no** cloud credentials. This guide covers the key and its
allowlist, the Secret and the chart values, and the one thing a Vultr account needs before the first
node: the snapshot of the pinned Flatcar version. Everything here follows the
[Vultr driver reference](../reference/vultr-driver.md); none of it has run against a real account yet
(the live spikes of plan 20 do that, see
[what is not verified](../reference/vultr-driver.md#what-is-not-verified-against-vultr-yet)).

- [How the driver authenticates](#how-the-driver-authenticates)
- [The key and its allowlist](#the-key-and-its-allowlist)
- [The Secret and the chart](#the-secret-and-the-chart)
- [The snapshot of the Flatcar version](#the-snapshot-of-the-flatcar-version)
- [Troubleshooting](#troubleshooting)

## How the driver authenticates

`VULTR_API_KEY` is a bearer token. One key is one account: there is no project, subscription or tenant
to name in the NodeClass. Vultr has no workload identity for callers, so there is no federation and no
other mode: the key is a long-lived secret, rotate it like one. The trust boundary is the cluster:
whoever can read the Secret or run a Pod as the controller's ServiceAccount can act with the key.

## The key and its allowlist

1. Use a **dedicated user** for SCarpenter (Account > Users), not your own, and create its API key
   (Account > API). The user's ACLs are coarse (`provisioning`, `subscriptions`, `subscriptions_view`,
   `firewall`, ...): the controller needs to create and delete instances and firewall groups, create and
   delete VPCs, and read snapshots. Which ACL covers snapshots and VPCs is unverified (plan 20, spike S5);
   grant the narrowest set that lets the L5 lane pass and write it down for your account. There is no
   per-region or per-plan deny.
2. Vultr binds an API key to an **IP allowlist**. Put the controller's **egress address** in it (the
   NAT address of the cluster, or the node's public address). A request from another address is
   refused with 401 or 403, and the NodeClass is `VendorReady=False` with the reason naming the key and
   the allowlist.
3. Set low **account limits** (Account > Limits: maximum instances and maximum instance cost) as the
   ceiling of a mistake: credentials cannot deny instance types or regions.

## The Secret and the chart

```bash
kubectl -n scarpenter create secret generic vultr-api-key --from-literal=apiKey=<the key>
```

```yaml
# values.yaml
vendors:
  vultr:
    enabled: true
    credentials:
      secret:
        existingSecret: vultr-api-key
        key: apiKey          # the default
    # availabilityTTL: 5m    # how long the stock of a region is cached
```

The chart sets `--vendors=aws,vultr` (or only `vultr` with `vendors.aws.enabled=false`) and
`VULTR_API_KEY` from the Secret. The controller refuses to start with `vultr` enabled and no key.

## The snapshot of the Flatcar version

Flatcar publishes no Vultr image, and Vultr snapshots cannot be shared between accounts, so **each
account creates the snapshot of each pinned Flatcar version once**. SCarpenter never creates or deletes
one. The image is built by the images repository (Flatcar with the OEM id `vultr`) and published at a
public URL; import it in your account with the description `flatcar-<channel>-<version>-amd64`, for
example:

```bash
vultr-cli snapshot create-url --url "<the image URL of the release>" \
  --description "flatcar-stable-4757.2.3-amd64"
```

or a snapshot-from-URL resource of your own tooling. The snapshot is global in the account (the same ID
in every region) and costs $0.05 per GB-month of its compressed size. Wait until its status is
`complete`. Without a `resources.images` in the NodeClass, SCarpenter finds the snapshot by that exact
description; with one, you name it:

```yaml
vendors:
  vultr:
    resources:
      images:
        use:
          ids: {"*": [5a000000-0000-4000-8000-000000000001]}   # or name: "flatcar-{channel}-{version}-{arch}"
```

The snapshot must **say** which release it is by its description (`flatcar-<channel>-<version>-amd64`,
optionally followed by a space and a note), or the region's images are not ready: an image of another
release or of none is never booted. See [Images](../reference/vultr-driver.md#images).

## Troubleshooting

| Symptom | Cause |
|---|---|
| the controller does not start: `VULTR_API_KEY is not set` | `vendors.vultr.enabled` without the Secret wired, or an empty key |
| `VendorReady=False`, "the API key ... is refused by Vultr" | the key is wrong, its user lacks an ACL, or the controller's egress address is not in the key's allowlist |
| `ImagesReady=False`, `ImageNotFound` | no snapshot is described `flatcar-<channel>-<version>-amd64` (or the `use` matches none), or it is still `pending` |
| `ImagesReady=False`, `ImageVersionMismatch` or `ImageVersionUnknown` | the chosen snapshot's description says another release, or none |
| `ImagesReady=False`, `ImageAmbiguous` | two complete snapshots share the description: delete one, or name an ID |
| `VendorReady=False`, `publicIPv4: false` | not supported on Vultr in v1 |
| nodes of a default NodeClass create a VPC | the default `networking.isolation` is `run`, which puts nodes in a VPC; set `isolation: none` to attach none |
