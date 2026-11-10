# 14 — Multi-vendor groundwork: shared core and API changes, sc-infra hand-off

Depends on: 10. Precedes 15 to 21 (every vendor plan assumes it landed). Read
`docs/plans/v1/CONVENTIONS.md` first. Spec: overview §7, §9; 00a §9; ADR-017,
ADR-018, ADR-020, ADR-021; `docs/reference/driver-contract.md`,
`docs/reference/vendor-resources.md`, `docs/reference/infrastructure-handoff.md`,
`docs/explanation/flatcar-images.md`.

Written 2026-11-10 from the seven reviewed vendor plans (15 to 21) and sc-infra's
`inventory` v1 hand-off contract. The vendor plans each found the same gaps in the
core and in the hand-off; this plan fixes them once, so that no driver carries its
own variant. The decisions below are made; each one says why, and the maintainer may
overturn any of them.

## Goal

Part A changes the SCarpenter core, the driver contract and the API so that the
vendor plans need no core change of their own. Part B lists the changes in sc-infra
(another repository; this plan's model does not make them) that the same vendor plans
need, so that they are one set of requests instead of seven; its last item is the list
of per-vendor follow-ups (controller identities and credentials).

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
| B1 to B5 (sc-infra) | all | all | all | all | all | all | all |

AWS is affected by A1 (optional `images`), A4 and A9.

## Part A: SCarpenter

### A1. The `images` resource kind and `ImageRequest`

**Decision.** One generic resource kind `images` in `spec.vendors.<vendor>.resources`,
in the shape of ADR-018: `use` with `ids` per region, a `name` pattern and a
`selector` (tags or labels) only where the vendor can filter images by them. No
`create`: that would be rung 4 (ADR-021). *Why:* the same three lookups serve every
vendor; five of the seven plans had invented their own variant of them.

```yaml
resources:
  images:
    use:
      ids: {fsn1: ["123456789", "987654321"]}   # per region; a global image is repeated in every region
      # name: "flatcar-{channel}-{version}-{arch}"
      # selector: {tags: {flatcar-version: "{version}"}}
```

- **Shape.** `ids` maps a region to a list: the list may hold both architectures
  (and UpCloud's two storage tiers); the driver matches an image to the request
  by its own architecture, channel and version. Placeholders in `name` and in
  selector values: `{channel}`, `{version}`, `{arch}`, `{region}`. A vendor with
  global images (hcloud, Vultr, GCP, Azure) still keys by region and repeats the
  same ID. *Why:* one rule for every vendor, and the NodeClass `regions` list stays
  the unit of validation.
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
- **`ImageRequest`** (`pkg/vendors/driver.go`) gets `IDs []string`, `NamePattern
  string` and `Selector map[string]string` (placeholders already expanded), next to
  `Region`, `Arch`, `Channel`, `Version` and `Owner`. The core fills them from the
  NodeClass's `images` kind for the request's region; `ResolveImage` and
  `ReportImages` honour them. `Image` gains the channel and version read from the
  image (`Channel`, `Version`). *Why:* sc-infra's code knows IDs, not names; and the
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
  or per zone and tier). The plans are aligned to this shape.
- **Status and metrics:** unchanged (`status.vendors.<vendor>.images`, `ImagesReady`,
  the `ImagesPartial` rule of the AWS driver for regions without an image).
- **Fake driver and tests:** the fake resolves `images` references against an
  `images` list in its configuration (id, region, arch, channel, version, labels);
  an L3 scenario covers a region without image, a version mismatch, and an
  explicit ID. The AWS driver honours `images.use` too (an AMI ID checked against
  the name `Flatcar-<channel>-<version>-hvm`), covered by its conformance run.
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
guest `poweroff`, a crashed node; still billed on most vendors) is reported as
`terminating` by every driver and then deleted by the garbage collector. The same
rule on every vendor. *Why:* a node that is off is not a node; leaving it costs money
for nothing, and a rule per vendor would be a surprise.

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
prices in the committed snapshot maps or is listed as unmapped; the hand-off, the
NodeClass `regions` list and sc-infra's inventory use the API names. *Why:* the
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

