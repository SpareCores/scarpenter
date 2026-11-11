# 17 — Azure driver and L5 lane

Depends on: 14 (the shared groundwork: the optional `images` kind, the stopped-node rule,
the resolution order and the hand-off contract), 08 (the model driver), 10 (the model lane), and the
vendor-resources, node-options and quota machinery of 04 and 06. Independent of the other
vendor plans. No image-import plan is needed (rung 1 of the image ladder, ADR-021, below). Read
`docs/plans/v1/CONVENTIONS.md` first. Spec: overview §5.8, §5.9, §7, §9; 00a §6, §9,
§11; ADR-002, ADR-017, ADR-018, ADR-020, ADR-021. Researched 2026-11-10 from documentation,
the public Flatcar release server and the committed Keeper snapshot; no Azure call
was made. Every fact not confirmed by a source is marked *(unverified)* and is on the
live verification list at the end.

## Goal

`pkg/vendors/azure`: launch, list and delete Flatcar VMs in one Azure subscription,
complete against an in-memory ARM fake and the conformance suite, with vendor-native
node settings, resources, quotas and error classes, plus the Azure L5 lane (workflow
parts, scripts, canary, sweeper). Plan 08's scope for AWS is the template; this plan
only spells out what differs. No Azure calls while writing code (CONVENTIONS rule 8);
the coordinator runs the first live lane.

## 1. Image policy: rung 1 (Azure Marketplace, official Flatcar offers)

**Rung 1 works, with confidence high for amd64 and medium for arm64.** Rungs 2 to 4 are
not needed.

| Question | Answer | Source |
|---|---|---|
| Does Flatcar publish for Azure? | Yes: `flatcar_production_azure_image.vhd.bz2` for both `amd64-usr` (584 MiB) and `arm64-usr` (471 MiB) on the release server (checked for 4757.2.3), plus the `oem-azure` sysext-style OEM image. | https://stable.release.flatcar-linux.net/amd64-usr/current/ , https://stable.release.flatcar-linux.net/arm64-usr/current/ |
| Official docs page? | Yes: "Microsoft Azure" in the Flatcar docs. | https://www.flatcar.org/docs/latest/deploy/cloud/azure/ |
| Public image offered by the vendor? | Azure Marketplace, publisher `kinvolk` (Microsoft lists Flatcar among its endorsed distributions). Offers: `flatcar-container-linux-free` (legacy `flatcar-container-linux`; **needs the one-time marketplace terms**, issue 82), `flatcar-container-linux-corevm-amd64` (**"doesn't require accepting image terms or specifying plan information"** per the docs) and `flatcar-container-linux-corevm` (arm64; Gen2 only). | docs page above; https://github.com/flatcar/Flatcar/issues/82 ; https://learn.microsoft.com/azure/virtual-machines/linux/endorsed-distros |
| SKUs | amd64: `alpha-gen2`, `beta-gen2`, `stable-gen2`, `lts2024-gen2` (Gen1 without the suffix). arm64: `alpha`, `beta`, `stable` (no LTS listed, although the release server has an arm64 LTS Azure VHD). The docs list the SKUs per arch, not per offer, and their `corevm` URN examples are old (`3815.2.0`): whether the `corevm` offers carry every current release and an `lts2024-gen2` SKU is *unverified* (the canary's first check). | docs page above |
| Versions | Marketplace image versions are the Flatcar release numbers (URN examples `...:stable-gen2:4547.1.0`, `...:3975.2.0`). `az vm image list --all` lists every published one. | docs page, https://capz.sigs.k8s.io/self-managed/flatcar |
| Community gallery | Exists (`flatcar-23485951-...`, definition `flatcar-stable-amd64`) but holds only recent **stable amd64** images, per the docs ("for now"). No LTS, no arm64, no old pins. Not used. | docs page above |
| Ignition platform | `azure` is a supported platform id; "Ignition will read its configuration from the custom data". The Flatcar Azure image sets it itself; nothing for SCarpenter to configure. | https://coreos.github.io/ignition/supported-platforms/ |
| User-data size | Custom data is Base64; the docs say it "can't exceed 64 KB", the API reference (`OSProfile.customData` in `armcompute`) that the decoded binary array is at most 65535 bytes, i.e. 87,380 Base64 characters. SCarpenter's 16 KiB budget fits. Custom data is not served by IMDS (docs FAQ; it reaches the VM in `ovf-env.xml`, kept root-only under `/var/lib/waagent`: better for the bootstrap token than AWS user-data); it cannot be changed after creation. | https://learn.microsoft.com/en-us/azure/virtual-machines/custom-data |
| Import from URL | Not needed. (For the record: VHD fixed-size page blob into a storage account, then a managed disk or gallery image per region; the uploader `ghcr.io/flatcar/azure-flatcar-image-upload` does it. Regional storage cost and time make the marketplace the better route.) | docs page above |
| Cost, scope, sharing | Marketplace images cost nothing to store; the `free`/`corevm` plans are $0. They are global offers, available per region (a pinned version may be missing in a region: `ImagesPartial`, as on AWS). | issue 82 |

**Pinned version to image.** `ImageRequest{Region, Arch, Channel, Version}` (plus, from plan 14, the optional `images`
reference) resolves to the URN `kinvolk:<offer>:<sku>:<version>`:

| Arch | Offer | SKU |
|---|---|---|
| amd64 | `flatcar-container-linux-corevm-amd64` | `<channel>-gen2`; for `lts`, the `lts*-gen2` SKU that has the version (`ListSKUs` then `Get`; the LTS stream name carries the year, `lts2024` today) |
| arm64 | `flatcar-container-linux-corevm` | `<channel>`; `lts` has no arm64 SKU (unverified): the region reports the image not ready with that reason |

`VirtualMachineImages.Get(location, publisher, offer, sku, version)` returns the image
(and its `plan`, if any). Exactly one must exist: none is `NotFound` (`ImageNotFound`).
Results are cached for 1 hour. `Image.Ref` is the URN. The `images` kind (plan 14, A1) is
optional and rarely needed here: `images.use.ids` per region holds URNs (or the ID of a
gallery image) to override the marketplace lookup; the version is checked against the URN
or the gallery image's version (ADR-017). `azure.imageOwner` overrides the
publisher (the AWS `imageOwner` analogue); offers are driver constants.

**Plan and terms.** If the resolved image has a purchase `plan`, the VM must carry
`plan{name: <sku>, product: <offer>, publisher}` and the subscription must have accepted
the terms (`MarketplaceAgreements.Get` / `Create`). The two `corevm` offers should have
none (docs; arm64 *unverified*). **SCarpenter never accepts terms**: legal consent
belongs to the account owner. If terms are needed and not accepted, `ImagesReady=False`
with the reason `MarketplaceTermsNotAccepted` and the exact `az vm image terms accept`
command; the operator's IaC can accept them (a marketplace agreement resource).

**Generation.** Gen2 only in v1 (arm64 is Gen2 only; amd64 Gen2 is the Flatcar default).
Sizes without `HyperVGenerations` V2 are dropped from the launchable types (Gen1-only
families such as Av2; decided per SKU from the SKU table, not by family name); Gen1 SKUs are
deferred.

## 2. Hand-off: `spec.vendors.azure`

Azure's unit is the **region** (location name, Keeper `api_reference`, e.g. `westeurope`;
verified in the snapshot) with **logical zones** `1`, `2`, `3` (Keeper zone `0` means a
region without zones; eastus2 also has a `4` row in the snapshot, to be ignored unless the
SKU data lists it). Zone mapping is the identity (overview §5.8: global zone names); logical
zones map to different physical zones per subscription, which the driver may expose later
as a label from the locations API's `availabilityZoneMappings` (`logicalZone`,
`physicalZone` in `armsubscriptions`). Zone `0` is the zone name of non-zonal regions.

