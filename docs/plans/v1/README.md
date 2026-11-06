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
| 04 | `wip/04-cloudprovider-and-fake-driver.md` | review | CloudProvider, driver contract + registry + conformance, quota tracker, fake driver, GC, L3 KWOK lane, scale measurement | 03 |
| 05 | `wip/05-bootstrap.md` | todo | tokens, artifacts, templates → Ignition, NodeClass controller, CSR approver | 04 |
| 06 | `wip/06-node-options-and-resources.md` | todo | settings resolution and profiles, vendor resources (use/create, lifecycles, GC), pricing deltas, first-boot volume unit; fake driver + L3 scenarios | 05 |
| 07 | `wip/07-qemu-driver.md` | todo | qemu driver, L4 lane with real Flatcar VMs, k3s and a data volume | 06 |
| 08 | `wip/08-aws-driver.md` | todo | AWS driver incl. quotas, storage/placement/network profiles and resources (in-memory EC2 + Service Quotas fake), IAM policy | 06 |
| 09 | `wip/09-packaging.md` | todo | Helm chart, release workflow, install and credentials how-tos, controller reference | 08 |
| 10 | `wip/10-aws-e2e.md` | todo | L5 AWS e2e over Tailscale + sweeper (written and validated, run by the coordinator) | 09 |
| 11 | `wip/11-docs-and-release.md` | todo | tutorials, how-tos for profiles, selection and resources, add-a-vendor guide, first release (v0.1.0) | 07, 10 |
| 12 | `wip/12-aws-gpu.md` | optional | NVIDIA sysext on AWS GPU types | 10 + GPU spike |
| 13 | `wip/13-keeper-catalog-snapshot.md` | proposal | per-vendor price snapshot from Keeper; hourly catalog reads it; on-disk price file replaced per hash. Not next, and not a dependency of 04–12 | 03 |

Shared rules for every plan: [`CONVENTIONS.md`](CONVENTIONS.md).

## Further vendors

More vendors are planned for v1 after AWS; their plans are added to the table
above as they are written. Each further vendor is one plan: driver + in-memory fake + conformance + an
opt-in e2e workflow. Vendors without official Flatcar images (hcloud first)
are preceded by one plan for the image-import machinery (`SCImage` CRD,
`ImageImporter`, image GC; overview §9).
