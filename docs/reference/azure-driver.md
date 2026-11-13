# Azure driver

`pkg/vendors/azure` launches, lists and deletes Flatcar VMs in one Azure subscription. It is a
complete `vendors.Driver` (registered as `azure`, enabled with `--vendors=azure`) with node options,
managed resources, quotas and a NodeClass validator, and it passes the shared conformance suite
against an in-memory ARM. **Nothing in it has run against Azure**: no test makes a cloud call. The
design is [plan 17](../plans/v1/wip/17-azure.md); the contract it implements is the
[driver contract](driver-contract.md). The NodeClass block is `spec.vendors.azure` ([API](api.md)).

- [Account and credentials](#account-and-credentials)
- [Regions, zones and images](#regions-zones-and-images)
- [Launch](#launch)
- [Node settings](#node-settings)
- [Resources and network](#resources-and-network)
- [Quotas, offerings and errors](#quotas-offerings-and-errors)
- [Billing](#billing)
- [Tests and the L5 lane](#tests-and-the-l5-lane)
- [What is not verified against Azure yet](#what-is-not-verified-against-azure-yet)

## Account and credentials

One controller works in one subscription: `AZURE_SUBSCRIPTION_ID` is required (the chart value
`vendors.azure.subscriptionID`). `spec.vendors.azure.subscriptionID` is only a guard: a different value
fails the NodeClass validation. The credential chain is workload identity (`AZURE_CLIENT_ID`,
`AZURE_TENANT_ID`, `AZURE_FEDERATED_TOKEN_FILE`), then, in mode `secret`, the client secret of the
environment, then the managed identity; the Azure CLI's login only under `SCARPENTER_AZURE_DEVELOPMENT=true`.
How to set it up: [Azure credentials](../how-to-guides/azure-credentials.md). The custom role is
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
returned pending. Disks, NIC and public IP carry `deleteOption: Delete`. The VM has `core` as admin user and one
public key (`adminSSHPublicKey`, else a key generated at start whose private half is discarded: access is
Ignition's business). Spot: priority `Spot`, eviction `Delete`, `maxPrice: -1`. States: `Creating` pending,
`running` running; stopped, deallocated, `Deleting` terminating (the stopped-node rule); a `Failed` VM that exists
is running (it costs: the NodeClaim deletes it). The instance reference is `<region>/<resource group>/<vm>`.
Tags are `scarpenter-<key>` plus `Name`. `DeleteInstance` refuses a VM without the cluster tag.

## Node settings

| Block | Fields |
|---|---|
| `storage.root` | `storageAccountType` (default `Premium_LRS`), `diskSizeGB` (64), `caching` (`ReadWrite`), `diskEncryptionSetID`, `ephemeral.placement` |
| `storage.data[]` | `name`, `storageAccountType`, `diskSizeGB`, `caching`, `filesystem`, `mountPath`; the device is `/dev/disk/azure/scsi1/lun<N>`, N the index |
| `storage.localStorage` | `ignore` only |
| network profile | `acceleratedNetworking.enabled`: `auto` (on where the size has it), `"true"` (other sizes unsupported), `"false"` |
| placement profile | `proximityPlacementGroup`: `strategy: cluster` (one group per run key) or `use.name` |
| settings | `adminSSHPublicKey`, `encryptionAtHost`, `imageOwner`, `subscriptionID` |

Premium disks need the `PremiumIO` capability: sizes without it fail the default root disk (`Unsupported`, the
offering is unavailable for a while); set `StandardSSD_LRS` to include them. Sizes without Gen2 or without a SCSI disk
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
of `spec.networking.inbound` at priorities 100 and up, and a deny of the virtual network's traffic at 4000 (Azure's built-in
`AllowVnetInBound` would let every node reach every other). Isolation `run` with a run key gets a group of its own per run.
**A NIC takes one network security group, so a group brought with `use` decides alone and `networking.isolation` has no
effect on it** (the vendor condition `ConfigurationWarning` says so, with one Warning event). A resource group is deleted only if it carries the marker and holds nothing
but SCarpenter's own leftovers; nothing brought is ever modified or deleted. Nodes are IPv4 only; without a public address
nothing gives them egress, so `publicIPv4: false` needs `subnets.use`.

## Quotas, offerings and errors

Quota is per subscription and region: the Usage APIs report the whole subscription's usage, so nothing is counted by
hand; the buckets are `cores` (total regional), the size's family (from the SKU table, never from names),
`lowPriorityCores` (Spot) and `StandardPublicIPAddresses`. `Counted` lists this cluster's VMs so the in-flight ledger
releases its reservations. Offerings come from the SKU table (zones, restrictions, Gen2, SCSI). Errors are classified
by ARM code (never message text, except `OperationNotAllowed`, `InvalidParameter` and the zones of `SkuNotAvailable`);
`SkuNotAvailable` is `InsufficientCapacity` for Spot, `ZoneUnusable` on demand when ARM names the zones (the size is
not offered in that zone for the subscription) and `Unsupported` otherwise; 429 is always `Retryable`. The table is in
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

Everything. In particular: the marketplace SKUs and versions per channel and architecture and whether the arm64
offer has terms; that a VM with custom data and a throwaway key is accepted; the Compute and Network usage names, the
SKU capability names and the family mapping; whether a failed allocation leaves a `Failed` VM; the `deleteOption`
cascade; the codes of capacity, quota, zone and in-use errors (the zonal `SkuNotAvailable` wording and the in-use codes
`InUseSubnetCannotBeDeleted`, `InUseNetworkSecurityGroupCannotBeDeleted`, `PublicIPAddressCannotBeDeleted` and
`NicInUse` are documented; `ZoneNotAvailableForSubscription` and the other in-use codes are guesses); the role actions;
Keeper's storage ids for Azure disks; device paths of data disks on Flatcar; accelerated networking on Flatcar; NVMe-only
sizes; boot time; the `DenyVnetInbound` rules between nodes; per-minute billing.
