# 14 — Multi-vendor groundwork: shared core and API changes, the hand-off contract and the resolution order

Depends on: 10. Precedes 15 to 21 (every vendor plan assumes it landed). Read
`docs/plans/v1/CONVENTIONS.md` first. Spec: overview §7, §9; 00a §9; ADR-017,
ADR-018, ADR-020, ADR-021, ADR-023; `docs/reference/driver-contract.md`,
`docs/reference/vendor-resources.md`, `docs/reference/resource-selectors.md`,
`docs/reference/infrastructure-handoff.md`,
`docs/explanation/flatcar-images.md`.

Written 2026-11-10 from the seven reviewed vendor plans (15 to 21). The vendor plans
each found the same gaps in the core and in the hand-off; this plan fixes them once, so that no driver carries its
own variant. The decisions below are made; each one says why, and the maintainer may
overturn any of them.

## Goal

Part A changes the SCarpenter core, the driver contract and the API so that the
vendor plans need no core change of their own. Part B is also SCarpenter-side and
IaC-neutral: the documented hand-off contract (any tool, Pulumi, Terraform or OpenTofu,
a CLI or the console, passes vendor resources in one unified way: the one `use`
selector of ADR-023, by ID, name, tags, portable attributes or the vendor's own filters), the published CRD schema and examples an IaC renders against, and the
resource resolution order of ADR-022. SCarpenter's code, docs and plans name no
particular IaC repository and design against none of its outputs; what an operator's
IaC must create is stated as requirements in each vendor plan.

Nothing here adds a vendor. The AWS driver changes only where a rule below is a
rule for every vendor (the stopped-node rule, the conformance additions).

## Which vendor plan needs which item

| Item | 15 hcloud | 16 gcp | 17 azure | 18 ovh | 19 upcloud | 20 vultr | 21 alicloud |
|---|---|---|---|---|---|---|---|
| A1 `images` kind and `ImageRequest` | yes | yes (arm64, overrides) | optional | yes | yes | yes | yes |
| A2 `RootDiskForType` | yes | no | no | where applicable | yes | yes | no |
| A3 idempotency label | yes | yes | no (name) | yes | yes | yes | no (token) |
| A4 stopped-node rule | yes | yes | yes | yes | yes | yes | yes |
| A5 account-wide quota errors | yes | no | no | no | yes | yes | no |
| A6 message-based classification | no | no | no | yes | no | yes | no |
| A7 Butane hook | no | yes | no | no | no | no | yes |
| A8 region aliases | no | no | no | yes | no | no | no |
| A9 malformed references | yes | yes | yes | yes | yes | yes | yes |
| A10 one selector (`use`) | yes | yes | yes | yes | yes | yes | yes |
| B1 to B4 (contract, schema, resolution order) | all | all | all | all | all | all | all |

AWS is affected by A1 (optional `images`), A4, A9 and A10 (the migration of its `use`).

## Part A: SCarpenter

### A1. The `images` resource kind and `ImageRequest`

**Decision.** One generic resource kind `images` in `spec.vendors.<vendor>.resources`,
in the shape of ADR-018 and with the one selector of ADR-023 (A10): `use` with `ids`,
`name`, `tags` only where the vendor can filter images by them, `match`, native `filters`,
`select`, `orderBy`. No `create`: that would be rung 4 (ADR-021). *Why:* the same three lookups serve every
vendor; five of the seven plans had invented their own variant of them.

```yaml
resources:
  images:
    use:
      ids: {"*": ["123456789", "987654321"]}   # per region; a global image under the key "*" (every region)
      # name: "flatcar-{channel}-{version}-{arch}"
      # tags: {flatcar-version: "{version}"}, match: {architecture: [amd64]}, select: newest
```

- **Shape.** `ids` maps a region to a list: the list may hold both architectures
  (and UpCloud's two storage tiers); the driver matches an image to the request
  by its own architecture, channel and version. Placeholders in every
  string value: `{channel}`, `{version}`, `{arch}`, `{vendorArch}`, `{region}`, `{zone}`, `{nodeclass}`. A
  vendor with global images (hcloud, Vultr, GCP, Azure) lists the IDs once under the region key `*`
  (every region in scope), or repeats them per region. *Why:* one rule for every vendor, and the
  NodeClass `regions` list stays the unit of validation.
- **CRD.** The CEL rule "at most one of `use`, `create`, `forbid`" (ADR-018) gains
  "`images` allows `use` only". The kind exists in every vendor block's
  `resources`, optional.
- **Optional.** For rungs 1 and 2 `images` is optional: without it the driver
  resolves the pinned Flatcar version itself (AWS by name and owner, Azure by URN,
  GCP by name in `kinvolk-public` or the Spare Cores project). For rung 3 the driver
  has a documented naming convention per vendor and `images` overrides it. The
  vendor fields that name an owner (`aws.imageOwner`, `azure.imageOwner`, GCP
  `imageProject`, OVH `imageOwner` and `includeCommunityImages`, Alibaba
  `imageOwner`) stay: they say whose images to look in, not which.
- **`ImageRequest`** (`pkg/vendors/driver.go`) gets `Use UseRef` (the A10 selector,
  placeholders already expanded), next to `Region`, `Arch`, `Channel`, `Version` and `Owner`. The core fills it from the
  NodeClass's `images` kind for the request's region; `ResolveImage` and
  `ReportImages` honour them. `Image` gains the channel and version read from the
  image (`Channel`, `Version`). *Why:* an IaC tool knows IDs, not names; and the
  core must not branch on a vendor.
- **Version check (ADR-017).** Whatever the lookup, the driver compares the image's
  own labels, tags, properties or name with the pinned channel and version. A
  mismatch, or an image that cannot say, makes the region's images not ready
  (`ImagesReady`, reason `ImageVersionMismatch` or `ImageVersionUnknown`, naming the
  region and the image). Never booted, never silently replaced. The conformance
  suite tests it.
- **Drift.** Image IDs are not part of the drift hash; the pinned version is
  (ADR-012). *Why:* a rebuilt image of the same version is the same OS.
- **Replaces** the per-plan variants: OVH `images.{ids,namePattern}`,
  Alibaba `image.use`, hcloud, Vultr and UpCloud `images.use` (keyed per architecture
  or per zone and tier). The request's architecture (and UpCloud's tier) narrow the
  selector's matches, so an `ids` list that holds both architectures is not ambiguous. The plans are aligned to this shape.
- **Status and metrics:** unchanged (`status.vendors.<vendor>.images`, `ImagesReady`,
  the `ImagesPartial` rule of the AWS driver for regions without an image).
- **Fake driver and tests:** the fake resolves `images` references against an
  `images` list in its configuration (id, region, arch, channel, version, labels);
  an L3 scenario covers a region without image, a version mismatch, and an
  explicit ID. The AWS driver honours `images.use` too (an AMI ID checked against
  the name `Flatcar-<channel>-<version>-hvm`; its default is the built-in selector
  `name` + `match.owner: [<imageOwner>]` + `architecture` + `state: [available]`), covered by its
  conformance run.
- **Docs:** `docs/reference/vendor-resources.md` (the kind, the no-`create` rule),
  `docs/reference/driver-contract.md` (`ImageRequest`), the API reference,
  `docs/reference/infrastructure-handoff.md`.

Needed by: 15, 16 (arm64, and the override), 18, 19, 20, 21; optional in 17.

### A2. `RootDiskForType`

**Decision.** `NodeOptions` gets `RootDiskForType(settings VendorSettings,
instanceType CatalogType) (bytes int64, ok bool)`, for vendors whose root disk is
fixed by the type (hcloud, Vultr, UpCloud; OVH where the root is the flavor's local
disk). `Facts.RootDiskBytes(vendor, class)` takes the type: it asks the driver and,
for `0` or unknown (`ok` false), falls back to the catalog (Keeper's `storage_size`
of the type, in bytes). *Why:* `RootDiskBytes(settings)` is per NodeClass, and
without a per-type answer pods that request ephemeral storage never fit on these
vendors. The driver may answer where the fallback is wrong (OVH `i1` flavors:
`storage_size` includes the local NVMe disks, the root is the 50 GB `ssd` entry).

- The ephemeral storage of an instance type is computed per type; the catalog
  caches per (class, type). `Supported` and the storage profiles are unchanged: a
  profile cannot change a type-defined root disk (ADR-017).
- Docs: node settings (the ephemeral-storage rule), driver contract table, the
  Keeper unit of `storage_size` (GB or GiB, unverified in plan 15) is fixed with it.

Needed by: 15, 19, 20; 18 where applicable (flavors with a local root disk).

### A3. The idempotency label

**Decision.** Vendors without a create token store the NodeClaim UID on the instance
as the label or tag `scarpenter.sparecores.com/idempotency-key`, looked up before
every create (any zone, type or capacity type of the replay returns the original
instance; a failed create leaves the key unused). `idempotency-key` becomes a
logical tag key next to `cluster`, `nodeclaim`, `nodepool`, `nodeclass` and
`managed-by`, so `TagKey` encodes it per vendor: `scarpenter.sparecores.com/idempotency-key`
by default (hcloud labels, Vultr tags), `scarpenter-idempotency-key`
where `/` or `.` are not allowed (GCP labels, UpCloud labels of at most 32
characters: 26 here, OVH Nova metadata). *Why:* one name and one lookup rule; the
UID is 36 lower-case characters and fits every vendor's value rules.

- The helper in `pkg/vendors` (`IdempotencyTag`) builds and reads it; the
  conformance suite requires that the tag round-trips through `GetInstance` and
  `FindInstance`.
- Vendors with a create token or a deterministic name keep them: AWS `ClientToken`,
  Alibaba `ClientToken` (plus the lookup by the `nodeclaim` tag), Azure `PUT` by
  deterministic name. They need not write the label.
- Plan 16's `scarpenter-uid` and plan 18's `scarpenter-claim-uid` become this label.

Needed by: 15, 16, 18, 19, 20.

### A4. A stopped node is terminating, and is collected

**Decision.** An instance that is stopped, powered off or suspended by itself (a
guest `poweroff`, a crashed node; its disks and addresses, and on most vendors the
compute too, keep billing) is reported as `terminating` by every driver and then
deleted by the garbage collector. The same rule on every vendor. *Why:* a node that
is off cannot take pods and is not coming back; leaving it costs money for nothing,
and a rule per vendor would be a surprise. What the deletion saves is the charges
that go on from now: the following billing periods, the disks and the addresses. On
a vendor that bills by the started period (hcloud, UpCloud, Vultr, OVH US, per
[ADR-024](../../../explanation/DECISIONS.md#adr-024-billing-period-aware-node-retention-accepted-2026-11-10)),
the period that has started is paid whether the node is deleted now or at its end, so
deleting early never saves the current period. The rule is not a billing optimisation
for the current period and needs no billing data.

Today this does not happen, even for drivers that already map it so: `Instance.Gone()`
is true for `terminating`, `CloudProvider.Delete` returns without deleting an instance
that is `terminating` (it assumes the vendor is deleting it), and the garbage
collector's orphan sweep skips `Gone()` instances. So a stopped instance reported
`terminating` would be left alone for ever. The change:

- Core: an instance seen `terminating` for longer than a grace period
  (`--terminating-grace`, default 10 minutes; the first sighting is kept in memory, so
  a restart restarts the clock, like the resource idle time) is deleted again by the
  garbage collector's orphan sweep and by `CloudProvider.Delete` (`DeleteInstance` is
  idempotent). Instances the vendor is really deleting disappear well inside the grace
  and cost no extra call. Metric: `scarpenter_gc_deletions_total` gets the reason
  `Terminating`.
- Listing: `ListInstances` returns `terminating` instances and excludes only
  `terminated` ones, as the contract already says; the AWS driver's `ListInstances`
  today drops every `Gone()` instance and changes with it. The CloudProvider's
  instance list (`Get`, `List`) keeps skipping `Gone()` instances, so for a stopped node
  whose NodeClaim still exists Karpenter core's NodeClaim garbage collection deletes the
  NodeClaim, and `CloudProvider.Delete` deletes the instance once the grace has passed.
- Drivers: the state tables map stopped, off, powered-off, suspended and deallocated to
  `terminating`: AWS `stopping` and `stopped` (today `running`), GCP `STOPPING`,
  `STOPPED`, `TERMINATED`, `SUSPENDED` (plan 16 had them `running`), Azure `stopped` and
  `deallocated` (plan 17 had them `running`), Alibaba `Stopping` and `Stopped` (plan 21
  had them `running`); hcloud `off`, OVH `SHUTOFF`, UpCloud `stopped`, Vultr `stopped`
  already are. The AWS reference changes with it. On AWS a guest `poweroff` already
  terminates the instance (`InstanceInitiatedShutdownBehavior: terminate`), so `stopped`
  arises only from a `StopInstances` call outside SCarpenter.
- Conformance: a fixture hook powers an instance off; it must report `terminating`,
  stay listed (the garbage collector can find it) and delete cleanly.

Needed by: all vendors (the AWS driver included).

### A5. Account-wide quota errors

**Decision.** `vendors.Error` gets a flag (`AccountWide`) a driver sets on a
`QuotaExceeded` of a vendor whose limits are account-wide (hcloud project limits,
Vultr account limits, UpCloud account limits). The unavailable-offerings cache then
marks the capacity type unavailable in every region of the vendor for the quota TTL
(30 minutes) instead of the one region. *Why:* otherwise every region fails once
before all are gated, and the status names the wrong scope. Cheap: one field, one
branch.

- The quota tracker's `GlobalQuotas` already models account-wide buckets; the flag
  covers the reactive path, where a vendor reports no usage.
- Docs: the error table of the driver contract, the controller reference.
- Conformance: a fixture error with the flag gates every region.

Needed by: 15, 19, 20.

### A6. Message-based classification

**Decision.** Contract amendment: a vendor whose API has no error codes may be
classified by HTTP status plus message text, with the texts pinned by tests. Today
only the `qemu` driver is allowed to. *Why:* Vultr's error body is `{"error": text,
"status": n}` and OVH's OpenStack faults carry only the status; refusing text would
mean classifying every failure `Fatal`.

- Overview §7.2 and the driver contract's "Classification uses vendor error codes, not
  message text" gain the exception and the conditions: the driver documents each text,
  a table-driven test pins every text, the status is captured per request, and an
  unmatched text is `Fatal` (never `InsufficientCapacity`, ADR-013: a quota limit must
  never be reported as capacity).
- The conformance suite has no new test; the per-driver tests are the pin.

Needed by: 18 (OVH), 20 (Vultr).

### A7. A vendor Butane fragment hook

**Decision.** A capability interface (`NodeHardener`) with which a driver returns named
Butane fragments that the core merges into the base config. The first fragment is
`block-metadata-from-pods`: drop forwarded traffic to the vendor's metadata address
(`169.254.169.254` on GCP, `100.100.100.200` on Alibaba), which is reachable from Pods
because there is nothing like AWS's hop limit. Off by default, enabled per NodeClass
(the field name is decided in the implementing change; proposed
`spec.bootstrap.hardening: [metadata-from-pods]`), documented with the trade-off. *Why:*
the core must not branch on a vendor, and the user-data (a one-hour bootstrap token and
the Tailscale key) is readable from Pods on these vendors. Off by default because it
changes what Pods on the node can reach.

- Only the forward path is dropped: the node's own units (Ignition, the kubelet, time
  sync) use the output path and are unaffected. Host-network Pods are not Pods in this
  sense; the docs say so.
- The fragment is part of the rendered Ignition and of the size budget; a test renders
  it and checks the budget.
- A driver may also return a fragment that is always on: the GCP arm64 image lacks the
  udev rules that create `/dev/disk/by-id/google-<name>` (plan 22), and the spike of plan
  16 may put them here. The interface allows it from the start; the hardening fragments
  stay opt-in.
- Docs: node bootstrap reference, driver contract, the GCP and Alibaba driver
  references.

Needed by: 16 (GCP), 21 (Alibaba).

### A8. Region aliases

**Decision.** A driver may map Keeper's region names to the vendor's API region names
(OVH, where Keeper prices mostly legacy names such as `GRA11` and the catalog may say
otherwise). The table lives in the driver, with a test that every Keeper region with
prices in the committed snapshot maps or is listed as unmapped; the
NodeClass `regions` list and the hand-off contract use the API names. *Why:* the
Keystone catalog is the truth for what a project can use; renaming rows in Keeper is
not ours to do.

- `vendors.Region` carries the API name as its ID and the Keeper name it joins to; the
  facts join the catalog on it. Which name the `topology.kubernetes.io/region` label
  carries is checked against how the catalog keys offerings in the implementing change
  (the proposal: the API name, the one the NodeClass uses).
- Docs: the driver contract (`Regions`).

Needed by: 18.

### A9. Malformed references are `NotFound`

**Decision.** `GetInstance`, `DeleteInstance` and the resource calls given a reference
the driver cannot parse (the conformance suite's `<region>/i-never-existed`, an ID
without the parts a vendor needs) return `NotFound`, never `Fatal`. *Why:* a reference
that cannot exist is an instance that does not exist, and `Fatal` would put an Event on
a NodeClass for a stale annotation. The AWS driver does it already; the contract and
the suite now say it for everybody (plans 16 and 17 wrote it individually).

Needed by: all vendors.

### A10. One selector for every vendor resource

**Decision.** `use` is the one selector of [ADR-023](../../../explanation/DECISIONS.md#adr-023-one-selector-for-every-vendor-resource-accepted-2026-11-10),
format and per-vendor tables in [`docs/reference/resource-selectors.md`](../../../reference/resource-selectors.md):
`ids`, `name`, `tags`, `match`, `filters`, `expression`, `select`, `orderBy` on every kind of every
vendor; all given fields ANDed; names the vendor's driver does not support rejected, never ignored; the
vendor is asked for what it can filter and the shared matcher re-applies every field to the answer.
*Why:* A1 (images) and B1 (the CIDR form) each added a field to a different `use`; a vendor-by-vendor
growth would end in seven shapes. The research (eight vendors, 2026-11-10) is summarised in ADR-023.

What it takes:

- **API type** (`pkg/apis/v1alpha1/common/resources.go`). `ResourceUse` becomes `{ids map[string][]string,
  name string, tags map[string]string, match *ResourceMatch, filters []ResourceFilter{name, values},
  expression string, select, orderBy}`; `TagSelector` goes away. `ResourceMatch` is a typed struct
  (`architecture`, `owner`, `state`, `includeDeprecated`, `createdAfter`, `createdBefore`, `description`,
  `zone`, `network`, `cidr`, `within`) so a misspelt attribute is a schema error. Limits: 64 `ids` keys of at most 64 IDs, 32
  tags, 32 filters of at most 32 values, strings of at most 255 characters (`expression` 4096).
- **CEL.** "Set exactly one of ids, name and selector" becomes "set at least one of ids, name, tags, match,
  filters and expression" (an empty `use` would select everything); `orderBy` only with `select` `newest` or
  `oldest` and one of `created`, `name`, `label:<key>`; `ids` keys non-empty (a region name or `*`);
  `createdAfter` and `createdBefore` RFC 3339 (typed `metav1.Time`); `cidr` and `within` `isCIDR`; each
  filter has a name and at least one value. `select` has no schema default (the type is shared by every kind;
  the kind's default is applied in Go from the capability declaration); per-kind rules such as `images` rejecting
  `select: all` are `XValidation` rules on the kind's field of each vendor's `Resources`. Placeholders (known names, `{{` and `}}` as braces) are checked in
  Go by the NodeClass validator: a CEL regex over every string would exceed the cost budget.
- **Per-driver capability declaration.** `ManagedResources` gains `Selectors() map[ResourceKind]SelectorCaps`:
  per kind, whether it is zonal, has a creation time, and which tags form it has (`tags`, `labels`, plain
  `strings`, `none`); the name field and where it is evaluated; per `match` attribute the native field and
  `server`, `server+client` or `client` (or absent: unsupported); the `filters` names (exact or a prefix
  pattern such as `tag:*`, `labels.*`, `property:*`) with their side and, for vendors that may ignore a
  parameter, the response field that re-applies them; whether `expression` is accepted and in which language;
  the default `select`. `ValidateNodeClass` rejects everything outside it with the vendor, kind, field and the
  supported names. A test keeps `resource-selectors.md` and the declarations in step (every declared
  attribute and filter name appears in the vendor's section).
- **Shared matcher and placeholder expansion** (`pkg/vendors/selector`, no vendor imports): `Expand`
  (placeholders, per-vendor `{vendorArch}`, escapes), glob matching (`*`, `?`, `\`), natural-order comparison,
  tag matching in the three forms (key and value, plain strings `k=v`, `"*"`), CIDR `cidr` and `within`, and
  `Select` (exactly one, all, newest, oldest, ties) returning typed `NoMatch` and `Ambiguous` errors that carry
  the candidate count after each stage (what the status message prints). A driver supplies the vendor call
  and an accessor per item (ID, name, tags, creation time, attributes); it does not write its own matching.
- **Core.** `vendors.UseRef` is the expanded selector (`IDs`, `Name`, `Tags`, `Match`, `Filters`, `Expression`,
  `Select`, `OrderBy`); `Resolve` takes it; `IsDefault` stays "nothing given". `ImageRequest.Use` (A1) is the
  same type. The status entry gets `name` and `source` (B3); `ResourcesReady`'s message carries the stage counts;
  a launched node records the resolved IDs it used. The resource check keeps its 30-second cache; a kind global
  at the vendor (hcloud firewalls, Vultr firewall groups, GCP images) is listed once per check, not per region,
  and an `ids`-only selector is looked up by ID, not by listing (hcloud's 3600 requests an hour per project).
- **Conformance** (fake driver, with configurable sides so a "server" filter, a superset and "client only"
  all run the same assertions, and the AWS driver): fields are ANDed; an unsupported name is rejected at
  validation; `ids` are ANDed with the rest and a missing or non-matching ID is named; a missing ID does not
  fail the other IDs; zero matches and two matches under `exactlyOne` make the region unusable with the counts;
  `newest`, `oldest` and a tie; `*` under `ids`; a vendor-side superset gives the same answer as the client;
  `{zone}` per zone; glob escapes; tags `"*"` and plain-string tags; `created` rejected on a kind without
  a creation time; an unknown placeholder rejected; an images selector narrowed to the request's architecture.
- **AWS migration.** `use.selector.tags` is renamed `use.tags`; `ids` and `name` keep their meaning
  (subnets: the `Name` tag, groups: `group-name`, wildcards as today); the CEL "exactly one" becomes "at least
  one". `useFilters` builds the EC2 filter list from the whole selector (tags to `tag:<k>` or `tag-key`, `ids`
  to the kind's ID filter, `match` per the AWS table, `filters` passed through), and `validateResource`
  checks `filters` names against the per-kind list taken from the SDK's documentation of the
  `Describe*` calls (a test asserts every name appears in the pinned SDK's doc comments). The AWS `images`
  kind (A1) is built on it. **Old fields: a clean rename, no deprecated alias (decided by the maintainer, 2026-11-10, renamed before v0.1.0).** The API is `v1alpha1`, an alias
  would keep two spellings in the CRD, the docs and the code for ever, and nothing is lost: a NodeClass with
  the old `selector` is pruned by the API server (or reported by strict field validation), its `use` then has
  no field and the new non-empty rule rejects it, so it fails loudly instead of selecting everything. A NodeClass
  already stored with `selector` is pruned on read and CEL does not re-run until it is written, so the controller
  too treats a present but empty `use` as invalid (`ValidationSucceeded=False`), never as "nothing given": it must not
  fall back to discovery or a default. The
  release note says "`use.selector.tags` is now `use.tags`"; the testing guide, `crd_test.go` and the AWS
  validation tests are updated with it.
- **Docs.** `docs/reference/resource-selectors.md` (written) stays in step with each driver's declaration; the
  API reference is regenerated from the CRD (`make generate manifests`); `vendor-resources.md`,
  `infrastructure-handoff.md`, the how-to and `00a` §9 already describe it.

Needed by: all vendors (the AWS driver included); A1 and B1 are built on it.

### Gates of part A

CONVENTIONS gates 1 to 4 (L3 stays green; the new L3 scenarios of A1 and A4 pass; the A10
conformance cases pass for the fake and the AWS driver);
conformance additions pass for the fake and the AWS driver; `hack/check-docs.py`
clean; the reference docs of A1 to A9 updated in the same change; the CRD and API
reference regenerated (`make generate manifests`). Gate 5 (L4) when the base config
changes (A7).

## Part B: the hand-off contract and the resolution order

SCarpenter takes vendor resources from the NodeClass, never from an IaC tool's state or
outputs. The contract is its own: `spec.vendors.<vendor>`, its `regions` and
`resources.<kind>` with `use` (the one selector of A10: ids per region, name, tags,
portable `match` attributes such as the CIDR of a network or subnet, and the vendor's own filters). Every operator pre-provisions with their own tool and passes
the result in this one way; what they do not pass, SCarpenter finds or manages itself.

### B1. The documented contract

`docs/reference/infrastructure-handoff.md` is the contract, per vendor: the kinds, the
`use` fields each supports, what SCarpenter does without `use`, the account scope and
credentials, the images of rung 3, and an example NodeClass fragment an IaC renders. It
is already written for the eight vendors from the plans and stays in step with each
driver: a driver's change updates its section. It never describes an operator's tool.
*Why:* the maintainers' rule is that no user depends on someone else's infrastructure
code; one page serves all of them.

New in the contract, implemented with A10: the attributes `match.cidr` (the range equals it) and
`match.within` (the range lies inside it; a subnet inside `10.8.0.0/16`) for the kinds that have
a range (`networks`, `subnets`), for tools that know the plan of addresses and not the IDs. Equality is
sent to the vendor where it can filter by range (AWS `cidr-block`, GCP `ipCidrRange`, Neutron `cidr`);
containment is always client-side. A kind without a range (GCP networks, Hetzner firewalls) rejects both in
the driver's capability declaration. Conformance: a fixture with two ranges resolves the right one by `cidr`
and by `within`.

### B2. Published schema and examples

The CRD (`config/crd`) and the API reference are the schema an IaC renders against; they
are published with each release as release assets (the CRD YAML and a JSON Schema of the
`SCNodeClass`), next to one example NodeClass per vendor under `docs/examples/`, each
checked by a test against the CRD (so an example never drifts). The vendor blocks of the
plans (`regions`, `resources.<kinds>`, `images`, `project` or `subscriptionID`) are in
the schema as each driver's API types land. *Why:* a Terraform module or a Pulumi program
can validate what it renders without reading Go.

### B3. The resolution order: core and per-driver hooks

ADR-022 (accepted 2026-11-10, amending ADR-018): for each kind, in a region: explicit `use`;
for a kind without one, discovery of resources tagged `scarpenter.sparecores.com/use:
"true"` (vendors without tags skip it); the vendor's own default where it exists and is
safe; create (managed, ADR-018 lifecycle); else fail gracefully (`ResourcesReady=False`,
the kind, the region and what was tried; no launch; nothing half-created). An explicit
`create` skips discovery and defaults.

What exists: the AWS driver and the core implement explicit `use`, the default VPC and
subnets (`Resolve` with an empty reference), create, and the `ResourcesReady` failure.
What this plan adds:

- **Core.** The resolver runs the five steps in order per (vendor, kind, region) and
  records in `status.vendors.<vendor>.resources` which step produced each resource (a
  new `source` field: `use`, `discovered`, `default`, `created`) and, on failure, the
  steps tried, in the condition message. A step that errors (rather than finds nothing)
  stops the order: a vendor error is `CheckFailed`, never "fall through to create".
  Kinds that live in a network (subnets, security groups) are resolved after it and
  constrained to it (an implicit `match.network`), whichever step found the network.
- **Driver hooks** on `ManagedResources`, all optional with a no-op default so a driver
  adds them as it needs: `Discover(kind, region)` (the selector `tags: {<opt-in key>: "true"}` with
  the kind's default `select`, run through the A10 matcher; a kind whose capability declaration has no tags skips the step),
  `Default(kind, region)` (the vendor default resource, or none), and
  `DefaultSuits(kind, resource, settings)` (whether the default can honour the
  requested settings, ADR-017: a default firewall that cannot isolate runs or restrict
  inbound rules, a default network without IPv6; false skips the default and the order
  continues, it never degrades the request). Today's empty-reference `Resolve` becomes
  `Default`.
- **The opt-in tag** is a logical tag key `use`, encoded per vendor by `TagKey` like the
  others (`scarpenter-use` where `/` is not allowed), documented as reserved.
- **No `policy` knob.** `use`, `create` and `forbid` cover the known cases; one is
  proposed only if a real case needs it (for example forbidding vendor defaults in
  an organisation that bans the default network).
- **Conformance cases** (fake and AWS): explicit beats tagged beats default beats
  create; an explicit `use` matching nothing fails and does not fall through; a default
  that does not suit the request is skipped; `forbid` ends at the failure; the failure
  message names kind, region and the steps; a creation that fails midway leaves nothing
  (the next pass adopts or completes it).
- **Docs:** `docs/reference/vendor-resources.md` and `00a` §9 (written), the how-to, the
  driver contract, each driver reference (which kinds have a default, which have tags).

Needed by: all vendors.

### B4. Per-vendor controller identities: documented requirements

SCarpenter creates no identity in the operator's account. Each vendor plan lists, as a
documented requirement, the role, service account or token the operator must create and
the permissions it needs (the AWS policy is `docs/reference/aws-iam-policy.json`); the
how-to guides carry the steps for the console or CLI, and the hand-off contract the
credentials. The e2e lane of a vendor lists what the CI account needs in the same
neutral terms. Summary of what each plan documents:

| Vendor | The operator creates |
|---|---|
| aws | the IAM role (web identity) and its policy |
| gcp | the workload identity pool and provider, the service account with the custom role, the Cloud Quotas permission if the spike needs it |
| azure | the user-assigned managed identity, its federated credential, the custom role and its assignment, the registered resource providers (`Microsoft.Compute`, `Microsoft.Network`, `Microsoft.MarketplaceOrdering`) |
| hcloud | a separate read-write token of the project as a Secret |
| ovh | a dedicated service account with an IAM policy on the project; the `nodes` group's rules for `Ext-Net` nodes, or `tailscale` only |
| upcloud | a token (sub-account) that sees the templates and networks the operator created |
| vultr | the API key with the minimal ACLs and the controller's egress address in its allowlist |
| alicloud | the RAM OIDC identity provider and role; vSwitch size (a /25 allows about 120 nodes per zone) |

The images of rung 3 are created by the operator's IaC or a CI job; the images
repository and its published GCP arm64 project are plan 22's.

## Scope (what the executing model builds)

Part A, in this order: A9 and A4 (contract and conformance rules, the state tables of
AWS), A3, A5, A6, A8 (small contract changes), A2, A10 (the selector type, matcher and
capability declaration; the AWS migration), A1 (API, CRD, `ImageRequest`, fake,
status), A7; then B3 (the resolver and its hooks, with conformance) and B1/B2 (the CIDR
attributes, the published schema and examples). Docs in the same change.

## Dependencies

- Plans 06, 08, 09, 10 (done). Independent of the vendor plans, which depend on it.
- Plan 22 (the images repository) does not depend on this plan.

## Open questions for the maintainers

1. **A7 field name and default.** `spec.bootstrap.hardening` is a proposal; the other
   candidate is a vendor-block field. Off by default is a decision, but a safer default
   for GCP and Alibaba (on) is possible once the L5 lane shows nothing breaks.
2. **A4 grace.** 10 minutes of `terminating` before a second delete. Shorter costs
   delete calls on slow vendor deletes; longer leaves a powered-off node billed.
3. **A1 strictness.** An explicit image ID whose version cannot be read back is not
   ready. A softer rule (a Warning) would allow images without labels; it would break
   ADR-017 for benchmarks.
4. **A10 `ids` for global kinds.** The research found that a flat list cannot share a field with a
   per-region map in a structural CRD schema; the plan uses the key `*` (`ids: {"*": [id]}`). The
   alternative is a second field (`globalIds`).
5. **A10 negation.** The portable format has no `not`: AWS and Alibaba have none, GCP, Hetzner and
   Neutron do. Negation is available only through the native `filters`, `expression` (hcloud label selector,
   GCP filter, Resource Graph) and Neutron's `not-tags`. A portable `not` could be added if a real use needs it.
6. **A10 hcloud `expression`.** The Hetzner label selector is offered as `expression` (it adds `!=`, `in` and
   `notin` to `tags`), although it is a selector syntax rather than a query language; the format's text says
   "where the vendor has a query language".
7. **A10 status size.** `status.vendors.<vendor>.resources` holds at most 128 entries; `select: all` over many
   regions with several subnets each can exceed it. Raise the cap, or list only counts per region past it.

## Gates

CONVENTIONS gates 1 to 4 and gate 5 when A7 changes the base config; conformance
additions green for the fake driver and the AWS driver; `hack/check-docs.py` clean;
`docs/plans/v1/README.md` status `review`.
