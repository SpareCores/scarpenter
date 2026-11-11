# Resource selectors

How a NodeClass finds the vendor resources a node depends on: one `use` selector,
with the same shape for every kind of resource on every vendor. It can express every
way the vendor's API offers to find the kind (AWS `DescribeImages` by name, owner,
architecture, root device type, tag, creation date; GCP filter expressions; Hetzner
label selectors), portably where the concept is shared and natively where it is not.
Design: [ADR-023](../explanation/DECISIONS.md#adr-023-one-selector-for-every-vendor-resource-accepted-2026-11-10).
What the kinds are, `create` and the lifecycles: [Vendor resources](vendor-resources.md).
What to hand over per vendor: the [hand-off contract](infrastructure-handoff.md).
A walk-through: [Bring your own vendor resources](../how-to-guides/bring-your-own-resources.md).

Status: the `aws` and `alicloud` drivers (and the development drivers) exist; `images` is implemented for `aws`, `alicloud` and the fake driver; the selector
replaced the `ids` / `name` / `selector` forms of the first AWS driver in
[plan 14](../plans/v1/done/14-multi-vendor-groundwork.md) (A10): `use.selector.tags` is now
`use.tags`, with no alias. The AWS tables are what the driver declares
(`ManagedResources.Selectors`, checked by a test against this page); `match.owner` is not offered
on AWS subnets and security groups yet (it needs the account ID of the caller).
Every other vendor table below is written from the vendors' API references and SDKs
(researched 2026-11-10, no cloud calls) and is what the vendor plans implement; a driver's
change updates its table. Items marked *unverified* were not confirmed in a reference or
SDK source and are checked in the driver's spike before it relies on them.

## Table of Contents

- [The format](#the-format)
- [Matching](#matching)
- [Selecting one or several](#selecting-one-or-several)
- [Placeholders](#placeholders)
- [Portable attributes (`match`)](#portable-attributes-match)
- [Validation](#validation)
- [Resolution, status and caching](#resolution-status-and-caching)
- [Per vendor](#per-vendor): [aws](#aws), [gcp](#gcp), [azure](#azure), [hcloud](#hcloud),
  [ovh](#ovh), [upcloud](#upcloud), [vultr](#vultr), [alicloud](#alicloud)

## The format

`use` sits under a kind in `spec.vendors.<vendor>.resources.<kind>`. Every field is
optional, at least one must be given, and every field a vendor does not support for the
kind is rejected.

```yaml
resources:
  images:
    use:
      ids: {fsn1: ["123", "456"]}          # explicit IDs per region; "*" = every region in scope
      name: "flatcar-{channel}-*"          # glob: * and ?; exact without wildcards
      tags: {os: flatcar, env: "*"}        # key=value; "*" = the key exists
      match:                               # portable attributes, one spelling on every vendor
        architecture: [amd64]
        owner: [self]
        state: [available]
        createdAfter: 2026-01-01T00:00:00Z
      filters:                             # the vendor's own filters, passed through
        - {name: root-device-type, values: [ebs]}
      expression: "..."                    # the vendor's query language, where it has one
      select: newest                       # exactlyOne (default for images), all, newest, oldest
      orderBy: created                     # created, name or label:<key>; only with newest and oldest
```

| Field | Type | Meaning |
|---|---|---|
| `ids` | map region to list of strings | The resource IDs in the region. The key `*` applies to every region in scope (a global resource, the same ID in every region); a region key adds to it. With other fields the IDs are ANDed with them: an ID that exists but does not match is reported as such. |
| `name` | string | A glob (`*` any run, `?` one character, `\` escapes them) on the kind's name field; exact when it has no wildcard. The name field of each kind is in the vendor tables (AWS subnets: the `Name` tag; Vultr and UpCloud: description and title). |
| `tags` | map string to string | Tags, or labels where the vendor has no tags. Each pair must be on the resource, the value exactly; the value `"*"` means the key exists with any value. Keys are used as written, except `scarpenter.sparecores.com/use`, which is encoded per vendor like every SCarpenter tag. A kind without tags rejects the field. |
| `match` | object | Portable attributes with one spelling and one set of values on every vendor, [translated by the driver](#portable-attributes-match). |
| `filters` | list of `{name, values}` | The vendor's own filters for the kind, passed through: `name` is the vendor's own filter or parameter name, `values` are ORed. Nothing the API can filter on is lost. |
| `expression` | string | A query in the vendor's own language, only where there is one: GCP filter (AIP-160 or RE2), Azure Resource Graph, Hetzner label selector. |
| `select` | enum | How many of the matches are used: `exactlyOne`, `all`, `newest`, `oldest`. The default depends on the kind ([below](#selecting-one-or-several)). |
| `orderBy` | string | `created`, `name` or `label:<key>`: what `newest` and `oldest` order by. |

Rules that hold for every vendor and kind:

- **All given fields are ANDed; list values are ORed.** `ids`, `name`, `tags`, every `match`
  attribute and every `filters` entry must hold. Two `filters` entries with the same name
  are two conditions.
- **Unknown or unsupported names are rejected, never ignored.** A field, `match`
  attribute or `filters` name that the vendor's driver does not support for the kind makes
  the NodeClass invalid ([Validation](#validation)). Several vendor APIs silently ignore
  what they do not know (OpenStack Neutron, possibly Vultr, Hetzner and UpCloud), which
  [ADR-017](../explanation/DECISIONS.md#adr-017-never-degrade-an-option-silently-accepted-2026-11-05)
  forbids: a typo must not become "match everything".
- **Zero matches or too many is a stated condition, never a guess** ([Resolution](#resolution-status-and-caching)).
- **`use` without any field is rejected.** An empty selector would mean every resource.

## Matching

**Server first, client always.** The driver sends to the vendor every field it can (to
limit the list, the rate-limit budget and the cost) and then runs the shared matcher over
what came back with all the fields given. So a vendor-side filter may narrow to a
superset (substring or fuzzy name matching, a parameter the API ignores) and never to a
subset, and the result is the same whichever side did the work. The vendor tables say
for each field whether the vendor does it (`server`), narrows and the client finishes
(`server+client`), or only the client does (`client`). A filter a vendor may silently ignore is
offered only where the driver can re-apply it to a field of the response; where the
response has no such field it is not offered.

Two exceptions keep the rule honest. `expression` is sent as written; fields that cannot
be combined with it in one request (GCP: AIP-160 and RE2 filters cannot be mixed) are
applied client-side. `orderBy` is evaluated client-side unless the whole selection is
already exact on the server (one request ordered by the vendor and cut to one row).

- **Case.** Names, tag keys and values are compared case-sensitively, except where the
  vendor's own names are case-insensitive (Azure names and resource types, UpCloud label
  keys): the tables say so.
- **Names that are not unique.** Several vendors allow the same name twice (OpenStack, UpCloud,
  Vultr, Alibaba vSwitches): a name that matches several is `Ambiguous` under `exactlyOne`, as
  for any selector.
- **Kinds without a name field** match `name` against the field the tables name (a tag
  `Name`, a description, a title). A kind that has none rejects `name` (Hetzner snapshots
  have no name: use `tags`).
- **Tags that are plain strings.** OpenStack (OVH) tags have no key and value: the pair
  `tags: {k: v}` is the tag string `k=v`, and `"*"` matches any tag that starts with
  `k=`. The reserved key `scarpenter.sparecores.com/use` is encoded `scarpenter-use`
  there (Neutron tags cannot contain `/`; unverified).
- **Missing IDs.** A call that fails as a whole when one ID is missing (AWS
  `Invalid<Kind>ID.NotFound`) is never used for `ids`: the driver looks the IDs up with
  the kind's ID filter, so a missing ID is reported by name and the others still resolve.

## Selecting one or several

`select` says how many matches are used, per region (and per zone when the selector
has `{zone}`):

| `select` | Zero matches | More than one match |
|---|---|---|
| `exactlyOne` | the region is unusable | the region is unusable (`Ambiguous`), the condition names the first matches |
| `all` | the region is unusable | all are used (subnets: one per zone, chosen at launch as today) |
| `newest` / `oldest` | the region is unusable | the one first or last by `orderBy`; two with the same value are `Ambiguous` |

Defaults when `select` is not given:

| Kind (the generic name; vendors name some differently) | Default |
|---|---|
| `images` | `exactlyOne` (`all` is rejected: a launch needs one image) |
| `networks`, `resourceGroups`, `placementGroups`, `serverGroups`, `deploymentSets`, `resourcePolicies` | `exactlyOne` |
| `subnets` | `all` |
| `securityGroups`, `firewalls` | `all` where an instance can carry several (AWS, Alibaba, hcloud, GCP network tags), else `exactlyOne` (Azure NSG, Vultr firewall group, UpCloud ruleset) |

`orderBy` is only valid with `newest` or `oldest`:

- `created`: the vendor's creation time (images, and the kinds in the tables that have one).
  A kind without a creation time rejects `created`, and `newest` and `oldest` default to
  it only where it exists; on the others `orderBy` is required.
- `name`: the name, compared in natural order (runs of digits compare as numbers, so Flatcar
  `4081.3.1` sorts before `4081.10.0`). Right for Azure Marketplace versions, whose name is the
  version, and for names that carry a version.
- `label:<key>`: the value of the tag or label `<key>`, in natural order. A resource without the
  key is not a candidate. For vendors with no creation time on the kind (UpCloud templates,
  Vultr OS entries): the label `flatcar-version` orders the images.

**Images are selected per request.** A launch asks for an image for a region, an
architecture, a Flatcar channel and version (and, on UpCloud, a storage tier). The
selector's matches are first narrowed by those: the architecture of the request (the `ids`
list may hold both architectures, the image's own architecture decides), the tier, and the
pinned channel and version, which must equal the image's labels or name
([ADR-017](../explanation/DECISIONS.md#adr-017-never-degrade-an-option-silently-accepted-2026-11-05),
[ADR-021](../explanation/DECISIONS.md#adr-021-flatcar-images-the-image-ladder-accepted-2026-11-10)).
`select` applies to what is left. An image with the wrong version is not ready; it is never
booted. `owner` for images defaults to the block's owner field (`imageOwner`, `imageProject`),
never to "every image the account can see": that would let a stranger's image of the same name win. A
selector that names `ids` is exact and needs no owner (the version check still applies).

**The version check applies to every image, also one named by ID.** The driver reads the channel and version
the chosen image says it is (its labels, tags, properties or name, per vendor) and compares them with the
pin: another release is `ImageVersionMismatch`, an image that does not say is `ImageVersionUnknown`; the
region's images are then not ready (`ImagesReady=False` with that reason, naming the region and the image) and the
image is never booted. There is no softer mode (a warning) for images without labels: it would break
[ADR-017](../explanation/DECISIONS.md#adr-017-never-degrade-an-option-silently-accepted-2026-11-05).
`images` takes `use` only (no `create`, no `forbid`) and rejects `select: all`; both are CRD rules.

## Placeholders

Expanded in every string value of `use` (`ids`, `name`, tag keys and values, `match`, `filters`
and `expression` values) before anything is sent to the vendor:

| Placeholder | Value |
|---|---|
| `{region}` | The region in scope, the vendor's API name. |
| `{zone}` | The zone in scope, the vendor's zone name (the account's name on AWS, see [ADR-009](../explanation/DECISIONS.md#adr-009-zones-are-matched-by-vendor-zone-id-accepted-2026-11-05)). Only on zonal kinds; the check expands it for every zone of the region, a region is unusable only when no zone has a match, and a zone without one is unavailable alone. |
| `{arch}` | `amd64` or `arm64`: the spelling of the Flatcar image names and of the images repository. Only in `images`, where a launch request has an architecture; rejected on other kinds, as is `{vendorArch}`. |
| `{vendorArch}` | The vendor's own spelling, for native filters and expressions: AWS `x86_64` / `arm64`, GCP `X86_64` / `ARM64`, Azure `x64` / `Arm64`, hcloud `x86` / `arm`, Alibaba `x86_64` / `arm64`, OVH image property `x86_64` / `aarch64`. UpCloud and Vultr have none (amd64 only). |
| `{channel}` | The NodeClass's Flatcar channel. |
| `{version}` | The pinned Flatcar version. |
| `{nodeclass}` | The NodeClass name. |

`{{` and `}}` are a literal brace (a Resource Graph query or a GCP filter may need them);
any other `{word}` is an error. `match.architecture` takes the portable `amd64` and `arm64` and
the driver translates them, so `{vendorArch}` is only needed inside `filters` and
`expression`.

## Portable attributes (`match`)

`match` is a fixed set of attributes with the same name and values on every vendor. A
driver declares per kind which it supports and whether the vendor or the client evaluates
each; an unsupported attribute is rejected. Lists are ORed, attributes are ANDed.

| Attribute | Value | Meaning |
|---|---|---|
| `architecture` | list of `amd64`, `arm64` | The CPU architecture of an image. Translated per vendor. |
| `owner` | list of `self`, an account / project ID, or the vendor's public alias | Whose resources to look at: AWS `Owners` (`self`, an account ID, `amazon`, `aws-marketplace`), GCP the project to list, Alibaba `ImageOwnerAlias` / `ImageOwnerId`, OVH Glance `owner`, UpCloud `private` / `public`. Not a filter on vendors without an owner notion (Hetzner, Vultr): rejected there. |
| `state` | list of `available`, `pending`, `failed` | The vendor's state, normalised (the mapping is in each vendor section). Default `[available]` for every kind that has a state, so a resource still being created is never selected. |
| `includeDeprecated` | bool | Images the vendor marked deprecated are excluded unless `true` (AWS, GCP, Hetzner, Azure, Alibaba have the notion; an old pinned system image disappears from lists otherwise). |
| `createdAfter`, `createdBefore` | RFC 3339 time | The creation time. Only kinds that have one (below); a resource whose creation time the vendor does not report is excluded. |
| `description` | glob | The description field, where the kind has one. |
| `zone` | list of zone IDs | The zone of a zonal resource, by the vendor's zone ID ([ADR-009](../explanation/DECISIONS.md#adr-009-zones-are-matched-by-vendor-zone-id-accepted-2026-11-05)); the driver maps it to the field. Rejected on regional kinds. `{zone}` (a zone name) is rejected here: use it in `name` or `filters`. |
| `network` | list of IDs | The network a subnet or a security group belongs to (AWS `vpc-id`, Alibaba `VpcId`, GCP `network`, Neutron `network_id`). A group in another network than the subnet is a launch failure on AWS and Alibaba, so this is how a `use` stays consistent. |
| `cidr` | a CIDR | The resource's range equals the CIDR (a network with several prefixes matches if any equals). |
| `within` | a CIDR | The resource's range lies inside the CIDR (`10.8.0.0/16` matches a subnet `10.8.4.0/24`). Evaluated client-side on every vendor. |

`cidr` is compared on parsed prefixes, not strings, and where a vendor can filter by exact
range it does so server-side. Containment is never a vendor filter: no API offers it.

## Validation

Two layers, so that nothing is ignored:

- **CRD (CEL), static.** `use` is non-empty; `orderBy` needs `select` of `newest` or `oldest`
  and has the form `created`, `name` or `label:<key>`; `ids` keys are region names or `*`;
  `createdAfter` and `createdBefore` parse as RFC 3339 and `cidr` and `within` as CIDRs
  (`isCIDR`); `filters` entries have a name and at least one value; placeholders are known.
  The `match` attributes are typed fields, so a misspelt attribute is a schema error.
- **The driver, per vendor and kind** (`ValidateNodeClass`, condition `ValidationSucceeded`):
  every field, `match` attribute and `filters` name is checked against the driver's capability
  declaration for the kind (the tables below). A name the vendor does not offer, or a field
  that needs a creation time, labels or a zone on a kind that has none, fails with the vendor,
  kind, field and the supported names: `aws.resources.images.use.filters[0]: "rootdevice-type"
  is not an images filter of aws (root-device-type, ...)`. Values are checked where the vendor
  has rules (tag key characters, ID shapes, AWS `aws:` prefix).
  A vendor-specific check that needs the API (the Neutron tag extension, the GCP field list) is a
  status condition of the region instead, naming what is missing.

The capability declaration also fixes, per kind, whether it is zonal, whether it has a
creation time, its default `select`, and for each `match` attribute and `filters` name the
side (server or client). The shared matcher, the placeholder expansion and the conformance
cases are in `pkg/vendors` and run for every driver (plan 14, A10).

## Resolution, status and caching

- **Without a `use`**, the opt-in step of the [resolution order](vendor-resources.md#resolution-order)
  is this selector too: `tags: {scarpenter.sparecores.com/use: "true"}` (the key is encoded per vendor
  like the other SCarpenter tags) with the kind's default `select`, for a kind that has tags or labels.
- **Per region** (and per zone for zonal kinds with `{zone}`): the placeholders are expanded, the
  driver lists the kind with the server-side part of the selector, the matcher finishes it, `select`
  chooses.
- **Zero matches, or more than `select` allows**: the region is unusable. `ResourcesReady=False`,
  reason `ResourcesUnavailable`, names the kind, the region and what excluded the rest: the
  candidates after the vendor's list and after each field (`eu-west-1 subnets: 14 listed, 3 match
  tags, 0 match match.zone [euw1-az1]`), and for `ids` the IDs that are missing or do not match. This
  is step 5 of
  [ADR-022](../explanation/DECISIONS.md#adr-022-resolving-vendor-resources-explicit-discovered-default-created-or-a-clear-failure-accepted-2026-11-10);
  a `use` that was given never falls through to discovery, a default or `create`. Only a kind with no
  `use` at all follows the other steps.
- **What was chosen** is in `status.vendors.<vendor>.resources` (kind, region, ID, name, `source`),
  and a launched node records the IDs it launched with. Images stay pinned per node: a later
  re-resolution that finds another image changes new launches only, never a running node (the pinned
  version, not the image ID, is what drift watches).
- **When**: on the NodeClass change and every `--resource-check-interval` (30 seconds, the answer
  trusted for 30 seconds), so a resource added to match a selector is picked up without a restart.
- **Cost.** Lists are paged to the end (the driver's job: `NextToken`, `marker`, `cursor`, page
  numbers differ per vendor and are in the tables), cached per (vendor, kind, region, selector), and a
  kind that is global at the vendor (Hetzner firewalls, Vultr firewall groups, GCP images) is listed once
  per check, not once per region. A selector of only `ids` is looked up by ID, not by listing. Where
  the vendor's budget is tight (Hetzner: 3600 requests an hour per project; Azure: a subscription-wide
  list per kind) the tables point at the narrowest scope the selector allows.

## Per vendor

Reading the tables. **Where**: `server` (the vendor evaluates it), `server+client` (the vendor narrows
to a superset and the client finishes), `client` (listed, then matched locally), `by id` (a get, not a
list), `no` (not offered: rejected). **Filters** lists the vendor's own names accepted under `filters`;
`<k>` stands for a free key. **Order**: the source of `created`. Mapped states are normalised as
`available`, `pending`, `failed`.

### aws

EC2 `Describe*` calls, one regional client per region; every kind is regional. `filters` entries
are EC2 `Filter{Name, Values}` (names and values case-sensitive, values ORed, entries ANDed, `*` and `?`
wildcards in string values, `\` escapes; at most 50 filters of 200 values), so `filters` is exactly
the call's filter list. An unknown filter name is rejected by EC2 (`InvalidParameterValue`, unverified);
a wrong-case value silently matches nothing. Never use an ID parameter (`ImageIds`, `SubnetIds`,
`GroupIds`): one missing ID fails the whole call with `*.NotFound`; the driver uses the ID filter. No
ordering and no negation anywhere; `*` for `tags` is the `tag-key` filter. Tag keys and values are
exact: the driver escapes `*`, `?` and `\` in them before sending. Each filter name is sent once
(EC2 does not document a repeated name): `filters` entries with the same name are ANDed by
intersecting their values (rejected when one has a wildcard), and a `filters` entry that names a
filter a field also produces (`state`, `vpc-id`, `tag:<k>`) replaces it in the request while the
matcher still applies the field. `match.cidr` is sent as the
canonical IPv4 range (`cidr-block`); an IPv6 range is matched client-side against the subnet's IPv6
blocks. AMI tags of other
accounts' images are not visible to a consumer (unverified): tags select your own images. No kind has
an `expression`. API reference per call: `https://docs.aws.amazon.com/AWSEC2/latest/APIReference/API_<Action>.html`.

**images** (`DescribeImages`, paginated; **always `Owners`** unless the selector names `ids`: without it
the call returns every public image, and a name alone would match a stranger's image):

| Field | Maps to | Where |
|---|---|---|
| `ids` | filter `image-id` | server |
| `name` | filter `name` (wildcards) | server |
| `tags` | `tag:<k>` ; `tag-key` for `"*"` | server |
| `match.architecture` | filter `architecture` (`x86_64`, `arm64`) | server |
| `match.owner` | request `Owners` (`self`, account ID, `amazon`, `aws-marketplace`); default `imageOwner` | server |
| `match.state` | filter `state` (`available`, `pending`, `failed`) | server |
| `match.includeDeprecated` | request `IncludeDeprecated` (the owner always sees its own) | server |
| `match.description` | filter `description` | server+client |
| `match.createdAfter`, `match.createdBefore` | `CreationDate` (the `creation-date` filter takes only an exact value or a prefix wildcard) | client |

Order: `CreationDate`. Filters: `architecture`, `block-device-mapping.{delete-on-termination,
device-name,snapshot-id,volume-size,volume-type,encrypted}`, `boot-mode`, `creation-date`, `description`,
`ena-support`, `free-tier-eligible`, `hypervisor`, `image-allowed`, `image-id`, `image-type`,
`image-watermark.{source-image-creation-time,source-image-id,source-image-region,watermark-creation-time,
watermark-key}`, `instance-type-specification.{supported-instance-type,unsupported-instance-type}`,
`is-public`, `kernel-id`, `manifest-location`, `name`, `owner-alias`, `owner-id`, `platform` (only
`windows`: Linux images have no platform), `product-code`, `product-code.type`, `public-ssm-parameter-name`,
`ramdisk-id`, `root-device-name`, `root-device-type`, `scan-result.{critical-fix-available-since,
finding-scan-date,high-fix-available-since,highest-finding-severity-label,informational-fix-available-since,
low-fix-available-since,medium-fix-available-since}`, `source-image-id`, `source-image-region`,
`source-instance-id`, `state`, `state-reason-code`, `state-reason-message`, `sriov-net-support`,
`tag:<k>`, `tag-key`, `virtualization-type`; the request parameters `executable-user` (`ExecutableUsers`:
an account ID, `self`, `all`) and `include-disabled` (`IncludeDisabled`).

**subnets** (`DescribeSubnets`, paginated; zonal; the VPC is implied by the subnet):

| Field | Maps to | Where |
|---|---|---|
| `ids` | filter `subnet-id` | server |
| `name` | tag `Name` (subnets have no name field) | server |
| `tags` | `tag:<k>` ; `tag-key` | server |
| `match.zone` | filter `availability-zone-id` (the stable zone ID); `{zone}` in `name` or `filters` uses the account's name | server |
| `match.network` | filter `vpc-id` | server |
| `match.cidr` | filter `cidr-block` (exact) | server |
| `match.state` | filter `state` (`pending`, `available`) | server |
| `match.within` | `CidrBlock` | client |

No creation time (`createdAfter`, `orderBy: created`, `newest`, `oldest` by creation are rejected; `orderBy: name` or
`label:<k>` work). Filters: `availability-zone`, `availability-zone-id`, `available-ip-address-count` (exact
number only), `cidr-block`, `customer-owned-ipv4-pool`, `default-for-az`, `enable-dns64`,
`enable-lni-at-device-index`, `ipv6-cidr-block-association.{ipv6-cidr-block,association-id,state}`, `ipv6-native`,
`map-customer-owned-ip-on-launch`, `map-public-ip-on-launch`, `outpost-arn`, `owner-id`,
`private-dns-name-options-on-launch.{hostname-type,enable-resource-name-dns-a-record,enable-resource-name-dns-aaaa-record}`,
`state`, `subnet-arn`, `subnet-id`, `tag:<k>`, `tag-key`, `vpc-id`. AZ names differ per account; the zone ID
does not.

**securityGroups** (`DescribeSecurityGroups`, paginated; belongs to one VPC):

| Field | Maps to | Where |
|---|---|---|
| `ids` | filter `group-id` | server |
| `name` | filter `group-name` (unique per VPC; wildcards unverified) | server+client |
| `tags` | `tag:<k>` ; `tag-key` | server |
| `match.network` | filter `vpc-id` | server |
| `match.description` | filter `description` | server+client |

No state, zone or creation time. Filters: `description`, `group-id`, `group-name`, `owner-id`, `vpc-id`, `tag:<k>`,
`tag-key`, `ip-permission.{cidr,from-port,group-id,group-name,ipv6-cidr,prefix-list-id,protocol,to-port,user-id}`
and the same with the prefix `egress.` (several rule filters match across different rules of the group). A group must be
in the VPC of the chosen subnets: combine with `match.network`.

**placementGroups** (`DescribePlacementGroups`, not paginated):

| Field | Maps to | Where |
|---|---|---|
| `ids` | request `GroupIds` (a missing ID fails the call: the driver lists and checks) | client |
| `name` | filter `group-name` (a shared group resolves by ID only) | server |
| `tags` | `tag:<k>` ; `tag-key` | server |
| `match.state` | filter `state` (`pending`, `available`, `deleting`, `deleted`) | server |

Filters: `group-name`, `group-arn`, `spread-level` (`host`, `rack`), `state`, `strategy` (`cluster`, `spread`, `partition`),
`tag:<k>`, `tag-key`. No creation time.

Other kinds (not used by the driver today; the same rules): **networks** (`DescribeVpcs`: name is the `Name` tag,
`match.cidr` filter `cidr`, filters `cidr-block-association.cidr-block`, `dhcp-options-id`, `is-default`, `owner-id`,
`state`, `vpc-id`, `ipv6-cidr-block-association.*`, tags); **key pairs** (`DescribeKeyPairs`, not paginated: `name`
is `key-name`, filters `key-pair-id`, `fingerprint`, tags; `CreateTime` client-side); **volumes** (`DescribeVolumes`:
`match.zone` `availability-zone-id`, filters `size`, `status`, `volume-type`, `snapshot-id`, `encrypted`,
`attachment.*`, `create-time`, tags; `CreateTime` client); **Elastic IPs** (`DescribeAddresses`, not paginated: filters
`allocation-id`, `association-id`, `instance-id`, `public-ip`, `private-ip-address`, `network-interface-id`, tags);
**internet gateways, route tables, NAT gateways** (`attachment.vpc-id`, `vpc-id`, `route.*`, `association.*`, `state`,
tags); **network interfaces** (about 40 filters, `status`, `subnet-id`, `vpc-id`, `group-id`, tags).
Cross-service tag search (`resourcegroupstaggingapi`) is not used: it has no wildcard and is eventually consistent.

### gcp

Compute Engine `*.list` per project (the NodeClass's `project`; every kind belongs to it, a list of another
project's images names that project in `match.owner`). One generic mechanism: the `filter` string of every list call,
in **one of two syntaxes per request**: AIP-160 (`field = value`, `!=`, `<`, `>`, `:` substring, `field:*` is set,
`(a) OR (b)`, labels as `labels.<k>`) or RE2 (`field eq <regex>`, the regex must match the whole field). They cannot be
mixed in one request. There is no glob and no list-valued filter; the driver therefore sends equality predicates in
AIP-160 and, for a glob `name`, the longest literal segment as `name:<segment>` (a substring, a superset), finishing
the glob client-side. Any field of the returned resource is filterable in principle; which fields work for repeated or
nested fields is not documented per resource (*unverified*), so `filters` offers the fields below, and `expression` is
the escape hatch for the rest. An unknown field is an API error (`Invalid value for field 'filter'`, unverified), not
an empty list. `orderBy` is `name` or `creationTimestamp desc` only. Paging `maxResults` up to 500 with `pageToken`
(a page may be empty with a token); `aggregatedList` returns all regions in one call (`returnPartialSuccess`).
`expression`: the `filter` string, AIP-160 or RE2, sent as written; with it the driver sends no generated predicate and
applies every other field client-side. Reference: `https://docs.cloud.google.com/compute/docs/reference/rest/v1/<collection>/list`.

**images** (`images.list`, global; **no labels on networks, subnets, firewalls, resource policies; labels on images**):

| Field | Maps to | Where |
|---|---|---|
| `ids` | `images.get` by name or `projects/<p>/global/images/<n>` | by id |
| `name` | `name = x`; a glob: `name:<longest literal segment>` | server+client (exact: server; a name without wildcard is also looked up with `images.get`) |
| `tags` | `labels.<k> = v` ; `labels.<k>:*` for `"*"` | server |
| `match.architecture` | `architecture = X86_64 / ARM64` | server |
| `match.owner` | the project listed (default `imageProject`; the public projects `debian-cloud`, `ubuntu-os-cloud`, `cos-cloud` need list rights) | server (one list per project) |
| `match.state` | `status` (`READY`, `PENDING`, `FAILED`) | server |
| `match.includeDeprecated` | `deprecated.state` (`ACTIVE`, `DEPRECATED`, `OBSOLETE`, `DELETED`); how an image with no `deprecated` object compares is unverified | client |
| `match.description` | `description:<segment>` | server+client |
| `match.createdAfter`, `createdBefore` | `creationTimestamp > / <` (comparison semantics unverified) | server+client |

**Publicly shared images do not appear in `images.list`** (`allAuthenticatedUsers`, as `kinvolk-public` and the
Spare Cores arm64 project): they are reachable only by `images.get` / `getFromFamily`. So `ids`, a `name` without
wildcard, and `filters: family` (`getFromFamily`, which skips deprecated images) work for them; a glob `name`,
`tags` and `match` on such a project list nothing, and the `ResourcesUnavailable` message says so. Order:
`orderBy=creationTimestamp desc` (newest on the server; `oldest` client-side). Filters: `family`, `deprecated.state`,
`sourceType`, `diskSizeGb`, `id`, `labels.<k>`, `licenses`, `storageLocations`, `guestOsFeatures` (the last three repeated
fields: unverified).

**subnets** (`subnetworks.list` per region; `listUsable` for Shared VPC; regional, **no zone, no labels**):

| Field | Maps to | Where |
|---|---|---|
| `ids` | `subnetworks.get` | by id |
| `name` | as images | server+client |
| `match.network` | `network = <URL>` (the driver builds the URL from the network name) | server |
| `match.cidr` | `ipCidrRange = x` (string equality) | server |
| `match.within` | `ipCidrRange` | client |
| `match.owner` | the host project of a Shared VPC (the driver then calls `listUsable` with `serviceProject`) | server (*unverified*) |
| `match.state` | `state` (`READY`, `DRAINING`) | server |
| `match.description`, `createdAfter`, `createdBefore` | as images | server+client |

`tags` and `zone` are rejected (the marker of a managed subnet is its name prefix and description). Filters: `purpose`,
`role`, `stackType`, `ipv6AccessType`, `ipv6CidrRange`, `privateIpGoogleAccess`, `enableFlowLogs`, `network`. Order:
`creationTimestamp desc`. Free-address counts exist only on an aggregated list with `views=WITH_UTILIZATION` and are
client-side.

**firewalls** (`firewalls.list`, global rules; the kind resolves to **network tags**, which are not resources: `ids` are
tag names and `name` is a glob on the tag names found in the rules' `targetTags`, read client-side):

| Field | Maps to | Where |
|---|---|---|
| `ids` | network tag names (no get: a tag exists if a rule names it) | client |
| `name` | the tag names in `targetTags` of the matching rules | client |
| `match.network` | `network = <URL>` | server |
| `match.state` | `disabled = false` (a disabled rule does not make the tag available) | server |

Rule fields are filterable under `filters` (the rules that carry the tag are the candidates): `direction`, `priority`,
`disabled`, `logConfig.enable`, `network`, `name` (of the rule); repeated fields (`targetTags`, `sourceRanges`,
`allowed`) are client-side. No labels, no zone.

**resourcePolicies** (placement; `resourcePolicies.list` per region; no labels): `ids` by `get`; `name` as images;
`match.state` `status` (`CREATING`, `READY`, `DELETING`, `INVALID`, `EXPIRED`); filters `groupPlacementPolicy.collocation`,
`groupPlacementPolicy.vmCount`, `groupPlacementPolicy.availabilityDomainCount`, `groupPlacementPolicy.gpuTopology`;
`orderBy` `creationTimestamp desc`.

Other kinds: **networks** (`networks.list`, global, no labels, no range: `match.cidr` is rejected, use subnets; filters
`autoCreateSubnetworks`, `routingConfig.routingMode`, `mtu`, `enableUlaInternalIpv6`); **disks** (`disks.list` zonal:
labels yes, `match.zone`, filters `status`, `type`, `sizeGb`, `users`, `sourceImage`, `architecture`);
**addresses** (regional and global: labels yes, filters `addressType`, `ipVersion`, `status`, `networkTier`, `purpose`,
`address`, `subnetwork`); **disk types** (zonal catalogue, `name`). SSH keys are project metadata, not a kind;
NICs are part of the instance. Cloud Asset Inventory (`searchAllResources`, which does see Resource Manager tags) is not
used: another API and permission, eventually consistent, so unsuitable right after a create.

### azure

**No per-kind list takes a filter** (compute and network alike: scope and paging only), so the mechanism is
generic. The driver uses, in this order: (A) the ARM generic resource list (`GET /subscriptions/{s}/resources`
with `$filter`) for `resourceType`, `location`, `resourceGroup`, `name` and tags; the per-kind list or `get` when the
resource group is known (cheapest); (B) Azure Resource Graph only when `expression` is given (eventually consistent:
never right after a create). Everything else is client-side. Names, resource types and tag keys are
**case-insensitive**, tag values case-sensitive. ARM ids are the only unique handle: a name is unique only inside its
resource group and type, so a `name` search is subscription-wide and may be ambiguous. A is limited to
`eq`/`ne` on `location`, `resourceType`, `name`, `resourceGroup`, `plan*`, `substringof` (name and resourceGroup only),
and tags by name and value or tag-name prefix; **a tag filter cannot be combined with another filter and returns the
resources without their tags**, so the driver filters by tag first and reads the rest from the resource. `filters`
entries: `resourceGroup` (scopes the call), `location`. `expression` is a Resource Graph `where` predicate over the
kind's table (`Resources`, or `ResourceContainers` for resource groups), `tags['k']`, `isnotnull(tags['k'])`,
`matches regex`, `startswith`, `in (...)`; subscription-scoped, 1000 rows a page with `$skipToken`; not available for
marketplace images (not in the graph) and subnets (rows of the VNet's `properties.subnets`: the predicate applies to
the VNet). Throttling: per-subscription ARM token buckets, `429` with `Retry-After`. References:
`https://learn.microsoft.com/rest/api/resources/resources/list`,
`https://learn.microsoft.com/rest/api/azureresourcegraph/resourcegraph/resources/resources`.

**resourceGroups** (`ResourceGroupsClient`; subscription-wide; not regional: the group's `location` is metadata):

| Field | Maps to | Where |
|---|---|---|
| `ids` | `ResourceGroups.Get` (name or ARM id) | by id |
| `name` | the name; exact: `Get`; a glob | by id / client |
| `tags` | `$filter=tagName eq 'k' and tagValue eq 'v'` for one pair (`tagName eq 'k'` for `"*"`, unverified); the other pairs | server (one pair) + client |
| `match.state` | `properties.provisioningState` (`Succeeded`: available) | client |

No creation time. `expression`: `ResourceContainers | where type == 'microsoft.resources/subscriptions/resourcegroups'` plus the predicate.

**subnets** (the VNet and its subnet, one unit; regional; **subnets have no tags: `tags` matches the VNet's tags**):

| Field | Maps to | Where |
|---|---|---|
| `ids` | the full subnet ARM id (`.../virtualNetworks/<vnet>/subnets/<subnet>`); a `get` | by id |
| `name` | `<vnet>/<subnet>` (a bare name cannot find a child resource); the VNet part may be a glob, matched with A's `name` / `substringof`, the subnet part client-side | server+client |
| `tags` | A `tagName/tagValue` on `resourceType eq 'Microsoft.Network/virtualNetworks'` (one pair; exclusive), the others read from the VNet | server (one pair) + client |
| `match.network` | the VNet ARM id | client |
| `match.cidr`, `within` | `properties.addressPrefix(es)` of the subnet (the VNet's `addressSpace.addressPrefixes[]` with `network`) | client |
| `match.state` | `provisioningState` | client |

No `zone` (subnets are regional), no creation time. Filters: `resourceGroup`, `location`. `{zone}` is rejected.

**securityGroups** (NSG; regional): `ids` ARM id by `get`; `name` and `tags` through A (`resourceType eq
'Microsoft.Network/networkSecurityGroups'`, `location`; tag filter exclusive); `match.state` `provisioningState`
client; a NIC takes exactly one NSG in its own region, so the default `select` is `exactlyOne`. Filters
`resourceGroup`, `location`. Same for **proximityPlacementGroups** (placement) and **sshPublicKeys**.

**images** (four sources, chosen by the shape of the reference):

| Source | `ids` | `name` | Where, and notes |
|---|---|---|---|
| Marketplace (rung 1) | the URN `publisher:offer:sku:version` | `publisher:offer:sku:<glob>`: the first three exact, a glob on the version; `latest` is the highest version | `VirtualMachineImages.List` per (publisher, offer, sku) lists versions; the path segments are exact, **no wildcard or filter exists**; architecture, Hyper-V generation and deprecation need a `Get` per version (`match.architecture`, `includeDeprecated`: client). `orderBy: name` is the version in natural order (the API's `$orderby=name desc` is a string sort, not semver: unverified), no creation time. No tags. Per location: a version may exist in some regions only. `expression` is rejected |
| Managed image | ARM id | `name` | A: `resourceType eq 'Microsoft.Compute/images'`, `location`, `resourceGroup`, tags; `match.architecture` is rejected (no field); `hyperVGeneration`, `osType` via `expression` |
| Compute Gallery (own or RBAC-shared) | `.../galleries/<g>/images/<i>/versions/<v>` (all three levels; `.../versions/latest`) | `<gallery>/<image>/<version glob>` | the gallery list and its children (A or Graph); tags on gallery, definition and version; `match.architecture` (definition `architecture`), `state` (`provisioningState`), `includeDeprecated` (`endOfLifeDate`, `excludeFromLatest`), `createdAfter` (version `publishedDate`) client-side; **a version is usable only in `publishingProfile.targetRegions`** |
| Shared and community gallery | the shared or community gallery image id | gallery unique name and image | per location; no list of community galleries exists (the public name must be known); `artifactTags` are returned but not filterable: `tags` is client-side |

### hcloud

One project per token; every kind is project-wide (no owner, no region), one list call. Filters are
**query parameters**: `name` is **exact only** (no wildcard), `label_selector` is the whole tag mechanism:
Kubernetes-like, comma is AND, `k=v`, `k!=v`, `k` (exists), `!k`, `k in (a,b)`, `k notin (a,b)`; one expression per request. Typed
filters repeat the parameter (OR); different parameters are ANDed. `sort=<field>[:asc/:desc]` (`id`, `name`,
`created`; per kind). Paging `page` / `per_page` (max 50); 3600 requests per hour per project. An unknown parameter is most
likely ignored (unverified), so only filters with a response field are offered. Empty `name` or `label_selector` in
hcloud-go means no filter: the driver guards it. Labels: a key `prefix/name`, value up to 63 characters (empty allowed).
`expression`: the **label selector**, ANDed with `tags` (the way to get `!=`, `!k`, `in` and `notin`).
Reference: `https://docs.hetzner.cloud/reference/cloud`.

**images** (`GET /images`; global in the project, per architecture):

| Field | Maps to | Where |
|---|---|---|
| `ids` | `GetByID` | by id |
| `name` | `name` (exact; **system images only**, snapshots have no name: use `tags`) | server (a glob: client) |
| `tags` | `label_selector` `k=v` ; `k` for `"*"` | server |
| `match.architecture` | `architecture` (`x86`, `arm`) | server |
| `match.state` | `status` (`available`, `creating` pending, `unavailable` failed) | server |
| `match.includeDeprecated` | `include_deprecated` | server |
| `match.createdAfter`, `createdBefore` | `created` | client |
| `match.description` | `description` (the only free text on a snapshot) | client |

Default `filters: type: [snapshot]` (the images an operator brings); `type: [system]` selects Hetzner's images. Order: `sort=created:desc/asc`
(server). Filters: `type` (`system`, `app`, `snapshot`, `backup`), `status`, `architecture`, `bound_to` (backups),
`include_deprecated`. The same name exists once per architecture: always combined with `architecture`. Never selects another account's image (no sharing).

**networks** (`GET /networks`; a network is project-level, its subnets carry a `network_zone`):

| Field | Maps to | Where |
|---|---|---|
| `ids` | `GetByID` | by id |
| `name` | `name` (exact) | server (glob: client) |
| `tags` | `label_selector` | server |
| `match.cidr`, `within` | `ip_range` | client |
| `match.zone` | the `network_zone` of a subnet that holds the location (eu-central: fsn1, nbg1, hel1; us-east: ash; us-west: hil; ap-southeast: sin) | client |
| `match.createdAfter`, `createdBefore` | `created` | client |

Order: `sort=created`, `name`. No separate subnet kind. **firewalls**, **placementGroups** (`type`: `spread`): `ids`, `name`
(exact), `tags`, `created` client, `sort=created`; firewall rules and attachments are client-side only.
Other kinds: **volumes** (`status`; location and attachment client-side), **SSH keys** (`fingerprint`; no `created` sort),
**primary IPs** (`ip`; type and location client-side), **floating IPs**.

### ovh

Plain OpenStack per region (Keystone region names, upper case): Glance, Neutron, Nova, Cinder; one project. The driver lists the
kind in the region and applies what its API cannot filter itself, so every field below is answered completely; none of it
has run against OVH yet (the plan's spikes S1 and S5 verify the deviations from upstream OpenStack on OVH). **Neutron silently
ignores an unknown query parameter** and Glance treats an unknown key as an image property and returns zero rows: so the driver offers only
`filters` it can re-apply to a response field. Neutron tags (the tag extension) are not offered for security groups: it is
unverified on OVH (S5). Glance tags are plain strings with no key and value: see [Matching](#matching). Names are not unique
(`default` security group per project, `Ext-Net` per region), so a name that matches two groups is ambiguous under
`exactlyOne`. No `expression`. Nothing here reads zones: no kind of the driver is zonal. References: Glance
`https://docs.openstack.org/api-ref/image/v2/index.html`, Neutron `https://docs.openstack.org/api-ref/network/v2/index.html`,
Nova `https://docs.openstack.org/api-ref/compute/`.

**images** (Glance `GET /v2/images`; regional; the driver never creates one):

| Field | Maps to | Where |
|---|---|---|
| `ids` | the image ID | client |
| `name` | `name` (exact; a glob is client side) | server (glob: client) |
| `tags` | `tag`: the Glance tag string `k=v` (repeatable, AND); `"*"` means a tag that starts with `k=` | server (`"*"`: client) |
| `match.architecture` | the image property `architecture` (`x86_64`, `aarch64`); an image without the property never matches | client |
| `match.owner` | `owner` (a project ID); `self` is the credentials' own private images; one value only, and `self` not with a `visibility` filter other than `private` (refused) | server |
| `match.state` | `status` (`active` is available; `queued`, `saving`, `uploading`, `importing` are pending; the rest failed); the default is available | server (the default), client |
| `match.createdAfter`, `match.createdBefore` | `created_at` | client |

`orderBy: created` orders by `created_at`. Without `ids` and `match.owner` (and the owner of the block's `imageOwner`) the list is the
project's own private images: a public, shared or community image needs its `match.owner` or a `visibility` filter. Filters:
`visibility` (`public`, `private`, `community`, `shared`; the default list excludes community images; server side and re-applied) and
`property:<k>` (a custom image property such as `flatcar_version`: exact, one of the values, client side). The pinned release must
be readable from the image's name `flatcar-<channel>-<version>-<arch>`, its properties `flatcar_channel` and `flatcar_version` or its
tag `flatcar-<channel>-<version>`, whatever selects it, and the sources must not contradict each other.

**securityGroups** (Neutron `GET /v2.0/security-groups`; Nova attaches a group by name):

| Field | Maps to | Where |
|---|---|---|
| `ids` | the group UUID | client |
| `name` | `name` (exact or a glob) | client |
| `match.description` | `description` (exact or a glob) | client |

No `tags`, no `filters`, no state, zone or creation time. The managed groups SCarpenter creates carry an ownership marker in the
description, so `use` a group of your own, never a wide-open one meant for other workloads.

**serverGroups** (Nova `os-server-groups`: no server filter at all, no tags, names repeat):

| Field | Maps to | Where |
|---|---|---|
| `ids` | the group UUID | client |
| `name` | `name` (exact or a glob) | client |

Other kinds the plan names (networks, subnets, keypairs, volumes, flavors, floating IPs, ports) are not kinds of the driver in v1: a
node attaches to the public `Ext-Net` network, and volumes are part of the server create call.

### upcloud

One global API endpoint; the zone is a field of each resource. The only filter on every kind is the **label**: `?label=<key>`
(the key exists) or `?label=<key>=<value>` (exact), repeated for AND; no OR, no wildcard, no negation; **label keys
are case-insensitive**, values exact. Keys of 2 to 32 printable characters; the SDK does not URL-escape, so a key or value with
`&`, `#`, `+` or `=` is rejected for `tags`. Unknown parameters are probably ignored (unverified): only fields with a response
field are offered. No paging in the v8 SDK (everything in one response). No `expression`. Reference: `https://developers.upcloud.com/1.3/`.

**images** (storage templates, `GET /storage/private/template`; zonal; the path is the owner scope):

| Field | Maps to | Where |
|---|---|---|
| `ids` | `GET /storage/{uuid}` | by id |
| `name` | `title` | client (a v9 `search` substring is *unverified* on 1.3) |
| `tags` | `label` (`key`, `key=value`) | server |
| `match.owner` | the path: `self` is `private`, `public` is UpCloud's own templates | server |
| `match.zone` | `zone` (the list is account-wide: a template belongs to its zone) | client |
| `match.state` | `state` (`online` available; `maintenance`, `cloning`, `backuping`, `syncing` pending; `error` failed) | client |

No architecture field (amd64 only: `match.architecture` accepts `amd64` and rejects `arm64`; carry it in a label such as
`flatcar-arch` and select with `tags`), **no creation time on templates**, so `newest` needs `orderBy: label:<key>` (the label
`flatcar-version`) or `name`. Filters (all client-side from the response): `tier` (`maxiops`, `standard`, `hdd`: the template deploys
only on its own tier), `template_type`, `encrypted`, `size`, `license`. The label may be missing for a short time after templatise (set by a second call):
a template without labels is not a match, and a template not `online` is not ready.

**networks** (`GET /network/`; SDN `private`, plus public and utility entries): `ids`; `name` client; `tags` label server;
`match.zone` `zone` (server: `?zone=`, one value); `match.cidr`, `within` `ip_networks[].address` client; `match.state` none. Default
`filters: type: [private]` (client: `public`, `utility`, `private`). No creation time. **firewalls** (SDN rulesets; the public
interface's rules are per server and cannot be selected): the list has **no label, name or zone filter** (v9 only; path
unverified), so `name`, `tags` (if the items carry labels: unverified) and `match.createdAfter` (`created_at`) are client-side and
cost one get per item when the list omits labels; `sort=created_at` is unverified. **serverGroups**: `tags` label server (the format
`key=value` versus `key:value` is *unverified*), `name` (`title`) client. Other kinds: **IP addresses** (no filter, no labels: `address`,
`family`, `floating`, `zone`, `server` client-side), **servers** (`label`, `tag`, `state`, `uuid`, `search`, `sort_by`; not a `use` kind).

### vultr

**There is no filter language.** Every list takes `per_page` (max 500) and `cursor` (follow `links.next`); a few endpoints
add one or two hard-coded parameters, whose matching semantics (exact, substring, case) are *unverified*, and an unknown
parameter is probably ignored. So everything is `client` except what is marked, and **a server-side hint is always
re-checked**. Names are not a field but a description or label, and are not unique. Tags exist only on instances: `tags`
is rejected for every other kind. No ordering parameter; no owner or sharing (one key is one account). amd64 only for cloud.
No `expression`. Reference: `https://www.vultr.com/api/`.

**images** (account snapshots, `GET /v2/snapshots`; global in the account, the same ID in every region):

| Field | Maps to | Where |
|---|---|---|
| `ids` | `id` (the key `"*"` is every region: the snapshot is global) | client (one list call) |
| `name` | `description` | server+client (the `description` parameter, semantics unverified) |
| `match.state` | `status` (`complete` available, `pending`) | client |
| `match.createdAfter`, `createdBefore` | `date_created` | client |
| `match.architecture` | none: `amd64` accepted, `arm64` rejected | client |

Order: `date_created` client-side. The description is mutable. Filters (client): `os_id`, `app_id`, `size`, `compressed_size`. The
public OS catalogue (`GET /v2/os`, rung 1, `ids` are integers as strings): `name`, `family` and `arch` (`filters: arch`) client-side, no
creation time (`orderBy: name`). **firewallGroups** (account-wide, listed once per check): `ids`, `name` (`description`), `createdAfter`,
`createdBefore` and `orderBy: created` (`date_created`) client; filters `instance_count`, `rule_count`. **networks** (VPC, regional): `ids`, `name` (`description`), `match.cidr`
(`v4_subnet` and `v4_subnet_mask` joined), `within`, `createdAfter` (`date_created`) client; the region is the `region` field (matched
to the region in scope). Other kinds: **block storage** (`label`, `region`, `status`, `block_type`, `size_gb` client), **reserved IPs**
(`label`, `region`, `ip_type`, `instance_id` client), **SSH keys** (`name`), **startup scripts**; instances are the only kind with
server-side `tag` (one tag string), `label`, `main_ip`, `region`. No placement groups, resource groups or projects exist.

### alicloud

Typed top-level request parameters, **not** a generic filter list: a different parameter is ANDed, one value each (a few take a
comma or JSON list, OR), so `filters` entries are parameter names, and a second value in `values` is one call per value.
`Filter.N` exists but only for creation times and a few capability keys: a trap, not a general filter. Tags:
`Tag.N.Key` / `Tag.N.Value` (up to 20, exact, AND; one value per key; with tag filtering at most 1000 tagged resources of
the kind per region or the call degrades: use `ListTagResources`); a key-only filter is *unverified* on `Describe*`, so `"*"` is client-side.
`ResourceGroupId` cannot filter the default resource group. Name parameters are fuzzy on some kinds and undocumented on others
(*unverified*): always re-checked on the client. Paging differs (`PageNumber`/`PageSize` up to 50 or 100, or `NextToken`/`MaxResults`;
random deep pages are throttled: page sequentially). Creation of everything is asynchronous. A documented but inapplicable parameter
is silently ignored (`ImageOwnerId` outside `others`): the driver validates the combinations. No `expression`. Every kind is regional.
Reference: `https://www.alibabacloud.com/help/en/ecs/developer-reference/api-ecs-2014-05-26-<action>` (and `api-vpc-2016-04-28-<action>`).

**images** (`DescribeImages`, `PageSize` up to 100):

| Field | Maps to | Where |
|---|---|---|
| `ids` | `ImageId` (one) | server (one call per ID) |
| `name` | `ImageName` (fuzzy substring) | server+client |
| `tags` | `Tag.N` ; `"*"` | server (`"*"`: client) |
| `match.architecture` | `Architecture` (`x86_64`, `arm64`, `i386`) | server |
| `match.owner` | `ImageOwnerAlias`: `self`, `system`, `marketplace`; an account UID is `others` plus `ImageOwnerId` | server (one call per value) |
| `match.state` | `Status` as a comma list (`Available`; `Creating`, `Waiting` pending; `UnAvailable`, `CreateFailed` failed); the default of the API is `Available` | server |
| `match.includeDeprecated` | `Status` including `Deprecated` | server |
| `match.createdAfter`, `createdBefore` | `Filter.N` `CreationStartTime` / `CreationEndTime` (UTC, minute precision) | server+client |

The driver implements the filters `ImageFamily`, `OSType`, `Usage`, `ActionType`, `InstanceType`, `SnapshotId`, `ResourceGroupId`, `IsPublic`, `Usable`,
`IsSupportCloudinit`, `IsSupportIoOptimized` and `ShowExpired` (one call per value, at most 64 calls per selector); the `Filter.*` keys below are not offered.
Omitting the owner alias never returns marketplace images; community images are `others` with `IsPublic=true`. Order: `CreationTime`
client-side (no sort parameter). Filters: `ImageFamily`, `OSType` (`linux`, `windows`), `Usage`, `ActionType`, `InstanceType`, `SnapshotId`,
`ResourceGroupId`, `IsPublic`, `Usable`, `IsSupportCloudinit`, `IsSupportIoOptimized`, `ShowExpired`, `Filter.NetworkType`,
`Filter.CpuOnlineUpgrade`, `Filter.CpuOnlineDowngrade`, `Filter.MemoryOnlineUpgrade`, `Filter.MemoryOnlineDowngrade`. Client-side only fields of the
response (not filterable): `Platform`, `BootMode`, `OSName`, `ImageVersion`, `Features.*`, `Size`, `Description`. A just-imported image is
`Creating` and hidden by the default state.

**subnets** (vSwitches, `DescribeVSwitches`, up to 50 per page; zonal; the call needs a region or a VPC):

| Field | Maps to | Where |
|---|---|---|
| `ids` | `VSwitchId` (one) | server |
| `name` | `VSwitchName` (exact or fuzzy: unverified; names repeat) | server+client |
| `tags` | `Tag.N` | server |
| `match.zone` | `ZoneId` (the zone name) | server |
| `match.network` | `VpcId` | server |
| `match.cidr`, `within` | `CidrBlock` | client |
| `match.state` | `Status` (`Available`, `Pending`) | client |
| `match.createdAfter`, `createdBefore` | `CreationTime` | client |

Filters: `IsDefault`, `EnableIpv6`, `RouteTableId`, `ResourceGroupId`. Order: `CreationTime`. Free addresses (`AvailableIpAddressCount`) are client-side.

**securityGroups** (`DescribeSecurityGroups`, `MaxResults` up to 100): `ids` `SecurityGroupIds` (JSON array, up to 100); `name`
`SecurityGroupName` (`FuzzyQuery`, unverified exactness: server+client); `tags` `Tag.N`; `match.network` `VpcId`; `match.createdAfter` and `match.createdBefore` `CreationTime` client;
`match.description` client. Filters: `NetworkType`, `SecurityGroupType` (`normal`, `enterprise`), `ServiceManaged`, `ResourceGroupId`. A
group must be in the vSwitches' VPC (`match.network`). **deploymentSets** (placement, `DescribeDeploymentSets`): `ids` `DeploymentSetIds`; `name`
`DeploymentSetName`; **no tags** (rejected); filters `Strategy` (`Availability`, `AvailabilityGroup`, `LowLatency`), `Type` (`host`, `sw`, `rack`);
`match.createdAfter` and `match.createdBefore` on `CreationTime` client.
Other kinds: **networks** (`DescribeVpcs`: `VpcId` list, `VpcName`, `IsDefault`, `EnableIpv6`, `VpcOwnerId`, `Tag.N`; `CidrBlock` client; the default VPC
is `IsDefault=true`); **key pairs** (`KeyPairName` with `*` wildcards server-side, `KeyPairFingerPrint`; the name is the identity); **resource
groups** (Resource Manager `ListResourceGroups`, global: `Name` and `DisplayName` fuzzy, `Status`, `Tag.N`); **disks** (`DescribeDisks`: `ZoneId`,
`Category`, `Status`, `DiskType`, `InstanceId`, creation-time `Filter.N`, `Tag.N`); **EIPs** (`DescribeEipAddresses`: `Status`, `AssociatedInstanceId`,
`ISP`, `Tag.N`); **network interfaces** (`DescribeNetworkInterfaces`: `VpcId`, `VSwitchId`, `Status`, `Type`, `Tag.N`).
