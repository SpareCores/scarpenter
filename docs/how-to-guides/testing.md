# Test and develop

How to build, test and lint SCarpenter, and what the test lanes are. Every
command runs in the dev container; the host only needs Docker (and `/dev/kvm`
for the qemu lane that arrives with plan 07).

## The dev container

`hack/devcontainer/Dockerfile` defines every tool with a pinned version: Go,
controller-gen, setup-envtest with the control-plane assets baked in,
golangci-lint, kind, kubectl, Helm, kwok, k3s, qemu (x86_64 and aarch64
system emulators, `qemu-img`, UEFI firmware), butane, iproute2, iptables,
dnsmasq, actionlint, shellcheck, yq, jq, python3 and the Docker CLI with
buildx. Versions are `ARG`s at the top of the file; bump them there and
nowhere else. The Go version must be at least the one Karpenter core's
`go.mod` asks for (Go refuses to build a dependency that needs a newer
toolchain, and the container sets `GOTOOLCHAIN=local`).

`hack/dev.sh <command...>` runs a command in it as the calling user:

```bash
hack/dev.sh make test
hack/dev.sh make lint
hack/dev.sh go test ./pkg/apis/...
hack/dev.sh                      # an interactive shell
```

- The repository is mounted at `/src`; a named Docker volume
  (`scarpenter-dev-cache`) holds the Go module and build caches, so repeated
  runs are fast.
- The image is built on first use and rebuilt when the Dockerfile changes (the
  tag is a hash of the Dockerfile). `make dev-image` only builds it.
- The container uses `--network host` and mounts the Docker socket, so it can
  run kind clusters and `docker build`.
- `hack/dev.sh --privileged <command...>` adds `/dev/kvm` (when the host has
  it), `/dev/net/tun` and the `NET_ADMIN` and `NET_RAW` capabilities, and runs in
  a private network namespace instead of the host's. It is for the L4 lane: VMs,
  bridges, dnsmasq and iptables rules then never touch the host network. Docker
  grants capabilities only to root, so the container starts as root and the
  command drops to your uid and gid with the network capabilities kept as
  ambient capabilities (`setpriv`). `hack/dev.sh --privileged make
  dev-check-privileged` proves it: it creates and deletes a bridge and checks
  that `/dev/kvm` is usable.
- With a temporary `replace github.com/SpareCores/sc-keeper-go => ../sc-keeper-go`
  in `go.mod` (developing both together), the sibling checkout is mounted
  read-only at `/sc-keeper-go`.
- Settings of the local lanes (`KWOK_SCENARIOS`, `KEEP_CLUSTER`, ...) pass
  through from your environment when set.

Environment variables: `SCARPENTER_DEV_IMAGE_REPO` and
`SCARPENTER_DEV_CACHE_VOLUME` rename the image and the cache volume. Inside the
container `SCARPENTER_DEV_CONTAINER=1` is set, which the lane scripts check.

## The gates

A change is done when all of these pass, in this order
([`CONVENTIONS.md`](../plans/v1/CONVENTIONS.md) is the authority):

