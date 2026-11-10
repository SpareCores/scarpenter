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
| 00a | `wip/00a-node-options.md` | accepted | vendor-native node settings and profiles, instance selection (attribute and metric labels, sc-karpenter-nodepool selection profiles), vendor resources (bring your own or managed, with lifecycles), multi-vendor NodeClass, workload interface | 00 |
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
| 12 | `wip/12-aws-gpu.md` | optional | NVIDIA sysext on AWS GPU types | 10 + GPU spike |
| 13 | `wip/13-keeper-catalog-snapshot.md` | blocked | catalog from Keeper snapshots pinned per data version, with local selection; waits for Keeper's snapshot routes | 03 + Keeper snapshots |
| 14 | `wip/14-multi-vendor-groundwork.md` | todo | shared core and API changes the vendor plans need (the `images` kind and `ImageRequest`, per-type root disk, idempotency label, stopped-node rule, account-wide quota errors, message-based classification, Butane hook, region aliases, malformed references, the one `use` selector of ADR-023), the published hand-off contract (IaC-neutral), CRD schema and examples, and the resource resolution order of ADR-022 | 10 |
| 15 | `wip/15-hcloud.md` | todo | Hetzner Cloud driver, fake, conformance, L5 lane; images on rung 3 | 14 |
| 16 | `wip/16-gcp.md` | todo | GCP driver, fake, conformance, L5 lane; amd64 on rung 1, arm64 on rung 2 | 14, 22 (arm64) |
| 17 | `wip/17-azure.md` | todo | Azure driver, fake, conformance, L5 lane; images on rung 1 | 14 |
| 18 | `wip/18-ovh.md` | todo | OVHcloud Public Cloud driver (OpenStack), fake, conformance, L5 lane; images on rung 3 | 14 |
| 19 | `wip/19-upcloud.md` | todo | UpCloud driver, fake, conformance, L5 lane; images on rung 3 from the images repository | 14, 22 |
| 20 | `wip/20-vultr.md` | todo | Vultr driver, fake, conformance, L5 lane; images on rung 3 from the images repository | 14, 22 |
| 21 | `wip/21-alicloud.md` | todo | Alibaba Cloud driver, fake, conformance, L5 lane; images on rung 3 from the images repository | 14, 22 |
| 22 | `wip/22-flatcar-images.md` | todo | the Flatcar images repository (proposed `SpareCores/flatcar-images`): OEM-patched images for vultr, upcloud and aliyun, the GCP arm64 image, signatures, release tracking, upstream proposals | — |
| 23 | `wip/23-billing-retention.md` | todo | opt-in `spec.billing`: keep period-billed nodes (hcloud, UpCloud, Vultr; OVH where hourly) until the end of their paid period and reuse them; Keeper vendor billing facts, `BillingStart`, the retention controller, metrics, fake-clock tests | 14 and the Keeper vendor table (live; plan 13 is not needed) |

Shared rules for every plan: [`CONVENTIONS.md`](CONVENTIONS.md).

## Further vendors

More vendors follow AWS as their own plans (15 to 21), each one driver, an in-memory
fake, conformance and an opt-in e2e workflow, all on the shared groundwork of plan 14.
Where a vendor's Flatcar image comes from is decided by the image ladder of
[ADR-021](../../explanation/DECISIONS.md#adr-021-flatcar-images-the-image-ladder-accepted-2026-11-10)
([per vendor](../../explanation/flatcar-images.md)): the vendor's own
public image (AWS, Azure, GCP amd64), Spare Cores' published image (GCP arm64),
or an image created in the user's account by infrastructure as code (hcloud, OVH,
UpCloud, Vultr, Alibaba). No image-import plan precedes any vendor: SCarpenter does not
import images, and `SCImage` and `ImageImporter` stay reserved.

Order:

1. **14**, the groundwork.
2. **16 (GCP) and 17 (Azure)** first: rung 1, no image work. GCP's arm64 images wait
   for plan 22.
3. **15 (Hetzner)**: rung 3 with Flatcar's own image.
4. **18 to 21** (OVH, UpCloud, Vultr, Alibaba); the order among them is open.

**23**, billing-period retention, needs 14 only (its Keeper data is the live vendor table, not
plan 13's snapshots) and is fully testable on the fake driver; it can be built as soon as 14 is done,
before or alongside the vendor plans. Each of 15, 18, 19 and 20 then turns it on for its vendor once its
live billing check passes (AWS, GCP, Azure and Alibaba bill per second or minute and never use it).

**22**, the images repository, is built before GCP arm64 and before 19, 20 and 21
(OVH and Hetzner use Flatcar's own images and do not wait for it). It is a separate
repository with its own milestones, so it can start as soon as the maintainer confirms
its name.
