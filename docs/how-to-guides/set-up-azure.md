# Set up Microsoft Azure

The Azure driver launches Flatcar VMs from the Azure Marketplace in one subscription, and creates the
resource group, virtual network and network security group the nodes need unless you bring your own.

**Preview.** A first real-cloud smoke run passed on 2026-11-16 (one amd64 node in centralus, then scale to zero); the
full test lane has not run yet.

## What you need

- **A subscription** and the right to create and assign a custom role (Owner or User Access Administrator).
- **Registered resource providers** `Microsoft.Compute`, `Microsoft.Network` and
  `Microsoft.MarketplaceOrdering` (without them every call fails with `MissingSubscriptionRegistration`):
  ```bash
  for p in Microsoft.Compute Microsoft.Network Microsoft.MarketplaceOrdering; do az provider register --namespace "$p"; done
  ```
- **Quota** in each region: total regional vCPUs, the vCPUs of each size family you allow, Spot vCPUs
  (`lowPriorityCores`) if you launch Spot, and Standard public IP addresses (one per node). A new
  subscription's defaults are small: check them with `az vm list-usage --location westeurope`.
- **Network reachability from the nodes** to `cluster.apiServerEndpoint` (with the `direct` template an
  address routable from the Azure network, see [Choose a network template](choose-a-network-template.md)),
  to `<channel>.release.flatcar-linux.net` and to the sysext-bakery releases on GitHub (or the mirrors of
  `artifacts.*`). Nodes get a public IPv4 address by default; that address is their only egress.

## 1. The Flatcar image

Nothing to build or import. The driver boots the official Marketplace images of publisher `kinvolk`:
`flatcar-container-linux-corevm-amd64`, SKU `<channel>-gen2`, for amd64 and
`flatcar-container-linux-corevm`, SKU `<channel>`, for arm64, at the version the NodeClass pins.

SCarpenter never accepts Marketplace terms. If an image carries a purchase plan whose terms are not
accepted in the subscription, the NodeClass's `ImagesReady` is `False` with reason
`MarketplaceTermsNotAccepted`, and the message holds the exact command; the subscription's owner runs it
once, for example:

```bash
az vm image terms accept --publisher kinvolk --offer flatcar-container-linux-corevm --plan stable
```

## 2. Credentials

Nodes get no cloud credentials; only the controller calls Azure. Create the custom role
[`azure-role.json`](../reference/azure-role.json) in your subscription and assign it there (the driver
creates resource groups and reads the subscription's SKU table and usages):

```bash
SUB=00000000-0000-0000-0000-000000000000   # your subscription
sed "s/00000000-0000-0000-0000-000000000000/$SUB/" docs/reference/azure-role.json > role.json
az role definition create --role-definition @role.json
```

**Workload identity (recommended, chart default).** No long-lived secret: the chart projects a
ServiceAccount token with audience `api://AzureADTokenExchange` and Entra ID exchanges it. Your cluster's
ServiceAccount issuer must be publicly reachable (AKS: `az aks show --query oidcIssuerProfile.issuerUrl`;
otherwise the `issuer` of `kubectl get --raw /.well-known/openid-configuration`).

```bash
az identity create --name scarpenter --resource-group <rg> --location <region>
az role assignment create --role "SCarpenter controller" --scope "/subscriptions/$SUB" \
  --assignee-object-id "$(az identity show -n scarpenter -g <rg> --query principalId -o tsv)" \
  --assignee-principal-type ServicePrincipal
az identity federated-credential create --name scarpenter --identity-name scarpenter --resource-group <rg> \
  --issuer <issuer URL> --subject system:serviceaccount:scarpenter-system:scarpenter \
  --audiences api://AzureADTokenExchange
```

The subject is the controller's ServiceAccount (`scarpenter` in `scarpenter-system` with the commands of
[Install](install.md)). The chart needs the identity's `clientId` (`az identity show`) and the tenant ID.

**Client secret (fallback).** For a cluster whose issuer Azure cannot reach. Rotate the secret.

```bash
az ad sp create-for-rbac --name scarpenter --role "SCarpenter controller" --scopes "/subscriptions/$SUB"
kubectl -n scarpenter-system create secret generic azure-credentials \
  --from-literal=AZURE_CLIENT_ID=<appId> --from-literal=AZURE_TENANT_ID=<tenant> \
  --from-literal=AZURE_CLIENT_SECRET=<password>
```

**None.** The chart wires nothing; the controller tries workload identity from the environment, then the
managed identity of its VM. Use it with the AKS workload identity webhook (`podLabels`
`azure.workload.identity/use: "true"`, `serviceAccount.annotations` `azure.workload.identity/client-id`)
or on an Azure VM whose managed identity has the role. A client secret is not read in this mode.

