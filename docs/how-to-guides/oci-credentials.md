# Set up OCI credentials

The controller calls the OCI Compute, Virtual Network, Identity and Limits APIs and needs an OCI identity
for that. The nodes it launches get **no** credentials (no instance principal). This guide creates the
policy, wires an identity into the chart and shows the alternatives. The policy is
[`oci-iam-policy.txt`](../reference/oci-iam-policy.txt); none of it has run against a real tenancy yet (the
verbs each call needs are verified by the first live run). OCIDs and names below are placeholders.

- [Choose a mode](#choose-a-mode)
- [The compartment](#the-compartment)
- [The policy](#the-policy)
- [An API key](#an-api-key)
- [Workload identity federation](#workload-identity-federation-keyless)
- [In the chart](#in-the-chart)
- [Troubleshooting](#troubleshooting)

## Choose a mode

| Mode (`--oci-auth`, chart `vendors.oci.credentials.mode`) | The controller authenticates with | Use when |
|---|---|---|
| `apiKey` | an API signing key of a user: from a Secret mounted as a directory (`--oci-credentials-dir`), from the `OCI_CLI_*` environment, or from an OCI config file with a profile (`--oci-config-file`, `--oci-config-profile`; session-token profiles work too) | it works anywhere; the key is a long-lived credential: rotate it |
| `workloadIdentityFederation` | the ServiceAccount's projected token (or any OIDC token in a file), exchanged at an IAM identity domain for a short-lived user principal session token | the identity domain can trust the cluster's token issuer. No stored key; the one secret is the confidential application's client secret. **Recommended where it is available**, and the mode of the CI live lane. |
| `none` (chart only) | nothing is wired: set `OCI_CLI_*` with `extraEnv`, or `configFile` | you manage the environment or a mounted config file yourself |

`instancePrincipal` (a controller on an OCI compute instance in a dynamic group) and `okeWorkloadIdentity`
(a controller on OKE) are **not supported yet**; the controller refuses them and names the supported modes.

The trust boundary is the compartment: whoever can run a Pod as the controller's ServiceAccount, or read its
Secret, acts with the policy. OCI policy conditions cannot test freeform tags on every verb, so the policy
cannot be limited to the instances of one cluster. **Use a compartment dedicated to the nodes**: the driver
refuses to delete an instance without its cluster's tag, but that check and the compartment are the only
walls.

## The compartment

Create (or pick) the compartment, note its OCID and subscribe the tenancy to every region you want nodes in
(a one-way, tenancy-wide action). The controller targets this one compartment: set `--oci-compartment`
(chart: `vendors.oci.compartment`, required). Instances, boot volumes, the managed network and the default
image lookup live in it; brought subnets, groups and images may live in other compartments the identity can
read. There is no per-NodeClass compartment.

## The policy

Create a group (the identity below becomes a member), then the statements of
[`oci-iam-policy.txt`](../reference/oci-iam-policy.txt), replacing `<group>` and `<compartment>`:

- `manage instance-family`, `use volume-family` and `use virtual-network-family` in the compartment: the
  instances, their boot volumes and VNICs, and the subnet and groups of a launch;
- `inspect tenancies` and `inspect compartments` in the tenancy: the region subscriptions and the
  availability domains;
- `inspect resource-availability` and `inspect limits` **in the tenancy**: the quotas are read on the root
  compartment, which is how the usage of the whole account is seen;
- `manage virtual-network-family` in the compartment, unless every NodeClass brings both its subnets and its
  network security groups (`use`) and none has `isolation: run`: SCarpenter creates the network without a
  subnets `use`, a group per NodeClass without a groups `use`, and a group per run for `isolation: run`.

Tenancy-level statements belong to the root compartment's policy; the compartment ones can be in the
compartment's own policy.

## An API key

1. Create a user (or use a service user) in the group, and an API signing key for it
   (`oci iam user api-key upload`, or the console). Note the fingerprint.
2. Put the pieces in a Secret with exactly these keys (the optional `passphrase` for an encrypted key):

   ```sh
   kubectl -n scarpenter-system create secret generic oci-api-key \
     --from-literal=tenancy=ocid1.tenancy.oc1..example \
     --from-literal=user=ocid1.user.oc1..example \
     --from-literal=fingerprint=aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99 \
     --from-literal=region=eu-frankfurt-1 \
     --from-file=key=./oci-api-key.pem
   ```

   `region` is the tenancy's home region: the region subscriptions are read there.
3. The chart mounts the Secret as a directory and passes `--oci-credentials-dir`.

Other sources of the same key: the environment (`OCI_CLI_TENANCY`, `OCI_CLI_USER`, `OCI_CLI_FINGERPRINT`,
`OCI_CLI_REGION`, and `OCI_CLI_KEY_FILE` or `OCI_CLI_KEY_CONTENT`, optional `OCI_CLI_PASSPHRASE`), read when
no directory and no config file is set; and an OCI config file with `--oci-config-file` and
`--oci-config-profile` (the development path; `~/.oci/config` of the person running the controller).

## Workload identity federation (keyless)

The controller reads a token from a file at every exchange, sends it with the confidential application's
client ID and secret to the identity domain's token endpoint and gets a short-lived user principal session
token, which the SDK refreshes. What the operator creates:

1. An **IAM identity domain** (the tenancy's default one is enough) and, in it, a **confidential
   application** with the client credentials grant; note its client ID and client secret.
2. A **trust** in the domain for the token issuer (the cluster's ServiceAccount issuer, or the OIDC issuer of
   your CI), so that the subject of the token (for a cluster:
   `system:serviceaccount:scarpenter-system:scarpenter`) maps to an identity domain user and group.
3. The **group** of that identity with the [policy](#the-policy).
4. The client secret in a Secret:

   ```sh
   kubectl -n scarpenter-system create secret generic oci-client --from-literal=client-secret=...
   ```

The token's audience is what the domain's trust expects: the chart projects a ServiceAccount token with the
audience you set (`audience`). The exact audience, the claims the trust must match and the lifetime of the
session token are **unverified** against a real identity domain (spike S11 of the plan); the first live run
settles them. The controller needs the exchange to work at start-up, because the tenancy OCID is a claim of the
session token.

## In the chart

```yaml
vendors:
  aws:
    enabled: false          # or leave it on to run both drivers
  oci:
    enabled: true
    compartment: ocid1.compartment.oc1..example
    credentials:
      mode: workloadIdentityFederation
      workloadIdentityFederation:
        region: eu-frankfurt-1
        identityDomainURL: https://idcs-example.identity.oraclecloud.com
        clientId: scarpenter-client
        clientSecret:
          existingSecret: oci-client
        audience: oci-federation
```

For an API key: `mode: apiKey` and `apiKey.existingSecret: oci-api-key`. The flags and mounts are in the
[controller reference](../reference/controller.md). OCI is served from the catalog embedded in the binary
until Keeper serves it (`catalog.embeddedVendors`, default `oci`), so an OCI-only deployment needs no Keeper
key.

## Troubleshooting

| Symptom | Cause |
|---|---|
| the controller exits with "--oci-compartment ... is required" | the oci vendor is enabled without a compartment OCID |
| "the OCI mode ... is not supported yet" | `instancePrincipal` and `okeWorkloadIdentity` are deferred: use `apiKey` or `workloadIdentityFederation` |
| "reading the key ... of the OCI credentials Secret" | the Secret lacks one of `tenancy`, `user`, `fingerprint`, `region`, `key`, or it is not mounted |
| `NotAuthenticated` (401) | the key, the fingerprint or the user do not match, or the federation token was refused by the identity domain |
| `NotAuthorizedOrNotFound` (404) on the first call | the policy lacks a statement, or the compartment OCID is wrong: OCI does not say which |
| a region is missing from the offerings | the tenancy is not subscribed to it (`inspect tenancies`, the console) |
