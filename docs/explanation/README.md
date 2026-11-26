# Explanation

Understanding-oriented discussion: why SCarpenter exists, how it fits
together, and the decisions that shaped it.

- [Decisions (ADRs)](DECISIONS.md) — what was chosen, what was rejected, and
  why
- [Flatcar images](flatcar-images.md) — the image ladder: where the boot image
  of each vendor comes from, who creates it and how a pinned version finds it
  (ADR-021, researched 2026-11-10)
- [Image copies](image-copies.md) — why SCarpenter copies the boot image into a region only when
  a launch wants it, how a pending copy gates the region without blocking a launch, and when
  a copy is deleted (ADR-032, plan 29)
- [OCI bare metal](oci-bare-metal.md) — running OCI bare metal shapes with the SpareCores Flatcar iSCSI image:
  why the stock image fails, getting the image, operations
- [Vendor capabilities](vendor-capabilities.md) — storage, networking,
  placement, tenancy and location options of the eight vendors, and what the
  Spare Cores data covers (researched 2026-11-05)

The goals, architecture and design of v1 are currently described in the v1
overview plan: [`../plans/v1/wip/00-overview.md`](../plans/v1/wip/00-overview.md).
Once v1 ships, its stable parts move here as an architecture document.
