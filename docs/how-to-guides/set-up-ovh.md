# Set up OVHcloud Public Cloud

The OVHcloud driver launches Flatcar nodes as Nova servers in **one Public Cloud project** through its OpenStack APIs.
Nodes attach to the public `Ext-Net` network with a security group SCarpenter manages. Details are in the
[OVHcloud driver reference](../reference/ovh-driver.md).

**Preview.** A first real-cloud smoke run passed on 2026-11-16 (one amd64 node in GRA11, then scale to zero); the
full test lane has not run yet.

## What you need

- **A Public Cloud project** with the regions you want **activated** (a region has no endpoints before that). Regions
  are named as in the project's Keystone catalog, upper case: `GRA11`, `SBG5`, `EU-WEST-PAR`. List them with
  `openstack catalog show compute`.
- **Quota.** New projects start with low per-region limits on instances, cores and RAM. Check them with
  `OS_REGION_NAME=GRA11 openstack limits show --absolute` and raise them in the Control Panel
  ([increasing quotas](https://docs.ovhcloud.com/en/guides/public-cloud/cross-functional/increasing-public-cloud-quota)).
- **Reachability.** Nodes have a public IPv4 on `Ext-Net`. They must reach `cluster.apiServerEndpoint` (over the
  tailnet with the `tailscale` template, publicly with `direct`) and the artifact hosts: the Flatcar release server
  and the sysext-bakery releases on GitHub, or your mirrors (`artifacts.*`). The controller needs HTTPS to
  `auth.cloud.ovh.net` and the regional OpenStack endpoints.

## 1. The Flatcar image

SCarpenter never creates images. OVH has no public Flatcar image and Glance images are regional, so upload Flatcar's
OpenStack image **once per region and release** with the `openstack` CLI. Only amd64 is launched on OVH.

```sh
version=4757.2.3; channel=stable
base=https://$channel.release.flatcar-linux.net/amd64-usr/$version
curl -fLO $base/flatcar_production_openstack_image.img
curl -fLO $base/flatcar_production_openstack_image.img.DIGESTS
grep -A1 -i sha512 flatcar_production_openstack_image.img.DIGESTS | grep openstack_image.img$ | sha512sum -c

for region in GRA11 EU-WEST-PAR; do
  OS_REGION_NAME=$region openstack image create "flatcar-$channel-$version-amd64" \
    --file flatcar_production_openstack_image.img \
    --disk-format qcow2 --container-format bare --private \
    --property architecture=x86_64 \
    --property flatcar_channel=$channel --property flatcar_version=$version \
    --tag "flatcar-$channel-$version"
done
```

Use the uncompressed `.img` (about 0.6 GB): Glance stores the bytes as given. Any user with the Image Operator role
can upload (the one of step 2 will do).

Without an `images` block in the NodeClass, the driver looks for **exactly one private, active** image of the project
named `flatcar-<channel>-<version>-amd64` with the property `architecture=x86_64`. The image's release (from its name,
its `flatcar_channel`/`flatcar_version` properties or its `flatcar-<channel>-<version>` tag) must be the NodeClass's
pinned one, or it is never booted. To pick images by Glance UUID, pattern, tag or owner instead, see
`vendors.ovh.resources.images.use` in the [selector reference](../reference/resource-selectors.md#ovh) and
[`ovh-bring-your-own.yaml`](../examples/ovh-bring-your-own.yaml).

For a new release, upload its image next to the old one, then change `spec.flatcar.version`: the nodes drift and are
replaced. Delete the old image once nothing uses it.

## 2. Credentials

**An OpenStack user of the project** is the supported mode. In the Control Panel (*Public Cloud, your project, Users &
Roles*) create a user for SCarpenter only, with these five roles:

| Role | Used for |
|---|---|
| Compute Operator (`computeOperator`) | flavors, servers, availability zones, limits, server groups |
| Network Operator (`networkOperator`) | the `Ext-Net` network |
| Network Security Operator (`networkSecurityOperator`) | security groups and their rules |
| Image Operator (`imageOperator`) | reading Glance images |
| Volume Operator (`volumeOperator`) | volume types, boot and data volumes |

Put its OpenStack environment (from its `openrc` file) in a Secret in the controller's namespace:

```sh
kubectl -n scarpenter-system create secret generic ovh-openstack \
  --from-literal=OS_AUTH_URL=https://auth.cloud.ovh.net/v3 \
  --from-literal=OS_USERNAME=user-AbCdEf123456 \
  --from-literal=OS_PASSWORD='...' \
  --from-literal=OS_PROJECT_ID=0123456789abcdef0123456789abcdef \
  --from-literal=OS_USER_DOMAIN_NAME=Default
```

All five keys are required. The password is the only credential (no workload identity reaches OVH); the controller
authenticates on its first call and renews the token itself.

Alternatives: `credentials.mode: none` wires nothing, and you set the `OS_*` variables with `extraEnv` (also the way to
use `OS_USER_DOMAIN_ID`). An OVH IAM service account is not supported yet;
[`ovh-iam-policy.json`](../reference/ovh-iam-policy.json) is the policy it will need.

## 3. Enable the driver

```yaml
vendors:
  aws:
    enabled: false            # on by default; leave it on only if you also use AWS
  ovh:
    enabled: true
    credentials:
      mode: secret
      secret:
        existingSecret: ovh-openstack
        # Other key names: authUrlKey, usernameKey, passwordKey, projectIdKey, userDomainNameKey.
```

The chart adds `ovh` to `--vendors`. `scheduleWait` (default 45s) and `regionAliases` are rarely needed
([controller reference](../reference/controller.md)); the other values are in [Install SCarpenter](install.md).

## 4. A first NodeClass and NodePool

```yaml
apiVersion: scarpenter.sparecores.com/v1alpha1
kind: SCNodeClass
metadata:
  name: ovh
spec:
  flatcar: {channel: stable, version: "4757.2.3"}   # the image of step 1
  kubernetes: {version: "v1.37.1"}
  network: {template: direct}
  vendors:
    ovh:
      regions: [GRA11]
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: ovh
spec:
  template:
    spec:
      nodeClassRef: {group: scarpenter.sparecores.com, kind: SCNodeClass, name: ovh}
      requirements:
        - {key: scarpenter.sparecores.com/vendor, operator: In, values: [ovh]}
        - {key: karpenter.sh/capacity-type, operator: In, values: [on-demand]}
        - {key: kubernetes.io/arch, operator: In, values: [amd64]}
  limits: {cpu: "16"}
```

With `direct`, allow the control plane's address in `spec.networking.inbound.rules` (at least TCP 10250, the kubelet):
the managed security group denies other inbound traffic. The tested setup uses the `tailscale` template
([choose a network template](choose-a-network-template.md)). More: [`ovh-minimal.yaml`](../examples/ovh-minimal.yaml),
[`ovh-bring-your-own.yaml`](../examples/ovh-bring-your-own.yaml) (boot from volume, data volumes, server groups).

## 5. Check that it works

```sh
kubectl get scnodeclass ovh
kubectl get scnodeclass ovh -o jsonpath='{range .status.vendors.ovh.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
```

`Ready` must be `True`, as must `VendorReady` (the block is valid, its regions are in the catalog) and `ImagesReady`
(every region has its image, listed in `status.vendors.ovh.images`). Then start a Pod on the pool:

```sh
kubectl run ovh-test --image=busybox:1.37 --restart=Never \
  --overrides='{"spec":{"nodeSelector":{"scarpenter.sparecores.com/vendor":"ovh"}}}' -- sleep 600
kubectl get nodeclaims -w
```

A NodeClaim appears, its server shows in `openstack server list`, and the node joins within minutes; delete the Pod
and it goes. Errors show in `kubectl describe` of the SCNodeClass and the NodeClaim, and in the controller's log
(`kubectl -n scarpenter-system logs deploy/scarpenter`).

## Limits

- amd64 and on-demand only: no arm64, no spot, no Metal flavors. GPU flavors (`t1`, `t2`, `a10`, `a100`, `h100`, `h200`, `l4`, `l40s`, `rtx5000`) are launched when the region has them and the project has their quota ([GPU flavors](../reference/ovh-driver.md#gpu-flavors)).
- Public `Ext-Net` only: no private (vRack) networks, gateways or IPv6-only nodes; `networking.publicIPv4: false` is refused.
- No `localStorage`: invalid in the vendor block; a storage profile asking for it fits no type, so its Pods stay Pending.
- No billing retention (OVH's billing rules are unverified), and no automatic image import.
- Only the user/password mode; one controller serves one project.

See also [not built yet](../reference/ovh-driver.md#not-built-yet) and
[not verified against OVH](../reference/ovh-driver.md#not-verified-against-ovh).

## Troubleshooting

- **The controller exits with "the OVH driver needs OpenStack credentials"**: a variable is missing or empty; the
  message names it. A Pod stuck in `CreateContainerConfigError` means the Secret lacks one of the five keys.
- **`ImagesReady=False` (`ImageResolutionFailed`), 401 / `Unauthorized` in the log**: wrong user, password, project
  ID or user domain; test the same values with `openstack token issue`.
- **`ImagesReady=False` (`ImageNotFound` or `ImagesNotReady`)**: no private, active image named
  `flatcar-<channel>-<version>-amd64` with `architecture=x86_64` in that region; upload it (step 1) or fix the name.
- **`ImageVersionMismatch` or `ImageVersionUnknown`**: the image's name, properties or tag name another release than
  the NodeClass, or contradict each other; fix the image's metadata.
- **`VendorReady=False`**: `ValidationFailed` on a region name wants the catalog's upper-case name (`GRA11`);
  `UnknownRegions` means neither the driver nor Keeper has the region; a region that is not activated shows in `RegionsUnavailable` (the log says so).
- **`QuotaExceeded`**: the project's per-region instance, core or RAM limit is reached; raise it.
- **`InsufficientCapacity` (`NoValidHost`)**: the flavor has no capacity in the region; the next candidate type is
  tried. Allow more types or regions in the NodePool.
