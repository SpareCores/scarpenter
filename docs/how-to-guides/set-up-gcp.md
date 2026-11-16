# Set up Google Cloud (GCP)

The `gcp` driver launches Compute Engine instances in one project, booted from Flatcar's public GCE
images, and creates (or uses) the network, subnetworks and firewall rules they need. The controller
authenticates with workload identity federation, a service account key or the metadata server.
Details are in the [GCP driver reference](../reference/gcp-driver.md).

**Preview.** The driver is implemented and covered by unit and offline tests, but it has not run against the real cloud yet: expect rough edges and report them.

## What you need

- **A project dedicated to the nodes**, with the Compute Engine API enabled
  (`gcloud services enable compute.googleapis.com --project=my-project`). Note its ID (`my-project`) and
  number (`123456789012`). IAM conditions cannot test labels, so the roles cannot be limited to the
  cluster's instances; a dedicated project also keeps other workloads' SSH keys and quotas away.
- **Quotas** in the regions you use: `CPUS`, the series' own (`N2_CPUS`, ...), `CPUS_ALL_REGIONS`,
  `PREEMPTIBLE_CPUS` for Spot, and `IN_USE_ADDRESSES` (one external IPv4 per node by default).
- **Reachability**: the nodes must reach `cluster.apiServerEndpoint` (see
  [choose a network template](choose-a-network-template.md)), `{channel}.release.flatcar-linux.net` and
  `github.com` (sysexts), unless you set mirrors (`artifacts.*BaseURL`). The controller must reach
  `compute.googleapis.com` (and `sts.googleapis.com`, `iamcredentials.googleapis.com` with federation).
- **Organization policies**: `constraints/compute.trustedImageProjects` must allow `kinvolk-public`;
  `constraints/iam.disableServiceAccountKeyCreation` rules out the key mode.

## 1. The Flatcar image

