# Set up AWS credentials

The controller calls EC2 and Service Quotas, and needs an IAM identity for that.
The nodes it launches get **no** cloud credentials (unless you give them an
instance profile). This guide creates the IAM policy and role, wires them into
the chart, and shows the two alternatives. Everything here is derived from
[`aws-iam-policy.json`](../reference/aws-iam-policy.json) and the
[AWS driver reference](../reference/aws-driver.md#iam); none of it has run
against a real account yet (plan 10 does that, see
[what is not verified](../reference/aws-driver.md#what-is-not-verified-against-aws-yet)).

- [Choose a mode](#choose-a-mode)
- [The IAM policy](#the-iam-policy)
- [Web identity](#web-identity-recommended)
- [EKS Pod Identity](#eks-pod-identity)
- [Static keys](#static-keys-the-fallback)
- [Account setup for Spot](#account-setup-for-spot)
- [Troubleshooting](#troubleshooting)

## Choose a mode

| Mode (`vendors.aws.credentials.mode`) | The controller authenticates with | Use when |
|---|---|---|
| `webIdentity` (default) | the ServiceAccount's projected token, exchanged for an IAM role by STS | any cluster whose ServiceAccount issuer AWS can reach: EKS (IRSA), or a self-managed cluster that publishes its issuer. No long-lived secret. |
| `none` | what the SDK's default chain finds | EKS Pod Identity (the agent injects the credentials), or the instance profile of the node the controller runs on |
| `secret` | `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` from a Secret you create | the cluster cannot federate (no public issuer), as the fallback. Long-lived keys: rotate them. |

The trust boundary is the cluster: whoever can run a Pod as the controller's
ServiceAccount (or read its Secret) can act with the role. The policy limits what
that is: instances, volumes and network interfaces can be created and terminated
only with the cluster tag `scarpenter.sparecores.com/cluster=<cluster name>`.

## The IAM policy

[`docs/reference/aws-iam-policy.json`](../reference/aws-iam-policy.json) is the
policy. Copy it for your release (the file of the tag you install), replace four
placeholders and create the policy:

| Placeholder | Replace with |
|---|---|
| `CLUSTER_NAME` | the value of `cluster.name` (`--cluster-name`) |
| `123456789012` | your AWS account ID |
| `NODE_ROLE_NAME` | the role of the instance profile you give nodes (`aws.instanceProfile`), or delete the `OptionalPassInstanceProfileRole` statement |
| `KEY_ID` | the ID of the customer KMS key you encrypt volumes with (`kmsKeyID`), or delete the two `OptionalCustomerKmsKey*` statements |

```bash
sed -e 's/CLUSTER_NAME/my-cluster/g' -e 's/123456789012/<your account id>/g' \
    -e 's/NODE_ROLE_NAME/my-node-role/g' -e 's/KEY_ID/<your key id>/g' \
    aws-iam-policy.json > scarpenter-policy.json
aws iam create-policy --policy-name scarpenter-my-cluster \
  --policy-document file://scarpenter-policy.json
```

What the statements allow, in short (the full table is in the
[AWS driver reference](../reference/aws-driver.md#iam)):

- `Describe` and `QuotaLimits`: read-only calls on everything (EC2 `Describe*`
  and `servicequotas:Get/ListServiceQuota`); EC2 `Describe*` calls cannot be
  limited by tag.
- `RunInstances*`, `TagAtCreation`, `TerminateInstances`, `CreateSecurityGroups`,
  `ChangeSecurityGroups`, `CreatePlacementGroups`, `DeletePlacementGroups`:
  creation requires the cluster tag in the request, and changes and deletions
  require it on the resource.
- **Optional statements, remove what you do not use:**
  - `OptionalManagedNetwork*` (create and delete VPCs, subnets, internet gateways
    and route tables with the cluster tag, `ReplaceRoute` to replace a stale default
    route, IPv6 CIDR association): needed only in regions where an SCNodeClass
    uses (or defaults to) `create` for `subnets`, that is where the account has no
    default VPC. With your own `use` references for the network, delete them.
  - `OptionalPassInstanceProfileRole`: `iam:PassRole` for the one role you give
    nodes through `aws.instanceProfile`, only to `ec2.amazonaws.com`. Without an
    instance profile, nodes hold no credentials and you delete it.
  - `OptionalCustomerKmsKey` and `OptionalCustomerKmsKeyGrant`: encryption of
    volumes with your own key (`kms:Decrypt`, `DescribeKey`,
    `GenerateDataKeyWithoutPlaintext`, and `CreateGrant` for AWS resources only,
    both only through EC2). The default `aws/ebs` key needs none.

## Web identity (recommended)

The Pod's ServiceAccount token is a JWT issued by your cluster's API server.
AWS STS accepts it for a role (`AssumeRoleWithWebIdentity`) if the account has an
IAM OIDC identity provider for the token's issuer, which AWS must be able to
reach over HTTPS to fetch the issuer's signing keys.

### On EKS

EKS already publishes the issuer (the cluster's OIDC provider URL). Create the
IAM provider once per cluster:

```bash
eksctl utils associate-iam-oidc-provider --cluster my-cluster --approve
aws eks describe-cluster --name my-cluster --query cluster.identity.oidc.issuer --output text
# https://oidc.eks.eu-central-1.amazonaws.com/id/EXAMPLED539D4633E53DE1B71EXAMPLE
```

### On a self-managed cluster (k3s, kubeadm, ...)

1. **Give the API server a public issuer.** Choose an HTTPS URL that AWS can
   reach and that you control, for example a static site or a bucket behind a
   domain (`https://oidc.example.com/my-cluster`). Set the issuer on the API
   server (k3s: `--kube-apiserver-arg=service-account-issuer=...`; the old issuer
   stays accepted with a second `service-account-issuer` flag, which avoids
   invalidating tokens that are in use):

   ```text
   --service-account-issuer=https://oidc.example.com/my-cluster
   --service-account-jwks-uri=https://oidc.example.com/my-cluster/openid/v1/jwks
   ```

2. **Publish the two discovery documents** the API server serves, so AWS can read
   them without access to your cluster:

   ```bash
   kubectl get --raw /.well-known/openid-configuration > openid-configuration
   kubectl get --raw /openid/v1/jwks > jwks
   # upload them to
   #   https://oidc.example.com/my-cluster/.well-known/openid-configuration
   #   https://oidc.example.com/my-cluster/openid/v1/jwks
   # with content type application/json, served over HTTPS with a certificate
   # from a public CA.
   ```

   `issuer` in `openid-configuration` must equal the `--service-account-issuer`
   URL exactly, and the file must be republished when the signing keys rotate
   (the jwks changes). They contain only public keys.

3. **Register the issuer with IAM** (once per cluster):

   ```bash
   aws iam create-open-id-connect-provider \
     --url https://oidc.example.com/my-cluster --client-id-list sts.amazonaws.com
   ```

   (A thumbprint is needed only for issuers whose certificate does not chain to a CA
   AWS trusts; `--thumbprint-list` accepts it.)

### The role

The trust policy names the issuer, the audience (`sts.amazonaws.com`, which the chart
sets on the projected token) and the ServiceAccount (`system:serviceaccount:<namespace>:<name>`;
the chart's ServiceAccount is named like the release, `scarpenter`):

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {"Federated": "arn:aws:iam::123456789012:oidc-provider/oidc.example.com/my-cluster"},
    "Action": "sts:AssumeRoleWithWebIdentity",
    "Condition": {
      "StringEquals": {
        "oidc.example.com/my-cluster:aud": "sts.amazonaws.com",
        "oidc.example.com/my-cluster:sub": "system:serviceaccount:scarpenter-system:scarpenter"
      }
    }
  }]
}
```

```bash
aws iam create-role --role-name scarpenter-my-cluster \
  --assume-role-policy-document file://trust-policy.json
aws iam attach-role-policy --role-name scarpenter-my-cluster \
  --policy-arn arn:aws:iam::123456789012:policy/scarpenter-my-cluster
```

On EKS the provider ARN is `arn:aws:iam::123456789012:oidc-provider/oidc.eks.<region>.amazonaws.com/id/<ID>`
and the condition keys carry the same path (without `https://`).

### In the chart

```yaml
cluster: {name: my-cluster, apiServerEndpoint: https://api.example.com:6443}
vendors:
  aws:
    credentials:
      mode: webIdentity
      webIdentity:
        roleArn: arn:aws:iam::123456789012:role/scarpenter-my-cluster
```

The chart sets `AWS_ROLE_ARN` and `AWS_WEB_IDENTITY_TOKEN_FILE` on the container
and mounts a projected ServiceAccount token (audience `sts.amazonaws.com`, one
hour, refreshed by the kubelet) at `/var/run/secrets/aws/token`, so it works on any
cluster; it also annotates the ServiceAccount with `eks.amazonaws.com/role-arn`
(`annotateServiceAccount`, for clusters that run the EKS pod identity webhook,
which would inject the same). The chart sets `AWS_REGION` too (`vendors.aws.region`,
else the home region, default `us-east-1`): the web identity provider builds its
STS client from the SDK's default region, and without one every call fails with
"Missing Region". The controller reaches `sts.<region>.amazonaws.com` and the
regional EC2 and Service Quotas endpoints: allow that egress.

## EKS Pod Identity

Pod Identity needs the `eks-pod-identity-agent` add-on and an association; the agent
then injects the credentials into the Pod, and the chart wires nothing:

```bash
aws eks create-pod-identity-association --cluster-name my-cluster \
  --namespace scarpenter-system --service-account scarpenter \
  --role-arn arn:aws:iam::123456789012:role/scarpenter-my-cluster
```

The role's trust policy allows the service principal instead of an OIDC provider:

```json
{"Version": "2012-10-17", "Statement": [{
  "Effect": "Allow",
  "Principal": {"Service": "pods.eks.amazonaws.com"},
  "Action": ["sts:AssumeRole", "sts:TagSession"]
}]}
```

```yaml
vendors:
  aws:
    credentials: {mode: none}
```

The same `none` mode covers credentials from the instance profile of the node the
controller runs on (the node role then needs the policy, and every Pod on that node
can use it unless IMDS is restricted: prefer one of the other modes).

## Static keys (the fallback)

Create an IAM user (or use a role with a long-lived key source) that has the policy
and nothing else, create an access key, and keep it in a Secret of the release
namespace:

```bash
kubectl -n scarpenter-system create secret generic aws-credentials \
  --from-literal=AWS_ACCESS_KEY_ID=AKIAEXAMPLE \
  --from-literal=AWS_SECRET_ACCESS_KEY=example-secret
```

```yaml
vendors:
  aws:
    credentials:
      mode: secret
      secret:
        existingSecret: aws-credentials
        # sessionTokenKey: AWS_SESSION_TOKEN   # for temporary credentials
```

The values go into the container as environment variables from the Secret. Rotate
the key regularly (create a second key, update the Secret, restart the controller,
delete the old key): the controller picks credentials up when it starts.

## Account setup for Spot

Launching Spot capacity needs the account's service-linked role for Spot
(`AWSServiceRoleForEC2Spot`). It is an account setup step, not a permission of the
policy above; without it a Spot launch fails with
`AuthFailure.ServiceLinkedRoleCreationNotPermitted` (class `Fatal`). Create it once:

```bash
aws iam create-service-linked-role --aws-service-name spot.amazonaws.com
```

(An error that it already exists means the account is set up.)

## Troubleshooting

| Symptom (controller log, Events on the SCNodeClass) | Cause |
|---|---|
| `Missing Region` | no `AWS_REGION` and no default region: set `vendors.aws.region` (the chart does by default). |
| `WebIdentityErr` / `InvalidIdentityToken` ("couldn't retrieve verification key") | AWS cannot read the issuer's discovery documents: the issuer URL, HTTPS, and the published jwks (after a key rotation, republish it). |
| `AccessDenied` on `AssumeRoleWithWebIdentity` | the trust policy: the provider ARN, the `aud` and `sub` conditions (the ServiceAccount name and namespace). |
| `UnauthorizedOperation` on `RunInstances` or `CreateTags` | the cluster tag: `CLUSTER_NAME` in the policy must be `cluster.name`; the policy is scoped by it. |
| `UnauthorizedOperation` on `CreateVpc`, `CreateSubnet` | the optional `OptionalManagedNetwork*` statements, needed in regions without a default VPC (or use `use` references). |
| `AuthFailure.ServiceLinkedRoleCreationNotPermitted` | [Account setup for Spot](#account-setup-for-spot). |
| `Unsupported` / `VcpuLimitExceeded` | not credentials: [quotas](../reference/aws-driver.md#quotas) and the NodeClass's `QuotaAvailable`. |
