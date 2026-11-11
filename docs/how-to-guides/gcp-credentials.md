# Set up GCP credentials

The controller calls the Compute Engine API and needs a Google identity for that. The nodes it launches
get **no** cloud credentials (unless you attach a service account). This guide creates the custom roles,
wires an identity into the chart, and shows the alternatives. It is derived from
[`gcp-iam-role.yaml`](../reference/gcp-iam-role.yaml), [`gcp-iam-role-managed.yaml`](../reference/gcp-iam-role-managed.yaml)
and the [GCP driver reference](../reference/gcp-driver.md#iam); none of it has run against a real project
yet ([what is not verified](../reference/gcp-driver.md#what-is-not-verified-against-gcp-yet)). Project IDs,
numbers and names below are placeholders.

- [Choose a mode](#choose-a-mode)
- [A project for the nodes](#a-project-for-the-nodes)
- [The roles](#the-roles)
- [Workload identity federation](#workload-identity-federation-recommended)
- [A service account key](#a-service-account-key-the-fallback)
- [The metadata server](#the-metadata-server-gke-and-gce)
- [In the chart](#in-the-chart)
- [Egress for nodes without a public address](#egress-for-nodes-without-a-public-address)
- [Troubleshooting](#troubleshooting)

## Choose a mode

| Mode (`vendors.gcp.credentials.mode`) | The controller authenticates with | Use when |
|---|---|---|
| `workloadIdentity` (default) | the ServiceAccount's projected token, exchanged by Google's Security Token Service for a short-lived access token (an `external_account` credential configuration) | a cluster outside Google Cloud whose ServiceAccount issuer Google can reach (or whose JWKS you uploaded to the provider). No long-lived secret. |
| `secret` | a service account key (JSON) from a Secret you create | the cluster cannot federate; the fallback. Long-lived key: rotate it. |
| `none` | Application Default Credentials as the pod finds them | the controller runs on GKE with Workload Identity, or on a GCE VM whose service account has the roles (the metadata server) |

The trust boundary is the cluster: whoever can run a Pod as the controller's ServiceAccount, or read its
Secret, acts with the roles. GCP IAM conditions cannot test labels, so the roles cannot be limited to the
instances of one cluster. **Use a project dedicated to the nodes**: the driver refuses to delete an
instance without its cluster's label, but a project shared with other workloads also shares their
project-wide SSH keys and their quotas.

## A project for the nodes

Create (or pick) the project, enable the Compute Engine API, and note its ID (`my-project`) and number
(`123456789012`). The controller targets this one project: set `vendors.gcp.project` (the flag
`--gcp-project`). Without it the controller uses the project of its credentials, which a federated
identity does not carry.

## The roles

```sh
gcloud iam roles create scarpenter --project=my-project --file=docs/reference/gcp-iam-role.yaml
# Only if SCarpenter creates the networks and firewall rules itself (the default without `use`):
gcloud iam roles create scarpenterManaged --project=my-project --file=docs/reference/gcp-iam-role-managed.yaml
```

The first role holds what launching, listing and deleting nodes and reading the project's facts and quotas
need; the second the networks, subnetworks and firewall rules SCarpenter owns. Skip the second when your
own tooling provides the subnetworks and firewall tags and the NodeClass only uses them
([bring your own resources](bring-your-own-resources.md)). `vendors.gcp.serviceAccount` additionally
needs `iam.serviceAccounts.actAs` on that service account. Bind the roles to the identity of the next
sections at the project:

```sh
gcloud projects add-iam-policy-binding my-project --member=<principal> --role=projects/my-project/roles/scarpenter
gcloud projects add-iam-policy-binding my-project --member=<principal> --role=projects/my-project/roles/scarpenterManaged
```

## Workload identity federation (recommended)

1. A workload identity pool and an OIDC provider for the cluster's ServiceAccount issuer. The issuer must
   be reachable by Google, or you upload its JWKS (`--jwk-json-path`):

   ```sh
   gcloud iam workload-identity-pools create my-pool --project=my-project --location=global
   gcloud iam workload-identity-pools providers create-oidc my-provider --project=my-project --location=global \
     --workload-identity-pool=my-pool --issuer-uri=https://oidc.example.com \
     --attribute-mapping=google.subject=assertion.sub \
     --attribute-condition="assertion.sub == 'system:serviceaccount:scarpenter-system:scarpenter'"
   ```

2. The identity the controller acts as. Impersonating a service account works for every Compute method and is
   the safe default; granting the roles directly to the federated principal
   (`principal://iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/my-pool/subject/system:serviceaccount:scarpenter-system:scarpenter`)
   avoids the service account but whether Compute accepts it for every method SCarpenter calls is
   unverified.

   ```sh
   gcloud iam service-accounts create scarpenter --project=my-project
   gcloud iam service-accounts add-iam-policy-binding scarpenter@my-project.iam.gserviceaccount.com \
     --role=roles/iam.workloadIdentityUser \
     --member="principal://iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/my-pool/subject/system:serviceaccount:scarpenter-system:scarpenter"
   ```

   Then bind the [roles](#the-roles) to `serviceAccount:scarpenter@my-project.iam.gserviceaccount.com`.

3. The credential configuration, a file Google's tooling writes for you:

   ```sh
   gcloud iam workload-identity-pools create-cred-config \
     projects/123456789012/locations/global/workloadIdentityPools/my-pool/providers/my-provider \
     --service-account=scarpenter@my-project.iam.gserviceaccount.com \
     --credential-source-file=/var/run/secrets/gcp/token --credential-source-type=text \
     --output-file=credential-configuration.json
   kubectl -n scarpenter-system create configmap gcp-credential-configuration \
     --from-file=credential-configuration.json
   ```

   The file holds no secret: it names the provider and where the token is. `credential_source.file` must be
   `/var/run/secrets/gcp/token`, where the chart projects the token.

## A service account key (the fallback)

```sh
gcloud iam service-accounts keys create key.json --iam-account=scarpenter@my-project.iam.gserviceaccount.com
kubectl -n scarpenter-system create secret generic gcp-key --from-file=key.json
```

Bind the [roles](#the-roles) to that service account. Keys do not expire: rotate them, and prefer federation.
An organization policy often forbids key creation (`constraints/iam.disableServiceAccountKeyCreation`).

## The metadata server (GKE and GCE)

On GKE with Workload Identity, or on a VM whose service account has the roles, nothing is wired:
Application Default Credentials find the identity. Use `mode: none`, and `vendors.gcp.project` if the
credentials carry no project.

## In the chart

```yaml
vendors:
  aws:
    enabled: false          # or leave it on to run both drivers
  gcp:
    enabled: true
    project: my-project
    credentials:
      mode: workloadIdentity
      workloadIdentity:
        configMap: gcp-credential-configuration
        audience: //iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/my-pool/providers/my-provider
```

`audience` is the provider's resource name: the projected token carries it, which is what the provider
accepts by default. For a key: `mode: secret` and `secret.existingSecret: gcp-key`. The environment
(`GOOGLE_APPLICATION_CREDENTIALS`) and the mounts are in the [controller reference](../reference/controller.md).
A NodeClass names the project it is for only if it must be self-describing: `vendors.gcp.project` must equal
the controller's.

## Egress for nodes without a public address

`networking.publicIPv4: false` is accepted only with your own subnetworks (`resources.subnets.use`) that
have egress: a Cloud NAT on the subnetwork's region, or a proxy. The default and the managed network have no
NAT, so the node could not fetch its sysexts or container images.

## Troubleshooting

| Symptom | Cause |
|---|---|
| the controller exits with "the GCP project is not known" | federated credentials carry no project: set `vendors.gcp.project` |
| `ValidationSucceeded` false: "not the project of the controller" | the NodeClass's `vendors.gcp.project` differs from `--gcp-project` |
| `ImagesReady` false for amd64 | no image of the pinned Flatcar release in `kinvolk-public` (check the version), or an organization policy `constraints/compute.trustedImageProjects` forbids it |
| `ImagesReady` false for arm64 | there is no arm64 image yet: set `imageProject` or an `images` selector ([images](../reference/gcp-driver.md#images)) |
| launches fail `OrganizationPolicyViolation` | an organization policy forbids the request: the message names the constraint |
| `403` on `regions.get` | the role is not bound in the project, or the Compute Engine API is not enabled |
| `Quota 'CPUS' exceeded` | the region's quota: raise it, or lower the NodePool limit |
