# Vendor resources

What a node depends on at the vendor besides the instance (resource groups,
networks, subnets, security groups and firewalls, placement groups, separate
volumes), and how SCarpenter gets them: brought by the user (`use`) or managed
by SCarpenter (`create`) with an explicit lifecycle. Design: ADR-018 and
`docs/plans/v1/wip/00a-node-options.md` section 9. How `use` finds resources, for every
kind and vendor: [Resource selectors](resource-selectors.md) (ADR-023). How to reference
your own resources: [Bring your own vendor resources](../how-to-guides/bring-your-own-resources.md).

## Table of Contents

- [Kinds](#kinds)
- [use, create, forbid](#use-create-forbid)
- [Resolution order](#resolution-order)
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
For UpCloud they are `networks`, `firewalls` and `serverGroups`, plus the use-only
`images` ([UpCloud driver](upcloud-driver.md#resources-and-network)).
For GCP they are `subnets`,
`firewalls` (network tags) and `resourcePolicies`, with the internal `runFirewalls`, see the
[GCP driver](gcp-driver.md#resources-and-network); their networks, subnetworks, rules and policies have no
labels, so they are marked by a name prefix and a description and `tags` is rejected.
Each kind has:

| Property | Meaning |
|---|---|
| scope | `region`: shared infrastructure, one per region (networks, subnets, firewalls); `run`: one per placement run key (placement groups); `node`: one per instance (separate volumes) |
| default policy | the lifecycle of a managed resource of the kind when the NodeClass says nothing: `idleTTL` for shared infrastructure, `perUse` for per-run and per-node resources, `retain` for kinds that should stay stable (Azure resource groups) |
| delete order | the position in the dependency order of deletions (instances, volumes, placement groups, security groups, subnets, networks, resource groups) |

The driver says what an instance needs (`Prepare` asks the `ResourceProvider`
for each kind); the core decides where each comes from.

**`images`** is a kind of its own (plan 14 A1): the boot image of a launch, in every vendor block's
`resources`, optional. It takes `use` only, with the one selector: there is no `create`
(SCarpenter does not create images, rung 4 of the [image ladder](../explanation/flatcar-images.md) is
reserved) and no `forbid`, and `select: all` is rejected (a launch needs one image); the CRD enforces all three.
It is not a kind the engine lists, creates or deletes: the driver resolves it per launch request
(`ImageRequest.Use`, [driver contract](driver-contract.md#images)), narrowed to the request's
architecture, and the image must say it is the pinned Flatcar release or its region's images are not
ready. For rungs 1 and 2 `images` is optional (AWS without it finds the pinned release by name in the
owner's account); for rung 3 the driver has a documented convention and `images` overrides it. The vendor
fields that name an owner (`aws.imageOwner`) stay: they say whose images to look in, not which. The qemu
development vendor has no `images` kind (it downloads the release itself).

## use, create, forbid

Every kind has at most one of three settings (a CEL rule of the CRD):

```yaml
vendors:
  aws:
    resources:
      subnets:
        use: {tags: {scarpenter.sparecores.com/use: "true"}}
      securityGroups:
        use: {ids: {us-east-1: [sg-0123], eu-central-1: [sg-0456]}}
      placementGroups:
        create: {lifecycle: {policy: perUse}, timing: {mode: onDemand}}
```

- **`use`** brings existing resources, found by the one selector of
  [Resource selectors](resource-selectors.md) (ADR-023), evaluated in every region in
  scope: explicit `ids` per region, a `name` glob, `tags` (labels, where the vendor has
  no tags), portable `match` attributes (architecture, owner, state, CIDR, zone,
  creation time), the vendor's native `filters` and `expression`, and `select` and
  `orderBy` for how many and which. All given fields are ANDed; a field a vendor does
  not support for the kind is rejected. `{zone}` is replaced by the zone
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
- **Neither**: the resolution order below (explicit, discovered, vendor default,
  created, or a clear failure). This is the zero-configuration route. Per-run and
  per-node kinds never have a vendor default.

A placement profile may reference an existing group instead of creating one
(`aws: {placementGroup: {use: {name: my-pg}}}`): the group is resolved, never
created or deleted, and the run key only separates NodeClaims.

## Resolution order

[ADR-022](../explanation/DECISIONS.md#adr-022-resolving-vendor-resources-explicit-discovered-default-created-or-a-clear-failure-accepted-2026-11-10).
For each kind, in each region (steps 2 to 4 only for a kind without `use`):

1. **Explicit `use`** (the [selector](resource-selectors.md)): as given, validated,
   never modified. A `use` that matches nothing, or more than `select` allows, fails;
   it never falls through.
2. **Discovery**: resources carrying the opt-in tag or label
   `scarpenter.sparecores.com/use: "true"` (encoded as the other SCarpenter tags:
   `scarpenter-use` where `/` is not allowed; the selector `use: {tags: {...: "true"}}` with
   the kind's default `select`, built in). The operator's explicit opt-in, so
   it wins over a vendor default. Vendors whose resources have no tags or labels
   skip this step. Treated as `use`. Nothing tagged goes on to step 3; more than
   `select` allows fails.
3. **Vendor default**, where the vendor has one and it is safe: the AWS default VPC
   and its default subnets, GCP's `default` network, Alibaba's default VPC and
   vSwitches (Azure, Hetzner, OVH, UpCloud and Vultr have none for the kinds
   SCarpenter needs). A default that cannot honour the request (ADR-017: run
   isolation, inbound rules, `publicIPv4: false`, IPv6) is skipped, not degraded.
   Treated as `use`.
4. **Create**, managed with the lifecycle below, where the vendor and the
   credentials allow it. `forbid: true` removes this step.
5. **Fail gracefully**: `ResourcesReady=False` (reason `ResourcesUnavailable`) names
   the kind, the region and what was tried; no launch is attempted in that region;
   nothing half-created is left behind.

An explicit `create` skips steps 2 and 3. Kinds that live in a network (subnets,
security groups) are resolved within the network that was resolved, whichever step found it.

**How it runs.** The core runs the order for a launch and for the status check alike
(`pkg/resources/order.go`), per vendor, kind and region, and records which step produced each
resource in the status (`source`). Only regional kinds (`scope: region`) are discovered or have a
default: a per-run or per-node kind (placement groups, separate volumes) is `use`, `create` or
created. A step that finds nothing goes on; a step that errors stops the order. A vendor error is
`CheckFailed` (the last verdict stays) and a launch fails with the vendor's class, never "fall
through to create"; an opt-in that is ambiguous for the kind's `select` is the failure of step 5.
The drivers add three optional hooks (below): the AWS driver has defaults for `subnets` (the default
VPC's default subnets, skipped for `publicIPv6` when any of them lacks an IPv6 CIDR block, so the
managed network, which has IPv6, is created instead) and the qemu driver for `networks` (the
`--qemu-bridge` bridge). The other vendors' drivers declare their defaults, if any, with their
`ManagedResources` implementation. A kind the driver declares optional (`KindSpec.Optional`, a
network the operator may bring) ends after step 3: nothing found means the launch goes without
it, never a creation or a failure.

A kind that lives in a network (`KindSpec.InNetwork`: AWS security groups live in the network of
the subnets) is resolved after it and only inside the networks it resolved to, whichever step
found them: the core adds `match.network` to the selector of steps 1 and 2 where the driver declares
it (an explicit `match.network` of the NodeClass is kept) and filters a default by the network the
driver reports on each resource. At a launch the driver names the network it chose
(`ResourceRequest.Network`: the VPC of the subnet), and a resource outside it skips that zone
only. Where the network is the managed one, the status check constrains the dependent kind to it
once it exists, as a launch does; while it does not exist yet (it is created on demand) nothing
constrains the dependent kind.

The failure of step 5 names the kind and what was tried, for a launch and the status condition
alike: `subnets: no resource found; tried the opt-in tag scarpenter.sparecores.com/use=true: nothing
is tagged; the vendor's default cannot honour the request: publicIPv6 needs an IPv6 CIDR block and
the default subnets subnet-0a1 have none; creating it is forbidden`. A launch's error names the
region too. Nothing half-created is left behind: a creation that fails midway is adopted and
completed by the next attempt (`Ensure` is idempotent).

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
  `region`, `id`, `name`, `managed`; `lastUsed` for managed ones, `message` for a
  deletion that failed; `source`: the step of the [resolution order](#resolution-order) that
  produced the resource, `use`, `discovered`, `default` or `created`).
  At most 128 entries, in the order kind, region, ID. A `use` with `select: all` lists
  every match, so many regions with many subnets can reach the cap; the list is then cut
  at 128 and `resourceCounts` (`kind`, `region`, `count`) says how many resources there
  are in every region and kind. Without the cap `resourceCounts` is absent.
- a launched node records the IDs it launched with in the annotation
  `scarpenter.sparecores.com/resources` ([labels](labels.md)); a later re-resolution that
  finds other resources changes new launches only.
- the condition `ResourcesReady`: `True` when every region has what the NodeClass
  needs; `False` (reason `ResourcesUnavailable`) with a message that names each
  region and kind and what excluded the rest (`eu-west-1: subnets: 14 listed, 3 match
  tags, 0 match zone`, see [Resource selectors](resource-selectors.md#resolution-status-and-caching)), and a
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
(`--qemu-bridge`) is used when it exists; `use` finds bridges with the selector by `ids`,
`name` or `tags` (`key=value` pairs in the interface alias, `ip link set <bridge> alias "env=lab"`);
`create` makes a bridge with an address (a /24 of `10.88.0.0/16` derived from its
name), no DHCP server and no NAT: those are the machine's job, so VMs on a created
bridge only get an address when something serves DHCP there. Created bridges are
recorded in the state directory, found again after a restart, recreated if the
machine lost them, and deleted only while no VM is attached.

## Driver contract

`vendors.ManagedResources`: `Kinds`, `Resolve` (existing resources by the selector, `UseRef`: IDs, name, tags, `match`,
`filters`, `expression`, `select` and `orderBy`, placeholders already expanded; an empty
reference selects nothing; a selector that selects nothing, or too much, is
a typed `NoMatch` or `Ambiguous` error with the counts), `Selectors` (what the driver
supports of the selector per kind, and on which side), `Ensure` (find by name and marker, else create; idempotent;
reports whether it created), `InUse` (in use now, and the last use or creation),
`Delete` (idempotent; refuses resources that are not managed; a resource the
vendor will not delete is `Retryable`) and `List` (by marker, per region). Optional hooks, all with a no-op default:
`ResourceDefaults.Default` (the vendor's own default resources of a kind, none when it has none:
step 3), `DefaultChecker.DefaultSuits` (whether the default honours the settings of the request;
`false` skips it with a reason, never degrades the request) and `ResourceDiscoverer.Discover`
(an override of step 2; without it the core runs the selector `tags: {<opt-in key>: "true"}` with
the kind's default `select` through `Resolve`, and skips the step for a kind whose declaration has no
tags). See
[the driver contract](driver-contract.md#node-options-and-vendor-resources).
