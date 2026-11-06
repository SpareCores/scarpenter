# 05 — Node bootstrap

Depends on: 04. Read `docs/plans/v1/CONVENTIONS.md` first.
Spec: overview §6.2–§6.3, §8 (all), §15 Q2–Q3.

## Goal

Everything a Flatcar node needs to join, rendered deterministically and
tested offline: bootstrap tokens, artifact resolution, the Ignition user-data
from embedded Butane templates, the NodeClass controller that makes a
NodeClass `Ready`, and the serving-CSR approver. Replaces plan 04's stubs.

## Scope

- **`pkg/artifacts`**: URL building (overview §8.4), `SHA256SUMS` fetching
  and parsing with an HTTP cache keyed by URL, configurable bases; Flatcar
  release file URLs (for plan 07). Tests against an `httptest` server with
  fixture `SHA256SUMS` files (copy real ones for `kubernetes-v1.37.1` and
  `tailscale-v1.98.10` into `testdata/`, downloaded once — public artifact
  access is allowed).
- **`pkg/bootstrap`**:
  - Token minting (§8.1): random IDs from `crypto/rand`, Secret shape,
    labels; the `bootstraptoken` controller deleting tokens of registered or
    missing NodeClaims.
  - Rendering (§8.2): templates `base.bu`, `tailscale.bu`, `direct.bu`
    embedded with `embed`; `text/template` with the variables of §8.2 (strict:
    missing keys are errors); Butane translation with the library (`flatcar`
    variant, strict); the outer Ignition config with gzip `data:` URL merges;
    custom templates from ConfigMaps; extra Butane from `spec.butane`; extra
    sysexts.
  - kubelet configuration (`KubeletConfiguration` YAML): `serverTLSBootstrap:
    true`, `cgroupDriver: systemd`, the NodeClass kubelet settings, defaults
    documented; kubelet unit drop-in with `--provider-id`,
    `--hostname-override`, `--register-with-taints`, `--node-ip` (from the
    network template's env file).
  - **§15 Q2**: download the `kubernetes-v1.37.1-x86-64.raw` sysext once,
    inspect it (`unsquashfs -l` or loop mount in the dev container) to see
    whether it ships `kubelet.service` and with which `ExecStart`/env
    contract; design the drop-in accordingly and record the finding.
  - **§15 Q3**: check Flatcar's Ignition version for gzip `data:` URL merges
    and the supported file hash algorithms (Ignition and Butane docs at the
    versions Flatcar 4757.2.3 ships); if gzip merges are unsupported, fall
    back to a single translated config whose largest files use gzip `data:`
    contents, and record it.
  - Size budget: golden tests for every default template × arch, asserting
    ≤ 12 KiB encoded; a test that a NodeClass with a 20 KiB custom snippet
    yields a clear error.
  - Wire the renderer and minter into `Create` (render once per arch per
    Create, §4.1).
- **`pkg/controllers/nodeclass`**: resolve artifacts per arch into status,
  Kubernetes version skew check against the API server version, vendor
  readiness (registry + `NodeClassValidator`), compute and publish the drift
  hash (§6.3, with `hashVersion`), set conditions (`ArtifactsReady`,
  `VendorReady`, `CatalogReady` from the catalog, `Ready`); image resolution
  is a hook the driver fills (plan 08 for AWS; fake: always ready).
- **`pkg/controllers/csr`**: the approver of §8.5 step 3, with the full rule
  set and envtest cases (wrong user, unknown node, provider ID mismatch, extra
  SAN, IP SAN not in addresses, non-serving signer — all denied or ignored,
  never approved).
- **RBAC** (kustomize `config/rbac`, later the chart): the three
  ClusterRoleBindings of §8.5, SCarpenter's own permissions including Secrets
  in `kube-system` (create/delete/list, label-selected where possible) and
  CSR approval (`certificatesigningrequests/approval`, `signers` with
  `resourceNames: [kubernetes.io/kubelet-serving]`).
- **Docs**: `docs/reference/node-bootstrap.md` (rendered layout, variables,
  templates, size budget, prerequisites, security notes of §8.6),
  `docs/how-to-guides/choose-a-network-template.md`.

## Carried over from the plan 04 review

- Remove the dev status hook (`cmd/controller/dev.go`, field manager
  `scarpenter-dev-status`); the NodeClass controller replaces it, it must not
  run alongside it. Write status with server-side apply or update-with-retry.
- The token minter is idempotent per NodeClaim: a retried `Create` gets the
  existing token, never a rotated one (plan 04 keeps the token on `Retryable`
  errors), and the token TTL outlives core's retries of one NodeClaim.
- Emit Events for bootstrap-token deletions (GC and registration).
- A vendor that is not ready in a multi-vendor NodeClass must not turn
  failures of another vendor's NodeClaim into `NodeClassNotReady` (plan 04
  N5 makes the reason misleading); scope readiness to the NodeClaim's vendor.

## Gates

CONVENTIONS gates 1–4. Golden files reviewed. Report Q2 and Q3 findings.