Nothing to do for amd64: the driver boots `kinvolk-public`'s image of the pinned release, by name
(`flatcar-stable-4757-2-3` for `4757.2.3`), and refuses an image of another release. Flatcar publishes no
GCE arm64 image, so arm64 offerings stay unavailable unless you set `vendors.gcp.imageProject` to a
project with your own images of the same name pattern (`...-arm64`), or an `images` selector
([images](../reference/gcp-driver.md#images)).

## 2. Credentials

Create the custom roles from the repository's files and bind them at the project. The managed role
covers the network and firewall rules SCarpenter creates; it is needed unless every NodeClass brings its
own subnetworks and firewall tags (`use`, see [bring your own resources](bring-your-own-resources.md)).

```sh
gcloud iam roles create scarpenter --project=my-project --file=docs/reference/gcp-iam-role.yaml
gcloud iam roles create scarpenterManaged --project=my-project --file=docs/reference/gcp-iam-role-managed.yaml
gcloud iam service-accounts create scarpenter --project=my-project
for r in scarpenter scarpenterManaged; do
  gcloud projects add-iam-policy-binding my-project --role=projects/my-project/roles/$r \
    --member=serviceAccount:scarpenter@my-project.iam.gserviceaccount.com
done
```

Nodes get no service account. If a NodeClass sets `vendors.gcp.serviceAccount`, the controller's identity
also needs `iam.serviceAccounts.actAs` on it.

**Workload identity federation (recommended, cluster outside Google Cloud).** Google must reach the
cluster's ServiceAccount issuer, or you upload its JWKS (`--jwk-json-path`). The chart's ServiceAccount is
`scarpenter` in `scarpenter-system` with the install of [install.md](install.md).

```sh
SUBJECT=system:serviceaccount:scarpenter-system:scarpenter
POOL=projects/123456789012/locations/global/workloadIdentityPools/my-pool
gcloud iam workload-identity-pools create my-pool --project=my-project --location=global
gcloud iam workload-identity-pools providers create-oidc my-provider --project=my-project \
  --location=global --workload-identity-pool=my-pool --issuer-uri=https://oidc.example.com \
  --attribute-mapping=google.subject=assertion.sub --attribute-condition="assertion.sub == '$SUBJECT'"
gcloud iam service-accounts add-iam-policy-binding scarpenter@my-project.iam.gserviceaccount.com \
  --role=roles/iam.workloadIdentityUser --member="principal://iam.googleapis.com/$POOL/subject/$SUBJECT"
gcloud iam workload-identity-pools create-cred-config $POOL/providers/my-provider \
  --service-account=scarpenter@my-project.iam.gserviceaccount.com \
  --credential-source-file=/var/run/secrets/gcp/token --credential-source-type=text \
  --output-file=credential-configuration.json
kubectl -n scarpenter-system create configmap gcp-credential-configuration \
  --from-file=credential-configuration.json
```

The file holds no secret; its token path must stay `/var/run/secrets/gcp/token`, where the chart
projects the token.

**A service account key** (`mode: secret`): `gcloud iam service-accounts keys create key.json
--iam-account=scarpenter@my-project.iam.gserviceaccount.com`, then `kubectl -n scarpenter-system create
secret generic gcp-key --from-file=key.json`. Keys do not expire: rotate them. The Secret may also hold
any other credential file the Google libraries accept (an `external_account` file, for example).

**The metadata server** (`mode: none`, controller on GKE or GCE): nothing is mounted; Application Default
Credentials find the identity. On GKE with Workload Identity, bind the roles to the Pod's principal, or
annotate the ServiceAccount (`serviceAccount.annotations`) for a Google service account. Set
`vendors.gcp.project` anyway, since the cluster's project is otherwise taken.

## 3. Enable the driver

```yaml
vendors:
  gcp:
    enabled: true
    project: my-project            # required with federation, which carries no project
    credentials:
      mode: workloadIdentity       # or secret, or none
      workloadIdentity:
        configMap: gcp-credential-configuration
        audience: //iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/my-pool/providers/my-provider
      # secret:
      #   existingSecret: gcp-key
      #   key: key.json
```

`audience` is the provider's resource name. The chart fails with a message when a required value is
missing.

## 4. A first NodeClass and NodePool

[`gcp-minimal.yaml`](../examples/gcp-minimal.yaml) names no network: SCarpenter uses the project's
`default` network only when no firewall rule applies to every instance (the stock `default-allow-*`
rules do), and otherwise creates its own VPC, subnetworks and firewall rules. Firewall rules are always
created unless you `use` a tag. See also [`gcp-managed.yaml`](../examples/gcp-managed.yaml) and
[`gcp-bring-your-own.yaml`](../examples/gcp-bring-your-own.yaml).

```yaml
apiVersion: scarpenter.sparecores.com/v1alpha1
kind: SCNodeClass
metadata: {name: gcp-minimal}
spec:
  flatcar: {version: "4757.2.3"}
  kubernetes: {version: "v1.37.1"}
  network: {template: direct}
  vendors:
    gcp: {project: my-project, regions: [europe-west3]}
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata: {name: gcp}
spec:
  template:
    spec:
      nodeClassRef: {group: scarpenter.sparecores.com, kind: SCNodeClass, name: gcp-minimal}
      requirements:
        - {key: scarpenter.sparecores.com/vendor, operator: In, values: [gcp]}
        - {key: kubernetes.io/arch, operator: In, values: [amd64]}
        - {key: karpenter.sh/capacity-type, operator: In, values: [on-demand]}
  limits: {cpu: "8"}
```

`vendors.gcp.project` in the NodeClass is optional; when set it must equal the chart's.

## 5. Check that it works

```sh
kubectl get scnodeclass gcp-minimal          # Ready=True
kubectl get scnodeclass gcp-minimal -o jsonpath='{.status.vendors.gcp.conditions}'
kubectl run probe --image=registry.k8s.io/pause:3.10 \
  --overrides='{"spec":{"nodeSelector":{"scarpenter.sparecores.com/vendor":"gcp"}}}'
kubectl get nodeclaims -w
```

Under `status.vendors.gcp.conditions`, `VendorReady`, `ImagesReady` and `ResourcesReady` should be
`True`. Launch errors are events on the NodeClaim (`kubectl describe nodeclaim`) and in the controller's
log. The instance has the NodeClaim's name; a node that never joins shows why on its serial console
(`gcloud compute instances get-serial-port-output <name> --zone=<zone>`).

## Limits

- One project per controller; arm64 only with your own images; no GPU, TPU, bare metal, sole-tenant or
  custom machine types.
- Spot VMs only (not preemptible or Flex-start); a preempted VM is deleted and replaced, without
  using the 30-second notice.
- `networking.publicIPv4: false` requires your own subnetworks with egress (Cloud NAT or a proxy).
- Quota is learnt from `QUOTA_EXCEEDED` errors; billing retention is off.
  More in the [reference](../reference/gcp-driver.md#deferred).

## Troubleshooting

| Symptom | Fix |
|---|---|
| Controller exits: "the GCP project is not known" | Set `vendors.gcp.project`. |
| `VendorReady` false: "not the project of the controller" | Make the NodeClass's `vendors.gcp.project` equal `vendors.gcp.project` of the chart, or drop it. |
| `ImagesReady` false for amd64 | Check the Flatcar version exists in `kinvolk-public`, and `constraints/compute.trustedImageProjects`. |
| `ImagesReady` false for arm64 | Expected: set `imageProject` or an `images` selector, or restrict the NodePool to amd64. |
| `403` on `regions.get`, or permission denied | Bind the roles in the project; enable the Compute Engine API. |
| `ResourcesReady` false, firewall or network create denied | Bind the `scarpenterManaged` role, or `use` your own subnetworks and firewall tag. |
| Launch `OrganizationPolicyViolation` | An organization policy forbids the request; the message names the constraint. |
| `Quota 'CPUS' exceeded` | Raise the region's quota, or lower the NodePool limit. |
