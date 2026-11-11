# Azure driver (foundation)

`pkg/vendors/azure` is the vendor-specific foundation of the Azure driver. It is
not yet a `vendors.Driver`: it is not registered, has no NodeClass API types and
no conformance wiring, and nothing in it has run against Azure (no cloud call is
made by any test). The design is plan 17; the contract the finished driver
implements is the [driver contract](driver-contract.md).

## What exists

| Part | Where | What it does |
|---|---|---|
| API interfaces | `api.go` | The calls the driver needs, one narrow interface per service (VMs, NICs, public IPs, tag listing, SKUs, usages, images, marketplace agreements, locations). Long-running operations are awaited inside the call, pagers are drained. |
| SDK adapters | `sdk.go` | The real track-2 clients behind those interfaces, created lazily per subscription, with the `scarpenter/<version>` user agent. |
| Credentials | `credential.go` | The chain: workload identity, environment (mode `secret`), managed identity, Azure CLI only under a development flag. |
| In-memory ARM | `fake/` | A fake subscription behind the same interfaces: idempotent VM `PUT` by name, provisioning and power states, quota and capacity failures, the delete-option cascade, tag listing without tags, error injection. Errors are real `*azcore.ResponseError` values. |
| Errors | `errors.go` | ARM error codes to the contract's classes (table test with an independent copy); `SkuNotAvailable` depends on the priority, quota and `InvalidParameter` have message exceptions. |
| Mapping | `names.go`, `states.go`, `skus.go` | VM name `<nodeclaim>-<hash8(cluster)>`, tag keys `scarpenter-<key>`, provider ID, instance reference `<region>/<resource group>/<vm>`, image URN, VM state, the SKU table (Gen2, SCSI, restrictions, zones). |
| Launch request | `launch.go`, `settings.go` | The VM, NIC and public IP bodies with every field of plan 17 section 3, the settings of section 7 and their checks against the size's capabilities. |
| Instances | `compute.go` | Launch (replay by name, wait, cleanup of a failed attempt), get, find, list, delete (cluster-tag check). |
| Images | `image.go` | Marketplace lookup of the official Flatcar offers, one hour cache, the terms check. |
| Quotas | `quota.go` | Both vCPU tiers, Spot and public IPs from the Usage APIs; the type to bucket mapping from the SKU table. |

## Behaviour worth knowing

- The deterministic VM name replaces the idempotency label. A replay returns the
  existing VM whatever zone, size or capacity type it asks for; a `Failed` VM of
  another size or zone is a leftover and is deleted first.
- A launch waits for the long-running operation for 4 minutes; a VM still
  `Creating` then is returned as pending. A failed create deletes its VM unless the error
  is transient, so the key stays unused.
- `stopped`, `stopping`, `deallocating`, `deallocated` and `Deleting` are
  `terminating`; a `Failed` VM that exists is `running`.
- Azure's limits are per subscription and region, so no quota error is
  account-wide.
- Sizes without Gen2, without a SCSI disk controller, or restricted for the
  subscription are not launchable.
- SCarpenter never accepts marketplace terms: the check fails with
  `MarketplaceTermsNotAccepted` and names the `az vm image terms accept` command.

## Not yet

The `vendors.Driver`, `NodeOptions` and `ManagedResources` implementations
(resource groups, VNet and subnet, NSG, application security groups, proximity
placement groups, and the public IP and NIC `Prepare` now done inside `Launch`),
registration, NodeClass types, the `images` kind, conformance and the L5 lane. The
seams are marked `TODO(plan 14)` in the code.