| # | Command | What it does |
|---|---|---|
| 1 | `hack/dev.sh make generate manifests chart-crds docs-api` | Regenerates DeepCopy code, the CRD (`config/crd/bases`), the chart's copy of it (`charts/scarpenter/crds`) and [`docs/reference/api.md`](../reference/api.md). CI fails if this changes anything, so commit the result. |
| 2 | `hack/dev.sh make lint` | `go vet` (with every build tag), golangci-lint, `actionlint`, `shellcheck` on `hack/` and `test/` scripts, and `hack/check-docs.py` (relative links and anchors of all Markdown files). |
| 3 | `hack/dev.sh make test` | L1 and L2: unit, golden and envtest suites, built with `-tags scarpenter_dev` so the dev-driver code is compiled and tested. No network access: Keeper calls go to the snapshot replay server, artifact fetches to local fixtures. |
| 4 | `hack/dev.sh make test-kwok` | L3: the controller with the fake driver in kind (see [The L3 lane](#the-l3-lane)). |
| 5 | `hack/dev.sh --privileged make test-qemu` | L4, from plan 07 on, when a change touches bootstrap, the qemu driver or `test/qemu`. |

Also: `hack/dev.sh make build` produces `bin/scarpenter-controller`, and
`hack/dev.sh make image` builds the controller image for the host architecture
(the Dockerfile cross-compiles for `TARGETARCH`; `make image-buildx` builds and
pushes `linux/amd64,linux/arm64`). `make help` lists every target.

`make test-qemu` is a placeholder until plan 07 lands; it fails on purpose so
that a green run never means "skipped".

## Test lanes

| Lane | Runs | What it proves | Status |
|---|---|---|---|
| L1 | `make test` | Unit and golden tests: pure packages with no Kubernetes client. | Started in plan 02 (flag parsing, labels; plan 04: the CloudProvider, quota tracker, garbage collector and the driver conformance suite against the fake driver). |
| L2 | `make test` | envtest: an API server and etcd (no kubelet), with the generated CRDs and Karpenter core's CRDs installed. | CRD admission tests (schema, defaults, every CEL rule, strict field validation) and a smoke test that starts the real manager with the production wiring (the NodeClass controller resolving sysext checksums from a local release server) and has core resolve an `SCNodeClass`; the NodeClass, quota status, serving certificate and bootstrap token controllers. |
| L3 | `make test-kwok` | Real Karpenter core, SCarpenter and the fake driver (KWOK nodes) in kind. | Plans 04 and 05: eleven scenarios, [The L3 lane](#the-l3-lane). |
| L4 | `make test-qemu` | The qemu driver: real Flatcar VMs, a k3s control plane. Needs `--privileged`. | Plan 07. |
| L5 | a manual GitHub workflow | Real AWS over Tailscale. Never run from the dev container, never by a contributor's tooling. | Plan 10. |

The design of the layers is in
[the overview](../plans/v1/wip/00-overview.md#13-testing) and ADR-010.

### The Keeper snapshot

`test/data/sc-snapshot/` holds recorded responses of the public Keeper API: the
region, zone, server and benchmark config tables, `/server_prices` of **every
active AWS region** (one region per request, so any subset replays) and of one
region of hcloud and gcp, and the benchmark scores the catalog tests use. The
query list is `test/data/sc-snapshot/queries.yaml`; the files are written by
`sc-keeper-go`'s fetcher and replayed by its `keepertest` server, through
`pkg/catalog/catalogtest`:

```go
src := catalogtest.Source(t)           // a Keeper source reading the replay server
facts := catalogtest.Facts(t, "aws")   // driver facts from the snapshot's zone table
```

Tests never reach the network. Refresh the snapshot only when the data should
change, with the manual workflow "Refresh Spare Cores snapshot" (or
`hack/dev.sh make snapshot`): it downloads about 1 GB, so run it rarely and not
while iterating. The committed snapshot is about 4.4 MB.

### The L3 lane

`hack/dev.sh make test-kwok` builds the controller with `-tags scarpenter_dev`
(real Karpenter core as a library plus the fake driver), a Keeper replay server
that serves the committed snapshot and, under `/sysext`, the `SHA256SUMS` fixtures
of `pkg/artifacts/testdata` (the controller's `--sysext-base-url` points there, so
the lane needs no access to GitHub), creates a kind cluster, installs
[kwok-controller](https://kwok.sigs.k8s.io) (the version is `KWOK_VERSION` in
`test/kwok/run.sh`; the release files are downloaded once into
`test/kwok/.cache`), the CRDs and the manifests of `test/kwok/manifests`, and
runs the scenarios of `test/kwok/e2e` (build tag `kwok`). The fake driver
creates a Node object for every instance; kwok-controller makes it Ready and
runs the pods on it. The lane has its own RBAC in
`test/kwok/manifests/controller/rbac.yaml` (core's permissions and the fake
driver's); SCarpenter's own permissions are the production ones, `config/rbac`,
which the lane applies, so a permission missing there fails the lane. The kind
cluster runs without the controller manager's taint-eviction controller
(`test/kwok/kind.yaml`): KWOK nodes are Ready at once, and that controller would
otherwise evict a pod bound within milliseconds of the removal of the
unregistered taint.

Unlike the lane of `sc-karpenter-nodepool` there is no core `kwok` image, no
core version matrix and no chart values: SCarpenter links core at the version of
`go.mod`, so core's settings are controller flags in
`manifests/controller/deployment.yaml` (batching windows 5 s and 500 ms,
NodeRepair and the other optional feature gates off). The controller runs with
short intervals: quota refresh 10 s, unavailable-offerings TTL 20 s, garbage
collection every 10 s with a 30 s grace period, managed resources checked every
5 s and unused `perUse` resources kept for 20 s.

Scenarios steer the fake driver through the ConfigMap `scarpenter-fake`
(`errors`, `quota`, `orphans`, `launchDelay`, `unsupported`, `existingResources`)
and read what it did from the ConfigMap `scarpenter-fake-state` (`calls`,
`instances`, `resources`, `revision`); the formats are in the
[driver contract](../reference/driver-contract.md#the-fake-driver). The fake
publishes its state debounced; the scenarios do not sleep for it: they read the
fake's current revision from the controller's metrics
(`scarpenter_fake_state_revision`) and wait until the ConfigMap's `revision` has
reached it (`awaitState`). The scenarios are `TestS01` to `TestS17`:

| Scenario | Proves |
|---|---|
| S01 | an exact `aws.*` type: NodeClaim, Ready node with every label; deleting the pod removes node, NodeClaim and instance |
| S02 | the cheapest offering wins among types, zones and capacity types |
| S03 | ICE falls back to the next offering in one `Create`; a failed `Create` marks offerings unavailable and the next round picks others; the TTL expiry makes them available |
| S04 | quota, flexible region: the exhausted cheapest region gets no vendor call; five pods spread over two regions without a quota error (the ledger) |
| S05 | quota, pinned region: pods stay Pending, no NodeClaim, `QuotaAvailable=False` (`QuotaExhausted`, `QuotaZero`); freed usage provisions without touching the pods (Q9) |
| S06 | a type that is not launchable in a zone is never attempted there |
| S07 | a driver without quota knowledge: injected `QuotaExceeded` skips the region and capacity type for the TTL |
| S08 | a NodeClass change drifts the node and replaces it |
| S09 | an orphan instance is deleted by the garbage collector after the grace period |
| S10 | scale (Q7): 50 pods with distinct exact types, every region vs. three; the numbers go to `test/kwok/.out/q7.txt` |
| S11 | the node bootstrap: the NodeClass resolves its artifacts and publishes a hash; the launch gets user-data within the size budget (the fake reports its size) and a bootstrap token, which is deleted, with an Event, when the NodeClaim registers |
| S12 | exact placement: a Job with vendor, type, region and capacity type gets exactly that offering; hostname anti-affinity and a taint give one pod per node |
| S13 | described, not named: continent, vCPU range and a benchmark percentile select the cheapest matching offering, a selection profile only its members (Q11: `Gt`/`Lt` match numeric requirements, `DoesNotExist` excludes unscored types); needs the NodeClass `l3-described`, applied by `run.sh` before the controller starts |
| S14 | storage profiles: a profile with a block for the vendor is applied (settings, device hints and volume in the fake, labels on the node, price with the volume), a type that cannot host it is never chosen, a profile without a block for the vendor leaves the pod Pending |
| S15 | the pair of 00a section 8.2: a StatefulSet and a Deployment with one placement profile and run key share one fake group in one zone on two nodes; the group is deleted (perUse) after both are gone |
| S16 | vendor resources: `use` by tag selector in two regions (never touched), a missing `use` resource makes the region unavailable with a reason and recovers, default `create` on the first launch and its reuse, adoption after a controller restart, `idleTTL` deletion, `preCreate` kept while listed, `forbid` |
| S17 | incomplete prices: offerings flagged `price-incomplete` and still used; excluded with `pricing.allowIncomplete: false` |

Knobs (environment): `KWOK_SCENARIOS=<go test -run regexp>` selects scenarios
(`KWOK_SCENARIOS=TestS0[12]`), `KEEP_CLUSTER=1` keeps the cluster and
`REUSE_CLUSTER=1` reuses it, `KIND_CLUSTER_NAME` renames it. `make kwok-up`
leaves the lane running (`KUBECONFIG=test/kwok/.cache/scarpenter-kwok.kubeconfig`)
and `make kwok-down` removes it. On failure the logs of the controller,
kwok-controller and the replay server, the NodeClaims and the fake ConfigMaps are
collected in `test/kwok/.out/logs` (CI uploads them).

### Artifact fixtures

`pkg/artifacts/testdata/<name>-<version>/SHA256SUMS` are the real checksum files of
the sysext-bakery releases `kubernetes-v1.37.1` and `tailscale-v1.98.10`,
downloaded once (public, unauthenticated) from
`https://github.com/flatcar/sysext-bakery/releases/download/<release>/SHA256SUMS`.
The artifact tests and the L3 lane serve them; refresh them only when a test
should use another release.

### Conventions

- Tests are table-driven and sit next to the code (`_test.go`). Controllers are
  tested with envtest; the envtest helpers are in `internal/testutil`.
- The envtest binaries (kube-apiserver, etcd) are baked into the image at
  `KUBEBUILDER_ASSETS`; `make test` refuses to run without it.
- Golden files live under `testdata/`. `make golden` rewrites them
  (`UPDATE_GOLDEN=1`); review the diff like code.
- Never weaken a test to make it pass. Never call a live system (cloud APIs,
  Tailscale, any cluster that is not a local kind or k3s one you created).

### Adding a CRD admission case

The cases of `pkg/apis/v1alpha1/crd_test.go` send YAML exactly as a user writes
it, with strict field validation like `kubectl`. A valid case goes into
`TestValidSpecsAreAccepted`; an invalid one into `TestInvalidObjectsAreRejected`
with the substring of the error it must produce, so a rule that stops firing for
the intended reason fails the test. Every CEL rule has at least one negative
case.

## Updating generated files

```bash
hack/dev.sh make generate manifests chart-crds docs-api
```

`docs/reference/api.md` is produced from the CRD by `hack/apidocs`; do not edit
it by hand. The CRD embeds the selector and ranking types of
[`sc-keeper-go`](https://github.com/SpareCores/sc-keeper-go) (package
`selection/api`), which carry `float64` fields, so controller-gen runs with
`crd:allowDangerousTypes=true`.

## Continuous integration

`.github/workflows/ci.yml` runs the gates inside the same dev container:
the dev image is built once per run (with a GitHub Actions layer cache, under
the tag `hack/dev.sh` looks for) and every step reuses it. The workflow checks
that generated files are up to date, lints, tests, and builds the binary and the
image. Run `hack/dev.sh actionlint` to check workflow changes locally.

## Troubleshooting

- *`KUBEBUILDER_ASSETS is not set`*: you ran `make test` on the host; use
  `hack/dev.sh make test`.
- *`go.mod requires go >= ...`*: the dev image's Go is older than a dependency
  needs; bump `FROM golang:` in `hack/devcontainer/Dockerfile` (and the same
  line in the `Dockerfile`).
- *A stale tool after a Dockerfile change*: `hack/dev.sh` rebuilds on a changed
  hash; to force it, remove the image
  (`docker rmi scarpenter-dev:<hash>`).
- *Stale caches*: `docker volume rm scarpenter-dev-cache`.
