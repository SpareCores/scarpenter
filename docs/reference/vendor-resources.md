# Vendor resources

What a node depends on at the vendor besides the instance (resource groups,
networks, subnets, security groups and firewalls, placement groups, separate
volumes), and how SCarpenter gets them: brought by the user (`use`) or managed
by SCarpenter (`create`) with an explicit lifecycle. Design: ADR-018 and
`docs/plans/v1/wip/00a-node-options.md` section 9. How to reference your own
resources: [Bring your own vendor resources](../how-to-guides/bring-your-own-resources.md).

## Table of Contents

- [Kinds](#kinds)
- [use, create, forbid](#use-create-forbid)
- [Lifecycles](#lifecycles)
- [Timing](#timing)
- [Names and ownership](#names-and-ownership)
- [Status](#status)
- [Garbage collection](#garbage-collection)
- [Metrics and Events](#metrics-and-events)
- [Driver contract](#driver-contract)

## Kinds

A driver declares its resource kinds (`ManagedResources.Kinds`); the kinds are
the keys of `spec.vendors.<vendor>.resources` (for AWS `subnets`,
`securityGroups`, `placementGroups`; the AWS driver also has the internal kind
`runSecurityGroups`, see the [AWS driver](aws-driver.md#resources-and-network)).
Each kind has:

| Property | Meaning |
|---|---|
| scope | `region`: shared infrastructure, one per region (networks, subnets, firewalls); `run`: one per placement run key (placement groups); `node`: one per instance (separate volumes) |
| default policy | the lifecycle of a managed resource of the kind when the NodeClass says nothing: `idleTTL` for shared infrastructure, `perUse` for per-run and per-node resources, `retain` for kinds that should stay stable (Azure resource groups) |
| delete order | the position in the dependency order of deletions (instances, volumes, placement groups, security groups, subnets, networks, resource groups) |

The driver says what an instance needs (`Prepare` asks the `ResourceProvider`
for each kind); the core decides where each comes from.

## use, create, forbid

Every kind has at most one of three settings (a CEL rule of the CRD):

```yaml
vendors:
  aws:
    resources:
      subnets:
        use: {selector: {tags: {scarpenter.sparecores.com/use: "true"}}}
      securityGroups:
        use: {ids: {us-east-1: [sg-0123], eu-central-1: [sg-0456]}}
      placementGroups:
        create: {lifecycle: {policy: perUse}, timing: {mode: onDemand}}
```

- **`use`** brings existing resources: by explicit IDs per region, by a name
  pattern with `{region}` and `{zone}` placeholders, or by a tag (label)
  selector, evaluated in every region in scope. `{zone}` is replaced by the zone
  of the launch; the status check expands it for every zone of the region: the
  region is unavailable when no zone has a match, and a zone without one is
  unavailable on its own (its offerings are gated per NodeClass, the other zones
  of the region are launched into). They are validated per region; a
  region where a referenced resource is missing has unavailable offerings and a
  stated reason. SCarpenter never modifies or deletes them: the engine never
  asks a driver to delete a resource that does not carry the ownership marker, and
  a driver refuses to.
- **`create`** lets SCarpenter create and own the resource when a launch needs it.
- **`forbid: true`** opts the kind out of creation: offerings that need it are
  unavailable unless the vendor's own default resource exists.
- **Neither**: the vendor's own default resource (the AWS default VPC's
  subnets, GCP's `default` network, Alibaba's default VPC) is used as found,
  like `use`, where it exists (`Resolve` with an empty reference); only where it
  does not exist the resource is created, with the kind's default lifecycle. This is
  the zero-configuration route. Per-run and per-node kinds never have a vendor
  default.

A placement profile may reference an existing group instead of creating one
(`aws: {placementGroup: {use: {name: my-pg}}}`): the group is resolved, never
created or deleted, and the run key only separates NodeClaims.

## Lifecycles

`create.lifecycle.policy`:

| Policy | The resource is deleted when | Default for |
|---|---|---|
| `perUse` | no instance uses it any more, after a grace period for launches in flight (`--resource-per-use-grace`, 2 minutes) | per-node resources (separate volumes), per-run resources (placement groups) |
| `idleTTL` (`ttl`) | no instance has used it for `ttl` (default `--resource-default-ttl`, 24 hours) | shared regional infrastructure |
| `retain` | its NodeClass is deleted, and it is unused | kinds that should stay stable |

A resource that an instance uses is never deleted. The time of the last use is
the later of the driver's answer (`InUse`: the resource is in use now, or since
when it is not) and the last launch that required it, so a launch in flight
keeps it. A shared (regional) resource has one deterministic name per region and kind, so
every NodeClass that maps to it (the region in its scope, the kind neither `use`
nor `forbid`) governs it: the most conservative lifecycle wins (`retain`, then
`idleTTL` with the longest TTL, then `perUse`), and `preCreate` of any of them
protects it. The NodeClass that created it only matters when no live NodeClass
maps to the resource any more; then its own settings apply while it exists,
else the kind's default policy (a kind whose default is `retain` is deleted once
unused for the grace period). The status of every NodeClass that maps to a
shared resource lists it. The idle time never counts from a moment before this
controller process first saw the resource: a restart restarts the idle clock, and
a resource seen in use counts as used now, whatever a driver that cannot say when
a resource was last used reports.

## Timing

`create.timing.mode`:

- `onDemand` (default): the first launch in a region that needs the resource
  creates it, and that launch waits for it. Creation is serialised per (vendor,
  kind, region) and idempotent (find by name and marker, else create); concurrent
  launches wait for the first one's result and share it.
- `preCreate` with `regions: [...]`: the resource is created in those regions by the
  status controller, ahead of any launch (it takes creation latency out of the
  first launch), and kept while the region stays listed: idle TTL does not apply.
  Removing the region from the list (or the NodeClass) ends the protection, and the
  lifecycle applies from then on.

## Names and ownership

A managed resource is named `scarpenter-<cluster>-<kind>-<region>`, with a
10-character hash of the zone and the key appended for resources that are not
shared per region (a placement group of a run key, a volume of a node; the create
parameters are part of the key, so a changed strategy does not adopt the group of
the old one). Names are at most 63 characters: a long cluster name is shortened
with a hash, and the middle of a long name is cut and replaced by a hash. The kind
in the name is written in lower case with hyphens (`placement-groups`). Every managed
resource carries the ownership marker as vendor tags (labels where there are no
tags), through the driver's `TagKey` mapping: `cluster`, `nodeclass`,
`managed-by` (`scarpenter`) and `resource-kind`, plus `spec.tags`. The
`resource-kind` tag holds the kind identifier as in the API (`placementGroups`,
not the hyphenated form of the name). **Both the
name prefix and the marker are required** before the garbage collector touches a
resource.

After a restart the controller finds its resources again by name and marker
(`Ensure` adopts an existing resource instead of creating a duplicate, counted as
an adoption) and the garbage collector lists them by marker.

## Status

The vendor status `status.vendors.<vendor>` holds:

- `resources`: the resources referenced and managed, per region (`kind`,
  `region`, `id`, `managed`, `lastUsed` for managed ones, `message` for a
  deletion that failed). At most 128 entries.
- the condition `ResourcesReady`: `True` when every region has what the NodeClass
  needs; `False` (reason `ResourcesUnavailable`) with a message that names each
  region and kind (`eu-west-1: subnets: nothing matches the tags ...`), and a
  Warning Event `ResourcesUnavailable`, when a `use` reference matches nothing, a
  `forbid` kind has no default, or a preCreate resource cannot be created;
  `Unknown` (reason `CheckFailed`) when the vendor could not be asked (the
  previous verdict stays).

The offerings of a region whose resources are missing or failed are unavailable
(the catalog asks the engine per NodeClass), and a launch that finds the resource
missing fails the candidate without marking the instance type or the zone. The
NodeClass itself stays `Ready`. Regions are checked every
`--resource-check-interval` (30 seconds); the engine trusts a recent answer for
30 seconds (`use`) or one minute (ensured resources). The status is written
with server-side apply under the field manager `scarpenter-resources`, which
owns only these fields.

## Garbage collection

The garbage collector (every `--gc-interval`) runs the resource sweep after the
instance and token sweeps (overview section 10). For every managed resource of
the cluster in the regions in use it asks the driver whether an instance uses it
(`InUse`), applies the lifecycle and deletes in the driver's dependency order per
region: lower delete orders first, and when a deletion fails, the kinds that
depend on it in the same region wait for the next sweep. A resource the vendor
refuses to delete (still used by something SCarpenter does not own) is left in
place, marked in the status, and retried with exponential backoff (30 seconds,
doubling, at most 10 minutes). `--gc-dry-run` logs and counts without deleting.

Every sweep lists the regions in use (the regions of the existing NodeClasses and
NodeClaims) and the regions where an earlier full sweep found managed resources.
Every `--resource-full-sweep-every` sweeps (default 10), and right after a
NodeClass disappeared, it lists every region of the driver by the ownership
marker, so resources of a region no NodeClass uses any more (the last NodeClass
deleted, a region or the vendor block removed) are cleaned by their lifecycle too.
A deletion waits for a launch that ensures the same resource (per vendor, kind and
region lock) and is skipped when a launch needed the resource after the sweep
planned it.

## Metrics and Events

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `scarpenter_resource_operations_total` | counter | `vendor`, `kind`, `region`, `operation`, `result` | `operation`: `create`, `adopt`, `delete`; `result`: `ok`, `error`, `dry-run`. |
| `scarpenter_resources_managed` | gauge | `vendor`, `kind`, `region` | Managed resources found by the last sweep. |

Events (on the SCNodeClass of the resource's `nodeclass` tag, else on the
controller Deployment): `ResourceCreated`, `ResourceAdopted`, `ResourceDeleted`
(Normal), `ResourceCreateFailed`, `ResourceDeleteFailed` (Warning,
deduplicated for 10 minutes), `ResourcesUnavailable` (Warning, on the NodeClass).
Every creation, adoption and deletion is also logged.

## The qemu driver's networks

The development vendor `qemu` has one kind, `networks` (scope `region`, default
policy `retain`): a Linux bridge of the machine, whose ID is its interface name.
Without a `networks` block, and with `forbid`, the default bridge
(`--qemu-bridge`) is used when it exists; `use` finds bridges by ID, name pattern
or `key=value` tags in the interface alias (`ip link set <bridge> alias "env=lab"`);
`create` makes a bridge with an address (a /24 of `10.88.0.0/16` derived from its
name), no DHCP server and no NAT: those are the machine's job, so VMs on a created
bridge only get an address when something serves DHCP there. Created bridges are
recorded in the state directory, found again after a restart, recreated if the
machine lost them, and deleted only while no VM is attached.

## Driver contract

`vendors.ManagedResources`: `Kinds`, `Resolve` (existing resources by IDs, name
pattern or tags; an empty reference means the vendor's default; nothing found is
an empty result), `Ensure` (find by name and marker, else create; idempotent;
reports whether it created), `InUse` (in use now, and the last use or creation),
`Delete` (idempotent; refuses resources that are not managed; a resource the
vendor will not delete is `Retryable`) and `List` (by marker, per region). See
[the driver contract](driver-contract.md#node-options-and-vendor-resources).
