# Set up Alibaba Cloud

The `alicloud` driver launches Flatcar nodes on ECS pay-as-you-go and spot instances from your own
Flatcar images, in the default VPC, a network it creates or vSwitches you bring, placed by the account's
vCPU quota and each zone's stock. Design and details: the
[Alibaba Cloud driver reference](../reference/alicloud-driver.md).

**Preview.** The driver is implemented and covered by unit and offline tests, but it has not run against the real cloud yet: expect rough edges and report them.

## What you need

- **An international-site account** with real-name verification and a balance (China regions are not supported).
- **vCPU quota** in each region you use: ECS's `postpaid-vcpu` and `spot-vcpu` account attributes
  (`DescribeAccountAttributes`). New accounts have small limits; raise them in Quota Center. Some
  accounts are refused spot (`InvalidAccount.NotSupportSpot`): use `on-demand` in the NodePool then.
- **Reachability from the nodes** to `cluster.apiServerEndpoint`, the Flatcar release server, the
  sysext-bakery releases on GitHub (or mirrors, `artifacts.*`) and your registries. Their egress is a
  public IPv4 (100 Mbit/s pay by traffic by default): the default VPC and the managed network have no NAT.
- **Outbound HTTPS from the controller** to `ecs.<region>.aliyuncs.com`, `vpc.<region>.aliyuncs.com`, `sts.aliyuncs.com`.

## 1. The Flatcar image

Flatcar publishes no Alibaba Cloud image, so the images are yours: one custom image per region and
architecture, in the account of the controller's credentials. SCarpenter only reads them. For each
Flatcar release you pin, and for each region (and for `amd64`, `arm64` or both):

