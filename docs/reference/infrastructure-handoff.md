# Hand-off contract: bring your own IDs

What you provide so that SCarpenter launches nodes at a vendor, and what it does
for you when you provide nothing. SCarpenter does not know, read or depend on how
you provision: Pulumi, Terraform or OpenTofu, a CLI script and the console are
equal. Whatever creates the resources passes them to SCarpenter in one way, the
`spec.vendors.<vendor>` block of an `SCNodeClass`: the vendor's `regions` and
`resources.<kind>`, each with `use`: one selector, the same for every kind and vendor
([Resource selectors](resource-selectors.md)). Your tool renders or templates that block
(an example is [below](#example-a-nodeclass-fragment-your-iac-can-render)); the
CRD schema and examples it renders against are published with the API reference.

The design is ADR-018 (vendor resources), ADR-022 (resolution order), ADR-023 (the selector) and
[ADR-021](../explanation/DECISIONS.md#adr-021-flatcar-images-the-image-ladder-accepted-2026-11-10)
(images). How the references work is in [Vendor resources](vendor-resources.md) and
[Bring your own vendor resources](../how-to-guides/bring-your-own-resources.md);
where the images come from is in [Flatcar images](../explanation/flatcar-images.md).

Status of this page: only the `aws` driver exists today; every other vendor block
is a plan ([`../plans/v1/wip/`](../plans/v1/wip/)), written from research on
2026-11-10, and its kinds and fields may change when the driver is written. Parts
that [plan 14](../plans/v1/wip/14-multi-vendor-groundwork.md) adds are marked
*planned*.

## Table of Contents

- [How the hand-off works](#how-the-hand-off-works)
- [The `use` selector](#the-use-selector)
- [Without `use`](#without-use)
- [Example: a NodeClass fragment your IaC can render](#example-a-nodeclass-fragment-your-iac-can-render)
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

Your tool creates the resources, tags or labels them where the vendor allows, and
puts what identifies them into the NodeClass. SCarpenter validates the references
per region and never modifies or deletes a resource it was given with `use`. Every
vendor block has `regions` (the vendor's API names; empty means every region the
credentials can use) and may have `resources.<kind>` and `resources.images`.

Keep the resources that SCarpenter may use apart from the ones it must not. A
wide-open "jobs" security group next to the "nodes" one in the same account is
safe only if you hand over the `nodes` one by ID, or tag only that one; a tag
selector selects everything that carries the tag.

## The `use` selector

Every kind takes the same selector, evaluated in every region of the block. All given
fields must hold (ANDed); at least one is required; a field the vendor does not support for
the kind is rejected. The full format, the semantics and what each vendor can filter on for
each kind are in [Resource selectors](resource-selectors.md); the ones a hand-off needs:

| Field | Meaning | What your tool outputs |
|---|---|---|
| `ids: {<region>: [...]}` | explicit IDs per region; a region without an entry is an error, not "the default"; the key `*` applies to every region (a global resource, the same ID everywhere) | the IDs, per region |
| `name: "<glob>"` | a name with `*` and `?` and the placeholders `{region}`, `{zone}`, `{channel}`, `{version}`, `{arch}`, `{nodeclass}`; exact without a wildcard. The name field is the vendor's (AWS subnets: the `Name` tag; Vultr and UpCloud: description and title) | the name it gave the resources |
| `tags: {k: v}` | every resource that carries all the tags or labels; `"*"` = the key exists | the tag or label it put on them (the opt-in tag `scarpenter.sparecores.com/use: "true"` is the one SCarpenter also looks for on its own, see below) |
| `match: {...}` | portable attributes: `architecture`, `owner`, `state`, `cidr`, `within`, `zone`, `network`, `createdAfter`, `description` | the plan of addresses (`cidr`), or the owning account |
| `filters`, `expression` | the vendor's own filters and query language, where the portable fields do not reach | the vendor's own names |
| `select`, `orderBy` | how many matches are used (`exactlyOne`, `all`, `newest`, `oldest`) and by what order | nothing |

A tag selector selects everything that carries the tag; a selector that is too wide fails
with `Ambiguous` where one resource is needed, and uses every match where several are
allowed (subnets): keep resources apart as above.

## Without `use`

For a kind with neither `use` nor `create`, per region (ADR-022, details in
[Vendor resources](vendor-resources.md#resolution-order)):

1. resources tagged `scarpenter.sparecores.com/use: "true"` (*planned*; the same as a `tags` selector with
   the kind's default `select`; vendors without tags or labels skip it);
2. the vendor's own default where it exists and is safe for the request (AWS default
   VPC and subnets, GCP `default` network, Alibaba default VPC and vSwitches);
3. a resource SCarpenter creates and owns, with a lifecycle, where the credentials
   allow it (`forbid: true` removes this step);
4. otherwise `ResourcesReady=False` naming the kind, the region and what was tried;
   nothing is launched there.

An explicit `use` skips all of this, and one that matches nothing (or too much for `select`) fails. Images are
never created by SCarpenter.

## Example: a NodeClass fragment your IaC can render

```yaml
spec:
  vendors:
    aws:
      regions: [us-east-1, eu-central-1]
      resources:
        subnets:
          use:
            ids:
              us-east-1: [subnet-0aa1, subnet-0aa2, subnet-0aa3]
              eu-central-1: [subnet-0bb1, subnet-0bb2]
        securityGroups:
          use:
            ids: {us-east-1: [sg-0123], eu-central-1: [sg-0456]}
        images:                              # optional on AWS (rung 1); for your own AMIs
          use:
            match: {owner: [self], architecture: [amd64]}
            tags: {flatcar-version: "{version}"}
            filters: [{name: root-device-type, values: [ebs]}]
            select: newest
```

Any tool can emit this: a Pulumi program or a Terraform `kubernetes_manifest` that
maps its outputs to the `ids` maps (or tags its resources and emits a `tags` selector), a Helm
values file, or a hand-written manifest.
A tool that only creates and tags resources can skip the NodeClass part and rely on
the opt-in tag. The same shape holds for every vendor; only the kinds differ.

## Images

The `images` kind, the same for every vendor, with the same selector:

```yaml
resources:
  images:
    use:
      ids: {<region>: [<image id>, ...]}     # per region; the key "*" for a global image (the same ID everywhere)
      # or  name: "flatcar-{channel}-{version}-{arch}"
      # or  tags: {flatcar-version: "{version}"}   # where the vendor has tags or labels on images
      # with match (architecture, owner, state, createdAfter), filters, select (default exactlyOne), orderBy
```

There is no `create`. The placeholders are `{channel}`, `{version}`, `{arch}`, `{vendorArch}`, `{region}`, `{zone}`
and `{nodeclass}` ([placeholders](resource-selectors.md#placeholders)). The selector's matches are narrowed to the
request: the image's own architecture (the `ids` list may hold both), channel and version, which must equal the
NodeClass's pin (ADR-017); several IDs in a region (the two architectures, UpCloud's tiers) are not an
ambiguity. `owner` defaults to the block's owner field, never to every image the account can see
(an `ids` selector needs none). **An image whose version cannot be read back is not used:** whatever names the
image, the driver reads the Flatcar channel and version from its labels, tags or name, and an image of another
release, or one that does not say, makes the region's images not ready (`ImagesReady=False`, `ImageVersionMismatch`
or `ImageVersionUnknown`) and is never booted. So label or tag the images you create (AWS: the tags
`flatcar-channel` and `flatcar-version` when the AMI name is not `Flatcar-<channel>-<version>[-arm64]-hvm`). Where a
vendor has no creation time on images (UpCloud templates, Azure marketplace), `select: newest` needs
`orderBy: label:<key>` or `name`. Without `images` the driver resolves the pinned version itself: from the vendor's
own images (rungs 1 and 2) or by the documented naming convention in your account (rung 3, which is a built-in
selector of the same shape). On rung 3 your IaC, or a CI job next to it, creates the images (per vendor below) and
either follows the convention or passes the IDs or a selector. Who creates the images per vendor:
[Flatcar images](../explanation/flatcar-images.md#overview).

## aws

Images: rung 1, nothing to provide (`imageOwner` overrides the AMI owner). Your own AMIs: `resources.images.use`
(IDs per region, or `name`, `tags`, `match.owner: [self]`), tagged `flatcar-channel` and `flatcar-version`.
Credentials: an IAM role with web identity (recommended), keys in a Secret, or the
node's role; the account is the one of the credentials. Defaults: the default VPC.

| Kind | Provide | `use` fields |
|---|---|---|
| `subnets` | subnet IDs per region, one per zone (the zone name is the account's) | `ids`, `name` (`{zone}`), `tags`, `match` (`zone`, `network`, `cidr`, `within`, `state`), `filters`, `select` (default `all`) |
| `securityGroups` | one security group ID per region for nodes | `ids`, `name`, `tags`, `match` (`network`: the subnets' VPC, `description`), `filters`; with it `networking.inbound` and `isolation: nodeclass` rules do not apply (the run-isolation group is still added) |
| `placementGroups` | nothing; referenced from placement profiles | `name` in the profile |

Without `use`: tagged subnets (*planned*), then the default VPC's default subnets
(skipped when the request needs what they cannot give, for example `publicIPv4: false`
or IPv6 on a VPC without it), then a managed VPC, subnet and security group, else a
stated failure.

## gcp

Images: rung 1 for amd64 (`kinvolk-public`), rung 2 for arm64 (a Spare Cores project):
nothing to provide. Credentials: `project` (must equal the controller's `--gcp-project`);
workload identity federation (recommended), service-account key, or the metadata server.

| Kind | Provide | `use` fields |
|---|---|---|
| `subnets` | subnet resource IDs (`projects/<p>/regions/<r>/subnetworks/<n>`) per region | `ids`, `name` (`{region}`), `match` (`network`, `cidr`, `within`), `filters`, `expression`; no `tags` (no labels) |
| `firewalls` | the network tag your firewall rules apply to, per region | `ids` are network tags, `name` on the tags found in the rules, `match.network`, `filters` on the rules; no `tags` |
| `resourcePolicies` | nothing; in placement profiles | |

Egress: nodes without a public IPv4 need a Cloud NAT on the subnet. Without `use`:
the `default` network where it exists and its firewall can honour the request (a
`default` network cannot isolate runs or restrict inbound rules: skipped then), then a
managed network, subnet and firewall rules, else a stated failure. No discovery
(no labels on networks).

## azure

Images: rung 1 (Marketplace), nothing to provide; the subscription must be allowed to
use the offers (SCarpenter never accepts marketplace terms). Credentials:
`subscriptionID` (a guard: must equal the controller's); workload identity
(recommended), secret, or managed identity; one subscription per controller.

| Kind | Provide | `use` fields |
|---|---|---|
| `resourceGroups` | the resource group name per region | `ids` (name or ARM ID), `name`, `tags`, `expression` |
| `subnets` | full subnet ARM IDs per region | `ids`, `<vnet>/<subnet>` `name`, `tags` (match the VNet's tags; subnets have none), `match` (`cidr`, `within`), `filters`, `expression`; no `{zone}` |
| `securityGroups` (NSGs) | one NSG ID per region | `ids`, `name`, `tags`, `filters`, `expression`; a NIC takes one NSG, so with a brought NSG `isolation` has no effect |

Egress: the subnet has no NAT, so nodes need `publicIPv4: true` (or you bring a NAT
gateway). Without `use`: tagged resource groups, VNets and NSGs (*planned*); Azure has
no default network, so then a managed resource group (`retain`), VNet, subnet and NSG,
else a stated failure.

## hcloud

Images: rung 3. Provide one snapshot per architecture and pinned version, labelled
`os=flatcar`, `flatcar-channel=<channel>`, `flatcar-version=<version>`; they are global
in the project (list the IDs under the key `*`). Create them with
`hcloud-upload-image` from Flatcar's `hetzner` image, in a CI job or a step of your IaC.
Credentials: none in the block; the token is bound to one project; give the controller
its own read-write token in a Secret.

| Kind | Provide | `use` fields |
|---|---|---|
| `images` | the snapshots, by the label convention (default) or IDs per location | `ids` (under `*`: global), `tags` (labels, `expression` for a full label selector), `match.architecture` |
| `networks` | optional: a private network per location (without one nodes have a public address only) | `ids`, `name` (exact), `tags`, `expression`, `match` (`cidr`, `within`, `zone`); no `create` in v1 |
| `firewalls` | optional: a firewall ID (the same in every location) | `ids`, `name` (exact), `tags`, `expression`; with it `networking.inbound` rules do not apply |

A server needs no subnet reference: it gets an address from the network's subnet in
its location. `sshKey` is optional (nodes use Ignition). Without `use`: labelled
networks and firewalls (*planned*), no vendor default; a managed firewall, else a
stated failure. Public-only nodes need nothing.

## ovh

Images: rung 3. Provide one Glance image per region and architecture (amd64 only in v1)
from Flatcar's OpenStack image, named `flatcar-<channel>-<version>-<arch>` with the
tags `flatcar` and `flatcar-<channel>-<version>`. Credentials: one Public Cloud project
per controller; a service account (OAuth2) or an OpenStack user (an IAM policy on the
project for the OpenStack rights the driver uses, see the OVH plan).

| Kind | Provide | `use` fields |
|---|---|---|
| regions | the Keystone catalog's names, upper case (`EU-WEST-PAR`, `GRA11`); the driver maps Keeper's names to them | `regions` |
| `images` | the Glance images per region | `ids`, `name` (exact on the server, glob on the client), `tags` (Glance tag strings), `match` (`architecture`, `owner`, `state`, `createdAfter`), `filters` |
| `securityGroups` | Neutron group UUIDs per region; a group that admits only private ranges leaves `Ext-Net` nodes reachable through the `tailscale` template only | `ids`, `name`, `tags` (Neutron tags, if the extension exists), `filters` |

No networks in v1 (public `Ext-Net` only). Without `use`: tagged groups (*planned*), no
vendor default; a managed group, else a stated failure.

## upcloud

Images: rung 3. Provide one template per zone and storage tier from the images
repository's `flatcar-upcloud-<channel>-<version>-amd64.img.gz` (import into a
storage, then a template; delete the source storage), labelled as in
[Flatcar images](../explanation/flatcar-images.md#upcloud). Credentials: none in the
block; an API token (Secret) of the account or a sub-account; tokens expire and have
no scopes; the controller's token must see what your IaC created.

| Kind | Provide | `use` fields |
|---|---|---|
| `images` | the templates per zone, both tiers | `ids`, `tags` (labels; the default), `match.zone`, `orderBy: label:flatcar-version` |
| `networks` | optional: an SDN network per zone (without one the node has the public and utility interfaces only); a managed network needs a range that does not overlap yours | `ids`, `name`, `tags` (labels), `match` (`zone`, `cidr`, `within`) |
| `firewalls` | optional: an SDN ruleset; covers only the private interface (the public interface's firewall is written per server by the driver) | `ids`, `name`, `tags` (client-side), `match.createdAfter` |

Without `use`: labelled networks and rulesets (*planned*), no vendor default; managed
ones, else a stated failure.

## vultr

Images: rung 3. Provide one snapshot per pinned version, created from the images
repository's image at its public URL (a snapshot-from-URL call), described
`flatcar-<channel>-<version>-amd64` (global in the account). Credentials: none in the
block; one API key is one account (a Secret); the key is bound to an IP allowlist that
must contain the controller's egress address.

| Kind | Provide | `use` fields |
|---|---|---|
| `images` | the snapshot per region (the same one repeated) | `ids`, description `name` pattern, `match.createdAfter`, `orderBy` |
| `firewallGroups` | optional: a group per region (inbound is open without one) | `ids`, description `name`; no `tags` (none exist) |
| `networks` (VPC) | optional; needed for `isolation: nodeclass` | `ids`, description `name`, `match` (`cidr`, `within`) |

Without `use`: no discovery (no tags), no vendor default; managed groups, else a stated
failure.

## alicloud

Images: rung 3. Provide one custom image per region and architecture: upload the images
repository's `aliyun` image to an OSS object, `ImportImage` in one region, `CopyImage` to
the others; name `Flatcar-<channel>-<version>-<arch>` with the tags `flatcar-channel`,
`flatcar-version`, `arch`, `built-by`. Credentials: a RAM role of an OIDC identity provider
(recommended), or access keys in a Secret.

| Kind | Provide | `use` fields |
|---|---|---|
| `images` | the custom images per region | `ids`, `name` (fuzzy on the server, finished on the client), `tags`, `match` (`architecture`, `owner`, `state`, `createdAfter`), `filters` |
| `subnets` (vSwitches) | one vSwitch per zone (a /25 holds about 120 nodes) | `ids`, `VSwitchName` `name` (`{zone}` is the zone name), `tags`, `match` (`zone`, `network`, `cidr`, `within`), `filters` |
| `securityGroups` | one group ID per region, in the vSwitches' VPC | `ids`, `name`, `tags`, `match.network`, `filters` |

IPv6 is unsupported when the VPC has no IPv6 CIDR. Without `use`: tagged resources
(*planned*), then the default VPC and its vSwitches where they exist, then managed
ones, else a stated failure.
