# Test and develop

How to build, test and lint SCarpenter, and what the test lanes are. Every
command runs in the dev container; the host only needs Docker (and `/dev/kvm`
for the qemu lane).

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
  that `/dev/kvm` is usable. While still root it also makes `/etc/rancher`
  writable (k3s keeps its node password there) and gives your uid a passwd entry
  (ssh refuses to run without one); both die with the container.
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
| 5 | `hack/dev.sh --privileged make test-qemu` | L4: real Flatcar VMs join a k3s control plane (see [The L4 lane](#the-l4-lane)). Run it when a change touches bootstrap, the qemu driver or `test/qemu`. |
| 6 | `hack/dev.sh make test-e2e-aws-dryrun` | The L5 lane's parts that need no AWS: its manifests against an API server, the sweeper against the in-memory EC2, its shell helpers against stubs, the chart with its values (see [The L5 lane](#the-l5-lane)). Part of `make test` for the Go tests; CI runs the whole target. |

Also: `hack/dev.sh make build` produces `bin/scarpenter-controller`, and
`hack/dev.sh make image` builds the controller image for the host architecture
(the Dockerfile cross-compiles for `TARGETARCH`; `make image-buildx` builds and
pushes `linux/amd64,linux/arm64`). `make help` lists every target.

## Test lanes

| Lane | Runs | What it proves | Status |
|---|---|---|---|
| L1 | `make test` | Unit and golden tests: pure packages with no Kubernetes client. | Started in plan 02 (flag parsing, labels; plan 04: the CloudProvider, quota tracker, garbage collector and the driver conformance suite against the fake driver; plan 08: the AWS driver against the in-memory EC2 and Service Quotas of `pkg/vendors/aws/fake`, golden `RunInstances` inputs, the conformance suite over the AWS type list of the snapshot). |
| L2 | `make test` | envtest: an API server and etcd (no kubelet), with the generated CRDs and Karpenter core's CRDs installed. | CRD admission tests (schema, defaults, every CEL rule, strict field validation) and a smoke test that starts the real manager with the production wiring (the NodeClass controller resolving sysext checksums from a local release server) and has core resolve an `SCNodeClass`; the NodeClass, quota status, serving certificate and bootstrap token controllers. |
| Chart | `make test-chart` | The Helm chart without a cluster: lint, golden `helm template` output, the values schema and the template checks against bad values, every controller flag settable and accepted by the controller's own parser, the RBAC and CRDs equal to `config/` and Karpenter core's. | Plan 09: [The chart tests](#the-chart-tests). The install into a cluster is the L3 lane. |
| L3 | `make test-kwok` | Real Karpenter core, SCarpenter (installed from the Helm chart) and the fake driver (KWOK nodes) in kind. | Plans 04 and 05: eleven scenarios, extended to seventeen by plan 06; the chart since plan 09. [The L3 lane](#the-l3-lane). |
| L4 | `make test-qemu` | The qemu driver: real Flatcar VMs, a k3s control plane. Needs `--privileged` and `/dev/kvm`. | Plan 07: [The L4 lane](#the-l4-lane). |
| L5 | `.github/workflows/e2e-aws.yml` (manual and weekly) | Real AWS over Tailscale. Never run from the dev container, never by a contributor's tooling. | Plan 10: [The L5 lane](#the-l5-lane). Has run green live (first green run 2026-11-10, all scenarios pass). |

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

`hack/dev.sh make test-kwok` builds the development image (the release `Dockerfile`
with `GO_TAGS=scarpenter_dev`: real Karpenter core as a library plus the fake
driver), a Keeper replay server that serves the committed snapshot and, under
`/sysext`, the `SHA256SUMS` fixtures of `pkg/artifacts/testdata` (the
controller's `--sysext-base-url` points there, so the lane needs no access to
GitHub), creates a kind cluster, installs
[kwok-controller](https://kwok.sigs.k8s.io) (the version is `KWOK_VERSION` in
`test/kwok/run.sh`; the release files are downloaded once into
`test/kwok/.cache`), **installs the Helm chart** (`charts/scarpenter`, with the
values of `test/kwok/manifests/chart-values.yaml`), and runs the scenarios of
`test/kwok/e2e` (build tag `kwok`). The fake driver creates a Node object for every
instance; kwok-controller makes it Ready and runs the pods on it. The controller
therefore runs with the chart's production RBAC (the fake driver's extra
permissions, ConfigMaps and creating Nodes, are `rbac.extraRules` of the lane's
values), so a permission missing from `config/rbac` or `config/rbac-core` fails the
lane, and `run.sh` fails when the controller logs a 403. The chart is installed in
two steps (without replicas, then with) so that the NodeClass of S13 exists before
the controller starts; `run.sh` also checks what reached the controller: the chart
sets no `--cluster-dns`, so the controller must discover the `kube-dns` Service itself
with the production RBAC (it logs the address), and the kubelet client CA is the one
Helm copied from `kube-system/extension-apiserver-authentication`
(`kubeletClientCA.source: extensionApiserverAuthentication`). The 403 check reads the
current and the previous logs of every controller pod, at start and after the scenarios. The kind cluster runs
without the controller manager's taint-eviction controller
(`test/kwok/kind.yaml`): KWOK nodes are Ready at once, and that controller would
otherwise evict a pod bound within milliseconds of the removal of the
unregistered taint.

Unlike the lane of `sc-karpenter-nodepool` there is no core `kwok` image and no
core version matrix: SCarpenter links core at the version of `go.mod`, so core's
settings are chart values (batching windows 5 s and 500 ms, NodeRepair and the
other optional feature gates off). The controller runs with
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

### The chart tests

`hack/dev.sh make test-chart` (no cluster; `UPDATE=1` rewrites the golden files)
runs `helm lint --strict` for every values file of `test/chart/testdata/values` and
the Go tests of `test/chart` (build tag `chart`):

- **golden** `helm template` output of each values file (`minimal`, `default` with
  web identity, `full` with every value set, `static-keys`, `development`);
- **flags**: the arguments the chart renders go through the controller's own flag
  parsers (core's and SCarpenter's), the flags of all values files together must
  cover every flag of `pkg/operator/options` (a new flag without a chart value fails
  the test), and the `full` values must land in the option each is named for;
- **negative cases**: the schema rejects unknown keys, bad ports, durations, URLs,
  ARNs and names, and the templates refuse a missing cluster name or endpoint, a
  web identity without a role, a token TTL under 20 minutes, no vendor at all, and
  a kubelet CA source without a CA;
- **RBAC drift**: the rendered ClusterRoles, Roles and the node-join bindings are
  compared with `config/rbac` and `config/rbac-core`; no rendered ClusterRole may
  grant Secrets;
- **CRDs**: `crds/` equals `config/crd/bases` and Karpenter core's CRDs at the
  version of `go.mod`;
- the credential modes, the leader election and PodDisruptionBudget switches, the
  extra auth key Secret Roles and the default affinity and security context.

The chart is installed into a cluster by the L3 lane.

### The L4 lane

`hack/dev.sh --privileged make test-qemu` boots real Flatcar VMs with SCarpenter's
rendered Ignition and lets them join a k3s control plane through the whole
Karpenter path: the first time the bootstrap runs on a real operating system.
Requirements: Docker, `/dev/kvm` (without it the VMs run under TCG emulation,
which is too slow to be useful; the lane warns), about 2 GB of disk for the
caches in `test/qemu/.cache` (the Flatcar image, 600 MB, is downloaded once and
kept decompressed, 13 GB virtual but sparse, about 1.4 GB on disk; the Kubernetes
sysext, 113 MB, the tailscale sysext, the Headscale and tailscale binaries, once) and 8 GB of memory for a few
2 to 4 GiB VMs. The container started by `--privileged` has its own network
namespace (see [the dev container](#the-dev-container)), so the lane's bridge,
DHCP server and NAT rules never touch the host.

`test/qemu/run.sh` builds, in that namespace: the controller (`-tags
scarpenter_dev`, with the qemu driver); a bridge `scbr0` (10.77.0.1/24) with
dnsmasq (DHCP and DNS) and NAT; an HTTP mirror of the sysext releases (downloaded
once into the cache, checked against the release's `SHA256SUMS`) that the
controller's `--sysext-base-url` and the VMs use; a k3s server (server only: the
nodes are the VMs) configured like a production k3s for remote workers
(`--flannel-backend=none --disable-kube-proxy --disable-network-policy
--egress-selector-mode=disabled`; bootstrap-token authentication and the
`tokencleaner` controller are k3s defaults); flannel and kube-proxy as
DaemonSets (`test/qemu/manifests`; they reach the API server by address, because
the ClusterIP needs the proxy itself) and the production RBAC of `config/rbac`; and a
Headscale (pinned release, checksum-verified) with an ephemeral pre-authorized key
minted per run, and a `tailscaled` that puts the container itself on the tailnet, so
that the tailscale template runs against a real tailnet with no external network and
no secrets.
SCarpenter runs out of cluster as the ServiceAccount `scarpenter` (a token
kubeconfig), not as the admin, so a permission missing from `config/rbac` fails
the lane. The scenarios (`test/qemu/e2e`, build tag `qemu`) start and kill the
controller themselves and look into the VMs with ssh: the NodeClass's user
Butane, read from a ConfigMap, adds the lane's key.

| Scenario | Proves |
|---|---|
| Q01 | a pending Pod makes a VM boot (gzip `data:` merges of Ignition, the Kubernetes sysext from the mirror, `Upholds=` starting the kubelet, masked `update-engine` and `locksmithd`, the user Butane as third merged config, the kubelet reading the CA file named in its bootstrap kubeconfig), join with its bootstrap token and become Ready; the Pod runs; `kubectl logs` and `exec` work (serving certificate approved, k3s client CA trusted by the kubelet); the bootstrap token Secret is gone; `kube-root-ca.crt` is the k3s server CA; the tools of the volume scripts exist; deleting the Pod removes Node, NodeClaim and VM; the boot-to-Ready time is recorded |
| Q02 | a NodeClass with a custom network template and a storage profile (three data disks, two of one size found by serial pattern and size, one by by-id, ext4, xfs and btrfs, plus the raid0 of the two local disks of `qemu.local`): everything is formatted and mounted on first boot; after a reboot nothing is wiped or created twice (UUIDs), the array keeps its identity, a disk whose filesystem was destroyed gets a new one while its sibling of the same size keeps its data |
| Q03 | a change of the root disk size drifts the NodeClaim; a replacement boots, the old node and VM go, the Pod moves |
| Q04 | SCarpenter killed while the launch is in progress (the VM runs, `Create` has not returned: the driver's `--qemu-launch-delay` holds it there, and the scenario checks the NodeClaim is not launched yet, so the replay of `Create` is certain): after the restart the node is the first launch's, there is one VM per NodeClaim and no orphan process |
| Q05 | an extra sysext that cannot be fetched: Ignition retries in the initrd (the mirror's log shows several requests of the VM, the console never reaches the login prompt), no node registers |
| Q06 | a tailscale auth key Secret in the controller's namespace is read through the namespaced Role of `config/rbac`; a Secret in another namespace is refused with the Role named in the message until the documented Role and RoleBinding exist |
| Q07 | why `--kubelet-client-ca-file` exists: without it the node joins but `kubectl logs` answers 401 on k3s |
| Q08 | a data disk that already carries a foreign filesystem is never overwritten; the volume's mount fails and the kubelet (which requires the mounts) does not start; the node stays reachable and never registers |
| Q09 | the tailscale template against the lane's Headscale: the VM joins the tailnet, the kubelet's `--node-ip` is its tailscale address, the node is Ready over the tailnet, the Pod runs, `kubectl logs` and `exec` work (the API server reaches the kubelet over the tailnet), the serving certificate is approved, the unit ordering holds |
| Q10 | the tailscale template without a tailnet (a dummy key, a control server nobody listens on): the sysext is merged, `tailscaled` runs, the join unit cannot finish, and the kubelet, ordered after and bound to it, never starts: the sysext's `Upholds=` and the template's units and ordering work on real Flatcar |
| Q11 | optional: Q09's node side against a tailnet of your own (Tailscale SaaS or another Headscale); needs `SCARPENTER_QEMU_TAILSCALE_AUTHKEY` (a pre-authorized, ephemeral key; `SCARPENTER_QEMU_TAILSCALE_LOGIN_SERVER` for a Headscale) and skips without it |

Knobs: `QEMU_SCENARIOS=<go test -run regexp>` selects scenarios
(`QEMU_SCENARIOS='Q0[12]'`), `QEMU_SETUP_ONLY=1` stops after the control plane is
up and waits (experiment with `docker exec`). Boot-to-Ready is about half a
minute with KVM; a full run takes about 7 minutes once the caches are warm. On
failure the logs of k3s, dnsmasq and the mirror, the cluster's objects and the
serial consoles of the VMs are collected in `test/qemu/.out` (CI uploads them);
`test/qemu/.out/report.txt` has the boot times, `serial-excerpt.txt` the end of the
console of the first boot.

CI: `.github/workflows/qemu.yml` runs the lane on GitHub-hosted `ubuntu-latest`
runners (KVM through a udev rule, the bridge inside the privileged container's own
network namespace) for pull requests that touch what it tests, nightly and on
demand. The job is `continue-on-error` until three green `workflow_dispatch` runs
under 20 minutes are on record (overview section 15, Q6); then the flag goes. A
self-hosted runner with `/dev/kvm` is the fallback, only for `schedule` and
`workflow_dispatch`, never for pull requests: a fork's pull request would run
arbitrary code on it (see the end of the workflow file).

### The L5 lane

L5 proves SCarpenter end to end on a real cloud: a k3s control plane on a GitHub
runner, nodes on AWS that join over a Tailscale tailnet, both architectures, spot,
the whole Karpenter path. It is the only lane that costs money and needs credentials,
so it exists only as a workflow, `.github/workflows/e2e-aws.yml`, run on demand
(`workflow_dispatch`) and weekly, from `main` only, in the GitHub environment
`e2e`. It never runs for a pull request, and nothing in the repository runs it
from a developer machine. A full run takes about 22 minutes (the job's limit is 90: the Scenarios step has its own
limit of 55 minutes, so that at least 20 minutes stay for the diagnostics and the
teardown, which always run) and costs well
under $0.20: a handful of small instances for minutes, a few GiB of EBS, no NAT
gateway.

```bash
gh workflow run e2e-aws.yml                                  # everything
gh workflow run e2e-aws.yml -f scenarios=amd64,arm64         # a subset
gh workflow run e2e-aws.yml -f fail_fast=false               # run every scenario despite failures
```

Inputs: `scenarios` (names below, comma separated), `k3s_version` and
`kubernetes_version` (the pins of `test/e2e-aws/defaults.sh`; together they run the
lane at another Kubernetes version, for example the chart's minimum, 1.31: the
kubelet sysext must exist in the sysext-bakery for the version and be at most the
API server's), `local_storage_type` (enables the optional local-storage scenario) and
`fail_fast` (default `true`, see below).
The check that the dry run [below](#without-aws) cannot make, is the run itself.

**Failing fast and watching a run.** A broken run must neither hide nor wait for the step
limit, so the Scenarios step shows what happens and stops early:

- **Fail fast** (`E2E_FAIL_FAST`, default `true`; the input `fail_fast`): after the first failed scenario
  the remaining ones are not run and appear as `SKIP` in the report and the step summary, and
  the step exits non-zero, so that the diagnostics and the teardown start at once. `false`
  runs every scenario whatever fails.
- **Never launched** (`E2E_LAUNCH_TIMEOUT`, default 180 s): every wait for a node (the wait
  for the Pod of a scenario) fails as soon as no NodeClaim of the Pod's NodePool is launched
  (core's `Launched` condition is not `True` and there is no `providerID`) that long after the
  wait began, or the Pod became Pending, whichever is earlier. The message starts with
  `never launched`: SCarpenter did not create an instance, so the boot and the join are not
  what to look at. A node that was launched but is not Ready or joined within
  `E2E_NODE_TIMEOUT` (720 s) fails with `launched but not ready` instead.
- **Live progress** (`E2E_PROGRESS_INTERVAL`, default 30 s): during a wait for a node the
  job log gets a collapsed group, `L5 progress`, at that interval: the NodeClaims (NodePool, instance type,
  capacity type, conditions as `type=status/reason`, age), the NodePools (`Ready`, resources against limits),
  the pending Pods of the scenario with their last event, the SCNodeClasses' conditions,
  the controller's warning and error log lines and the Warning events of all namespaces since the previous block.
- **Failure snapshot**: when a scenario fails, one group, `L5 failure snapshot`, prints the same
  plus `kubectl describe` of the NodePool's NodeClaims and the pending Pods (without the
  Pods' environment) and the last 150 controller log lines.

Everything these print goes through `redact()` of `test/e2e-aws/lib.sh`, the function
that `diagnostics.sh` uses for the artifact (the job log of a public repository is public): account IDs,
tailnet host names, Tailscale auth keys, AWS access key IDs, secret keys and session tokens, JWTs,
`Authorization` and `Bearer` credentials, kubeconfig key data, e-mail addresses and bootstrap
tokens. Only selected fields are printed, never a Secret, a ConfigMap's data, user-data or
a Pod's environment. `selftest.sh` tests the redaction.

**What a run does** (overview section 13.4; the scripts are in `test/e2e-aws`):

1. `tailscale/github-action` joins the runner to the tailnet with the federated identity (tag
   `tag:scarpenter-ci`, no stored secret); `bootstrap.sh` starts a k3s server on the runner's tailscale
   address (TLS SAN, node IP) with the flags of a production k3s for remote workers, flannel
   (`--iface=tailscale0`) and kube-proxy as DaemonSets. The runner's own node is
   tainted `CriticalAddonsOnly`, so only the controller, CoreDNS and the DaemonSets
   run on it.
2. `mint-authkey.sh` mints an ephemeral, pre-authorized, reusable auth key tagged
   `tag:scarpenter-ci-node`, valid for an hour, through the Tailscale API: the job's GitHub OIDC token (audience
   `TS_AUDIENCE`) is exchanged for a short-lived API access token at
   `POST /api/v2/oauth/token-exchange` (form fields `client_id` and `jwt`).
3. The CI role is assumed through GitHub's OIDC (`aws-actions/configure-aws-credentials`);
   `install.sh` puts the temporary credentials in a Secret (`vendors.aws.credentials.mode:
   secret` with a session token). Web identity from the cluster's own issuer needs a
   public issuer and EKS Pod Identity needs EKS: neither is testable on an ephemeral
   runner, and both stay manual walks of [AWS credentials](aws-credentials.md).
4. `install.sh` builds the controller image from the checkout (production build), imports
   it into k3s, applies the CRDs and the NodeClasses (they must exist before the controller
   starts, because the label keys of their metrics are registered at start-up), installs
   **the chart** with `cluster.name=scarpenter-ci-<run id>`, and applies the NodePools
   (`WhenEmpty`, `consolidateAfter: 0s`, limits, the instance types listed).
5. `scenarios.sh` runs the scenarios below. The workloads of all of them start together,
   so the nodes boot in parallel.
6. `teardown.sh` (`if: always()`): deletes SCarpenter's NodePools and waits
   for the NodeClaims, force-terminates and sweeps what the cluster tag finds, **fails the
   job if anything tagged with the cluster remains** (instances, security groups, network
   interfaces, placement groups, subnets, VPCs, gateways, route tables, volumes), revokes the
   auth key and logs the runner out of the tailnet. On failure `diagnostics.sh` collects the
   controller logs, events, NodeClaims, CSRs, the k3s journal and the serial console of every
   instance first.

| Scenario | Proves |
|---|---|
| preflight | the Flatcar AMIs exist by name (`Flatcar-<channel>-<version>[-arm64]-hvm`, owner `075585003325`, exactly one per architecture); the account's Standard quotas suffice (16 on-demand, 4 spot vCPUs); the NodeClasses are `Ready`, `VendorReady` and `ImagesReady` |
| `amd64` | on-demand, exact type `aws.t3a.small`: Node Ready, Pod Running, `kubectl logs` and `exec`; node labels of overview section 5.5 (zone name and zone ID consistent with `DescribeAvailabilityZones`); instance tags, IMDSv2 required with hop limit 1 (and a token request from a Pod gets no answer), encrypted gp3 root volume made from the unencrypted snapshot; the price annotation is complete; the bootstrap token is gone; the serving CSR is approved over the tailnet; on the node, the checks of L4 scenarios Q09 and Q10 (tailscale sysext merged, `tailscaled` upheld, the kubelet ordered after and bound to the join unit and started with `--node-ip` = the tailscale address); the cluster DNS (the controller found the Service itself, the kubelet got `clusterDNS`, a Pod resolves a cluster Service) |
| `arm64` | spot, exact type `aws.t4g.small`: the same on arm64, the instance's lifecycle is `spot`, the spot request carries the cluster tag |
| `db` | overview 13.4 / 00a section 8.2: an `r`-family StatefulSet with the `db-io2` storage profile (a 4 GiB io2 volume with 100 IOPS) and a client Deployment, both `colocated` with one run key: one cluster placement group, named and tagged by SCarpenter, both instances in it and in one zone; the io2 volume is attached, encrypted, found by its NVMe serial (the volume ID) and mounted as xfs at `/var/lib/pgdata`, the Pod's file is on it; the device unit has `JobRunningTimeoutSec=240`; the labels of the applied profiles; `price-incomplete` for the unpriced IOPS; the managed security group allows its members; after deleting both workloads the group and the volume are gone |
| `described` | a Deployment that selects by `vcpus`, `memory-mib` ranges and a `metric-single-core-pct` threshold within the allowed test types gets a type that satisfies all of them (checked against EC2's vCPUs of the type) |
| `byo` | a security group and a subnet that the workflow creates with the AWS CLI (tagged with the cluster and the run's creation time) are `use`d by a second NodeClass: the node launches into them, and after the workload, the NodePool and the NodeClass are gone SCarpenter has not deleted them (the teardown does) |
| `local` | optional (`local_storage_type`): a type with instance-store disks gets them striped and mounted at the profile's path |
| `negative` | `publicIPv4: false` is refused on the default VPC (`VendorReady=False`, reason `ValidationFailed`), `publicIPv6: true` gates the zones of a default VPC without IPv6 (`ResourcesReady=False`); no instance is launched |
| `quota` | the limits SCarpenter reports (`scarpenter_quota_limit`) equal `aws service-quotas get-service-quota` for all 18 codes; `scarpenter_quota_used` of the Standard on-demand and spot buckets equals what EC2 says is running and pending (the counting rules of overview section 11.8: a spot request with an instance is not counted twice); the in-flight reservations are released; the launched types are in `DescribeInstanceTypeOfferings` of their zones |
| `drift` | a change to a storage profile no running node uses drifts nothing; a change to `vendors.aws.storage.root.volumeSize` replaces the node (new root size, old instance terminated, old NodeClaim and Node gone, the Pod moves). The controller Pod is killed hard (`--grace-period=0 --force`) while the replacement's `Create` is in flight, and the replacement carries `karpenter.sh/do-not-disrupt` until the Pod has moved (core forgets its disruption command at a restart and would consolidate the empty replacement). **The replay of `Create` is opportunistic**: the window (the instance exists, `Create` has not returned) is narrow on AWS and no test-only driver flag stretches it, so when the attempt misses it the run notes that and never fails for it. This is what the carry-over items "a restart must assert the replay of `Create` ran" (plan 07) and `IdempotentParameterMismatch` with changed user-data (plan 08) depend on. Always asserted: a claim that survived the restart owns one instance, no instance is orphaned; when the window was hit, the replay found the first launch's instance instead of making a second |
| `scalezero` | the exit criterion: after the workloads are deleted every instance of the cluster is `terminated` within 2 minutes (`E2E_SCALE_DOWN_DEADLINE`); no node, NodeClaim or bootstrap token is left; a `client-token` filter still finds the terminated instance; the quota usage falls with the instances and no lingering spot request counts |
| `uninstall` | the procedure of [Uninstall](install.md#uninstall) on the live account: SCarpenter's NodePools by `nodeClassRef.group`, the SCNodeClasses (once they are gone their security groups fall under `managedResources.defaultTTL`, 5 minutes in the lane, 24 hours by default), then `helm uninstall` |

The report (`report.txt`, in the step summary), per-scenario logs and the notes
(boot-to-Ready times, the measured scale-down time, how the restart attempt went) are in
the job's artifact on failure.

**Reading a failure.** The scenario's log names the assertion. Where to look next:

| Symptom | Look at |
|---|---|
| a Pod stays Pending, no NodeClaim, or the failure says `never launched` | the `L5 progress` and `L5 failure snapshot` groups of the job log (live, redacted), then `controller.log` and `events.txt` of the diagnostics: the NodePool's requirements, `QuotaAvailable=False` on the NodeClass, a type the role denies (`UnauthorizedOperation` on `RunInstances`) |
| a NodeClaim, the instance runs, the node never registers | the instance's serial console (`console-<id>.txt`): Ignition, the sysext downloads, `scarpenter-tailscale-up` (a rejected auth key or an ACL that does not allow the tag); `kubectl get csr` |
| the node is Ready but `kubectl logs`/`exec` fail | the tailnet ACL (tcp 10250 from the runner to the nodes), the serving CSR (`ServingCertificateDenied` events), the kubelet client CA (401) |
| Pods on the nodes cannot reach cluster Services or DNS | flannel over the tailnet (udp 8472 between the tags), `flannel.log` |
| `AccessDenied` or `UnauthorizedOperation` | the role's policy: `test/e2e-aws/ci-policy.sh` prints the one the lane was written for; CloudTrail names the condition that denied |
| the teardown reports leftovers | the sweeper's output in the step log lists what it deleted and what is still in use; the next scheduled sweep takes the rest |

#### Provisioning (outside this repository)

The lane needs the following, provisioned once by a maintainer; nothing here is created
by a workflow. Use your own values; the repository holds none.

*GitHub*: the environment `e2e` (shared by the cloud providers' e2e lanes) with
deployments limited to the `main` branch and **no required reviewers** (the sweeper runs
on a schedule in the same environment, and each run would wait for an approval, so
leftovers would not be swept; the sweeper and the lane share the environment and with it
the role and its trust policy). In it:

- variables `AWS_REGION` (`us-east-2`), `TS_OAUTH_CLIENT_ID` and `TS_AUDIENCE` (the client
  ID and audience of the Tailscale federated identity; not sensitive);
- the secret `AWS_ROLE_ARN` (the workflow reads it as a secret; a variable would need the
  workflow changed);
- no Tailscale secret: there is no OAuth client secret.

In the repository's Actions settings, require approval for all outside collaborators.

The OIDC subject of the jobs is `repo:SpareCores@152076439/scarpenter@1447426340:environment:e2e`.
This repository was created after 2025-07-15, so GitHub issues *immutable* subject claims
for it (the organization and repository IDs are part of the claim; both are public
identifiers). The repository's OIDC customization reports `use_immutable_subject: true`
with the prefix `repo:SpareCores@152076439/scarpenter@1447426340`. The classic form
`repo:SpareCores/scarpenter:environment:e2e` is not what GitHub sends for it; a trust policy
may list both forms (the classic one is harmless and keeps a policy valid if the
customization is ever switched back).

*AWS*: an IAM OIDC provider for `token.actions.githubusercontent.com` (client ID
`sts.amazonaws.com`) and a role `scarpenter-ci-gha` with this trust policy (the
environment claim keeps every other workflow and branch out):

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {"Federated": "arn:aws:iam::123456789012:oidc-provider/token.actions.githubusercontent.com"},
    "Action": "sts:AssumeRoleWithWebIdentity",
    "Condition": {
      "StringEquals": {
        "token.actions.githubusercontent.com:aud": "sts.amazonaws.com",
        "token.actions.githubusercontent.com:sub": "repo:SpareCores@152076439/scarpenter@1447426340:environment:e2e"
      }
    }
  }]
}
```

Its permissions come from the policy of the controller
([`aws-iam-policy.json`](../reference/aws-iam-policy.json), the IAM statements are explained in
the [AWS driver reference](../reference/aws-driver.md#iam)) for the clusters `scarpenter-ci-*`;
the script prints it with the lane's additions:

```bash
hack/dev.sh test/e2e-aws/ci-policy.sh 123456789012 us-east-2 > ci-policy.json
aws iam create-policy --policy-name scarpenter-ci-gha --policy-document file://ci-policy.json
aws iam attach-role-policy --role-name scarpenter-ci-gha --policy-arn arn:aws:iam::123456789012:policy/scarpenter-ci-gha
```

What it adds to the controller's policy: the tag conditions become wildcards
(`scarpenter-ci-*`); the statements for an instance profile and a customer KMS key are
dropped; the optional managed-network statements stay (the sweeper deletes a managed VPC,
and the `byo` scenario creates and deletes one tagged subnet and security group in the
default VPC: `LaneBringYourOwn` allows `ec2:CreateSubnet` in the region's VPCs, the subnet
itself must carry the tag); `LaneReads` allows `DescribeVolumes`, `DescribeNetworkInterfaces` and
`GetConsoleOutput`; an explicit deny of `RunInstances` for every instance type but the ones the
lane launches (`t3a.small`, `t3a.medium`, `t3a.large`, `t4g.small`, `r6a.large`, and
`E2E_LOCAL_STORAGE_TYPE` if set; the pools of `test/e2e-aws/manifests/nodepools.yaml` name the
same types, and a test keeps this page and them together); an explicit deny of EC2 and Service
Quotas calls outside the region (the sweeper is told the lane's region and visits only that; with `-regions all` it is refused in the
others and skips them; the controller's driver facts do the same: a region whose zones the role
cannot read is skipped with one warning (`skipping a region the credentials cannot read`) and
the other regions load). The role's maximum session duration must be at least **two hours**: the controller keeps
the credentials of the first role assumption for the whole run (the workflow asks for 7200
seconds). The account also needs the Spot service-linked role
([AWS credentials](aws-credentials.md#account-setup-for-spot)) and **quotas that the new-account
defaults do not give**: Standard on-demand 16 vCPUs and Standard spot 4 vCPUs in the region
(`preflight` checks, and says so, before anything is launched).

*Tailscale*: two tags and a federated identity (workload identity federation,
[kb 1581](https://tailscale.com/kb/1581/workload-identity-federation)). In the tailnet policy file (HuJSON), with
`tag:scarpenter-ci` for the runner and `tag:scarpenter-ci-node` for the nodes:

```json
{
  "tagOwners": {
    "tag:scarpenter-ci": ["autogroup:admin"],
    "tag:scarpenter-ci-node": ["autogroup:admin", "tag:scarpenter-ci"]
  },
  "grants": [
    {"src": ["tag:scarpenter-ci"], "dst": ["tag:scarpenter-ci-node"], "ip": ["tcp:10250", "udp:8472"]},
    {"src": ["tag:scarpenter-ci-node"], "dst": ["tag:scarpenter-ci"], "ip": ["tcp:6443", "udp:8472"]},
    {"src": ["tag:scarpenter-ci-node"], "dst": ["tag:scarpenter-ci-node"], "ip": ["udp:8472"]}
  ]
}
```

The ports are the kubelet (`kubectl logs` and `exec`: the API server on the runner calls
10250), the API server (the nodes join and talk to 6443), and flannel's vxlan tunnel (udp 8472)
between the runner and the nodes and between the nodes. Nothing else is open between the tags. Create a
federated identity (Settings, Trust credentials) with the issuer GitHub Actions, the subject
`repo:SpareCores@152076439/scarpenter@1447426340:environment:e2e` (the immutable form, see above), the scope **`auth_keys`** (write; neither the join action nor the scripts call a device
API, so no `devices` scope) and the tag `tag:scarpenter-ci` **only**: the
join action uses it for the runner's key, `mint-authkey.sh` for the nodes' key and for
revoking it. Do not add `tag:scarpenter-ci-node` to the identity. Tailscale requires the tags of a
requested key to equal the credential's tags or be owned by them, and the runner joins with
`tag:scarpenter-ci` alone; the nodes' key (tag `tag:scarpenter-ci-node` alone) is allowed because
`tag:scarpenter-ci` owns that tag (the `tagOwners` above). With both tags on the identity, neither
single-tag request would match. Its client ID and audience are the two environment variables
`TS_OAUTH_CLIENT_ID` and `TS_AUDIENCE`; nothing secret exists, because the workflow proves
itself with its OIDC token (`id-token: write`). Nodes join as ephemeral devices, so they vanish from the tailnet when
their instances do. If a tailnet uses other prefixes than `100.64.0.0/10` set
`bootstrap.tailnetPrefixes` in `manifests/chart-values.yaml`.

#### The sweeper

`.github/workflows/e2e-aws-sweeper.yml` runs every three hours and on demand, in the same
environment with the same role. It terminates the instances and deletes the placement
groups, security groups and the network (subnets, route tables, internet gateways, VPCs) of every
cluster whose `scarpenter.sparecores.com/cluster` tag starts with `scarpenter-ci-` and that
is more than two hours old, in the lane's region (`AWS_REGION`, the only one the role may use; the
workflow's `regions` input takes another list or `all`). The age is the instance's launch time;
a security group, a subnet or a VPC has no creation time in the API, so the lane's NodeClasses
put the tag `ci-created-at` (Unix seconds) on everything they create through `spec.tags`, and
the workflow's own resources carry it too. **A resource without the tag has no known age and
is left alone**, so does a default VPC, a default security group and a main route table.
Spot requests still open or active are only reported (a one-time request closes with its
instance). The selection is `pkg/vendors/aws/sweep`, tested against the AWS driver's in-memory EC2
(old and young instances, resources by tag and age, a cluster of another name, a managed network
in a region without a default VPC deleted in dependency order, an unattached gateway, a region
the role may not use, a region whose endpoint is unreachable, the `-regions` parsing, a dry run);
`go run ./test/e2e-aws/sweeper -dry-run -regions all` (with credentials) lists what a sweep of every
enabled region would delete, `-cluster <name> -max-age 0 -regions <region>` is what the teardown uses
for its own cluster. `-regions` is a comma list; empty or `all` means every enabled region of the
account, and only then is a region that cannot be reached at all (dial or I/O timeout, name not
resolved, connection refused on the first listing) reported as `skipped: unreachable`; a region
listed explicitly that cannot be reached is a failure (exit status 1).

#### Without AWS

`hack/dev.sh make test-e2e-aws-dryrun` (CI runs it) is what can be checked without an account:
`manifests_test.go` renders every manifest of `test/e2e-aws/manifests` with sample values and
sends it to an envtest API server as a server-side dry run with strict validation, with the CRDs
of SCarpenter and Karpenter core (the CEL rules, the schema); `selftest.sh` runs the helpers of
`lib.sh` against fixtures and stubs, checks that every scenario has a function and the workflow
names it, that every action is pinned by SHA, that the lane's files hold no account ID, key or
local path, that the CI policy script produces what the docs say, and renders the chart with the
lane's values; the sweeper's tests run against the in-memory EC2. `make lint` runs `actionlint` and
`shellcheck` on the workflows and the scripts.

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
hack/dev.sh make generate manifests chart-crds chart-rbac docs-api
```

`chart-crds` copies the CRDs (SCarpenter's and Karpenter core's, at the version of
`go.mod`) into `charts/scarpenter/crds`, `chart-rbac` the rules of `config/rbac` and
`config/rbac-core` into `charts/scarpenter/files/rbac.yaml`.

`docs/reference/api.md` is produced from the CRD by `hack/apidocs`; do not edit
it by hand. The CRD embeds the selector and ranking types of
[`sc-keeper-go`](https://github.com/SpareCores/sc-keeper-go) (package
`selection/api`), which carry `float64` fields, so controller-gen runs with
`crd:allowDangerousTypes=true`.

## Continuous integration

`.github/workflows/ci.yml` runs the gates inside the same dev container:
the dev image is built once per run (with a GitHub Actions layer cache, under
the tag `hack/dev.sh` looks for) and every step reuses it. The workflow checks
that generated files are up to date, lints, tests, tests the chart, and builds the
binary and the production and development images; the L3 lane is a second job. Run
`hack/dev.sh actionlint` to check workflow changes locally.

Rules for the workflows of this public repository: actions are pinned by commit
SHA (with the version in a comment); `permissions:` default to `contents: read` and
a job gets more only where it needs it; nothing that holds a secret or a write
token runs for a pull request, and in the repository settings (Actions, General)
set "Require approval for all outside collaborators" for fork pull requests. The L4
workflow runs on GitHub-hosted runners. If a self-hosted runner is adopted for it
(overview section 15, Q6), it must be for `schedule` and `workflow_dispatch` only,
never for pull requests: a fork's pull request would run arbitrary code on it.

## Releases

`.github/workflows/release.yml` publishes on a tag `v<semver>` pushed by a
maintainer: a production image (`ghcr.io/sparecores/scarpenter`, linux/amd64 and
arm64, the drivers of the supported vendors only), a development image
(`scarpenter-dev`, with the fake and qemu drivers, for the lanes), the OCI chart
(`ghcr.io/sparecores/charts/scarpenter`), cosign keyless signatures of the image
and chart digests, an SBOM and provenance per image, and a GitHub release. The
version comes from the tag only (the Dockerfile stamps it, the chart is packaged
with it as `version` and `appVersion`); nothing in the repository carries one.
Before anything is published the tagged commit must be on `main` and pass the CI
gates. The build steps are in `hack/release.sh`; the workflow's `workflow_dispatch`
runs them as a dry run (read-only token, nothing pushed), and so can you:

```bash
hack/dev.sh hack/release.sh dry-run v0.1.0-rc.1
```

The dry run builds both images for amd64 and arm64 without pushing, checks that the
production image has no development driver flags and reports the stamped version and
commit, and packages and lints the chart. To put a human approval before the first
publish, add required reviewers to the environment `release` (Settings, Environments);
the publishing jobs run in it. Every action in the workflows is pinned by commit SHA.

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