1. Take the generic Flatcar image of the release with the OEM id `aliyun` set (without it Ignition never
   reads the user data); the Spare Cores images repository builds it ([Flatcar images](../explanation/flatcar-images.md#alicloud)).
2. Upload it to an OSS bucket in the region that the RAM role `AliyunECSImageImportDefaultRole` can read.
3. `ImportImage` with the architecture (`x86_64` or `arm64`), `Platform=Others Linux`,
   `Features.NvmeSupport=supported` (8th-generation types and later need it) and
   `Features.ImdsSupport=v1` (Ignition reads the metadata service without a token), and the name
   `Flatcar-<channel>-<version>-<arch>`, for example `Flatcar-stable-4757.2.3-amd64`.
4. Tag it `flatcar-channel=<channel>` and `flatcar-version=<version>`; `CopyImage` it to the other
   regions with the same name, and tag each copy.

The driver finds exactly one `Available` image of that name that you own; name and tags must agree
with `spec.flatcar`, or it is never booted. To name images otherwise or use another account's, see
`resources.images.use` and `imageOwner` in the [reference](../reference/alicloud-driver.md#images).

## 2. Credentials

The controller needs a RAM identity with the custom policy [`alicloud-ram-policy.json`](../reference/alicloud-ram-policy.json)
(reads, plus writes only on resources tagged with your cluster): replace `my-cluster` in it with your
`cluster.name`. The nodes get no RAM role.

**OIDC role (recommended, the chart's default).** The ServiceAccount token is exchanged for a role
session, with no long-lived secret. Alibaba Cloud must reach the cluster's ServiceAccount issuer over HTTPS.

1. In RAM, create an OIDC identity provider with the cluster's issuer URL and the client ID
   `sts.aliyuncs.com` (the token's audience, `vendors.alicloud.credentials.oidc.audience`).
2. Create a role whose trust policy lets that provider assume it for the controller's ServiceAccount
   (`scarpenter` in `scarpenter-system` with the commands of [Install](install.md)), and attach the policy:

```json
{"Version": "1", "Statement": [{"Action": "sts:AssumeRole", "Effect": "Allow",
  "Principal": {"Federated": ["acs:ram::123456789012:oidc-provider/my-cluster"]},
  "Condition": {"StringEquals": {"oidc:iss": "https://issuer.example.com", "oidc:aud": "sts.aliyuncs.com",
    "oidc:sub": "system:serviceaccount:scarpenter-system:scarpenter"}}}]}
```

**Access key (the fallback).** A RAM user with only the policy attached, never the account's root
key; rotate it. A temporary STS session works too: add its token to the Secret and set
`secret.securityTokenKey`.

```bash
kubectl -n scarpenter-system create secret generic alicloud-keys \
  --from-literal=ALIBABA_CLOUD_ACCESS_KEY_ID=<id> --from-literal=ALIBABA_CLOUD_ACCESS_KEY_SECRET=<secret>
```

**None.** The chart wires nothing and the SDK's default chain decides, for example the RAM role of the
ECS instance the controller runs on.

## 3. Enable the driver

```yaml
vendors:
  alicloud:
    enabled: true
    homeRegion: ap-southeast-1   # where the region list is read; empty: ap-southeast-1
    credentials:
      mode: oidc                 # oidc, secret or none
      oidc:
        roleArn: acs:ram::123456789012:role/scarpenter
        providerArn: acs:ram::123456789012:oidc-provider/my-cluster
        audience: sts.aliyuncs.com
```

For an access key: `mode: secret` and `secret.existingSecret: alicloud-keys` (key names in
`accessKeyIdKey`, `accessKeySecretKey`, defaulting to the ones above). With `oidc` the chart projects the
token to `/var/run/secrets/alibabacloud/token` and sets the `ALIBABA_CLOUD_*` variables. It refuses to
render without `roleArn` and `providerArn` (`oidc`) or `existingSecret` (`secret`).

## 4. A first NodeClass and NodePool

The images of the pinned release must exist in every listed region (step 1). Without `resources`, the
driver uses vSwitches and groups tagged `scarpenter.sparecores.com/use=true`, else the default VPC's
default vSwitches, else a VPC and security group it creates and owns.

```yaml
apiVersion: scarpenter.sparecores.com/v1alpha1
kind: SCNodeClass
metadata: {name: alicloud}
spec:
  flatcar: {version: "4757.2.3"}
  kubernetes: {version: "v1.37.1"}
  network: {template: direct}
  vendors:
    alicloud: {regions: [ap-southeast-1]}
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata: {name: alicloud}
spec:
  template:
    spec:
      nodeClassRef: {group: scarpenter.sparecores.com, kind: SCNodeClass, name: alicloud}
      requirements:
        - {key: scarpenter.sparecores.com/vendor, operator: In, values: [alicloud]}
        - {key: karpenter.sh/capacity-type, operator: In, values: [on-demand]}
        - {key: kubernetes.io/arch, operator: In, values: [amd64]}
  limits: {cpu: "16"}
```

More: [`alicloud-minimal.yaml`](../examples/alicloud-minimal.yaml), [`alicloud-bring-your-own.yaml`](../examples/alicloud-bring-your-own.yaml)
(image IDs, your vSwitches and group, a data disk, a deployment set).

## 5. Check that it works

```bash
kubectl get scnodeclass alicloud    # Ready must be True
kubectl get scnodeclass alicloud -o jsonpath='{.status.vendors.alicloud.conditions}'
kubectl run alicloud-test --image=registry.k8s.io/pause:3.10 \
  --overrides='{"spec":{"nodeSelector":{"scarpenter.sparecores.com/vendor":"alicloud"}}}'
kubectl get nodeclaims -w
```

The vendor conditions that matter are `VendorReady`, `ImagesReady` (the image per region and
architecture is in `status.vendors.alicloud.images`), `ResourcesReady` and `QuotaAvailable` (the vCPU
view is in `quota`). A NodeClaim appears and the node joins within a few minutes. Launch errors show
in `kubectl describe nodeclaim` and `kubectl -n scarpenter-system logs deploy/scarpenter`; boot
problems in the instance's console output in the ECS console. Delete the Pod and the node goes away.

## GPU instances

NVIDIA GPU types (`gn5`, `gn5i`, `gn6i`, `gn6v`, `gn6e`, `gn7i`, `gn8ia`, `gn8is`) need nothing in the NodeClass:
the node loads Flatcar's NVIDIA driver and advertises `nvidia.com/gpu` (see
[Run GPU workloads](run-gpu-workloads.md) and the
[reference](../reference/alicloud-driver.md#gpu-instance-types) for the offered families). The vGPU families
(`vgn*`, `sgn*`, `*-vws*`) are never offered. A NodePool that can launch GPU types lists the startup taint
`scarpenter.sparecores.com/gpu-pending:NoSchedule`, and the account needs vCPU quota and stock for the type.

**Unreachable upstreams.** China regions are unsupported. If your nodes run where Flatcar's release server,
GitHub or `nvcr.io` cannot be reached, mirror those three:

- the driver sysexts: build the tree with `hack/nvidia-mirror.sh` and set `nvidia.driverBaseURL`
  (`--nvidia-driver-base-url`); Ignition then writes the driver image itself and the initrd downloads nothing;
- the Kubernetes and runtime sysexts and the Flatcar files: `artifacts.sysextBaseURL` and
  `artifacts.flatcarBaseURL`;
- the device plugin image: `nvidia.devicePlugin.image.repository`, pointing at a registry you pull through
  (for example Alibaba Cloud Container Registry).

## Limits

- No China regions; regions Keeper does not list are never used.
- No IPv6 (`publicIPv6: true` is refused). Nodes without a public IPv4 are refused unless the
  NodeClass brings its own vSwitches (which then need a NAT gateway or a proxy).
- `metadataOptions.httpTokens` is `optional` only. Pods can reach the metadata service and the user
  data only if the NodeClass sets `spec.instanceMetadata.access: Open`: by default it is restricted to the
  node's own components ([instance metadata access](../reference/node-bootstrap.md#instance-metadata-access)).
- No local-disk families (`i`, `d`), bare metal (`ebm`, `ebmgn` GPU servers too), `scc`, dedicated hosts, EIPs or NAT gateways;
  no billing retention; at most 15 `spec.tags`. More in the [reference](../reference/alicloud-driver.md#deferred).

## Troubleshooting

| Symptom | Fix |
|---|---|
| `ImagesReady=False`: `ImageNotFound`, `ImageAmbiguous` | No (or more than one) `Available` image named `Flatcar-<channel>-<version>-<arch>` of yours in the region: import or copy it (step 1), or name it by ID in `resources.images.use`. |
| `ImageVersionMismatch`, `ImageVersionUnknown` | The image's name or tags do not say the pinned release, or disagree: fix the tags. |
| `Forbidden.RAM` | The policy is missing, or its `my-cluster` is not your `cluster.name`. |
| `RegionUnauthorized` or `RealNameAuthenticationError` | The account cannot use the region: verify the account or remove the region from `regions`. |
| `QuotaAvailable=False`, `QuotaExceed.*` | The vCPU limit of the region is used up: raise it in Quota Center. |
| The instance runs but the node never joins | Its console: an image without the OEM id `aliyun` or with `ImdsSupport` other than `v1` never gets its user data; else the API endpoint is not reachable. |
| `Account.Arrearage` | The account has no balance: top it up. |