### Gates of part A

CONVENTIONS gates 1 to 4 (L3 stays green; the new L3 scenarios of A1 and A4 pass);
conformance additions pass for the fake and the AWS driver; `hack/check-docs.py`
clean; the reference docs of A1 to A9 updated in the same change; the CRD and API
reference regenerated (`make generate manifests`). Gate 5 (L4) when the base config
changes (A7).

## Part B: sc-infra hand-off changes

sc-infra's `inventory` v1 is consumed by sc-infra's own `scarpenter` block, so it can
change additively without a new version. These are requests for sc-infra's
maintainers; sc-infra is not changed by this plan's model. Each lists the vendor
plans that need it. The code is `core/inventory.py` and `blocks/scarpenter.py` there.

### B1. Inventory `images`

A per-region key `regions.<region>.images: [{id, name, arch, channel, version}]`,
always present, `[]` where the vendor uses rung 1 or 2 (aws, azure, gcp) and where an
environment makes no images; inventory stays `version: 1` (the key is additive: the
only consumer is sc-infra's own block). A vendor with global images lists the same
image in every region. `validate_region` allows the key; the tests run it for all
eight vendors. The environments create the images (rung 3) with the sources of
[Flatcar images](../../../explanation/flatcar-images.md): a CI step or Pulumi component per
vendor (hcloud: `hcloud-upload-image`, no Pulumi resource; OVH: a Glance upload per
region; UpCloud: storage import and template per zone and tier; Vultr:
`SnapshotFromUrl`; Alibaba: `ImageImport` and `ImageCopy`). The block renders them
as `resources.images.use.ids`. UpCloud's tier is part of the image name
(`flatcar-<channel>-<version>-amd64-<tier>`), since the entry has no tier field.

*Replaces* the variants of the plans: environment-level `images` (15, 20),
`images: {amd64: {id, name}}` per region (18), `{maxiops, standard}` (19) and
`scope.image_owner` (21).

Needed by: 15, 18, 19, 20, 21 (their lanes cannot run without it); 16 arm64 only if a
user brings own images.

### B2. Scope additions

`scope` is the account identity; `validate()` requires its keys to equal
`SCOPE_KEYS[vendor]`. The plans found: azure `tenant_id` (for the chart's credentials
values; plan 17). Everything else exists (gcp `project_id`, azure `subscription_id`,
ovh `service_name`, alicloud `account_id`) or stays `{}` (hcloud, upcloud, vultr: the
token or key is the account). The rendered block carries `project` (gcp) and
`subscriptionID` (azure) from the scope.

Needed by: 16, 17.

### B3. `vendor_block()` per vendor

`vendor_block()` and `render_nodeclass()` are AWS-shaped (`subnets` and
`securityGroups` for every vendor). Each vendor gets its own map of resource kinds,
taken from the plans and reconciled; `ACCEPTED_VENDORS` gains a vendor when its driver
and API types exist.

| Vendor | Kinds rendered (`use.ids` from the inventory field) | Also in the block |
|---|---|---|
| aws | `subnets` (`subnets[].id`), `securityGroups` (`security_groups.nodes.id`) | `regions` |
| gcp | `subnets` (`subnets[0].id`, the resource ID), `firewalls` (`security_groups.nodes.id`, the network tag) | `regions`, `project` |
| azure | `resourceGroups` (`regions.<r>.scope.resource_group`, a name), `subnets` (`subnets[0].id`), `securityGroups` (`security_groups.nodes.id`, the NSG; plan 17's name) | `regions`, `subscriptionID` |
| hcloud | `networks` (`network.id`), `firewalls` (`security_groups.nodes.id`) | `regions` |
| ovh | `securityGroups` (`security_groups.nodes.id`) | `regions` (API names) |
| upcloud | `networks` (`network.id`), `firewalls` (`security_groups.nodes.id`, the ruleset) | `regions` |
| vultr | `networks` (`network.id`), `firewallGroups` (`security_groups.nodes.id`) | `regions` |
| alicloud | `subnets` (`subnets[].id`, the vSwitches), `securityGroups` (`security_groups.nodes.id`) | `regions` |

All vendors also get `images` from the inventory (B1) where it is not empty. A kind a
cluster does not want (Vultr's `networks`, which plan 20 makes optional because it
changes what the node attaches to) can be dropped with a per-vendor `omit` list in the
block's params. The vendor-specific "must not be set in the params" clash check lists
the vendor's own kinds.

`render_vendors()` refuses to merge environments of different projects or
subscriptions into one block: gcp `project_id`, azure `subscription_id`, ovh
`service_name`, alicloud `account_id` (when both are set). One controller serves one
project or subscription. hcloud, upcloud and vultr have no id; two environments there
are separated by the existing "region provisioned by two environments" check.

Needed by: all vendors.

### B4. The CRD-subset fixture

`tests/fixtures/scnodeclass_crd_subset.yaml` (a pruned copy of the CRD, with the source
commit in its header) is refreshed from SCarpenter's CRD when this plan's A1 lands and
again with each vendor's API types: the vendor's block (`regions`, `resources.<kinds>`,
`images`, `project` or `subscriptionID`) must be in the subset, or the block's rendering
is tested against a schema that rejects it.

Needed by: all vendors.

### B5. The per-vendor controller identities and credentials

The `scarpenter` block does not create the controller's identity or the Helm release
yet. One follow-up per vendor, in the order the vendors are implemented:

| Vendor | Follow-up |
|---|---|
| aws | the IAM role (web identity) and its policy |
| gcp | the workload identity pool and provider, the service account with the custom role, the Cloud Quotas permission if the spike needs it; a **Spare Cores GCP project** for the published arm64 images (plan 22), created by sc-infra, with the keyless identity of the images repository's workflow |
| azure | the user-assigned managed identity, its federated credential, the custom role and its assignment, the registered resource providers (`Microsoft.Compute`, `Microsoft.Network`, `Microsoft.MarketplaceOrdering`), the `scarpenter-use` tag on resource groups |
| hcloud | a separate read-write token of the project as a Secret, the CI project and its tokens |
| ovh | a dedicated service account with an IAM policy on the project; region discovery from the API instead of a static list (A8); the `nodes` group's rules for `Ext-Net` nodes, or the docs say `tailscale` only |
| upcloud | a token (sub-account) for the controller that sees the templates and networks the IaC created; the managed-network range outside sc-infra's pool |
| vultr | the API key with the minimal ACLs and the egress address in its allowlist |
| alicloud | the RAM OIDC identity provider and role; vSwitch size (a /25 allows about 120 nodes per zone) |

Each also needs its e2e project, account or subscription and the secrets in the
`scarpenter-e2e` tenant, which the vendor plans' lanes list.

## Scope (what the executing model builds)

Part A, in this order: A9 and A4 (contract and conformance rules, the state tables of
AWS), A3, A5, A6, A8 (small contract changes), A2, A1 (API, CRD, `ImageRequest`, fake,
status), A7. Docs in the same change. Part B is not built here: the report
lists the changes for sc-infra's maintainers, with the vendor plans each unblocks.

## Dependencies

- Plans 06, 08, 09, 10 (done). Independent of the vendor plans, which depend on it.
- Plan 22 (the images repository) does not depend on this plan; B5's GCP project is the
  one thing they share.

## Open questions for the maintainers

1. **A7 field name and default.** `spec.bootstrap.hardening` is a proposal; the other
   candidate is a vendor-block field. Off by default is a decision, but a safer default
   for GCP and Alibaba (on) is possible once the L5 lane shows nothing breaks.
2. **A4 grace.** 10 minutes of `terminating` before a second delete. Shorter costs
   delete calls on slow vendor deletes; longer leaves a powered-off node billed.
3. **A1 strictness.** An explicit image ID whose version cannot be read back is not
   ready. A softer rule (a Warning) would allow images without labels; it would break
   ADR-017 for benchmarks.

## Gates

CONVENTIONS gates 1 to 4 and gate 5 when A7 changes the base config; conformance
additions green for the fake driver and the AWS driver; `hack/check-docs.py` clean;
`docs/plans/v1/README.md` status `review`.
