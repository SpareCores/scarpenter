# Infrastructure hand-off

What your infrastructure as code, sc-infra for Spare Cores or your own, must
provide for SCarpenter to launch nodes at each vendor: the resources a
`SCNodeClass` references, the account and credentials, and the Flatcar images.
The design is ADR-018 (vendor resources) and
[ADR-021](../explanation/DECISIONS.md#adr-021-flatcar-images-the-image-ladder-accepted-2026-11-10)
(images); how the references work is in [Vendor resources](vendor-resources.md)
and [Bring your own vendor resources](../how-to-guides/bring-your-own-resources.md);
where the images come from is in [Flatcar images](../explanation/flatcar-images.md).

Status of this page: it is written from the vendor plans (researched 2026-11-10)
and from sc-infra's `inventory` v1. Only the `aws` driver exists today; every other
vendor block is a plan ([`../plans/v1/wip/`](../plans/v1/wip/)), and its kinds and
fields may change when the driver is written. The "inventory" columns say what
sc-infra's `inventory` v1 holds today and what it lacks; the groundwork that adds
the missing parts is [plan 14](../plans/v1/wip/14-multi-vendor-groundwork.md).

## Table of Contents

- [How the hand-off works](#how-the-hand-off-works)
- [The inventory](#the-inventory)
- [Images](#images)
- [aws](#aws)
- [gcp](#gcp)
- [azure](#azure)
- [hcloud](#hcloud)
- [ovh](#ovh)
- [upcloud](#upcloud)
- [vultr](#vultr)
- [alicloud](#alicloud)

## How the hand-off works

SCarpenter does not read your IaC state. It takes the resources of a NodeClass by
`use`: explicit `ids` per region, a `name` pattern, or a tag `selector` where the
vendor has tags. Your IaC therefore creates the resources, tags them where the
vendor allows, and renders the `spec.vendors.<vendor>` block (sc-infra renders it
from the inventory: explicit IDs, the same for vendors without tags). With `use`,
SCarpenter never modifies or deletes the resource. Without `use` it creates what
it needs with an explicit lifecycle (`create`), except images, which it never
creates.

Every vendor block has `regions` (the vendor's API names; empty means every region
the credentials can use) and may have `resources.images` (below). Everything else
is per vendor.

## The inventory

sc-infra's `inventory` v1 is a versioned document, one entry per environment (a
vendor account scope) and region, always with every key present (`null`, `[]` or
`{}` where there is nothing):

| Key | Content |
|---|---|
| `environments.<env>.vendor`, `scope` | the vendor, and the account the ids live in (below, per vendor) |
| `regions.<region>.network` | `{id, name, cidr, ipv6_cidr}` |
| `regions.<region>.subnets` | `[{id, name, cidr, ipv6_cidr, zone, zone_name}]` |
| `regions.<region>.security_groups` | `{nodes: {id, name}, jobs: {id, name}}`; only `nodes` is ever given to SCarpenter |
| `regions.<region>.firewall_model` | `security_group`, `nsg`, `network_tag`, `firewall`, `ruleset` or `firewall_group` |
| `regions.<region>.ssh_key`, `tags_supported`, `scope` | the SSH key (unused by SCarpenter), whether the network resources carry tags, region-level scope (Azure `resource_group`) |

Region keys are the vendor's API region names. The tag that marks what SCarpenter
may select is `scarpenter.sparecores.com/use: "true"`, encoded per vendor
(`scarpenter-use` where `/` is not allowed); the role tag `sc-infra.sparecores.com/role`
(`sc-infra-role`) separates the `nodes` and `jobs` groups, and the `jobs` group (wide
open) must never reach nodes.

**Not rendered yet:** sc-infra's `scarpenter` block renders only `aws` today
(`ACCEPTED_VENDORS`), with AWS's kinds (`subnets`, `securityGroups`); every other
vendor needs its own kind map ("the kind map" below).

**Not in the inventory yet:** the images. The plan is an additive key
`regions.<region>.images: [{id, name, arch, channel, version}]`, always present and
`[]` on rungs 1 and 2, inventory staying `version: 1`; a vendor with global images
lists the same image in every region. The tables below say "images (planned)" where
a vendor needs it.

## Images

The `images` kind, the same for every vendor:

```yaml
resources:
  images:
    use:
      ids: {<region>: [<image id>, ...]}     # one list per region; a global image is repeated
      # or  name: "flatcar-{channel}-{version}-{arch}"
      # or  selector: {tags: {...}}           # only where the vendor filters images by tags or labels
```

There is no `create`. Placeholders in `name` and in selector values are `{channel}`,
`{version}`, `{arch}` and `{region}`. Several IDs in a region (the two
architectures, UpCloud's tiers) are matched to the request by the image's own
architecture, channel and version. Without `images` the driver resolves the pinned
version itself: from the vendor's own images (rungs 1 and 2) or by the documented
naming convention in your account (rung 3). Who creates the images per vendor:
[Flatcar images](../explanation/flatcar-images.md#overview).

## aws

Images: rung 1, nothing to provide (`imageOwner` overrides the AMI owner).

| What | `spec.vendors.aws` | Inventory today | Missing today |
|---|---|---|---|
| Account and credentials | none in the block; credentials: IAM role with web identity (recommended), keys in a Secret, or the node's role | `scope.account_id` | the controller's IAM role is not created by the `scarpenter` block |
| Subnets | `subnets.use`: `ids` per region, name pattern (`{zone}` is the account's zone name), or tag selector | `regions.<r>.subnets[].id` (one per zone; `zone` is the stable zone ID, `zone_name` the account's name) | none |
| Security groups | `securityGroups.use`: `ids` or tag selector (only the `nodes` group carries the tag); with it, `networking.inbound` and `isolation: nodeclass` rules do not apply (the run-isolation group is still added) | `regions.<r>.security_groups.nodes.id` | none |
| Placement groups | `placementGroups`, in placement profiles | none (SCarpenter creates them) | none |

## gcp

Images: rung 1 for amd64 (`kinvolk-public`), rung 2 for arm64 (a Spare Cores
project): nothing to provide.

| What | `spec.vendors.gcp` | Inventory today | Missing today |
|---|---|---|---|
| Project and credentials | `project` (must equal the controller's `--gcp-project`); credentials: workload identity federation (recommended), service-account key, or the metadata server | `scope.project_id` | the workload identity pool, provider, service account and custom role are not created |
| Subnets | `subnets.use`: `ids` (`projects/<p>/regions/<r>/subnetworks/<n>`) or name pattern; no selector (no labels) | `regions.<r>.subnets[0].id` | the kind map in `vendor_block()` |
| Firewalls | `firewalls.use`: `ids` are network tags (the `nodes` group is the tag `nodes`) or name pattern; no selector | `regions.<r>.security_groups.nodes.id` (`firewall_model: network_tag`) | the kind map |
| Resource policies | `resourcePolicies`, in placement profiles | none | none |
| Egress | nodes without a public IPv4 need a Cloud NAT on the subnet | no NAT is created | document only |

## azure

Images: rung 1 (Marketplace), nothing to provide; the subscription must be allowed
to use the offers (SCarpenter never accepts marketplace terms).

| What | `spec.vendors.azure` | Inventory today | Missing today |
|---|---|---|---|
| Subscription and credentials | `subscriptionID` (a guard: must equal the controller's); credentials: workload identity (recommended), secret, or managed identity; one subscription per controller | `scope.subscription_id` | `scope.tenant_id`; the managed identity, its federated credential and the custom role; the registered resource providers |
| Resource groups | `resourceGroups.use`: `ids` (name or ARM ID), name pattern, tag selector | `regions.<r>.scope.resource_group` (a name; the group carries no `scarpenter-use` tag) | render by name, or tag the group |
| Subnets | `subnets.use`: `ids` (full subnet ARM ID), `<vnet>/<subnet>` name pattern, tag selector (matches the VNet's tags; subnets have no tags); no `{zone}` | `regions.<r>.subnets[0].id` (the VNet carries the use tag) | the kind map |
| Security groups (NSGs) | `securityGroups.use`: `ids` (ARM ID), name, tag selector; a NIC takes one NSG, so with a brought NSG `isolation` has no effect | `regions.<r>.security_groups.nodes.id` | the kind map |
| Egress | the subnet has no NAT: nodes need `publicIPv4: true` | none | none |

## hcloud

Images: rung 3. Provide one snapshot per architecture and pinned version, labelled
`os=flatcar`, `flatcar-channel=<channel>`, `flatcar-version=<version>`; they are
global in the project (the IDs are listed under every location).

| What | `spec.vendors.hcloud` | Inventory today | Missing today |
|---|---|---|---|
| Project and credentials | none: the token is bound to one project; a read-write token in a Secret, separate from the IaC token | `scope: {}` | the controller's own token |
| Images | `images.use`: `ids` per location, or the label convention (default) | none | images (planned): the snapshots, created by `hcloud-upload-image` from Flatcar's `hetzner` image |
| Networks | `networks.use`: `ids` per location, name pattern, label selector; optional (without one nodes have a public address only); no `create` in v1 | `regions.<r>.network.id` (labelled) | the kind map |
| Firewalls | `firewalls.use`: `ids`, name, label selector; with it `networking.inbound` rules do not apply | `regions.<r>.security_groups.nodes.id` (the same ID in every location; labelled) | the kind map |
| SSH key | `sshKey` (optional; nodes use Ignition) | `ssh_key.name` | none |
| Subnet | none: a server gets an address from the network's subnet in its location's network zone | `subnets[].id` is `null` for hcloud, unused | none |

## ovh

Images: rung 3. Provide one Glance image per region and architecture (amd64 only in
v1) from Flatcar's OpenStack image, named `flatcar-<channel>-<version>-<arch>` with
the tags `flatcar` and `flatcar-<channel>-<version>`.

| What | `spec.vendors.ovh` | Inventory today | Missing today |
|---|---|---|---|
| Project and credentials | none in the block; one Public Cloud project per controller; a service account (OAuth2) or an OpenStack user | `scope.service_name` | the controller's service account and its IAM policy |
| Regions | the Keystone catalog's names, upper case (`EU-WEST-PAR`, `GRA11`); the driver maps Keeper's names to them | region keys come from a static list of the 2026 names, which may not be the catalog's | sc-infra reads the project's regions from the API |
| Images | `images.use`: `ids` per region, name pattern, Glance tag selector | none | images (planned): a Glance upload in every region |
| Security groups | `securityGroups.use`: `ids` (Neutron UUIDs), name, tags | `regions.<r>.security_groups.nodes.id` (whether it is the Neutron UUID is unverified) | the `nodes` group admits the environment's vRack ranges only: with it only the `tailscale` template works on `Ext-Net` nodes |
| Networks | none in v1 (public `Ext-Net` only; a private network is deferred) | `regions.<r>.network.id` stays unused | none |

## upcloud

Images: rung 3. Provide one template per zone and storage tier from the images
repository's `flatcar-upcloud-<channel>-<version>-amd64.img.gz` (import into a
storage, then a template; delete the source storage), labelled as in
[Flatcar images](../explanation/flatcar-images.md#upcloud).

| What | `spec.vendors.upcloud` | Inventory today | Missing today |
|---|---|---|---|
| Account and credentials | none: an API token (Secret) of the account or a sub-account; tokens expire and have no scopes; the controller's token must see what the IaC created | `scope: {}` | a token for the controller |
| Images | `images.use`: `ids` per zone (both tiers), label selector (default) | none | images (planned): the templates |
| Networks | `networks.use`: `ids`, name pattern, label selector; optional (without one the node has the public and utility interfaces only) | `regions.<zone>.network.id` (labelled) | the kind map; SCarpenter-created networks need a range outside sc-infra's pool |
| Firewalls | `firewalls.use`: `ids` (SDN rulesets), name, selector; covers only the private interface: the public interface's firewall is written per server by the driver | `regions.<zone>.security_groups.nodes.id` | the kind map |

## vultr

Images: rung 3. Provide one snapshot per pinned version, created from the images
repository's image at its public URL, described `flatcar-<channel>-<version>-amd64`
(global in the account).

| What | `spec.vendors.vultr` | Inventory today | Missing today |
|---|---|---|---|
| Account and credentials | none: one API key = one account; a Secret; the key is bound to an IP allowlist that must contain the controller's egress address | `scope: {}` | none |
| Images | `images.use`: `ids` per region (the same snapshot repeated), description name pattern | none | images (planned): `SnapshotFromUrl` per pinned version |
| Firewall groups | `firewallGroups.use`: `ids` per region (the same group repeated), description name pattern; no selector (no tags); inbound is open without a group | `regions.<r>.security_groups.nodes.id` | the kind map |
| Networks (VPC) | `networks.use`: `ids` per region, description name pattern; optional; needed for `isolation: nodeclass` | `regions.<r>.network.id` | the kind map; optional per cluster |

## alicloud

Images: rung 3. Provide one custom image per region and architecture: upload the
images repository's `aliyun` image to an OSS object, `ImportImage` in one region,
`CopyImage` to the others; name `Flatcar-<channel>-<version>-<arch>` with the tags
`flatcar-channel`, `flatcar-version`, `arch`, `built-by`.

| What | `spec.vendors.alicloud` | Inventory today | Missing today |
|---|---|---|---|
| Account and credentials | none in the block; credentials: a RAM role of an OIDC identity provider (recommended), access keys in a Secret | `scope.account_id` (`null` with a plain access key; SCarpenter does not need it) | the controller's RAM OIDC provider and role |
| Images | `images.use`: `ids` per region, name pattern, tag selector | none | images (planned): the OSS import and copies |
| Subnets (vSwitches) | `subnets.use`: `ids`, `VSwitchName` pattern (`{zone}` is the zone name), tag selector | `regions.<r>.subnets[].id` (one per zone, a /25: about 120 nodes per zone) | none |
| Security groups | `securityGroups.use`: `ids`, name, tag selector; must be in the vSwitches' VPC | `regions.<r>.security_groups.nodes.id` | none |
| IPv6 | unsupported: the VPC has no IPv6 CIDR | none | none |
