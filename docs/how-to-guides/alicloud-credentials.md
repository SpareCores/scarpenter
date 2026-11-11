# Set up Alibaba Cloud credentials

The controller calls ECS and VPC, and needs a RAM identity for that. The nodes it launches get **no** cloud credentials
(no RAM instance role). This guide creates the RAM policy and role, wires them into the chart, and shows the alternatives.
Everything here is derived from [`alicloud-ram-policy.json`](../reference/alicloud-ram-policy.json) and the
[Alibaba Cloud driver reference](../reference/alicloud-driver.md#credentials-and-the-ram-policy); none of it has run against a real
account yet ([what is not verified](../reference/alicloud-driver.md#not-verified-against-alibaba-cloud-yet)).

- [Choose a mode](#choose-a-mode)
- [The RAM policy](#the-ram-policy)
- [OIDC role](#oidc-role-recommended)
- [Access key](#access-key-the-fallback)
- [Images and the account](#images-and-the-account)
- [Troubleshooting](#troubleshooting)

## Choose a mode

| Mode (`vendors.alicloud.credentials.mode`) | The controller authenticates with | Use when |
|---|---|---|
| `oidc` (default) | the ServiceAccount's projected token, exchanged for a RAM role through a RAM OIDC identity provider | any cluster whose ServiceAccount issuer Alibaba Cloud can reach. No long-lived secret. |
| `none` | what the SDK's default chain finds | the instance RAM role of the node the controller runs on, or a credentials URI |
| `secret` | `ALIBABA_CLOUD_ACCESS_KEY_ID` and `ALIBABA_CLOUD_ACCESS_KEY_SECRET` from a Secret you create | the cluster cannot federate, as the fallback. Long-lived keys: rotate them. |

## The RAM policy

[`docs/reference/alicloud-ram-policy.json`](../reference/alicloud-ram-policy.json) is the policy: reads for everything, creates
under the request tag and mutations under the resource tag `scarpenter.sparecores.com/cluster`. Copy it for your release, replace
`my-cluster` with the value of `cluster.name`, and create the policy. Whether Alibaba Cloud evaluates `acs:RequestTag` on
`RunInstances` as the policy assumes is *unverified*; if it does not, widen the first statement for the lane first and tighten it with the
result.

## OIDC role (recommended)

1. Create a RAM OIDC identity provider for the cluster's ServiceAccount issuer (its issuer URL, its JWKS, the client ID
   that is the token's audience, `sts.aliyuncs.com` by default).
2. Create a RAM role that trusts the provider for the ServiceAccount of the controller (condition on `oidc:sub`
   `system:serviceaccount:<namespace>:<service account>`) and attach the policy.
3. Set the chart values:

```yaml
vendors:
  alicloud:
    enabled: true
    homeRegion: ap-southeast-1
    credentials:
      mode: oidc
      oidc:
        roleArn: acs:ram::123456789012:role/scarpenter
        providerArn: acs:ram::123456789012:oidc-provider/my-cluster
```

The chart projects the token to `/var/run/secrets/alibabacloud/token` and sets `ALIBABA_CLOUD_ROLE_ARN`,
`ALIBABA_CLOUD_OIDC_PROVIDER_ARN` and `ALIBABA_CLOUD_OIDC_TOKEN_FILE`. The self-managed-cluster provider setup is *unverified*
(plan 21 step 0).

## Access key (the fallback)

```yaml
vendors:
  alicloud:
    enabled: true
    credentials:
      mode: secret
      secret:
        existingSecret: alicloud-keys   # keys ALIBABA_CLOUD_ACCESS_KEY_ID and ALIBABA_CLOUD_ACCESS_KEY_SECRET
```

Use a RAM user with only the policy attached, never the account's root key.

## Images and the account

The images are yours (rung 3 of the image ladder): one custom image per region and architecture named
`Flatcar-<channel>-<version>-<arch>` with the tags `flatcar-channel` and `flatcar-version`
([Images](../reference/alicloud-driver.md#images)). The account needs real-name verification and a balance; spot
instances can be refused by account rules (`InvalidAccount.NotSupportSpot`); the vCPU limits of a new account are small
(`DescribeAccountAttributes` shows them, the controller reports them as the quota metrics).

## Troubleshooting

| Symptom | Look at |
|---|---|
| `Forbidden.RAM` on a launch | the policy's conditions (request tag) and the region |
| `RegionUnauthorized` | the account or role cannot use the region; remove it from `regions` |
| `ImagesReady=False` | no image of the pinned release in the region: the name, the tags, the owner |
| the node never joins | the console of the instance: Ignition's `aliyun` provider needs `HttpTokens: optional` and an image with `ImdsSupport=v1` |
