# Azure driver

`pkg/vendors/azure` launches, lists and deletes Flatcar VMs in one Azure subscription. It is a
complete `vendors.Driver` (registered as `azure`, enabled with `--vendors=azure`) with node options,
managed resources, quotas and a NodeClass validator, and it passes the shared conformance suite
against an in-memory ARM. **Nothing in it has run against Azure**: no test makes a cloud call. The
design is [plan 17](../plans/v1/done/17-azure.md); the contract it implements is the
[driver contract](driver-contract.md). The NodeClass block is `spec.vendors.azure` ([API](api.md)).

- [Account and credentials](#account-and-credentials)
- [Regions, zones and images](#regions-zones-and-images)
- [Launch](#launch)
- [Confidential sizes](#confidential-sizes)
- [Node settings](#node-settings)
- [Resources and network](#resources-and-network)
- [Quotas, offerings and errors](#quotas-offerings-and-errors)
- [Billing](#billing)
- [Tests and the L5 lane](#tests-and-the-l5-lane)
- [What is not verified against Azure yet](#what-is-not-verified-against-azure-yet)

## Account and credentials

One controller works in one subscription: `AZURE_SUBSCRIPTION_ID` is required (the chart value
`vendors.azure.subscriptionID`). `spec.vendors.azure.subscriptionID` is only a guard: a different value
fails the NodeClass validation. The credential chain (`SCARPENTER_AZURE_CREDENTIALS_MODE`) is workload identity (`AZURE_CLIENT_ID`,
`AZURE_TENANT_ID`, `AZURE_FEDERATED_TOKEN_FILE`) in mode `workloadIdentity` (also the chart's mode `none`), or
the client secret or certificate of the environment in mode `secret`; then the managed identity; the Azure CLI's
login only under `SCARPENTER_AZURE_DEVELOPMENT=true`.
How to set it up: [Set up Microsoft Azure](../how-to-guides/set-up-azure.md#2-credentials). The custom role is
[`azure-role.json`](azure-role.json). The resource providers `Microsoft.Compute`, `Microsoft.Network` and
`Microsoft.MarketplaceOrdering` must be registered (a `MissingSubscriptionRegistration` is `Fatal`).

## Regions, zones and images

Regions are the subscription's physical locations (`westeurope`); zones are the logical zones `1`, `2`, `3`
(the identity mapping: Keeper's names), and zone `0` for a region without zones. An unknown zone is
`ZoneUnusable`.

Images come from the Azure Marketplace, publisher `kinvolk` (override: `imageOwner`): amd64
`flatcar-container-linux-corevm-amd64`, SKU `<channel>-gen2` (for `lts` the `lts*-gen2` SKU that has the
version); arm64 `flatcar-container-linux-corevm`, SKU `<channel>`. `Image.Ref` is the URN
`publisher:offer:sku:version`. The `images` selector takes URNs (`ids`) or a pattern
(`publisher:offer:sku:<version glob>`, `orderBy: name`); galleries and managed images are not supported. Whatever is
found must say the pinned version (`ImageVersionMismatch`). **SCarpenter never accepts marketplace terms**: an image
with a purchase plan whose terms are not accepted is not ready (`MarketplaceTermsNotAccepted`, the message holds
the exact `az vm image terms accept` command). Only Gen2 sizes are launched.

## Launch

One `VirtualMachines` PUT per attempt, after a public IP and a NIC of the NodeClaim; no ARM template. The VM
name `<nodeclaim>-<hash8(cluster)>` is the idempotency key: a replay returns the VM whatever zone or size it
asks for; a failed create deletes its VM, NIC and public IP, so the key stays unused (a transient failure keeps
them only when a VM exists, for the replay). NICs and public IPs that an interrupted launch leaves without a VM are
deleted by the garbage collector once their NodeClaim is gone (`CollectLeftovers`). The launch waits for the
long-running operation up to 4 minutes (capacity and quota errors arrive inside it); a VM still `Creating` is
returned pending. Disks and NIC carry `deleteOption: Delete` and go with the VM. The public IP does not: Azure keeps no `deleteOption` on an address created on its own (seen live, 2026-11-16), so `Delete` deletes it after the VM. It does so only if it carries the cluster and NodeClaim tags (an address of that name made by someone else is left), and the leftover collector catches one that a failed delete leaves. The VM has `core` as admin user and one
public key (`adminSSHPublicKey`, else a key generated at start whose private half is discarded: access is
Ignition's business). Spot: priority `Spot`, eviction `Delete`, `maxPrice: -1`. States: `Creating` pending,
`running` running; stopped, deallocated, `Deleting` terminating (the stopped-node rule); a `Failed` VM that exists
is running (it costs: the NodeClaim deletes it). The instance reference is `<region>/<resource group>/<vm>`.
Tags are `scarpenter-<key>` plus `Name`. `DeleteInstance` refuses a VM without the cluster tag.

## Confidential sizes

A size whose SKU says `ConfidentialComputingType` `SNP` (AMD SEV-SNP: DCasv5, DCadsv5, ECasv5, ECadsv5 and the v6 and
`cc` variants) or `TDX` (Intel TDX: DCesv6, DCedsv6, ECesv6, ECedsv6) is a confidential VM size. It boots Azure Container
Linux, not Flatcar from the marketplace, because Flatcar has no confidential VM image. The SGX sizes (DCsv2, DCsv3)
report no type and stay on Flatcar like every other size.

- **Image**: the newest version (by published date, versions excluded from latest skipped) of the community gallery image
  `AKSAzureLinux-f7c7cda5-1c9a-4bdc-a222-9614c968580b/aclgen2CVM`, read per region and cached for an hour. It replaces the
  image of the request (`Image.Ref`, `imageOwner`, the `images` selector and the Flatcar pin do not apply to it). A region
  without a version makes the size `Unsupported` there (code `ConfidentialImageNotFound`, cached for 5 minutes). The image has no
  purchase plan and the API takes no terms acceptance.
- **Launch fields**: `securityProfile.securityType: ConfidentialVM` with secure boot and vTPM on, OS disk
  `securityEncryptionType: VMGuestStateOnly`, the image by `communityGalleryImageId`, accelerated networking off. The user-data
  (Ignition) is the custom data as for Flatcar. Ephemeral OS disks, encryption at host and accelerated networking `"true"` are
  `Unsupported` on these sizes, and so is a confidential size with GPUs (`ConfidentialGPUNotSupported`: the image has no
  NVIDIA driver, e.g. NCC40ads_H100_v5).
- **Intel TDX**: launched with the same image and fields on a DC2es_v6; it boots, Ignition runs and the kubelet sysext is
  active, like on SEV-SNP.
- **Differences from Flatcar** the user-data accounts for: the image has its own `kubelet.service` in `/etc` (it needs
  `/opt/bin/kubelet` and `/etc/default/kubelet` and runs its own scripts first), which the kubelet drop-in clears; and the
  presets that Ignition writes for `enabled: true` are not applied, so the fail-safe poweroff timer is enabled by a link.
  The other units of the templates are pulled in by the kubelet (`Requires`, `RequiresMountsFor`), not by their `[Install]`
  section. `waagent` provisions the `core` user and an SSH key that nobody holds (the generated key, or `adminSSHPublicKey`);
  Ignition configures neither.
- **Instance metadata**: the `Restricted` firewall (`scarpenter-metadata-firewall.service`) loads on the image: `iptables` and
  the cgroup match exist, `waagent.service` is the agent's unit name, and both the kubelet (`Requires=`) and waagent (`Wants=`)
  pull the firewall in. A connection to the metadata service or the WireServer from outside `scarpenter-metadata.slice` is
  dropped, from inside it succeeds, and waagent runs in the slice. The image's own rules (root-owned processes only on the
  WireServer) stay in place next to it.

## Interruption

A Spot VM (eviction policy `Delete`) is evicted with a notice of at least 30 s, best effort, through Scheduled Events. The driver implements
[`InterruptionNotices`](driver-contract.md#interruption-notices-the-node-agent), so every Spot node runs the [node agent](node-bootstrap.md#the-interruption-agent)
with this check ([plan 26](../plans/v1/done/26-interruption.md) section 6), which reports through the Node's annotations:

- It asks `http://169.254.169.254/metadata/scheduledevents?api-version=2020-07-01` with the header `Metadata: true` every second. The first
  request registers the VM for Scheduled Events: Azure answers only after that (documented: up to 2 minutes, and the first events up to 5
  minutes later), so the agent starts at boot and an unanswered request is a transient error. A VM evicted inside that window has no notice and falls to
  the silent-reclaim path ([ADR-027](../explanation/DECISIONS.md)).
- A notice is an event with `EventType` `Preempt` whose `Resources` name **this VM**. The Node's name is the NodeClaim's, not the VM's, so the check
  reads the VM name once from `instance/compute/name` (`api-version=2021-02-01`, `format=text`) and caches it in `/run/sc-vm` (the agent runs the check in a command substitution, so a variable would not survive). Other event types (`Reboot`, `Redeploy`,
  `Freeze`, `Terminate`) and other VMs' events are ignored, as is `SpotRebalanceRecommendation`.
- The deadline is the event's `NotBefore` (RFC 1123, `Mon, 02 Jan 2006 15:04:05 GMT`), converted to RFC 3339 with `date -u -d`; a missing or
  unconvertible value leaves the deadline out and the controller assumes the minimum notice. The controller's window is 30 s to 2 minutes.
- The event is **not acknowledged** (`StartRequests`): approving it would end its window early, and the VM is deleted at `NotBefore` anyway.
- The response is matched with `sed`, `tr` and `grep` (no `jq` on the node), tolerant of pretty-printed and compact JSON and of the key order (the event type and the VM in `Resources` are two separate greps). With several matching events the first gives the deadline.
- Needs `curl`, `date` with `-d`, `sed`, `tr` and `grep`: Flatcar has them. The confidential sizes boot Azure Container Linux
  ([above](#confidential-sizes)); the agent's paths (`/opt/scarpenter/bin`, the kubelet's certificate) and the metadata firewall's unit name
  (`waagent.service`) are the same there, **but the agent has not run on that image** (a confidential size as Spot is not part of the lane; whether Azure
  allows Spot for confidential VMs is unverified). A shell tool missing there would show as a failing check in the journal
  (`journalctl -u scarpenter-interruption`).

The user-data of the Azure Spot worst case (the golden worst case plus the agent) is 12.8 KiB with `direct` and 12.4 KiB with `tailscale`
(13 061 and 12 709 bytes; the agent is 1.4 KiB of it). Azure's custom data limit is 64 KiB (65 535 bytes, before the base64 encoding),
so it is the driver's own budget of 16 KiB ([`MaxUserData`](#launch)) that applies: the agent leaves 3.2 KiB of it.

The manual L5 scenario `interruption` triggers a real eviction with `az vm simulate-eviction` ([the lane](../how-to-guides/testing.md#the-l5-lane-of-azure));
Simulate Eviction is documented to behave like a platform eviction, which Azure says delivers a `Preempt` event: **unverified** until the
first live run (spike S4).

## GPU sizes

GPU sizes need nothing from the launch: they use the same Flatcar image as every other amd64 size, the default
64 GiB root disk holds the driver sysext and CUDA images, and the size's own quota family (from the SKU table) is its
quota bucket ([Quotas](#quotas-offerings-and-errors)). The SCSI rule applies as everywhere: a size is offered only when
its `DiskControllerTypes` lists SCSI (or says nothing), so an NVMe-only GPU size would be dropped. NCasT4_v3 and NC A100 v4
are SCSI; NC H100 v5 lists SCSI and NVMe [U: from Azure's documentation, not read from a SKU row]. The manual `gpu`
scenario of the lane checks the rule against the subscription's SKU row before it launches. Which sizes the catalog offers is the vendor-neutral rule of
[GPU types](labels-and-offerings.md#gpu-types); the driver's part is that `GPUVirtualizer` reports every `Standard_NV*`
size as virtual: the NV series (NV v2 and v3 with the M60, NVadsA10 v5 with the A10) are visualization sizes whose GPUs
are vGPUs on NVIDIA's GRID driver, which SCarpenter does not boot. The confidential sizes with GPUs (`NCC40ads_H100_v5`) boot an image without the NVIDIA driver, so the driver's launchable
sizes leave them out (the catalog drops them as `NotLaunchable`: confidential GPU size, no NVIDIA path on the confidential image) and
Karpenter never picks a size that cannot launch; the launch still refuses them as `Unsupported` (`ConfidentialGPUNotSupported`,
[above](#confidential-sizes)) as a backstop. On Flatcar 4757.2.3:

| Offered | Dropped (reason) |
|---|---|
| `NC*as_T4_v3` (T4), `NC*ads_A10_v4` (A10), `NC*ads_A100_v4` (A100), `NC*ads_H100_v5` and `NC*adis_H100_v5` (H100), `ND40rs_v2` (V100, proprietary driver) | `NV*` (M60, A10 v5: `GPUVirtual`), `NCC*` (confidential H100: `NotLaunchable`) |
| with Fabric Manager: `ND96asr_v4` and `ND96amsr_A100_v4` (A100), `ND96isr_H100_v5` [U: SCSI], `ND96isr_H200_v5` [U: SCSI] | the AMD GPU sizes (`GPUManufacturer`) and fractional sizes (`GPUFractional`) |
| | Blackwell: `NC*_xl_RTXPRO6000BSE_v6`, `ND128isr_GB200_v6`, `ND128isr_GB300_v6` (`GPUNoDriverBranch` until a release builds 580-open, or `GPUNVSwitch`) |

The family quota of a new subscription is 0, so these sizes are `QuotaZero` until an increase is granted. Until a size
has a green real run it is "offered, not yet verified"; the manual `gpu` scenario of the [L5 lane](../how-to-guides/testing.md#the-l5-lane-of-azure)
(default `Standard_NC4as_T4_v3`) is how the maintainer runs one.

## Node settings

| Block | Fields |
|---|---|
| `storage.root` | `storageAccountType` (default `Premium_LRS`), `diskSizeGB` (64), `caching` (`ReadWrite`), `diskEncryptionSetID`, `ephemeral.placement` |
| `storage.data[]` | `name`, `storageAccountType`, `diskSizeGB`, `caching`, `filesystem`, `mountPath`; the device is `/dev/disk/azure/scsi1/lun<N>`, N the index |
| `storage.localStorage` | `ignore` only |
| network profile | `acceleratedNetworking.enabled`: `auto` (on where the size has it), `"true"` (other sizes unsupported), `"false"` |
| placement profile | `proximityPlacementGroup`: `strategy: cluster` (one group per run key) or `use.name` |
| settings | `adminSSHPublicKey`, `encryptionAtHost`, `imageOwner`, `subscriptionID` |

The default root disk stays `Premium_LRS`; SCarpenter never downgrades it. The catalog checks Keeper's compatible
storage of the size (`compatible_storage_ids`, the disk SKU names) against the settings a route resolves to: the
root disk type (the default `Premium_LRS`, or the `storageAccountType` the NodeClass's block or the profile pins; an
ephemeral root has none) and the data disks' types. A size that cannot attach them is not offered on that route.
With the default route that means sizes without premium storage (`D2_v5`) are not offered at all, unless a storage
profile makes them work: then the type is offered for claims that select such a profile (its `storage-profile`
label has no `default`; Karpenter's scheduler may still match it for a claim that names no storage profile, but
Create never launches it for that claim: the candidate is skipped, and if every type a pod fits is such a size,
launches fail until the pod or NodePool selects a storage profile). To get such sizes, pin `storageAccountType: StandardSSD_LRS` in the NodeClass's root disk, or in a
storage profile. An empty list means unknown and excludes nothing. Premium disks also need the `PremiumIO`
capability: a size without it that passes the check fails the default root disk at launch (`Unsupported`, the
offering is unavailable for a while). Sizes without Gen2 or without a SCSI disk
controller (NVMe-only: most of the v6 families) are not offered. `PriceDelta` rounds a disk to its billing tier and uses Keeper's
GB-month price per region and SKU; Standard SKUs and unpriced disks make it incomplete; the public IPv4 address is
Keeper's `ipv4_price` row of the region (`SCARPENTER_AZURE_IPV4_PRICE` overrides it, also where Keeper has no row). Both
prices come from the Keeper snapshot the catalog loaded.

## Resources and network

Kinds (the keys of `spec.vendors.azure.resources`): `resourceGroups` (ID is the name; default lifecycle `retain`),
`subnets` (the ID of a subnet; tags are the virtual network's; `idleTTL`), `securityGroups` (`idleTTL`),
`proximityPlacementGroups` and, internal, `runSecurityGroups` (`perUse`). Azure has no default resource: without `use`
the order is the opt-in tag `scarpenter-use=true`, then creation of a resource group
`scarpenter-<cluster>-resource-groups-<region>`, a virtual network `10.240.0.0/16` with one `/20` subnet `nodes`, and a
network security group. Selectors: [Resource selectors](resource-selectors.md#azure).

The managed network security group has an allow rule between the nodes of its application security group (isolation
`nodeclass`, or `run` without a run key; `-nodes` after the group's name, created and deleted with it), the inbound rules
of `spec.networking.inbound` at priorities 110 and up (the node allow is 100: Azure takes 100 to 4096), and a deny of the virtual network's traffic at 4000 (Azure's built-in
`AllowVnetInBound` would let every node reach every other). Isolation `run` with a run key gets a group of its own per run.
**A NIC takes one network security group, so a group brought with `use` decides alone and `networking.isolation` has no
effect on it** (the vendor condition `ConfigurationWarning` says so, with one Warning event). A resource group is deleted only if it carries the marker and holds nothing
but SCarpenter's own leftovers; nothing brought is ever modified or deleted. Nodes are IPv4 only; without a public address
nothing gives them egress, so `publicIPv4: false` needs `subnets.use`. Node IP: the node's only interface has the subnet's private address (the public one is NAT'ed), which the kubelet of the `direct` template registers without a hint, so the driver reports no node IP range ([node bootstrap](node-bootstrap.md#the-node-ip-on-a-private-network)).

## Quotas, offerings and errors

Quota is per subscription and region: the Usage APIs report the whole subscription's usage, so nothing is counted by
hand; the buckets are `cores` (total regional), the size's family (from the SKU table, never from names),
`lowPriorityCores` (Spot) and `StandardPublicIPAddresses`. `Counted` lists this cluster's VMs so the in-flight ledger
releases its reservations. Offerings come from the SKU table (zones, restrictions, Gen2, SCSI). Errors are classified
by ARM code (never message text, except `OperationNotAllowed`, `InvalidParameter` and the zones of `SkuNotAvailable`, and the "security type ... not supported" text);
`AllocationFailed`, `ZonalAllocationFailed`, the `Overconstrained*` codes and `SpotPriceGreaterThanProvidedMaxPrice` are
`InsufficientCapacity`; `QuotaExceeded`, `ResourceQuotaExceeded` and `OperationNotAllowed` with a "exceeding approved ... quota" or
"exceeding quota limits" text are `QuotaExceeded`; `SkuNotAvailable` is `InsufficientCapacity` for Spot, `ZoneUnusable` on demand when ARM names the zones (the size is
not offered in that zone for the subscription) and `Unsupported` otherwise; a "VM size not supported ... security type" error (the confidential DC\*/EC\* sizes, which need
`ConfidentialVM`, unsupported by the driver) is `Unsupported` under any code; 429 is always `Retryable`. The table is in
`errors.go` and tested against an independent copy.

## Billing

Azure bills per full minute with no documented minimum: not a retention candidate. `BillingRule` reports a verified
one-minute rule with no minimum (the NodeClass status says `PerSecondBilling`) and `BillingStart` stays unset.

## Tests and the L5 lane

`go test ./pkg/vendors/azure/...` runs against the in-memory ARM (`fake/`), which returns real `*azcore.ResponseError`
values; `TestConformance` runs the shared suite over every active Azure type of the Keeper snapshot, once per resource
kind. The suite's resolution-order cases assume a vendor default resource; Azure has none, so `resolution_test.go` runs
ADR-022's order through the engine instead. The L5 lane is `test/e2e-azure/`, the sweeper `pkg/vendors/azure/sweep`
and the canary `test/canary/azure.go`; see [Test and develop](../how-to-guides/testing.md#the-l5-lane). They are validated
statically only, and the lane is not in `test/e2e-vendors.txt` until its cost guards (the size and location policies on
the lane's resource group, the quotas and a budget alert, the sweeper with the real role, the read-only canary role) are
verified on a live account: [Per-vendor cost guards](../how-to-guides/testing-and-releasing.md#per-vendor-cost-guards).

## What is not verified against Azure yet

The [interruption check](#interruption) (spike S4): that `az vm simulate-eviction` produces a `Preempt` event with a 30 s `NotBefore`, the
shape and spacing of the Scheduled Events JSON, the time until the first answer and the first event after the opt-in, that the VM name in `Resources` is
the name of `instance/compute/name`, whether the opt-in requires the first request to be held open longer than the check's `curl -m3`, and the agent on Azure Container Linux. The scenario waits 5 minutes after the first answer before Simulate Eviction (events may start only within 5 minutes of the opt-in); it cannot be run together with `arm64` (the preflight refuses it).

Everything else, except that a VM of a confidential size (SEV-SNP and TDX, westeurope) boots from the community gallery image with
the launch fields above, runs Ignition and starts the kubelet of the kubernetes sysext (no join to a cluster was tried).
In particular: the marketplace SKUs and versions per channel and architecture and whether the arm64
offer has terms; that a VM with custom data and a throwaway key is accepted; the Compute and Network usage names, the
SKU capability names and the family mapping; whether a failed allocation leaves a `Failed` VM; the `deleteOption`
cascade; the codes of capacity, quota, zone and in-use errors (the zonal `SkuNotAvailable` wording and the in-use codes
`InUseSubnetCannotBeDeleted`, `InUseNetworkSecurityGroupCannotBeDeleted`, `PublicIPAddressCannotBeDeleted` and
`NicInUse` are documented; `ZoneNotAvailableForSubscription` and the other in-use codes are guesses); the role actions;
Keeper's storage ids for Azure disks; device paths of data disks on Flatcar; accelerated networking on Flatcar; NVMe-only
sizes; boot time; the `DenyVnetInbound` rules between nodes; per-minute billing.
