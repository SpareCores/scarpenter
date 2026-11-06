# 02 — Scaffold: dev container, module, `SCNodeClass` API, operator, CI

Depends on: 01. Read `docs/plans/v1/CONVENTIONS.md` first.
Spec: overview §3.1, §4 (wiring only), §6, §12; 00a §4–§9 (API shape only).

## Goal

A buildable, linted, tested repository: the dev container and tooling ported
from `sc-karpenter-nodepool`, the `SCNodeClass` API with its CRD, and a
controller binary that starts Karpenter core's operator with a `CloudProvider`
stub that supports no instance types yet.

## Scope

- **Tooling** (port from `sc-karpenter-nodepool` at
  `d715fb48935e2f1721eb5c6888ed14755aee916c`, adapting names):
  `hack/devcontainer/Dockerfile` (add k3s, kwok, qemu-system-x86/aarch64 +
  qemu-utils, butane CLI, iproute2, iptables, dnsmasq; keep versions pinned),
  `hack/dev.sh` (add the `--privileged` mode of CONVENTIONS), `Makefile`
  (`generate`, `manifests`, `lint`, `test`, `golden`, `build`, `image`;
  placeholders for `test-kwok`, `test-qemu`), `.golangci.yml`,
  `hack/boilerplate.go.txt` (MPL-2.0), `hack/check-docs.py` (link checker,
  adapted), `.github/workflows/ci.yml` (generate/manifests diff, lint, test;
  dev image built once per run as in the source repository).
- **Module**: `go.mod` with Karpenter core v1.15.x, controller-runtime and
  k8s libraries at the versions core v1.15 requires, `sc-keeper-go` (replace
  directive to `../sc-keeper-go` until it is tagged, as in plan 01), butane.
- **API** (`pkg/apis/v1alpha1`): the multi-vendor `SCNodeClass` exactly as
  overview §6 and 00a §4: shared spec, `networking`, `pricing`, `catalog`,
  `metrics`, `selections` (embedding `sc-keeper-go`'s `Selector`/`Ranking`
  types), `profiles` (storage, network, placement; one block per vendor),
  `vendors` (one typed block per vendor, each with `regions`, settings and
  `resources` kinds with `use` | `create` | `forbid`), status per vendor
  (conditions via `operatorpkg` status like other providers, hash, images,
  resources, quota), CEL validation of §6.1, printer columns (vendors, ready,
  age), kubebuilder markers, deepcopy, generated CRD under
  `config/crd/` and copied to `charts/scarpenter/crds/` by a sync script.
  Label and annotation constants of overview §5.5 and §4.1; registration of
  the `scarpenter.sparecores.com/*` labels as well-known with core in
  `init()`. Vendor block and profile block types live in
  `pkg/apis/v1alpha1/<vendor>` (they are part of the API); this plan defines
  the `aws`, `fake` and `qemu` blocks with the fields of overview §6 and 00a
  §6.1 (validated by the drivers later); other vendors' blocks are added by
  their driver plans.
- **Operator** (`pkg/operator`, `cmd/controller/main.go`): flags and env
  fallbacks of overview §12 (parsed and validated; unused ones accepted), core
  operator start, a `CloudProvider` stub in `pkg/cloudprovider` whose methods
  return empty results or "not implemented" errors, `GetSupportedNodeClasses`
  returning `SCNodeClass`.
- **Tests**: CRD admission tests in envtest (valid examples, every CEL rule
  negative case), flag parsing tests, a smoke test that the manager starts in
  envtest with the stub provider.
- **Docs**: `docs/how-to-guides/testing.md` (dev container, gates, lanes as
  they appear), `docs/reference/api.md` generated from the CRD (port
  `hack/apidocs` from the source repository), update the reference and
  how-to READMEs.

## Gates

CONVENTIONS gates 1–3, plus: `hack/dev.sh make build` produces
`bin/scarpenter-controller`; `hack/dev.sh make image` builds a multi-arch
capable image (amd64 build is enough locally); the CI workflow passes
`actionlint`.
