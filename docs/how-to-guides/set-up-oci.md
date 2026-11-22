# Set up Oracle Cloud Infrastructure (OCI)

The `oci` driver launches Flatcar instances in one compartment of your tenancy, in the subscribed regions a
NodeClass allows, as fixed sizes of the flexible shapes (on demand and preemptible), from a Flatcar image you
import, on a network it creates or one you bring. Prices and sizes come from Keeper, which needs an API key
([Keeper API key](install.md#keeper-api-key)). Details: [OCI driver](../reference/oci-driver.md).

**Supported** (amd64 and arm64). The real-cloud test lane runs on pull requests that touch OCI code,
nightly, and before every release.

## What you need

- **A compartment dedicated to the nodes** and its OCID. OCI policies cannot be limited to one cluster's
  instances, so the compartment is the boundary. One compartment per controller, not per NodeClass.
- **The regions subscribed** in the tenancy (tenancy-wide and permanent); others are never offered.
- **Limits and quotas** for the shape families you allow, such as `standard-e5-core-count` and
  `standard-e5-memory-count` (some names carry a scope: `standard-e3-core-ad-count`,
  `standard-a1-core-regional-count`). A compartment quota must set every name Compute checks (for A1 both
  the plain and the `-regional-` pair); a `QuotaExceeded` launch names the missing one.
  `custom-image-count` is 100 per region (25 on pay-as-you-go and trial).
- **Reachability.** Nodes reach `cluster.apiServerEndpoint`, `https://<channel>.release.flatcar-linux.net`
  and the sysext-bakery releases on GitHub (through the created network's internet gateway or your
  subnet's route). The controller reaches the OCI APIs and, in the keyless mode, the identity domain. It
  also reaches Keeper, and needs its API key like every vendor (`keeper.apiKey.existingSecret`).

## 1. The Flatcar image

OCI has no public Flatcar image and custom images cannot be shared across tenancies: import Flatcar's
`oraclecloud` image into your tenancy per region, architecture and version. SCarpenter never changes images.

1. Download `flatcar_production_oraclecloud_image.img` (qcow2, about 600 MB) from
   `https://stable.release.flatcar-linux.net/<amd64|arm64>-usr/<version>/` (4757.2.3 or newer), verify its
   signature, and upload it to Object Storage (other regions can import it through a read-only
   pre-authenticated request).
2. Import it per region, with this display name, operating system and tags:

   ```sh
   oci compute image import from-object-uri --region eu-frankfurt-1 \
     --compartment-id ocid1.compartment.oc1..example --uri "<object or PAR URL>" \
     --source-image-type QCOW2 --launch-mode PARAVIRTUALIZED \
     --operating-system Flatcar --operating-system-version 4757.2.3 \
     --display-name flatcar-stable-4757.2.3-amd64 \
     --freeform-tags '{"flatcar-channel":"stable","flatcar-version":"4757.2.3","arch":"amd64","os":"flatcar"}'
   ```

3. Add each shape SCarpenter may launch to the image's compatibility list
   (`oci compute image-shape-compatibility-entry add --image-id <image OCID> --shape-name VM.Standard.E5.Flex`).
   A custom image has only the shapes you add: add every flexible shape your NodePools allow, of every family
   (`VM.Optimized3.Flex`, `VM.Standard3.Flex`, ...), and check with
   `oci compute image-shape-compatibility-entry list --image-id <image OCID>`. A shape that is missing is not
   launched (`ImageNotCompatible`, OCI itself answers a bare 400 `InvalidParameter`): its offerings are
   unavailable for an hour, with the Event `OfferingRejected` on the NodeClass.
4. **arm64 only**: add an image capability schema (firmware `UEFI_64`, paravirtualized launch mode, network,
   boot and remote data volume types) and the entry `VM.Standard.A1.Flex` (A2 and A4 are untested).

Images in another compartment: set `vendors.oci.imageCompartment` (the tenancy OCID for the root) and grant
`read instance-images` there. Or list OCIDs per region in `resources.images.use.ids`.

## 2. Credentials

The controller's identity belongs to a group with the statements of
[`oci-iam-policy.txt`](../reference/oci-iam-policy.txt) (tenancy-level ones in the root compartment's
policy). Its `manage virtual-network-family` is needed unless every NodeClass brings its subnets and
network security groups and none uses `isolation: run`. Nodes get no credentials.

**Keyless (`workloadIdentityFederation`)**, where an IAM identity domain can trust your cluster's
ServiceAccount issuer (it must be reachable by OCI). The controller exchanges its projected token for a
short-lived session token; the only stored secret is a client secret. In the identity domain:

1. Create a confidential application with the client credentials grant; note its client ID and secret.
2. Create an identity propagation trust for the issuer that maps the subject
   `system:serviceaccount:scarpenter-system:scarpenter` to a domain user in the group; note its audience.
3. Store the client secret:
   `kubectl -n scarpenter-system create secret generic oci-client --from-literal=client-secret='<secret>'`.
   (The test lane runs this exchange with a GitHub OIDC token, not the chart's projected token.)

**API key (`apiKey`)**, which works anywhere. Upload an API signing key for a user in the group
(`oci iam user api-key upload`), then (`region` is the home region; add `passphrase` for an encrypted key):

```sh
kubectl -n scarpenter-system create secret generic oci-api-key \
  --from-literal=tenancy=ocid1.tenancy.oc1..example --from-literal=user=ocid1.user.oc1..example \
  --from-literal=fingerprint=aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99 \
  --from-literal=region=eu-frankfurt-1 --from-file=key=./oci-api-key.pem
```

**Mode `none`**: the chart wires nothing; set the `OCI_CLI_*` variables with `extraEnv`, mount an OCI config
file (`credentials.configFile`, `configProfile`), or pass `--oci-auth` and its flags with `extraArgs`.

## 3. Enable the driver

```yaml
vendors:
  aws:
    enabled: false                       # on by default
  oci:
    enabled: true
    compartment: ocid1.compartment.oc1..example
    credentials:
      mode: workloadIdentityFederation   # or apiKey, with apiKey.existingSecret: oci-api-key
      workloadIdentityFederation:
        region: eu-frankfurt-1           # the home region
        identityDomainURL: https://idcs-0123456789abcdef.identity.oraclecloud.com   # host only
        clientId: <client ID>
        clientSecret: {existingSecret: oci-client}                                  # key client-secret
        audience: <the trust's audience>
```

Flags behind these values: [controller reference](../reference/controller.md). Set
`vendors.oci.imageCompartment` only if the images are elsewhere.

## 4. A first NodeClass and NodePool

Without `resources`, SCarpenter uses subnets and groups tagged `scarpenter-use=true`, else creates a VCN, an
internet gateway, a route table, a security list, a public regional subnet and a network security group,
and deletes them once no node uses them. With the `direct` template the control plane must reach the nodes'
private addresses, so bring a subnet it routes to; with a created network, use
[`tailscale`](choose-a-network-template.md#with-tailscale). More: [minimal](../examples/oci-minimal.yaml),
[managed](../examples/oci-managed.yaml), [bring your own](../examples/oci-bring-your-own.yaml).

```yaml
apiVersion: scarpenter.sparecores.com/v1alpha1
kind: SCNodeClass
metadata: {name: oci}
spec:
  flatcar: {version: "4757.2.3"}
  kubernetes: {version: "v1.37.1"}
  network:
    template: tailscale
    tailscale: {version: v1.98.10, tags: [tag:k8s-node],
                authKeySecretRef: {name: ts-node-key, namespace: scarpenter-system, key: authkey}}
  vendors:
    oci: {regions: [eu-frankfurt-1]}
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata: {name: oci}
spec:
  template:
    spec:
      nodeClassRef: {group: scarpenter.sparecores.com, kind: SCNodeClass, name: oci}
      requirements:
        - {key: scarpenter.sparecores.com/vendor, operator: In, values: [oci]}
        - {key: node.kubernetes.io/instance-type, operator: In, values: [oci.VM.Standard.E5.Flex.1-4]}
  limits: {cpu: "8"}
```

## 5. Check that it works

```sh
kubectl get scnodeclass oci -o jsonpath='{.status.vendors.oci.conditions}'
kubectl run oci-test --image=registry.k8s.io/pause:3.10 \
  --overrides='{"apiVersion":"v1","spec":{"nodeSelector":{"karpenter.sh/nodepool":"oci"}}}'
kubectl get nodeclaims -w           # then: kubectl delete pod oci-test
```

`Ready` on the NodeClass, and `VendorReady`, `ImagesReady` and `ResourcesReady` under `status.vendors.oci`
should be `True`; `status.vendors.oci.images` lists one image per region and architecture. Launch errors
are NodeClaim events (`kubectl describe nodeclaim`), credential errors are in the controller log
(`kubectl -n scarpenter-system logs deployment/scarpenter`); a node that never joins: the instance's serial
console.

## Limits

- No data volumes, local storage or profiles; boot volumes of 50 to 32768 GB, not `lower-cost`.
- One compartment per controller; no instance principal or OKE workload identity.
- A created network has no NAT gateway and no IPv6: `publicIPv4: false` or `publicIPv6: true` needs a
  brought subnet.
- No extended-memory sizes; A2 and A4 untested; no billing-period retention.
  More in the [reference](../reference/oci-driver.md#deferred).

## Troubleshooting

| Symptom | Fix |
|---|---|
| `vendors.oci.compartment is required` / `--oci-compartment ... is required` | Set `vendors.oci.compartment`. |
| `reading the key "..." of the OCI credentials Secret` | Add the missing key (`tenancy`, `user`, `fingerprint`, `region`, `key`). |
| `NotAuthenticated` (401) | Key, fingerprint and user do not match, or the domain refused the token (trust, audience, subject). |
| `NotAuthorizedOrNotFound` (404) | A policy statement is missing or an OCID is wrong; OCI does not say which. |
| `ImagesReady=False` | No `flatcar-<channel>-<version>-<arch>` image in the image compartment, or a different `flatcar-version` tag. |
| `QuotaExceeded` | Raise the limit or compartment quota; set every name the error lists. |
| `DailyLimit` | The tenancy hit OCI's daily resource creation limit: the region takes no launch until the next 00:00 UTC (Oracle documents no reset time). Deleting nodes does not help; upgrade the account type or ask Oracle to raise the limit. |
| Preemptible "Out of host capacity" | Common on OCI: allow more shape families in the NodePool. |
| A region is missing from the offerings | Subscribe the tenancy to it. |