## 3. Enable the driver

AWS is enabled by default in the chart; turn it off if you do not use it. With workload identity:

```yaml
vendors:
  aws:
    enabled: false
  azure:
    enabled: true
    subscriptionID: "00000000-0000-0000-0000-000000000000"   # required
    credentials:
      mode: workloadIdentity
      workloadIdentity:
        clientId: "<client ID of the identity>"   # required in this mode
        tenantId: "<tenant ID>"                   # required in this mode
```

The chart also annotates the ServiceAccount with `azure.workload.identity/client-id`
(`workloadIdentity.annotateServiceAccount`). With a client secret, set `credentials.mode: secret` and
`credentials.secret.existingSecret: azure-credentials` (the key names default to `AZURE_CLIENT_ID`,
`AZURE_TENANT_ID` and `AZURE_CLIENT_SECRET`: `secret.clientIdKey`, `tenantIdKey`, `clientSecretKey`).

## 4. A first NodeClass and NodePool

[`azure-minimal.yaml`](../examples/azure-minimal.yaml) names no resources: SCarpenter uses resources you
tagged `scarpenter-use=true`, else creates a resource group, a virtual network `10.240.0.0/16` with a
subnet `nodes` and a network security group per region. To bring your own, see
[Bring your own resources](bring-your-own-resources.md) and the
[Azure selectors](../reference/resource-selectors.md#azure). `subscriptionID` in the NodeClass is an
optional guard that must equal the controller's.

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata: {name: azure}
spec:
  template:
    spec:
      nodeClassRef: {group: scarpenter.sparecores.com, kind: SCNodeClass, name: azure-minimal}
      requirements:
        - {key: scarpenter.sparecores.com/vendor, operator: In, values: [azure]}
        - {key: karpenter.sh/capacity-type, operator: In, values: [on-demand]}
  limits: {cpu: "16"}
```

## 5. Check that it works

```bash
kubectl get scnodeclass azure-minimal          # READY True
kubectl get scnodeclass azure-minimal -o jsonpath='{.status.vendors.azure.conditions}'
kubectl run probe --image=registry.k8s.io/pause:3.10 \
  --overrides='{"spec":{"nodeSelector":{"scarpenter.sparecores.com/vendor":"azure"}}}'
kubectl get nodeclaims -w
```

The vendor conditions that matter are `VendorReady`, `ImagesReady` (the Marketplace image per region and
architecture), `ResourcesReady` and `QuotaAvailable`. A failed launch shows on the NodeClaim
(`kubectl describe nodeclaim`) and in the controller's logs; a node that never joins has its boot log in
`az vm boot-diagnostics get-boot-log -g <resource group> -n <vm>`.

## Limits

- One subscription per controller; nodes are IPv4 only.
- Marketplace images only (no galleries or managed images); Gen2 sizes with a SCSI disk controller only
  (NVMe-only sizes, most v6 families, are not offered); the default `Premium_LRS` root disk rules out sizes
  without Premium storage (set `storage.root.storageAccountType: StandardSSD_LRS` to include them).
- `publicIPv4: false` needs your own subnet (`resources.subnets.use`) with its own egress.
- A network security group you bring decides alone: `networking.isolation` has no effect on it.
- `storage.localStorage` supports only `ignore`.
- What is not verified yet and the rest: [Azure driver](../reference/azure-driver.md).

## Troubleshooting

- `helm install` fails with "subscriptionID is required" or "clientId and tenantId are required": set them, or pick mode `secret` or `none`.
- `MissingSubscriptionRegistration`: register the providers of [What you need](#what-you-need).
- `AuthorizationFailed`: the role is not assigned to this identity at the subscription, or the assignment has not propagated yet (minutes).
- `AADSTS700213` or "no matching federated identity record": the federated credential's issuer or subject differs from the cluster's issuer and the ServiceAccount.
- `ImagesReady=False`, `MarketplaceTermsNotAccepted`: run the `az vm image terms accept` command of the message.
- `ImagesReady=False`, `ImagesNotReady` or `ImageVersionMismatch`: the pinned Flatcar version is not in the Marketplace (yet); pin a published one (`az vm image list --publisher kinvolk --all --offer flatcar`).
- `VendorReady=False`, `ValidationFailed`: the message names the field, for example a `subscriptionID` that is not the controller's.
- `QuotaAvailable=False` or launches fail on quota: request more regional, family or Spot vCPUs, or public IPs.