```yaml
vendors:
  azure:
    regions: [westeurope, northeurope]
    subscriptionID: 00000000-0000-0000-0000-000000000000   # optional guard: must equal the controller's
    imageOwner: ""                                          # publisher; empty = kinvolk
    storage:                    # layer 2 over the built-in defaults, section 7
      root: {storageAccountType: Premium_LRS, diskSizeGB: 64, caching: ReadWrite}
    networking: {publicIPv4: true}
    adminSSHPublicKey: ""       # optional, see below
    resources:
      resourceGroups:  {use: {ids: {westeurope: [my-env-westeurope]}}}
      subnets:         {use: {ids: {westeurope: ["/subscriptions/.../virtualNetworks/my-env-westeurope/subnets/nodes"]}}}
      securityGroups:  {use: {ids: {westeurope: ["/subscriptions/.../networkSecurityGroups/my-env-westeurope-nodes"]}}}
      proximityPlacementGroups: {create: {lifecycle: {policy: perUse}}}
```

**Resource kinds.** A VM needs a NIC; a NIC needs a subnet; everything lives in a resource
group; a public IP is needed for egress on a subnet without a NAT (newer subnets are
private by default, `defaultOutboundAccess: false`, and default outbound access is being
retired; whether an operator's subnet has it is *unverified* and should not be relied on). All are free except the public IP
(Standard SKU, about $0.005/h, *Keeper has no price*; constant like AWS, overridable).

| Kind | Scope / default policy | `use` forms | `create` | What your IaC must output |
|---|---|---|---|---|
| `resourceGroups` | region / `retain` | `ids` (name or ARM ID), `name` (glob, `{region}`), `tags` (one pair server-side, the rest client-side), `expression` (Resource Graph) | yes; name `scarpenter-<cluster>-resource-groups-<region>`; **deleted only if it carries the marker and holds no resource without it** (an RG delete removes everything inside) | the group's name per region, or the `scarpenter-use` tag on it |
| `subnets` (the VNet and its subnet, one unit like AWS's VPC) | region / `idleTTL` | `ids` (full subnet ARM ID), `name` pattern `<vnet>/<subnet>` (matched across the subscription's VNets), `tags` (**match the VNet's tags**: Azure subnets have no tags; `match.cidr`/`within`/`network`/`state` are client-side, `expression` is Resource Graph over the VNet; every subnet of a matching VNet with free addresses is a candidate). No `{zone}`: subnets are regional (validation error) | yes: VNet `10.240.0.0/16` and one `/20` subnet `nodes`, IPv4 only | the subnet's ARM ID per region, or the opt-in tag on the VNet |
| `securityGroups` (NSG) | region / `idleTTL` | `ids` (ARM ID), `name`, `tags` (tag the NSG for nodes `scarpenter-use`), `filters` `resourceGroup`/`location` | yes: no inbound rule except `spec.networking.inbound`, plus the isolation rules below | the NSG's ARM ID per region (hand over only an NSG nodes may use) |
| `applicationSecurityGroups` | region (per NodeClass) / `idleTTL`; `runApplicationSecurityGroups`: run / `perUse` (internal kinds, like AWS `runSecurityGroups`) | not configurable | yes | nothing |
| `proximityPlacementGroups` | run / `perUse` | `ids`, `name` | yes (placement profile) | nothing |
| `publicIPs`, `networkInterfaces` | node / `perUse` (internal, never `use`) | no | yes, by `Prepare`, one per (NodeClaim, region) | nothing |
| `images` (not managed; optional) | region key; marketplace offers are global | `ids` per region or under `*` (URN, or a gallery image ID), `name` (`publisher:offer:sku:<version glob>` for the marketplace, `<gallery>/<image>/<version glob>` for a gallery; `orderBy: name` for the newest version), `match` (`architecture`, `state`, `includeDeprecated`: one `Get` per version), no `expression` on the marketplace | never (ADR-021) | the marketplace lookup of section 1 | nothing (rung 1) |

Without `use` (ADR-022): tagged resource groups, VNets and NSGs (discovery, planned); Azure
has no default resource, so then everything is created: a resource group, a VNet with subnet,
an NSG per NodeClass (on-demand creation, first launch waits); else a stated failure. The `use`
route is the normal one for an operator with their own IaC.

Isolation. Azure's NSGs have built-in `AllowVnetInBound` (priority 65000) that lets every
node of the VNet reach every other, so `isolation: none|run` needs an explicit deny. A
managed NSG gets: an allow from the NodeClass's application security group to itself
(`nodeclass`; the NIC joins the group in its IP configuration) or from the run group
(`run`, a second group per run key), a `DenyVnetInbound` rule at priority 4000, then
`spec.networking.inbound` at 100 to 3999. `isolation: run` without a run key is one
implicit run (same semantics as AWS). A NIC takes **one** NSG, so a brought NSG (`use`) is
attached as it is and decides alone: unlike AWS, where the run group is a second group whose
allow is added, the NIC's run ASG membership grants nothing unless the brought NSG has a rule
for it (a brought NSG would allow the nodes' range at a low priority). With
`securityGroups.use`, `isolation` therefore has no effect, which the reference must say and
`VendorReady` should warn about (open question 11). A subnet NSG, where the user attached one,
applies as well (both must allow).

**Account scope and credentials.**

- One controller = one subscription (`--azure-subscription-id`, `AZURE_SUBSCRIPTION_ID`;
  required, no discovery). `spec.vendors.azure.subscriptionID` is only a guard: a
  mismatch gives `VendorReady=False` (`ValidationFailed`). Tenant from the credential.
  Public cloud only (Keeper has no sovereign clouds).
- `azidentity`: a chain of `WorkloadIdentityCredential` (env `AZURE_CLIENT_ID`,
  `AZURE_TENANT_ID`, `AZURE_FEDERATED_TOKEN_FILE`, set by the chart or by the Azure
  workload identity webhook), `EnvironmentCredential` (secret or certificate, mode
  `secret`), `ManagedIdentityCredential` (AKS, Azure VMs); `AzureCLICredential` only under
  a development flag. Chart value `vendors.azure.credentials.mode`:
  `workloadIdentity` (recommended), `secret`, `none`, mirroring AWS.
- Workload identity federation: a federated identity credential on a user-assigned managed
  identity (or app registration): issuer = the cluster's service account issuer (public
  discovery document, as for self-managed AWS), subject
  `system:serviceaccount:<namespace>:<sa>`, audience `api://AzureADTokenExchange`. For
  GitHub the audience is the same and the subject is the immutable form
  (`repo:<org>@<org id>/<repo>@<repo id>:environment:e2e`) of the CI account's setup.
- Roles: a custom role at the subscription (or resource group) scope; actions in
  `docs/reference/azure-role.json` (this plan writes it): `Microsoft.Compute/virtualMachines/{read,write,delete}`,
  `disks/{read,write,delete}`, `proximityPlacementGroups/*`, `locations/*/read`,
  `locations/usages/read`, `skus/read`, `locations/publishers/artifacttypes/offers/skus/versions/read` (marketplace image reads),
  `Microsoft.Network/{networkInterfaces,publicIPAddresses,networkSecurityGroups,applicationSecurityGroups,virtualNetworks}/*`
  (optional statement for the managed network), `.../subnets/join/action`,
  `.../networkSecurityGroups/join/action`, `.../publicIPAddresses/join/action`,
  `.../applicationSecurityGroups/joinIpConfiguration/action`, `Microsoft.Resources/subscriptions/resourceGroups/{read,write,delete}`
  (optional), `Microsoft.Resources/subscriptions/resources/read`, `Microsoft.MarketplaceOrdering/agreements/read`,
  `Microsoft.Network/locations/usages/read`. Resource providers `Microsoft.Compute`,
  `Microsoft.Network`, `Microsoft.MarketplaceOrdering` must be registered (a
  `MissingSubscriptionRegistration` error is `Fatal` with that hint). The exact list is
  *unverified* and is checked by the e2e lane.

**SSH key.** Azure's API wants a credential for a Linux VM: an `adminPassword` or
`linuxConfiguration.ssh.publicKeys` (*unverified* whether a custom-data-only VM is
accepted; Flatcar's docs pass `--ssh-key-values` and say the argument is not needed when
Ignition supplies the `core` user's key, which is about the CLI, not the API). The driver
sets `adminUsername: core`, `disablePasswordAuthentication: true` and one public key:
`azure.adminSSHPublicKey` (a public key the operator provides), else an
ed25519 key generated at `Init` whose private half is discarded (nobody can use it;
access is Ignition's business). `allowExtensionOperations: false`, VM agent on.

**What the operator's IaC must provide, and what SCarpenter self-manages.** Provided by
the operator (any tool, CLI or portal), all of it optional for the driver but the normal
route:

1. A **resource group** per region: its name (`resourceGroups.use.ids`) or the
   opt-in tag `scarpenter-use` on it for discovery (planned).
2. A **VNet with a subnet** per region (the subnet's ARM ID, or the opt-in tag on the VNet),
   and an **NSG** (its ARM ID or tag) whose rules allow the nodes' ranges, UDP 41641 and
   ICMP for the `tailscale` and `direct` templates. The subnet has no NAT: nodes need
   `publicIPv4: true` (default), as on AWS, or the operator adds a NAT gateway.
3. The **`subscriptionID`** of the block (a guard against the wrong controller) and the tenant
   ID for the chart's credentials values.
4. The **controller identity**, documented as a requirement: a user-assigned managed
   identity, its federated credentials, the custom role (`docs/reference/azure-role.json`)
   and its assignment.
5. The **registered resource providers** and the vCPU quotas of the regions in use (a
   precondition, checked by the canary).
6. A **marketplace agreement** for any Flatcar offer that has terms (probably none); the
   operator accepts it (the plan above). Nothing for images (rung 1), SSH keys beyond the
   above, or placement.

SCarpenter self-manages, without `use`: Azure has no default resource, so after discovery it
creates a resource group, a VNet with a subnet and an NSG per NodeClass, per section 2's
table, else reports `ResourcesReady=False` with what was tried.

## 3. Driver

**SDK.** `github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/...`: `compute/armcompute/v9`
(v9.0.0, 2026-10-15), `network/armnetwork/v12` (12.1.0, 2026-10-13),
`resources/armresources`, `resources/armsubscriptions`, `marketplaceordering/armmarketplaceordering`;
`sdk/azidentity` (1.15.0, 2026-10-14) and `sdk/azcore`. The track-2 SDK is the supported,
generated one; majors move fast (compute v9, network v12 within a month): pin exact
versions in `go.mod`, wrap each client in a narrow interface (`vmAPI`, `nicAPI`, `pipAPI`,
`nsgAPI`, `asgAPI`, `vnetAPI`, `rgAPI`, `ppgAPI`, `skuAPI`, `usageAPI`, `imageAPI`,
`agreementAPI`, `resourcesAPI`), with only the calls used. Sources: the package changelogs
under https://github.com/Azure/azure-sdk-for-go . Clients per subscription, created lazily
(the region is a parameter of the calls, not of the client). User agent
`ClientOptions.Telemetry.ApplicationID = scarpenter/<version>`; azcore's retry policy
(429 and 5xx, honours `Retry-After`); per-call deadlines 45 s, 90 s for the `VM`
`BeginCreateOrUpdate` request (a call that runs into it is `Retryable`, code `CallTimeout`).

**CreateInstance** (one launch attempt, no ARM templates):

1. `Prepare` (earlier) made the resource group, subnet, NSG, ASGs, PPG and, per
   (NodeClaim, region), the public IP and NIC (names carry the hash of the create params,
   so a different accelerated-networking flag is a different NIC).
2. Look the VM up by its deterministic name (`GET`; the name takes the place of the
   idempotency label of plan 14, A3): an existing one carrying this
   cluster's tag is the replay: return it, whatever zone, type or capacity type the
   replay asks for (contract). ARM `PUT` of an existing VM with another zone or size
   would fail or resize, so the `GET` comes first.
3. `VirtualMachines.BeginCreateOrUpdate` (a `PUT`, idempotent by name):

| Field | Value |
|---|---|
| `name` | `<nodeclaim>-<hash8(cluster)>` (at most 64; the name is also `computerName`; the node name stays the NodeClaim name through `--hostname-override`) |
| `location`, `zones` | the region; `[zone]`, omitted for zone `0` |
| `plan`, `storageProfile.imageReference` | the resolved marketplace URN (`publisher`, `offer`, `sku`, `version`); `plan` only if the image has one |
| `hardwareProfile.vmSize` | the type's `api_reference` (`Standard_D2s_v5`, Keeper's value; verified in the snapshot) |
| `osProfile` | `computerName`, `adminUsername: core`, `customData` (Base64 of the Ignition), SSH key as above, `allowExtensionOperations: false` |
| `storageProfile.osDisk` | `name: <vm>-os`, `createOption: FromImage`, `diskSizeGB`, `managedDisk.storageAccountType`, `caching`, `deleteOption: Delete` (and the ephemeral `diffDiskSettings` of a profile) |
| `storageProfile.dataDisks[]` | `lun`, `createOption: Empty`, size, type, caching, `deleteOption: Delete`, name `<vm>-<name>` |
| `networkProfile.networkInterfaces` | the NIC, `primary: true`, `deleteOption: Delete`; the public IP, a separate resource, carries `deleteOption: Delete` itself (`PublicIPAddressPropertiesFormat.deleteOption` in `armnetwork`, "when the VM using it is deleted") |
| `priority`, `evictionPolicy`, `billingProfile` | spot: `Spot`, `Delete`, `maxPrice: -1` (never evicted for price) |
| `diagnosticsProfile.bootDiagnostics` | enabled with managed storage (serial console for the e2e diagnostics; no storage account, *unverified*) |
| `proximityPlacementGroup` | from a placement profile |
| `tags` | the logical tags through `TagKey` (below) plus `spec.tags` |
| `additionalCapabilities`, `securityProfile` | nothing in v1 (Standard security type; Trusted Launch for Flatcar is *unverified*; `encryptionAtHost` from settings) |

`deleteOption: Delete` on the OS disk, data disks, NIC and public IP needs API 2021-03-01
or later (the SDK's is newer): deleting the VM then removes them all (docs page
https://learn.microsoft.com/azure/virtual-machines/delete ; property placement as in the
table, from the SDK models; the cascade itself is on the live list). Alternative, open
question 12: `networkProfile.networkInterfaceConfigurations` lets the VM `PUT` create the NIC
and public IP inline (with NSG, ASGs and `deleteOption`), saving two `PUT`s and the
leftovers of a failed launch. Leftovers of a failed launch (NIC and public IP without a VM) are `perUse`
resources and are collected by the engine; disks carry the VM's name as prefix for the
sweeper because inline disks are not tagged.

4. **Waiting.** The `PUT` is a long-running operation; allocation (capacity, quota,
   constraints) fails inside it. The driver polls (5 s) until the operation ends or a
   4-minute deadline: a failure is classified from the operation's `ResponseError`
   (section 5); at the deadline with the VM `Creating` the instance is returned `pending`
   and the replay (`GET` by name) finds it. A Flatcar that never signals the agent ends in
   `OSProvisioningTimedOut` after about 20 minutes (the core's registration timeout deletes
   it earlier; delete during `Creating` is on the verification list). Wait for completion
   versus return early is the main behavioural difference from AWS (open question 4).
5. **A failed create must leave the key unused** (contract). Whether an allocation or quota
   failure leaves the VM resource behind in `provisioningState: Failed` is *unverified*. If it
   does, the step 2 lookup would return that VM as the replay; so on a classified launch
   failure the driver deletes the VM it just tried (waiting for the delete, within the call's
   deadline, else `Retryable`) before returning the error, and step 2 treats a `Failed` VM
   whose size or zone differs from the request as a leftover to delete, not as the replay.

**Instance and identity.** `InstanceRef` = `<region>/<resourceGroup>/<vmName>` (the ID part
holds the group: Azure needs it for every call). `providerID` is the core's
`scarpenter://azure/<region>/<nodeclaim>`. States from `provisioningState` and the
instance view's `PowerState/*`: `Creating`/`starting` is `pending`; `running` is
`running`; `stopping`, `stopped` and `deallocated` are `terminating` (the stopped-node rule of plan 14,
A4: a VM that powered itself off is collected); a `Failed` VM that still exists is `running`
(it exists and costs: the NodeClaim deletes it); `Deleting` is `terminating`; a 404 is
`NotFound` (Azure keeps no terminated VMs; a malformed ID such as the conformance suite's
`<region>/i-never-existed` is `NotFound` too, not `Fatal`). `Addresses`: the NIC's private IPv4 and the
public IP's address (a Standard public IP is static, so it is known right after its `PUT`).

**Lookup, list, delete.** `GetInstance`: `GET` with `$expand=instanceView`. `FindInstance`
and `ListInstances`: the ARM resources list filtered by the cluster tag
(`resources.NewListPager` with `$filter=tagName eq ... and tagValue eq ...`, one paginated
call per subscription; consistency after a create *unverified*; per the API reference a tag
filter combines with no other filter and **returns the resources without their tags**, so
VMs are picked client side by type and `location`), then a `GET` with instance view for the
matches; fallback `VirtualMachines.NewListAllPager`.
No Resource Graph (eventual consistency breaks list-after-create). `DeleteInstance`:
read first and refuse (`Fatal`) a VM without this cluster's tag; `BeginDelete` with
`forceDeletion=true` (*unverified* that it combines with the delete options); NotFound
is success.

**Tags.** Azure tag names must not contain `< > % & \ ? /`, are case
insensitive, at most 50 per resource, values at most 256 characters. `TagKey(k)` is
`scarpenter-<key>` (`scarpenter-cluster`, `-nodeclaim`, `-nodepool`, `-nodeclass`,
`-managed-by`, `-resource-kind`), as `vendors.DefaultTagKey` says for restrictive
charsets; `Name=<nodeclaim>` too. The opt-in `scarpenter-use` is a different tag.

**Limits.** ARM throttles per subscription and region with `429 Retry-After` (the numbers
are *unverified*; older documentation gave 1200 writes and 12000 reads per hour per
subscription). One launch is about 3 writes (public IP, NIC, VM) plus polls, one delete
is a `DELETE` of the VM plus polls. Launches are serial per NodeClaim; the quota refresh (below) is one `usages` call per
region plus the SKU table hourly. The resource-check status reads are cached as for AWS.

### Billing period (ADR-024, plan 23)

Azure bills per full minute (seconds dropped) with **no documented minimum**; Keeper's 1/300 is not supported by the page (a data issue,
plan 13). Neither value is a period or a worthwhile minimum, so Azure is **not a retention candidate** (ADR-024, plan 23): the status reason
is `PerSecondBilling`, and nothing is held. `BillingStart` stays unset.

## 4. Catalog

Keeper already has Azure (snapshot of 2026-11-06): 64 regions (`api_reference` equals the
ARM location, e.g. `australiacentral2`), 150 zone rows, 1585 server rows of which 1307
`active` (the rest `planned-for-retirement`, `inactive`, `retired`: the catalog already
filters on status), 99 arm64 rows (Bpsv2, Dpsv5/Dpdsv5/Dplsv5, Epsv5, Dpsv6 and the
D/E `p` v6 families; four NVIDIA GB-class rows to exclude by the accelerator labels),
zones `0` to `3`. Types are named `azure.Standard_D2s_v5`; the family is Keeper's (`DSv5`),
not Azure's quota family (`standardDSv5Family`): the quota mapping comes from the live
SKU table, never from names. Some Keeper regions are staging or restricted
(`eastusstg`, `centraluseuap`, `brazilus`, and the `RESTRICTED`/`PREVIEW`
region types): `Regions` returns the subscription's physical locations
(`armsubscriptions` locations, `regionType: Physical`), and the facts skip a region
whose SKUs are all restricted.

**Offerings** (`OfferingLister`, per region, hourly): `ResourceSKUs.NewListPager` with
`$filter=location eq '<region>'` (the only supported filter; the response is large, so
one call per region and the result is cached), keep `resourceType == virtualMachines`;
a size is launchable in a zone when its `locationInfo[].zones` has the zone and no
`restrictions` entry (type `Zone` or `Location`, reason `NotAvailableForSubscription` or
`QuotaId`) covers it; drop sizes without Gen2 or without a SCSI disk controller (below).
Capabilities used: `vCPUs`, `HyperVGenerations`, `PremiumIO`, `LowPriorityCapable`,
`AcceleratedNetworkingEnabled`, `EphemeralOSDiskSupported`, `CachedDiskBytes`,
`MaxResourceVolumeMB`, `MaxDataDiskCount`, `DiskControllerTypes`, `CpuArchitectureType`
(names *unverified*; the fake and the canary pin them). **Azure has no stock check**: the
restrictions are subscription-level, capacity is learnt reactively (`AllocationFailed`,
3-minute unavailable TTL), like AWS. Spot placement scores are deferred.

## 5. Quotas, availability and errors

**Quota model** (docs: https://learn.microsoft.com/azure/quotas/per-vm-quota-requests ,
https://learn.microsoft.com/azure/quotas/spot-quota): per subscription and region,
standard vCPU quotas have **two tiers that both must have headroom**, Total Regional vCPUs
and the VM family's vCPUs; Spot has its own regional quota "across all VM families" and
is separate from the standard ones. Increasing a family quota raises the regional total.
The Usage API (`GET /subscriptions/{id}/providers/Microsoft.Compute/locations/{location}/usages`,
`UsageClient.NewListPager`) returns `{name.value, currentValue, limit}` for every quota
**with the usage of the whole subscription** (the contract's "Used counts the whole
account": nothing to count by hand, unlike AWS).

| Bucket | Usage name | Consumed by |
|---|---|---|
| Total Regional vCPUs | `cores` | every on-demand VM, by the size's vCPUs |
| VM family vCPUs | the SKU's `family`, e.g. `standardDSv5Family` | on-demand VMs of that family |
| Total Regional Spot vCPUs | `lowPriorityCores` | spot VMs |
| Standard public IPv4 addresses | `armnetwork.UsagesClient`, `StandardPublicIPAddresses` | 1 per node with `publicIPv4` |

`Consumption(type, capacityType)` returns two buckets (regional and family) for on-demand,
one for spot, plus the public IP bucket. The names of the Compute usages are *unverified* (the canary fails on a lane type that does not map, and a test
asserts the mapping for every size of the fake's SKU table). A size missing from the SKU
table has unknown quota (reactive path). New subscriptions often have small or zero
family quotas (the B and D families in some regions); `QuotaZero` is a normal state.
Spot is `Unsupported` for B-series and promo sizes ("`AzureSpotIsNotSupportedForThisVMSize`"),
which `LowPriorityCapable` shows up front; spot and an availability set exclude each other
(`AzureSpotVMNotSupportedInAvailabilitySet`).

**Error mapping** (by `azcore.ResponseError.ErrorCode`, never message text, with the
exceptions marked). Sources: https://learn.microsoft.com/troubleshoot/azure/virtual-machines/windows/error-messages ,
.../allocation-failure , https://learn.microsoft.com/azure/virtual-machines/error-codes-spot .

| Class | Azure codes |
|---|---|
| `InsufficientCapacity` | `AllocationFailed`, `ZonalAllocationFailed`, `OverconstrainedAllocationRequest`, `OverconstrainedZonalAllocationRequest` (the offering is infeasible with its constraints: size, zone, PPG, accelerated networking), `SkuNotAvailable` **with `priority: Spot`** (the spot docs say it means no spot capacity) |
| `Unsupported` | `SkuNotAvailable` on-demand (size not offered in the region or zone for the subscription), `AzureSpotIsNotSupportedForThisVMSize`, `InvalidParameter` naming an unavailable size (message exception, as AWS's `InvalidParameterCombination`) |
| `QuotaExceeded` | `QuotaExceeded`; `OperationNotAllowed` whose message says the operation "results in exceeding approved ... quota" (message exception, *unverified* wording); `PublicIPCountLimitReached`, and the network count limits of the region (*unverified* codes) |
| `ZoneUnusable` | a zone the subscription lacks for the size (code *unverified*); subnets are regional, so there is no `NoSubnetInZone` |
| `RegionUnavailable` (from `Zones` and the SKU read only) | `LocationNotAvailableForResourceType`, `InvalidResourceLocation`, `NotAvailableForSubscription` on every SKU; `AuthorizationFailed` on a region read stays `Fatal` on launches |
| `Retryable` | HTTP 429 (`TooManyRequests`), 5xx, `InternalServerError`, `ServerBusy`, `RetryableError`, `ReferencedResourceNotProvisioned`, `AnotherOperationInProgress`, `OperationNotAllowed` "another operation is in progress", `OSProvisioningTimedOut` after the VM exists, the per-call timeout and a canceled context; dependency errors on delete: `InUseSubnetCannotBeDeleted`, `NetworkSecurityGroupInUse`, `PublicIPAddressCannotBeDeleted`, `InUseNetworkSecurityGroupCannotBeDeleted`, `ScopeLocked` (names *unverified*) |
| `NotFound` | HTTP 404, `ResourceNotFound`, `ResourceGroupNotFound`, `VMNotFound`, `PlatformImageNotFound` on the image (`ImageNotFound`) |
| `Fatal` | everything else, in particular `AuthorizationFailed`, `LinkedAuthorizationFailed`, `MissingSubscriptionRegistration`, `VMMarketplaceInvalidInput`, `MarketplacePurchaseEligibilityFailed`, `ReadOnlyDisabledSubscription`, `InvalidParameter`, `UserDataTooLarge` (above 16 KiB, code `UserDataTooLarge`) |

A table test covers every code (with an independent second copy, as for AWS); one code, `SkuNotAvailable`, depends on the capacity type.

**Region and zone gating.** A region whose SKU read is fully restricted or denied is
`RegionUnavailable` (facts skip it with one warning). A zone without the size is gated per
zone by `LaunchableTypes`. A subscription where `Microsoft.Compute` is not registered is
`Fatal` on launch and shown in `VendorReady`.

## 6. Spot

Priority `Spot`, eviction policy `Delete` (no deallocated VMs left billing their disks;
the delete options remove the rest), `maxPrice: -1`. Prices are Keeper's spot prices
(overview §5.2: none fetched from Azure). Interruption handling (scheduled events from
IMDS, `Preempt` events) is deferred like AWS. A node evicted by Azure disappears with its
VM and the core reaps the NodeClaim.

## 7. Node options and profiles (ADR-020)

Azure terms, as in 00a §6.1 (defaults and field names; the quantity style follows the Azure
CLI, `diskSizeGB`, an open question against AWS's `volumeSize`).

| Block | Fields |
|---|---|
| `storage.root` | `storageAccountType` (`Standard_LRS`, `StandardSSD_LRS`, `StandardSSD_ZRS`, `Premium_LRS`, `Premium_ZRS`), `diskSizeGB`, `caching`, `diskEncryptionSetID` (customer-managed key), `ephemeral: {placement: CacheDisk\|ResourceDisk\|NvmeDisk}` |
| `storage.data[]` | `name`, `storageAccountType` (the root types), `diskSizeGB`, `caching`, `filesystem`, `mountPath` (LUN: assigned by order) |
| `storage.localStorage` | `ignore` only in v1 (`mount`/`raid0` of the temp disk and local NVMe wait for the device-path spike) |
| network profile | `acceleratedNetworking: {enabled: true\|false\|auto}` |
| placement profile | `proximityPlacementGroup: {strategy: cluster}` or `{use: {name}}` |
| settings | `adminSSHPublicKey`, `encryptionAtHost`, `imageOwner`, `subscriptionID` |

- **Built-in defaults:** root `Premium_LRS`, 64 GiB, `ReadWrite` cache, platform-managed
  encryption (always on in Azure), accelerated networking `auto`, public IPv4 on. Azure bills
  a managed disk by tier (a 64 GiB disk is a P6), so `PriceDelta` rounds the size up to the
  tier from Keeper's storage rows (*the tiers: unverified*).
- **ADR-017 consequences.** Premium needs the `PremiumIO` capability: types without it
  (Av2 and similar) get no offering under the default; the NodeClass sets
  `StandardSSD_LRS` to include them (open question 2). `auto` accelerated networking is
  on where `AcceleratedNetworkingEnabled` is true and off elsewhere, and is **stamped
  on the node** like an applied profile (otherwise a benchmark would silently differ by
  size); `true` makes the other sizes infeasible. Ephemeral OS disks need
  `EphemeralOSDiskSupported` and an image that fits the cache or temp disk.
  Sizes that offer **only an NVMe disk controller** (`DiskControllerTypes` without SCSI)
  are not offered in v1: the SCSI device path `/dev/disk/azure/scsi1/lun<N>` is what 00a §11
  relies on, and the NVMe path (`/dev/disk/azure/data/by-lun/<N>` from azure-vm-utils, if
  Flatcar ships it) is *unverified*. This is most of the current generation: Microsoft
  documents the v6 families (Dasv6 and others) as NVMe-only, needing an image tagged with
  NVMe support (Dpsv6 likely the same, *unverified*), so whether the Flatcar marketplace
  images are NVMe-tagged is a spike item and open question 8 decides the scope.
- **Device hints:** `DeviceHint.ByID = /dev/disk/azure/scsi1/lun<N>` with N the LUN the
  driver assigned (the user-data is rendered after `Prepare`, so the LUN is known up
  front); the hint carries the size for a defensive check. Needs the udev rules of the
  Azure agent on Flatcar (*unverified*, a spike item).
- **`Supported`:** storage types per `PremiumIO`; at most `MaxDataDiskCount` data disks;
  ephemeral placement per capability; cluster placement not for sizes the PPG cannot take
  (cannot be known in advance: `OverconstrainedAllocationRequest` makes the offering
  unavailable for 3 minutes); spread (an availability set) is **not offered in v1**
  because it excludes zones and spot; the zone choice already spreads (Karpenter's own
  zone requirement).
- **`PriceDelta`:** disks at Keeper's GB-month price per region and type (Azure storage
  prices in `/storage_prices` for the vendor *unverified*), the public IPv4 address (a
  constant like AWS). Transaction costs of Standard disks, PremiumV2/Ultra IOPS and
  bandwidth: incomplete.
- **Milestone 6 (separable, may move to a follow-up plan):** `PremiumV2_LRS` and
  `UltraSSD_LRS` data disks (provisioned `diskIOPSReadWrite`, `diskMBpsReadWrite`) cannot
  be created inline: a `disks` kind (node scope, `perUse`, zonal, `createOption: Attach`),
  `ultraSSDEnabled` on the VM, `OverconstrainedAllocationRequest` as the common failure.

## 8. In-memory fake, conformance and tests

`pkg/vendors/azure/fake` serves the **real SDK clients** through the SDK's generated fake
servers (`armcompute/fake`, `armnetwork/fake`, `armresources/fake`, `armsubscriptions/fake`
and `armmarketplaceordering/fake` exist on the SDK's main branch, with `azcore/fake`; a
client without a fake server gets a small `httptest` ARM server): real pollers, real
`ResponseError`s. State: locations, SKUs (capabilities, family, zones, restrictions),
usages, marketplace images and agreements (with and without a plan), resource groups,
VNets, subnets, NSGs, ASGs, PPGs, public IPs, NICs, disks and VMs (idempotent `PUT` by
name, `provisioningState` transitions, LRO completion on a fake clock or on demand,
`deleteOption` cascade, tag filters, a VM `Failed` after a delay), foreign VMs for the
usage numbers, a subscription without registered providers, a region without zones, and
error injection per call and per LRO outcome with any ARM code. Own tests: the golden of
the `VirtualMachine` body (every field of §3 asserted), idempotent replay with another
zone, the SKU and quota mapping, every error code, the `deleteOption` cleanup, the
resource-group safety (never delete a non-empty or foreign group), NSG isolation rules.

Conformance: `conformance.Run` with the fixture hooks (`Profiles`, `ProfileTypes`,
`ResourceKind`, `PreCreate`, `Restart`, `ResourceParams`) once per resource kind, over the
complete list of **active Azure types of the snapshot** (not a sample), with the unknown
quota list for sizes the fake SKU table does not cover. Gates: CONVENTIONS 1 to 4; L3 still
green; `go test ./pkg/vendors/azure/...` coverage >= 80 % (report it).

## 9. L5 lane, sweeper and canary

Follows `docs/how-to-guides/testing-and-releasing.md` ("Adding a vendor lane", eight
items; cost guards). New: `test/e2e-azure/` (the same script names as `test/e2e-aws/`),
`.github/workflows/e2e-azure-sweeper.yml`, the Azure credential steps in `e2e-cloud.yml`
and `e2e-canary.yml`, `test/canary/azure.go` and a write-refusing client guard (as
`aws_guard.go`), one line in `test/e2e-vendors.txt` **only when every cost guard is
true**. Not run in this plan.

**The lane is not in `test/e2e-vendors.txt` yet.** The CI cost-guard policy
(`docs/how-to-guides/testing-and-releasing.md`, "Per-vendor cost guards") is unmet until the
guards below are shown on a live account. Before the line `azure` is added, the operator
verifies that the "Allowed virtual machine size SKUs" policy (deny) on the lane's resource group
refuses a size outside `defaults.sh` (the negative scenario provokes it); that "Allowed
locations" and the CI identity's scope (that resource group only) keep the lane in its region;
that the vCPU quotas (regional and the lane's families 16, Spot 4, Standard public IPs) are set
no higher and a subscription budget alert reaches a person; that the sweeper deletes a dead
run's VMs, NICs, public IPs and disks with the lane's real role; that the read-only canary role
reads what the canary reads; and that `e2e-sweeper` and `e2e-canary` are limited to `main`, with
the federated credentials' subjects as above. Until then the workflows do not run the lane by
themselves, `make test-e2e-azure-dryrun` works, and the lane's selftest checks that the line is
absent (the list in `test/e2e-azure/defaults.sh` is the same).

**Identity.** GitHub OIDC straight to Azure: a federated credential (audience
`api://AzureADTokenExchange`) on a user-assigned managed identity per environment, with
the immutable subjects of `e2e`, `e2e-sweeper` (CI identity) and `e2e-canary` (read-only
identity); `azure/login` (or `az login --federated-token`) for the runner's own calls.
The controller in k3s needs credentials too: the workflow writes the federated token to
a Secret mounted as `AZURE_FEDERATED_TOKEN_FILE` and **refreshes it in a background loop**
(GitHub's OIDC token is short-lived, *unverified* lifetime; `WorkloadIdentityCredential`
re-reads the file), because a 55-minute scenario run outlives it. No secret anywhere. The
variables are IDs (`AZURE_CLIENT_ID`, `AZURE_TENANT_ID`, `AZURE_SUBSCRIPTION_ID`,
`AZURE_REGION`), not secrets, set by the CI account's operator like the AWS ones.

**Environment of the CI account.** Required of its operator: a dedicated one-region environment (its own resource group,
VNet, subnet and NSG) for the lane, so the CI role can
be scoped to that resource group; Microsoft.Compute and Microsoft.Network registered; vCPU
quotas (Standard regional and the lane's families 16, Spot 4, and Standard public IPs);
a budget alert; marketplace terms if any.

**Cost guards (the table of the how-to, for Azure).**

| Guard | Azure |
|---|---|
| CI role cannot create other sizes | Azure Policy "Allowed virtual machine size SKUs" assigned (deny) to the lane's resource group, from `defaults.sh` |
| Only the lane's region | "Allowed locations" policy plus the role's scope: the lane's resource group only |
| Run tag, attributable | `scarpenter-cluster=scarpenter-ci-<run id>` and `scarpenter-ci-created-at` on every resource (inline disks are covered by their name prefix) |
| Sweeper | `e2e-azure-sweeper.yml`, every 3 hours, environment `e2e-sweeper`: resources of the resource group tagged `scarpenter-cluster=scarpenter-ci-*` older than 2 hours: VMs (force), NICs, public IPs, disks by name prefix, managed NSGs and VNets, empty managed resource groups |
| Time limit, small types | 90/55 minutes as AWS; cheap sizes to be chosen from the canary's output: amd64 `Standard_B2ats_v2` (2 vCPU, 1 GiB), arm64 `Standard_B2pts_v2`; **B-series have no Spot**, so the spot scenarios use a small D or E size that reports `LowPriorityCapable` (for example a `Standard_D2ps_v6` or `Standard_D2as_v5`, to confirm) |
| Quota as ceiling | the quotas above are the bound |
| Read-only canary role | `Reader` on the subscription plus `MarketplaceOrdering/agreements/read` (a custom role); no writes |

**Scenarios** (the AWS list with Azure edits): preflight (providers, quotas, terms, image);
amd64 on-demand exact type (node Ready, Pod, `logs`/`exec`, labels with zone `1`/`2`/`3`,
tags, `az vm show` for the fields of §3); arm64 on-demand then spot; scale to zero **and
check that no NIC, public IP or disk of the VM remains** (the `deleteOption` cascade);
drift (`storage.root.diskSizeGB`); quota (usages equal the SDK's, `used` follows a
launch and a termination, both tiers); db plus client in one proximity placement group
with a Premium data disk at its LUN path (needs the device spike); described, not named;
bring your own (resource group, subnet and NSG by id and tag; not deleted); negative
(spot on a B-series, an unregistered-size error, terms missing if testable); uninstall
(nothing tagged remains). Diagnostics on failure: `az vm boot-diagnostics get-boot-log`
(the serial log), activity log, NodeClaims, controller log.

**Canary** (read-only, nightly): the shared sysext and k3s checks; regions and zones
(locations); `ResolveImage` for amd64 and arm64 at the pinned version; the marketplace
agreement state; the quota rows (both tiers and spot against `E2E_MIN_VCPU_*`); the SKU
read for every lane type (launchable, zone, `LowPriorityCapable`, quota mapping); the
read-only guard.

## 10. Dependencies

- Plans 01 to 10 (done).
- Plan 14 (the groundwork). **No image plan** (`SCImage`, `ImageImporter` stay reserved);
  the marketplace is the source (rung 1, ADR-021).
- The operator's IaC: the points of section 2 before the lane and before real use.
- Core: nothing new expected; if the driver needs a **multi-bucket quota with an unknown
  second bucket**, the quota tracker already takes several (`Consumption` returns a list).
- Docs in the same plan: `docs/reference/azure-driver.md`, `docs/reference/azure-role.json`
  (JSON-validated in `make lint`), `docs/how-to-guides/azure-credentials.md` (workload
  identity, secret, AKS), the API reference for `spec.vendors.azure`, the L5 section of
  the testing how-to. `pkg/apis/v1alpha1/azure` and the one line per type of `vendors.go`.

## 11. Milestones

1. API types and fake skeleton; validation; `Regions`, `Zones`, `ResolveImage` (+ `ImageReporter`).
2. Resources: resource groups, subnets, NSG, ASGs, `Prepare` for public IP and NIC, GC, resource-group safety.
3. `CreateInstance`, `Get`, `Find`, `List`, `Delete`, idempotency, error mapping, tags.
4. Quotas and offerings from the SKU and usage tables; spot.
5. Node options (storage, accelerated networking, proximity placement), device hints, prices.
6. L5 lane, sweeper, canary, docs. (Optional: PremiumV2 and Ultra disks.)

## Open questions for the maintainers

1. **Marketplace or community gallery?** Recommendation: marketplace (all channels, both
   arches, every pinned version, no terms on the `corevm` offers); keep the gallery as an
   optional override later.
2. **Default root disk.** 00a says `Premium_LRS` 64 GiB; sizes without Premium get no
   offering (ADR-017). Recommendation: keep `Premium_LRS`, document `StandardSSD_LRS`.
3. **Field names.** Azure terms (`storageAccountType`, `diskSizeGB`) as 00a, or AWS-style
   `volumeType`/`volumeSize` Quantities? Recommendation: Azure terms (ADR-020).
4. **Wait for the VM operation or return when it is accepted?** Waiting classifies
   capacity and quota errors in the launch (needed for the attempt loop) but holds the
   call up to 4 minutes. Recommendation: wait with the 4-minute bound, as designed.
5. **Public IP per node.** Needed for egress on a subnet without NAT. Alternative: a
   NAT gateway per subnet (about $0.045/h each plus data) and `publicIPv4:
   false`. Recommendation: public IP for v1, a NAT option later.
6. **Zone names:** `1` (Keeper, identity) or `<region>-<n>` (AKS convention)?
   Recommendation: `1`, consistent with Keeper and `zone-id`.
7. **`subscriptionID` as a guard** in the NodeClass: keep or drop? Recommendation: keep
   (the IaC fills it; it stops a NodeClass targeting the wrong controller).
8. **PremiumV2/Ultra disks and NVMe-only sizes in v1?** Recommendation: not in v1
   (milestone 6 and a spike), keep them out of `Supported`. Cost: NVMe-only covers the v6
   families, so v1 would offer only v5 and older sizes; decide after the spike shows whether
   the Flatcar images are NVMe-tagged and which device paths appear.
9. **Throwaway SSH key** versus a mandatory NodeClass key, if Azure requires a credential.
   Recommendation: the generated key, until the live run shows what Azure accepts.
10. **Resource-group deletion.** Recommendation: only managed, marked and empty; default
    `retain`; never from `use`.
11. **`isolation` with a brought NSG** (section 2): no effect, since a NIC has one NSG.
    Options: document and warn; refuse `isolation: none|run` with `securityGroups.use` (an
    ADR-017 reading); or let SCarpenter manage the NSG on a brought subnet. No recommendation.
12. **Separate NIC and public IP resources (`Prepare`) or inline `networkInterfaceConfigurations`
    in the VM `PUT`?** Inline is fewer calls and no failed-launch leftovers, but the NIC is
    then not a vendor resource the engine tracks. No recommendation.

**Decided by default, maintainer to confirm** (the end-to-end driver step):

- Q1: marketplace, as recommended; the `images` kind takes URNs and version patterns only (no gallery or managed images, no
  Resource Graph `expression`).
- Q2, Q3, Q4, Q5, Q6, Q7, Q9, Q10: the recommendations.
- Q8: no PremiumV2/Ultra disks; NVMe-only sizes are not offered.
- Q11: document it and show it as the vendor condition `ConfigurationWarning` with one Warning event (no refusal): a brought NSG decides alone and `networking.isolation` has no effect.
- Q12: the public IP and NIC are separate resources made by the launch itself (not engine kinds: their names do not carry
  the engine's prefix); a failed launch deletes them (a transient failure keeps them only when a VM exists), and the
  garbage collector deletes the ones an interrupted launch left without a VM once their NodeClaim is gone (the core's
  optional `vendors.LeftoverCollector`); the CI sweeper is the lane's last line.
- Application security groups are not kinds: each belongs to a managed network security group (`<nsg>-nodes`), created and
  deleted with it. Isolation `run` with a run key gets a network security group per run (`runSecurityGroups`, `perUse`),
  because a NIC takes one group.
- The subscription is read from `AZURE_SUBSCRIPTION_ID` only (no `--azure-subscription-id` flag: it would change the core's options).
- The shared suite's resolution-order cases assume a vendor default resource; Azure has none, so they are skipped and
  `resolution_test.go` covers the order. The `ImagesReady` reason `MarketplaceTermsNotAccepted` is set on the image
  status, and the NodeClass controller makes any driver reason the condition's when the images are not ready (a small
  generic core change; the version reasons keep their stronger rule).

## Live verification list (everything above ran against the fake only)

1. Marketplace: the exact image versions and SKUs per channel and arch (`lts2024-gen2`, no
   arm64 LTS), whether the `corevm` offers carry the current releases, whether the arm64
   `corevm` offer has a plan or terms, whether the images are NVMe-tagged, `Get` per region.
2. A VM with custom data only and a throwaway SSH key; the Azure admin-credential rule.
3. `customData` size limit in characters; the real user-data size with the k3s and an AKS-size CA.
4. Boot time to Ready; whether a bad Ignition leaves the VM `Creating` for 20 minutes;
   delete during `Creating`; `forceDeletion` with the delete options.
5. Allocation behaviour: the codes for capacity, quota, an unavailable size (on-demand and
   spot), a constrained PPG, a zone the subscription lacks; the real wording of the quota error;
   whether a failed allocation leaves a `Failed` VM resource.
6. The names of the Compute and Network usages and the SKU `family` mapping; both tiers;
   spot usage; whether spot also consumes the regional total.
7. `deleteOption` cascade for the NIC, public IP, OS and data disks; leftovers after a failed `PUT`.
8. The resources list with a tag filter, short pages, API volume; ARM throttling numbers.
9. Accelerated networking on Flatcar (mlx4/mlx5/MANA drivers) and the `auto` choice;
   SCSI and NVMe controller sizes; device paths of data disks and the temp disk.
10. A region without zones (zone `0`), eastus2's zone `4`, a restricted region's behaviour.
11. Role actions in `azure-role.json`; the Azure Policy denies; the workload identity chain
    with a self-managed issuer, token refresh in CI; `MissingSubscriptionRegistration`.
12. Managed network: create, delete, recreate; the `DenyVnetInbound` plus ASG rules between
    nodes of one run and of two runs; the `direct` template inside the VNet.
13. Per-second billing and the cost of one full lane run.
14. Trusted Launch / security type defaults for the Flatcar image under the API version used.
