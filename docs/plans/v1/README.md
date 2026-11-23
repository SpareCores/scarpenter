# Implementation plans (v1)

Sequenced, self-contained work orders for SCarpenter v1: the vendor-neutral
core and the vendor drivers, AWS first. The base document is
[`wip/00-overview.md`](wip/00-overview.md): architecture, API, catalog,
bootstrap, driver contract, AWS, testing. Every later plan assumes it.

Layout:

- [`done/`](done/) — completed plans
- [`wip/`](wip/) — plans in progress or not yet started

Execution protocol:

1. Hand the model exactly one plan file plus the overview and
   [`CONVENTIONS.md`](CONVENTIONS.md).
2. The model implements until all gates of the plan are green, does NOT
   commit, and writes a report.
3. A coordinator (separate session) reviews the diff, re-runs the gates
   independently, folds verified facts back into the overview, and commits.
4. Only then does the next plan start — plans assume all predecessors are
   committed (07 and 08 may run in parallel).
5. When a plan finishes, move it from `wip/` to `done/`.

| # | file | status | milestone | depends on |
|---|---|---|---|---|
| 00 | `wip/00-overview.md` | draft for review | architecture, API, catalog, bootstrap, drivers, AWS, testing | — |
| 00a | `done/00a-node-options.md` | done (accepted design, folded into the overview and built by 06, 14, 28 and later plans) | vendor-native node settings and profiles, instance selection (attribute and metric labels, sc-karpenter-nodepool selection profiles), vendor resources (bring your own or managed, with lifecycles), multi-vendor NodeClass, workload interface | 00 |
| 01 | `done/01-sc-keeper-go.md` | done | shared module: Keeper client and sc-karpenter-nodepool's selection engine; `sc-karpenter-nodepool` switched to it; `/table/*` endpoints | 00 |
| 02 | `done/02-scaffold.md` | done | dev container, module, multi-vendor `SCNodeClass` API + CRD, operator wiring, CI | 01 |
| 03 | `done/03-catalog.md` | done | Keeper → instance types and offerings; attribute, metric, selection, location and profile labels; refresh; unavailable cache | 02 |
| 04 | `done/04-cloudprovider-and-fake-driver.md` | done | CloudProvider, driver contract + registry + conformance, quota tracker, fake driver, GC, L3 KWOK lane, scale measurement | 03 |
| 05 | `done/05-bootstrap.md` | done | tokens, artifacts, templates → Ignition, NodeClass controller, CSR approver | 04 |
| 06 | `done/06-node-options-and-resources.md` | done | settings resolution and profiles, vendor resources (use/create, lifecycles, GC), pricing deltas, first-boot volume unit; fake driver + L3 scenarios | 05 |
| 07 | `done/07-qemu-driver.md` | done | qemu driver, L4 lane with real Flatcar VMs, k3s and a data volume | 06 |
| 08 | `done/08-aws-driver.md` | done | AWS driver incl. quotas, storage/placement/network profiles and resources (in-memory EC2 + Service Quotas fake), IAM policy | 06 |
| 09 | `done/09-packaging.md` | done | Helm chart, release workflow, install and credentials how-tos, controller reference | 08 |
| 10 | `done/10-aws-e2e.md` | done | L5 AWS e2e over Tailscale + sweeper (first green live run 2026-11-10) | 09 |
| 11 | `wip/11-docs-and-release.md` | todo | tutorials, how-tos for profiles, selection and resources, add-a-vendor guide, first release (v0.1.0) | 07, 10 |
| 12 | `done/12-gpu.md` | done (real runs verify the types) | NVIDIA GPU nodes on every vendor with GPU types (AWS, GCP, Azure, Alibaba, OVH, then UpCloud): support table by model and Flatcar release, catalog drops, `nvidia.com/gpu` capacity and driver labels, Flatcar's prebuilt driver sysext and the `nvidia-runtime` sysext (runc stays the default), optional mirror, device plugin and RuntimeClass in the chart, L4 boot without a GPU, Fabric Manager for 8-GPU NVSwitch types, manual-only `gpu` scenario (no live GPU gate; real runs verify); eleven steps | 14, 13, 06, the vendor drivers; a GPU spike per vendor |
| 13 | `done/13-keeper-catalog-snapshot.md` | done | catalog, vendor storage and IPv4 prices from Keeper snapshots pinned per data version, with local selection; API key handling; three steps: sc-keeper-go, part A, part B | 03, 14–21, 23 |
| 14 | `done/14-multi-vendor-groundwork.md` | done | shared core and API changes the vendor plans need (the `images` kind and `ImageRequest`, per-type root disk, idempotency label, stopped-node rule, account-wide quota errors, message-based classification, Butane hook, region aliases, malformed references, the one `use` selector of ADR-023), the published hand-off contract (IaC-neutral), CRD schema and examples, and the resource resolution order of ADR-022 | 10 |
| 15 | `done/15-hcloud.md` | done (L5 lane green live 2026-11-22, `hcloud` in `test/e2e-vendors.txt`; open: billing check and the unverified facts in its "Open" list) | Hetzner Cloud driver, fake, conformance, L5 lane; images on rung 3 | 14 |
| 16 | `done/16-gcp.md` | done (live: k8s-inspector benchmark pods on the frankenscube cluster since 2026-11-21; open: the L5 lane is not in `test/e2e-vendors.txt`, and its "Open" list) | GCP driver, fake, conformance, L5 lane; amd64 on rung 1, arm64 on rung 2 | 14, 22 (arm64) |
| 17 | `done/17-azure.md` | done (live: k8s-inspector benchmark pods on the frankenscube cluster since 2026-11-21; open: the L5 lane is not in `test/e2e-vendors.txt`, and its "Open" list) | Azure driver, fake, conformance, L5 lane; images on rung 1 | 14 |
| 18 | `done/18-ovh.md` | done (live: k8s-inspector benchmark pods on the frankenscube cluster since 2026-11-21; open: the L5 lane is not in `test/e2e-vendors.txt`, and its "Open" list) | OVHcloud Public Cloud driver (OpenStack), fake, conformance, L5 lane; images on rung 3 | 14 |
| 19 | `done/19-upcloud.md` | done (live: k8s-inspector benchmark pods on the frankenscube cluster since 2026-11-21; open: the L5 lane is not in `test/e2e-vendors.txt`, and its "Open" list) | UpCloud driver, fake, conformance, L5 lane; images on rung 3 from the images repository | 14, 22 |
| 20 | `done/20-vultr.md` | done (live: k8s-inspector benchmark pods on the frankenscube cluster since 2026-11-21; open: the L5 lane is not in `test/e2e-vendors.txt`, and its "Open" list) | Vultr driver, fake, conformance, L5 lane; images on rung 3 from the images repository | 14, 22 |
| 21 | `done/21-alicloud.md` | done (live: k8s-inspector benchmark pods on the frankenscube cluster since 2026-11-21; open: the L5 lane is not in `test/e2e-vendors.txt`, and its "Open" list) | Alibaba Cloud driver, fake, conformance, L5 lane; images on rung 3 from the images repository | 14, 22 |
| 22 | `wip/22-flatcar-images.md` | todo | the Flatcar images repository (proposed `SpareCores/flatcar-images`): OEM-patched images for vultr, upcloud and aliyun, the GCP arm64 image, signatures, release tracking, upstream proposals | — |
| 23 | `done/23-billing-retention.md` | done | opt-in `spec.billing`: keep period-billed nodes (hcloud first, then UpCloud, Vultr; OVH US if hourly; Alibaba until the end of its minimum) until the end of their paid period and reuse them; Keeper vendor billing facts, `BillingStart`, the retention controller, metrics, fake-clock tests | 14 and the Keeper vendor table (live; plan 13 is not needed) |
| 25 | `done/25-oci.md` | done (2026-11-15: the live lane is green, `oci` is in `test/e2e-vendors.txt`) | Oracle Cloud Infrastructure driver, fake, conformance, L5 lane; fixed named sizes per flexible shape (`VM.Standard.E5.Flex.2-16`), an embedded OCI catalog (Keeper did not serve OCI yet; swapped to Keeper 2026-11-17) behind the `catalog.Source` seam, images on rung 3 from Flatcar's own OCI image, managed VCN, subnet and network security groups; five steps: catalog data and rule R, driver foundation, embedded source, full driver and surfaces, L5 | 14, 23, 13 (part B, for the embedded-source step) |
| 26 | `done/26-interruption.md` | done (steps 1 to 9; live verification open: spikes S2 to S5 and the first manual `interruption` run per vendor, see its "Open" list; UpCloud left out) | spot and preemptible interruption handling: the interruption controller (tightened termination deadline, `SpotInterrupted` condition, Events and metric, unavailable spot offering for a TTL, NodeClaim delete), an OCI API poller, a node agent script in the ADR-025 metadata slice for AWS, GCP, Azure and Alibaba, reasons for silent reclaims; vendor order OCI, AWS, GCP, Azure, Alibaba; nine steps | 04, 05, 07, 14, 23, 25; [ADR-025](../../explanation/DECISIONS.md#adr-025-instance-metadata-reaches-only-the-nodes-own-components-accepted-2026-11-19) for the agent steps |
| 27 | `wip/27-tailnet-keys.md` | in progress (steps 1 and 2 of 6 done: API, flags, chart values, NodeClass validation, ADR-028; the `pkg/tailnet` Tailscale and Headscale backends and their fakes) | per-node tailnet auth keys: `network.tailscale.keySource: PerNode`, keys minted by the controller at launch (single use, pre-authorised, tagged, ephemeral, 1 h), recorded by ID on the NodeClaim and never stored, revoked at registration, the tailnet device deleted with the NodeClaim; Tailscale and Headscale (0.28+) backends behind one interface, controller-wide backend and credentials; static keys unchanged (no drift); six steps | 05, 07, 09, 14; [ADR-025](../../explanation/DECISIONS.md#adr-025-instance-metadata-reaches-only-the-nodes-own-components-accepted-2026-11-19) |
| 28 | `done/28-offered-types.md` | done (steps 1 to 5; the optional live check of step 6 was not run) | offered types: one reason taxonomy for every place a type or offering is dropped or marked unavailable, `Catalog.Explain` over the same path as `InstanceTypes`, a V(1) transition log, a bounded status summary per vendor, an opt-in per-class ConfigMap listing, a small fixed metric set (per-offering gauges behind a flag); six steps | 03, 04, 06, 14, 25 |
| 29 | `wip/29-image-copies.md` | todo | opt-in Flatcar image copies into regions without one, on demand: a vendor-neutral copy manager and GC (idle TTL, primary image appeared, release unpinned), both architectures per region, copies ahead of a release bump, per-cluster tags, `ImageCopyOnDemand`/`ImageCopying` reasons through the existing image gate, metrics, ADR-032 amending ADR-021; AWS first (`CopyImage`, a manual L5 the coordinator runs), then Alibaba, a distinct Event while copying, OCI and UpCloud after the coordinator's spikes; eight steps | 08, 14, 21, 28; ADR-030 |

Shared rules for every plan: [`CONVENTIONS.md`](CONVENTIONS.md).

## Further vendors

More vendors follow AWS as their own plans (15 to 21, and 25), each one driver, an in-memory
fake, conformance and an opt-in e2e workflow, all on the shared groundwork of plan 14.
Where a vendor's Flatcar image comes from is decided by the image ladder of
[ADR-021](../../explanation/DECISIONS.md#adr-021-flatcar-images-the-image-ladder-accepted-2026-11-10)
([per vendor](../../explanation/flatcar-images.md)): the vendor's own
public image (AWS, Azure, GCP amd64), Spare Cores' published image (GCP arm64),
or an image created in the user's account by infrastructure as code (hcloud, OVH,
UpCloud, Vultr, Alibaba, OCI). No image-import plan precedes any vendor: SCarpenter does not
import images, and `SCImage` and `ImageImporter` stay reserved.

Order:

1. **14**, the groundwork.
2. **16 (GCP) and 17 (Azure)** first: rung 1, no image work. GCP's arm64 images wait
   for plan 22.
3. **15 (Hetzner)**: rung 3 with Flatcar's own image.
4. **18 to 21** (OVH, UpCloud, Vultr, Alibaba); the order among them is open.
5. **25 (OCI)**: rung 3 with Flatcar's own OCI image, no wait for 22; its catalog was embedded
   (Keeper did not serve OCI then; it does since 2026-11-17) and its sizes are fixed named sizes per flexible shape.

**23**, billing-period retention, needs 14 only (its Keeper data is the live vendor table, not
plan 13's snapshots) and is fully testable on the fake driver; it can be built as soon as 14 is done,
before or alongside the vendor plans. Each of 15, 18, 19, 20 and 21 then turns it on for its vendor once its
live billing check passes, **Hetzner (15) first**; Alibaba qualifies only for the minimum of a minimum-then-per-second
rule, and AWS, GCP and Azure (per second or minute, no worthwhile minimum) never use it.

**22**, the images repository, is built before GCP arm64 and before 19, 20 and 21
(OVH and Hetzner use Flatcar's own images and do not wait for it). It is a separate
repository with its own milestones, so it can start as soon as the maintainer confirms
its name.

## Status of the vendor plans 15 to 21

All seven are in `done/`. Plan 15 (Hetzner), like plan 25 (OCI), has an L5 lane that ran green live and is listed in
`test/e2e-vendors.txt`. Plans 16 to 21 count as verified live by production use: nodes they launch have run
k8s-inspector benchmark pods on the frankenscube cluster since 2026-11-21. Their L5 lanes have not run green and are
not in `test/e2e-vendors.txt` until their CI cost guards are verified; each plan's Outcome lists what is still open.
(Plan 08 went to `done/` because its live run was a separate plan, 10.)

## Follow-ups found in the vendor reviews

Not part of any open plan; each needs an owner before the first release.

- (a) Done (40bb0d9): the shared conformance suite runs each resolution-order and selector case whenever its
  preconditions hold, by name for kinds without tags (GCP, Vultr, OVH and Alibaba deployment sets now run them;
  so do the Hetzner placement groups, the UpCloud server groups and the qemu bridges).
- (b) Vultr: the VPC interface of a Flatcar node gets no address (VPCs have no DHCP and Flatcar has no cloud-init to
  configure it from the metadata service), so node-to-node traffic and the `direct` template's `--node-ip` stay on the
  public address. A vendor fragment that configures the interface from the metadata would let the driver report the
  VPC range as `Prepared.NodeIPCIDR` ([node bootstrap](../../reference/node-bootstrap.md#the-node-ip-on-a-private-network));
  Hetzner and UpCloud (DHCP networks) report it already.
- (c) Alibaba: `InsufficientCapacity` after ECS releases an accepted instance never marks the offering unavailable
  (the core does not consume it from `GetInstance`).
- (d) The core half of A8 is still needed: a `vendors.Region` field for Keeper's region name, once OVH's catalog
  names differ from Keeper's.
