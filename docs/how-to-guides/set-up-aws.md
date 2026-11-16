# Set up Amazon Web Services (AWS)

The AWS driver launches EC2 instances with `RunInstances` (no launch templates) from the
official Flatcar AMIs, in every region of the account the NodeClass allows, on-demand
and Spot. It finds or creates the subnets, security groups and placement groups it
needs, and places nodes by the account's vCPU quotas.

**Supported.** The real-cloud test lane runs on pull requests that touch AWS code, nightly,
and before every release.

## What you need

- **An AWS account** and permission to create an IAM policy and a role (or user).
- **The Spot service-linked role**, once per account, if you launch Spot capacity:
  `aws iam create-service-linked-role --aws-service-name spot.amazonaws.com` (an error
  that it already exists means the account is set up). Without it a Spot launch fails
  with `AuthFailure.ServiceLinkedRoleCreationNotPermitted`.
- **vCPU quotas.** They are per region, per instance class, and separate for On-Demand
  and Spot. A new account has 5 Standard vCPUs and 0 for GPU and other classes; ask for
  an increase in Service Quotas before you rely on them (see [quotas](../reference/aws-driver.md#quotas)).
- **A network.** By default the driver uses the subnets you tagged
  `scarpenter.sparecores.com/use=true`, else the default VPC, else a VPC it creates.
  Nodes get a public IPv4 address; `publicIPv4: false` needs your own subnets with a
  NAT gateway or a proxy.
- **Reachability.** The nodes must reach `cluster.apiServerEndpoint` and the artifact
  hosts (the Flatcar release server and the sysext-bakery releases on GitHub, or your
  mirrors). The controller must reach the regional EC2, Service Quotas and STS
  endpoints.

## 1. The Flatcar image

Nothing to do. The driver looks up the official AMI of the NodeClass's release in every
region: owner `075585003325`, named `Flatcar-<channel>-<version>-hvm` (amd64) or
`Flatcar-<channel>-<version>-arm64-hvm`. A region where the release is not published
(for example `ap-northeast-3` for stable) reports its images not ready and is not used.

Your own AMIs: select them with `spec.vendors.aws.resources.images.use` (the
[selector](../reference/resource-selectors.md#aws)); one not named like the official ones
needs the tags `flatcar-channel` and `flatcar-version`. Another release is never launched.

## 2. Credentials

The controller needs an IAM identity; the nodes get none (unless you set
`spec.vendors.aws.instanceProfile`). Create the policy from
[`aws-iam-policy.json`](../reference/aws-iam-policy.json), taken from the release you
install. Replace `CLUSTER_NAME` with `cluster.name` and `123456789012` with your account
ID. Delete the optional statements you do not need: `OptionalManagedNetwork*` (needed
only where the driver creates the network: a region without a default VPC, `create`
for `subnets`, or `publicIPv6` on a default VPC without IPv6),
`OptionalPassInstanceProfileRole` (else replace `NODE_ROLE_NAME`) and
`OptionalCustomerKmsKey*` (else replace `KEY_ID`). The [IAM table](../reference/aws-driver.md#iam)
explains each statement.

```bash
sed -e 's/CLUSTER_NAME/my-cluster/g' -e 's/123456789012/<account id>/g' aws-iam-policy.json > policy.json
aws iam create-policy --policy-name scarpenter-my-cluster --policy-document file://policy.json
```

**Web identity (recommended, `mode: webIdentity`).** The controller's ServiceAccount
token is exchanged for an IAM role, with no stored secret. AWS must be able to fetch
the cluster's ServiceAccount issuer over HTTPS:

- On EKS: `eksctl utils associate-iam-oidc-provider --cluster my-cluster --approve`.
- On a self-managed cluster: set `--service-account-issuer` (and
  `--service-account-jwks-uri`) of the API server to an HTTPS URL you control, publish
  the output of `kubectl get --raw /.well-known/openid-configuration` and
  `kubectl get --raw /openid/v1/jwks` at those paths (as `application/json`, with a
  public CA certificate; republish the jwks when the keys rotate), and register it with
  `aws iam create-open-id-connect-provider --url <issuer> --client-id-list sts.amazonaws.com`.

Then create a role that trusts the provider for the chart's ServiceAccount, and attach
the policy. Save the trust policy as `trust.json`, with your issuer without `https://`
(on EKS, `oidc.eks.<region>.amazonaws.com/id/<ID>`):

```json
{"Version": "2012-10-17", "Statement": [{
  "Effect": "Allow",
  "Principal": {"Federated": "arn:aws:iam::123456789012:oidc-provider/oidc.example.com/my-cluster"},
  "Action": "sts:AssumeRoleWithWebIdentity",
  "Condition": {"StringEquals": {
    "oidc.example.com/my-cluster:aud": "sts.amazonaws.com",
    "oidc.example.com/my-cluster:sub": "system:serviceaccount:scarpenter-system:scarpenter"}}}]}
```

```bash
aws iam create-role --role-name scarpenter-my-cluster --assume-role-policy-document file://trust.json
aws iam attach-role-policy --role-name scarpenter-my-cluster \
  --policy-arn arn:aws:iam::123456789012:policy/scarpenter-my-cluster
```

**EKS Pod Identity (`mode: none`).** Install the `eks-pod-identity-agent` add-on, create
the role as above but trusting the service principal `pods.eks.amazonaws.com` (actions
`sts:AssumeRole` and `sts:TagSession`), and run
`aws eks create-pod-identity-association --cluster-name my-cluster --namespace scarpenter-system --service-account scarpenter --role-arn <role arn>`.
The same mode also picks up the instance profile of the node the controller runs on.

**Static keys (`mode: secret`, the fallback).** An IAM user with only this policy, its
access key in a Secret of the release namespace; rotate it and restart the controller:

```bash
kubectl -n scarpenter-system create secret generic aws-credentials \
  --from-literal=AWS_ACCESS_KEY_ID=... --from-literal=AWS_SECRET_ACCESS_KEY=...
```

## 3. Enable the driver

AWS is enabled by default (`vendors.aws.enabled: true`). Set the credentials mode:

```yaml
vendors:
  aws:
    credentials:
      mode: webIdentity
      webIdentity:
        roleArn: arn:aws:iam::123456789012:role/scarpenter-my-cluster
      # or mode: none (Pod Identity), or mode: secret with
      # secret: {existingSecret: aws-credentials}   # sessionTokenKey for temporary keys
```

With `webIdentity` the chart mounts a projected token (audience `sts.amazonaws.com`) and
annotates the ServiceAccount with `eks.amazonaws.com/role-arn`. Optional: `region`
(the controller's `AWS_REGION`; default `homeRegion`), `homeRegion` (the region of the
account-wide listings, default `us-east-1`), `ipv4Price` (overrides Keeper's IPv4 price).

## 4. A first NodeClass and NodePool

This is [`aws-minimal.yaml`](../examples/aws-minimal.yaml) with a NodePool. For the
`tailscale` template see [choose a network template](choose-a-network-template.md); for
other networks, [`aws-opt-in-tags.yaml`](../examples/aws-opt-in-tags.yaml),
[`aws-bring-your-own.yaml`](../examples/aws-bring-your-own.yaml) and
[`aws-managed.yaml`](../examples/aws-managed.yaml).

```yaml
apiVersion: scarpenter.sparecores.com/v1alpha1
kind: SCNodeClass
metadata: {name: aws}
spec:
  flatcar: {version: "4757.2.3"}
  kubernetes: {version: "v1.37.1"}
  network: {template: direct}
  vendors:
    aws: {regions: [us-east-1]}
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata: {name: aws}
spec:
  template:
    spec:
      nodeClassRef: {group: scarpenter.sparecores.com, kind: SCNodeClass, name: aws}
      requirements:
        - {key: scarpenter.sparecores.com/vendor, operator: In, values: [aws]}
        - {key: karpenter.sh/capacity-type, operator: In, values: [spot, on-demand]}
  limits: {cpu: "16"}
```

## 5. Check that it works

```bash
kubectl get scnodeclass aws        # READY True
kubectl get scnodeclass aws -o jsonpath='{range .status.vendors.aws.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
```

`VendorReady`, `ImagesReady` and `ResourcesReady` must be `True` (reason `ImagesPartial`:
some regions have no image and are skipped). `QuotaAvailable=False` only says that some
quota is used up. Then start a Pod that only an AWS node can take:

```bash
kubectl run aws-test --image=registry.k8s.io/pause:3.10 \
  --overrides='{"spec":{"nodeSelector":{"scarpenter.sparecores.com/vendor":"aws"}}}'
kubectl get nodeclaims -w
```

Errors show in the NodeClaim's conditions and Events, the SCNodeClass's Events and the
controller log (`kubectl -n scarpenter-system logs deploy/scarpenter`). A node that does
not join: read the instance's serial console (EC2 console, "Get system log").

## Limits

- No launch templates, no EFA, no multiple network interfaces, no Dedicated Hosts or
  Capacity Blocks, no capacity reservations as a launch target.
- Spot interruption notices are not handled (no EventBridge or SQS queue).
- `mac*` instance types are not offered; local zones and Wavelength zones are not used.
- User-data is limited to 16 KiB.

More in the [AWS driver reference](../reference/aws-driver.md#deferred).

## Troubleshooting

| Symptom | Fix |
|---|---|
| `InvalidIdentityToken`, "couldn't retrieve verification key" | AWS cannot read the issuer's discovery documents: check the URL, HTTPS, and republish the jwks after a key rotation. |
| `AccessDenied` on `AssumeRoleWithWebIdentity` | The trust policy: provider ARN, `aud`, and `sub` (namespace and ServiceAccount name). |
| `UnauthorizedOperation` on `RunInstances` or `CreateTags` | `CLUSTER_NAME` in the policy differs from `cluster.name`. |
| `UnauthorizedOperation` on `CreateVpc` or `CreateSubnet` | Add the `OptionalManagedNetwork*` statements, or bring subnets with `use`. |
| `AuthFailure.ServiceLinkedRoleCreationNotPermitted` | Create the Spot service-linked role ([What you need](#what-you-need)). |
| `VcpuLimitExceeded`, `QuotaAvailable=False` | Ask for a quota increase for the class and region. |
| `ImagesReady=False`, `ImageNotFound` | The Flatcar release is not published in that region: pick another region or release. |
