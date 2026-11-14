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
| 6 | `hack/dev.sh make test-e2e-aws-dryrun` | The L5 lane's parts that need no AWS: its manifests against an API server, the sweeper and the canary against the in-memory EC2, the path rules of the workflow planner, its shell helpers against stubs, the chart with its values (see [The L5 lane](#the-l5-lane)). Part of `make test` for the Go tests; CI runs the whole target. |

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
| L3 | `make test-kwok` | Real Karpenter core, SCarpenter (installed from the Helm chart) and the fake driver (KWOK nodes) in kind. | Plans 04 and 05: eleven scenarios, extended to seventeen by plan 06 and to nineteen by plan 14; the chart since plan 09. [The L3 lane](#the-l3-lane). |
| L4 | `make test-qemu` | The qemu driver: real Flatcar VMs, a k3s control plane. Needs `--privileged` and `/dev/kvm`. | Plan 07: [The L4 lane](#the-l4-lane). |
| L5 | `.github/workflows/e2e.yml` (pull requests that touch what it tests, manual), `e2e-cloud.yml` (the lane), `release.yml` (every tag) | Real cloud over Tailscale, AWS today. Never run from the dev container, never by a contributor's tooling; never on a timer ([when it runs](testing-and-releasing.md)). | Plan 10: [The L5 lane](#the-l5-lane). Has run green live (first green run 2026-11-10, all scenarios pass). |

The design of the layers is in
[the overview](../plans/v1/wip/00-overview.md#13-testing) and ADR-010.

### The Keeper snapshot

`test/data/sc-snapshot/` holds a recording of Keeper's snapshot files of one
database hash (the gzip bytes as Keeper sent them): for all eight vendors the
`region`, `zone`, `server`, `server_price`, `storage_price` and `ipv4_price`
tables, the global `vendor` table, the score files of the test benchmarks
(`geekbench:score`, `stress_ng:bestn`, `membench:latency`) for every vendor, and
`/benchmark_configs`, the one live route besides `/healthcheck`. The query list is
`test/data/sc-snapshot/queries.yaml`; the files are written by `sc-keeper-go`'s
recorder and replayed by its `keepertest` server (snapshot routes, manifest,
`401`, `503 snapshot-preparing`, a second hash), through
`pkg/catalog/catalogtest`:

```go
src := catalogtest.Source(t)           // a Keeper source reading the replay server
facts := catalogtest.Facts(t, "aws")   // driver facts from the snapshot's zone tables
```

Tests never reach the network and need no key. Refresh the recording only when
the data should change. Keeper's snapshot routes need an API key (request one at
<https://docs.sparecores.com/api/#authentication>), which is never written to the
repository or the recording:

```bash
KEEPER_API_KEY=<key> hack/dev.sh make snapshot            # key in the environment
KEEPER_API_KEY_FILE=<file> hack/dev.sh make snapshot      # key in a file (mounted read-only)
```

In CI the manual workflow "Refresh Spare Cores snapshot" gets the key from the
composite action `.github/actions/keeper-key`: it assumes the CI role of the
job's environment (`secrets.AWS_ROLE_ARN`) through GitHub's OIDC, reads the
SSM parameter named by the **repository variable** `KEEPER_API_KEY_PARAMETER`
(its ARN, kept in GitHub's configuration, never in the repository) with
`ssm:GetParameter` and decryption, masks the value and writes it to a `0600`
file under `$RUNNER_TEMP` (never to `$GITHUB_ENV`). The CI roles of the `e2e`
and `e2e-canary` environments must therefore allow `ssm:GetParameter` on that
parameter (and `kms:Decrypt` if it uses a customer-managed key). When the role or
the variable is not set (a fork), the refresh workflow fails with a clear message;
the L5 lanes skip the step and run against Keeper without a key. The committed
recording is about 2.4 MB.

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
(`errors`, `quota`, `orphans`, `launchDelay`, `unsupported`, `existingResources`, `images`, `billing`)
and read what it did from the ConfigMap `scarpenter-fake-state` (`calls`,
`instances`, `resources`, `revision`); the formats are in the
[driver contract](../reference/driver-contract.md#the-fake-driver). The fake
publishes its state debounced; the scenarios do not sleep for it: they read the
fake's current revision from the controller's metrics
(`scarpenter_fake_state_revision`) and wait until the ConfigMap's `revision` has
reached it (`awaitState`). The scenarios are `TestS01` to `TestS20` and, for billing retention, `TestR01` to `TestR09`:

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
| S16 | vendor resources: `use` by `tags` in two regions (never touched), a missing `use` resource makes the region unavailable with a reason and recovers, default `create` on the first launch and its reuse, adoption after a controller restart, `idleTTL` deletion, `preCreate` kept while listed, `forbid` |
| S16b | the resolution order (ADR-022): without `use` or `create` a subnet tagged `scarpenter.sparecores.com/use=true` wins over the vendor's default, the default serves when nothing is tagged, a default that cannot honour `publicIPv6` is skipped for a created subnet, and the status says which step produced each resource (`source`) |
| S17 | incomplete prices: offerings flagged `price-incomplete` and still used; excluded with `pricing.allowIncomplete: false` |
| S18 | a stopped (powered-off) orphan instance is reported terminating, left alone for the terminating grace and then deleted by the garbage collector |
| S19 | a node powered off while its NodeClaim exists (the fake's `powerOff` key): Karpenter core deletes the NodeClaim, and `Delete` deletes the instance after the terminating grace |
| S20 | the `images` kind (the fake's `images` list): explicit IDs of both architectures give `ImagesReady=True` with the image of each architecture and a node launches; an image of another Flatcar version, or one that says none, makes `ImagesReady` false with `ImageVersionMismatch` or `ImageVersionUnknown` and nothing is launched; a named region without an image is not ready and only the region that has one launches |
| R01 | billing retention (plan 23; the fake vendor's rule is shortened to a 4 minute period with a 90 s margin): jobs within the period reuse one node, the empty node is gone before the boundary |
| R02 | a minimum then per second: reuse inside the minimum, the node gone before it ends, the next job gets a fresh node |
| R03 | a node busy across the deadline is not deleted, its hold is released for the window and put back after the boundary |
| R04 | a per-second vendor ignores the setting; `status.billing` says `PerSecondBilling` |
| R05 | `expireAfter` shorter than the period, and the period minus the margin, still remove the held node |
| R06 | a budget of 0 for `Empty` holds the empty node into the next period; it goes at the next deadline once the budget opens |
| R07 | a budget of 1 with two empty nodes: never two releases at once, no overshoot |
| R08 | a NodePool at its `limits` with a held empty node: the pending pod gets the capacity after the node is released under pressure |
| R09 | a pod created one second before the deadline never ends on a node being deleted; no boundary is missed |

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
so it exists only as a workflow, the reusable `.github/workflows/e2e-cloud.yml` (one
vendor, AWS today; `mode` is `full` or `smoke`), called by `e2e.yml` and by `release.yml`,
in the GitHub environment `e2e` whose required reviewers approve every run. **It does not run
on a timer**: it runs for the pull requests that touch what it tests (a vendor's driver or
lane: its full lane; code every node goes through: a smoke of two scenarios), on demand, and
before every release; a free nightly canary watches the outside world. The tiers, the path
rules, the labels, the approval and the release gate are in
[Testing and releasing](testing-and-releasing.md). Nothing in the repository runs it
from a developer machine. A full run takes about 22 minutes (the job's limit is 90: the Scenarios step has its own
limit of 55 minutes, so that at least 20 minutes stay for the diagnostics and the
teardown, which always run) and costs well
under $0.20: a handful of small instances for minutes, a few GiB of EBS, no NAT
gateway. Alibaba Cloud has a lane of the same shape, described [below](#the-alibaba-cloud-lane);
it has not run yet.

```bash
gh workflow run e2e.yml -f vendor=aws -f mode=full                     # everything
gh workflow run e2e.yml -f vendor=aws -f mode=smoke                    # preflight, amd64, scalezero
gh workflow run e2e.yml -f vendor=aws -f scenarios=amd64,arm64         # a subset
gh workflow run e2e.yml -f vendor=aws -f fail_fast=false               # run every scenario despite failures
```

Inputs of `e2e.yml`: `vendor`, `mode`, `scenarios` (names below, comma separated) and
`fail_fast` (default `true`, see below). `e2e-cloud.yml` also takes `k3s_version` and
`kubernetes_version` (the pins of `test/e2e-aws/defaults.sh`; together they run the
lane at another Kubernetes version, for example the chart's minimum, 1.31: the
kubelet sysext must exist in the sysext-bakery for the version and be at most the
API server's) and `local_storage_type` (enables the optional local-storage scenario); it
cannot be dispatched by itself, so a minimum-version run is a one-off workflow change that
calls it with them.
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
| `negative` | `publicIPv4: false` is refused on the default VPC (`VendorReady=False`, reason `ValidationFailed`), `publicIPv6: true` skips the subnets of a default VPC without IPv6 (ADR-022: the status lists no `default` subnet and the region stays ready, the managed network would be created at the first launch); no instance is launched |
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

*GitHub*: the environment `e2e` (shared by the cloud providers' e2e lanes), with
**required reviewers** (every run is approved by a maintainer: a pull request's workflow code runs
with the role) and no limit to the `main` branch (pull requests run on `refs/pull/<n>/merge`, the
release gate on a `v*` tag; see [Testing and releasing](testing-and-releasing.md#approval-and-fork-pull-requests)).
The sweeper runs in an environment of its own, `e2e-sweeper` (limited to `main`, no reviewers, the
same role: see [The sweeper](#the-sweeper)), so that its scheduled runs never wait for an approval.
The nightly canary has its own environment, `e2e-canary`, with a read-only role. In `e2e` there are:

- variables `AWS_REGION` (`us-east-2`), `TS_OAUTH_CLIENT_ID` and `TS_AUDIENCE` (the client
  ID and audience of the Tailscale federated identity; not sensitive);
- the secret `AWS_ROLE_ARN` (the workflow reads it as a secret; a variable would need the
  workflow changed); the role also allows `ssm:GetParameter` on the Keeper API key
  parameter (and `kms:Decrypt` for its key), which the repository variable
  `KEEPER_API_KEY_PARAMETER` names (see [The Keeper snapshot](#the-keeper-snapshot));
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
environment claims keep every other workflow and branch out; the second subject is the sweeper's
environment, `e2e-sweeper`):

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
        "token.actions.githubusercontent.com:sub": [
          "repo:SpareCores@152076439/scarpenter@1447426340:environment:e2e",
          "repo:SpareCores@152076439/scarpenter@1447426340:environment:e2e-sweeper"
        ]
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

`.github/workflows/e2e-aws-sweeper.yml` runs every three hours and on demand with the lane's role,
in the environment `e2e-sweeper`: no required reviewers (a sweep must not wait for an approval) and
limited to `main` (a manual run from another branch, whose code would hold the role unreviewed, is
refused; the variable `AWS_REGION` and the secret `AWS_ROLE_ARN` are set there too). It terminates the instances and deletes the placement
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

#### The Alibaba Cloud lane

`test/e2e-alicloud/` is the lane of the vendor `alicloud`, **not in `test/e2e-vendors.txt`** until the guards of [Per-vendor cost guards](testing-and-releasing.md#per-vendor-cost-guards) are verified (below), so the workflows do not run it by themselves,
a copy of the AWS lane's shape with Alibaba Cloud's calls: the control plane on the runner, the tailnet, the
chart install and the scenarios' structure are the same; the cloud calls go through the Alibaba Cloud
CLI `aliyun` (the byo scenario's VPC, vSwitch and security group, the instance, disk and network checks, the
diagnostics, the no-leftover check). **It was written and validated statically and has not run against an
account**: the pins, the cloud facts and the assertions marked "unverified" in the scripts are settled by the
first run, which the coordinator triggers. The pins are in `test/e2e-alicloud/defaults.sh`: the region
`ALICLOUD_REGION` (`ap-southeast-1`; the zone IDs equal the zone names, `ap-southeast-1a`), the types
`ecs.e-c1m2.large` (amd64, on-demand), `ecs.c8y.large` (arm64, spot) and `ecs.g7.large` (the deployment-set
scenario), `ecs.e-c1m4.large` for the described scenario, the Flatcar release, and `E2E_SMOKE_SCENARIOS`
(`amd64,scalezero`).

How it differs from the AWS lane:

- **Before the lane is listed**: the guards are an instance-type deny plus the account's vCPU quotas, and
  whether RAM applies the `ecs:InstanceType` condition to `ecs:RunInstances` is unverified. The operator checks that
  a launch of another type with the CI role is refused, sets the quotas no higher than the lane needs, creates
  the budget alert and checks it fires, and then adds `alicloud` to `test/e2e-vendors.txt`.
- **Credentials.** The workflow writes a GitHub OIDC token to a file and exports the role and provider ARNs from
  the environment's secrets (`test/e2e-alicloud/oidc-token.sh`, no third-party action: `ALIBABA_CLOUD_ROLE_ARN`,
  `ALIBABA_CLOUD_OIDC_PROVIDER_ARN`, `ALIBABA_CLOUD_OIDC_TOKEN_FILE`). The scripts exchange it once for a RAM role
  session of two hours (`ali_login` in `lib.sh`, an unsigned `AssumeRoleWithOIDC` call whose body carries the
  token) and `install.sh` puts the session in a Secret (`vendors.alicloud.credentials.mode: secret`; the chart's
  `oidc` mode reads the cluster's own service account token, whose issuer is not public, so it stays a manual walk
  of the credentials guide). The teardown writes a fresh token and logs in again.
- **Images.** They are the operator's own (rung 3): the preflight checks that exactly one available image named
  `Flatcar-<channel>-<version>-<arch>` with the tags `flatcar-channel` and `flatcar-version` exists in the region,
  and imports nothing. The import contract is in
  [Alibaba Cloud provisioning](testing-and-releasing.md#alibaba-cloud-provisioning).
- **Quotas** are the two vCPU buckets of the region, `postpaid-vcpu` and `spot-vcpu`
  (`DescribeAccountAttributes`); the lane needs 16 and 4.
- **No terminated state.** A released instance leaves `DescribeInstances`, so "gone" and "not listed" are the same
  and there is no client-token lookup of a terminated instance.
- **Deployment sets take no tags**: the driver writes the cluster into the description, and the checks and the
  sweeper read it from there.
- **Sweeper** `go run ./test/e2e-alicloud/sweeper` (flags as the AWS one; `-home-region` defaults to
  `ALIBABA_CLOUD_REGION_ID` or `ap-southeast-1`) and `.github/workflows/e2e-alicloud-sweeper.yml`: instances,
  deployment sets, security groups, vSwitches and VPCs of `scarpenter-ci-*` clusters older than two hours, by the
  tag (`ci-created-at` where the API gives no creation time), never a default VPC or an unmarked resource; the
  selection is `pkg/vendors/alicloud/sweep`, tested against the driver's in-memory ECS and VPC.
- **Canary** `test/canary/alicloud.go` behind `alicloud_guard.go` (see
  [the canary](testing-and-releasing.md#the-canary)).

The RAM policy of the CI role is printed by `test/e2e-alicloud/ci-policy.sh <account UID> [region]` and the
canary's by `canary-policy.sh`. `RunInstances` is allowed in the lane's region with the cluster tag
(`scarpenter-ci-*`) on the request and denied for every instance type but the ones of `defaults.sh`
(`ecs.e-c1m2.large`, `ecs.e-c1m4.large`, `ecs.c8y.large`, `ecs.g7.large`, and `E2E_LOCAL_STORAGE_TYPE` if set; the pools of
`test/e2e-alicloud/manifests/nodepools.yaml` name the same types, and a test keeps this page and them together);
deleting or changing instances, security groups, vSwitches and VPCs needs the cluster tag on the resource; the
reads are `Describe*`, `ListTagResources`, `ecs:DescribeAccountAttributes`, `vpc:Describe*` and the Quota Center
reads. The condition keys' exact use with `RunInstances` is unverified.

| Scenario | Proves |
|---|---|
| preflight | the Flatcar images exist by name and tags (exactly one per architecture, `ImdsSupport` v1 when reported); the vCPU quotas suffice (16, 4); the four NodeClasses are `Ready`, `VendorReady` and `ImagesReady` |
| `amd64` | on-demand `ecs.e-c1m2.large`: Node Ready, Pod, `kubectl logs` and `exec`, the labels (the zone ID equals the zone name); the instance's tags and name (`DescribeInstances`), the metadata options (`DescribeInstanceAttribute`: `HttpTokens` stays `optional`, because Ignition's aliyun provider reads the user-data without a token), the system disk (cloud_essd PL0, 40 GiB, deleted with the instance); the bootstrap token is gone, the serving CSR approved, the L4 unit checks on the node; **the Ignition check**: `PLATFORM_ID=aliyun` and Ignition's aliyun provider fetched the user-data (the node's journal) |
| `arm64` | spot `ecs.c8y.large`: the same on arm64, the instance's image is an arm64 image |
| `spot` | `SpotStrategy=SpotAsPriceGo`, still running, a price annotation, the spot vCPUs counted |
| `imds` | the gap of plan 21 section 5, recorded: what a Pod reads at `100.100.100.200` on the plain NodeClass (the instance ID and the size of the user-data, never its content); with `bootstrap.hardening: [metadata-from-pods]` on a second NodeClass a Pod reads nothing, while the node itself still does |
| `db` | a LowLatency deployment set (`scarpenter-` name, the cluster in its description) holds a database and its client in one zone; the data disk is cloud_essd PL1, 20 GiB, mounted as xfs at `/var/lib/pgdata`; the managed security group allows its members; after the workloads go the instances, the set and the disk are gone |
| `described` | a Deployment that selects by `vcpus`, `memory-mib` and `metric-single-core-pct` gets a type that satisfies all of them (checked against `DescribeInstanceTypes`) |
| `byo` | the workflow creates a VPC, a vSwitch and a security group tagged with the run and `use`s them by tag; the node launches into them, and afterwards SCarpenter has touched none of them |
| `negative` | `publicIPv4: false` and `publicIPv6: true` are refused (`VendorReady=False`, `ValidationFailed`); nothing is launched |
| `image` | the instances booted the images found by name (ids and names, from `DescribeImages`) and the NodeClass status lists them |
| `quota` | `scarpenter_quota_limit` equals `max-postpaid-` and `max-spot-instance-vcpu-count` of `DescribeAccountAttributes`; `scarpenter_quota_used` equals the used attributes and follows launch and release; the launched types are in `DescribeAvailableResource` of their zones |
| `drift` | a change to a profile no node uses drifts nothing; a change to `vendors.alicloud.storage.root.size` replaces the node (new disk size, old instance released, the Pod moves), the controller is killed while the replacement is created (the replay is opportunistic, as on AWS) |
| `scalezero` | every instance of the cluster is released within 2 minutes of the Pods' deletion (`E2E_SCALE_DOWN_DEADLINE`); no node, NodeClaim or token left; the quota usage falls |
| `uninstall` | the procedure of [Uninstall](install.md#uninstall) on the live account: the NodePools, the NodeClasses (the managed groups and deployment sets go), `helm uninstall` |

The final no-leftover check is step 3 of `teardown.sh`, which fails the job when an instance, a disk, a network
interface, a security group, a deployment set, a vSwitch or a VPC of the run remains after the sweeper ran. There
is no `local` scenario: the driver supports no local-disk policy yet.

#### Without AWS

`hack/dev.sh make test-e2e-aws-dryrun` (CI runs it) is what can be checked without an account (the Alibaba Cloud lane's is `make test-e2e-alicloud-dryrun`: the same checks against the in-memory ECS and VPC, with the RAM policies' properties instead of the IAM ones):
`manifests_test.go` renders every manifest of `test/e2e-aws/manifests` with sample values and
sends it to an envtest API server as a server-side dry run with strict validation, with the CRDs
of SCarpenter and Karpenter core (the CEL rules, the schema); `selftest.sh` runs the helpers of
`lib.sh` against fixtures and stubs, checks that every scenario has a function and the workflow
names it, that every action is pinned by SHA, that the lane's files hold no account ID, key or
local path, that the CI policy and the canary's read-only policy are what the docs say, and renders the chart with the
lane's values; the sweeper's tests run against the in-memory EC2; `test/canary` runs the canary
against the same in-memory EC2 and compares its calls with the read-only policy; `test/e2e-plan-selftest.sh`
tests the path rules that decide which runs a pull request needs. `make lint` runs `actionlint` and
`shellcheck` on the workflows and the scripts.

#### The UpCloud lane

`test/e2e-upcloud/` is the L5 lane of the UpCloud driver ([plan 19](../plans/v1/wip/19-upcloud.md)):
the same shape as the AWS lane (a k3s control plane on the runner, nodes joined over Tailscale,
the scripts `bootstrap.sh`, `mint-authkey.sh`, `install.sh`, `scenarios.sh`, `diagnostics.sh`,
`teardown.sh`), with the cloud calls made with `curl` and `jq` against the UpCloud API
(`lib.sh`: `up`, the label filters of the list calls). It is **not in `test/e2e-vendors.txt`** until
the guards of [Per-vendor cost guards](testing-and-releasing.md#per-vendor-cost-guards) are verified
(the sub-account's limits and a spending alert, see [UpCloud](testing-and-releasing.md#upcloud)), so the
workflows do not run it by themselves; `make test-e2e-upcloud-dryrun` checks it without a cloud.
Differences that matter:

- **Credential.** UpCloud has no OIDC federation and no workload identity. The lane uses the API
  token of a dedicated sub-account, `scarpenter-ci`, as the secret `UPCLOUD_TOKEN` of the `e2e`
  environment (the same secret in `e2e-sweeper` and `e2e-canary`); the variable `UPCLOUD_ZONE` (default
  `fi-hel1`) is the one zone of the run, because UpCloud's regions are its zones. The controller gets the
  token as a Secret (chart `vendors.upcloud.credentials.mode: secret`). A token has no scopes and
  cannot be read-only; the sub-account's **resource limits** (cores, memory, networks, set by the
  main account) are the cost guard, together with the sweeper. Tokens expire (up to a year):
  the canary reads `GET /account` every night and says so, the rotation is manual.
- **Prerequisite.** In the lane's zone, persistent Flatcar **templates** of the pinned release, one per
  storage tier (MaxIOPS is required, Standard where the account can make one), labelled
  `scarpenter-use=true`, `scarpenter-image=flatcar`, `flatcar-channel`, `flatcar-version`,
  `flatcar-arch=amd64` and `flatcar-tier`. They are built by the images repository (plan 22) and
  imported by the operator; the lane's NodeClasses leave `resources.images` unset, so the driver finds
  them by that label convention. The preflight fails, naming the missing template, when there is none.
- **Plans.** `STARTER-1xCPU-1GB` (`upcloud.STARTER-1xCPU-1GB`) for every scenario, and
  `STARTER-2xCPU-4GB` and `STARTER-2xCPU-8GB` for the described-not-named one. Plan 19 names `1xCPU-1GB`,
  but the committed Keeper snapshot lists only the CLOUDNATIVE, PREMIUM, STARTER and GPU families as
  active; change `E2E_TYPE_AMD64` in `defaults.sh` if the live account says otherwise.
- **Scenarios** (`E2E_SCENARIOS`, in this order): `amd64`, `described`, `byo`, `data`, `servergroup`,
  `negative`, `quota`, `publicfw`, `drift`, `scalezero`, `uninstall`, after the preflight. `amd64`:
  node Ready, Pod running, `logs` and `exec`, the node labels, the server's plan, zone, labels, `metadata`,
  firewall, interfaces and disks, the user-data size noted. `scalezero`: every server stopped and
  deleted with its disks within `E2E_SCALE_DOWN_DEADLINE` (120 s) of the Pods' deletion, and the time noted
  (the driver's delete latency, which the billing retention margin must cover). `drift`: a root disk
  size change replaces the node, a change of an unused profile drifts nothing, the controller is
  killed while the replacement is created (the idempotency label). `quota`: the controller's `cores` and
  `memory` limits equal `GET /account` and the usage equals the sum over the account's servers.
  `publicfw`: from the runner, the node's public TCP ports 22 and 10250 do not answer while the node
  stays Ready over Tailscale, the rules on the server equal the driver's list (`fwrules`), and the
  rollout window is noted. `servergroup`: two nodes of one run key in one named, labelled group, both
  members and the anti-affinity met; the group goes with the workload. `described`: a `vcpus` range
  and a metric threshold pick a plan of the allowed set. `byo`: the workflow creates a labelled SDN
  network and ruleset, the NodeClass `use`s them by tag, the node gets a private address and the private
  firewall is on, and both survive until the teardown deletes them. `data`: a `storage.data` profile with a
  10 GB MaxIOPS disk mounted at the profile's path, deleted with the node. `negative`: `publicIPv4:
  false` without a network of the user's own, and a label key over 32 characters, are refused. The
  final check is the teardown: no server, disk, network, ruleset or server group labelled with the
  run's cluster remains.
- **The sweeper.** `.github/workflows/e2e-upcloud-sweeper.yml` (every three hours, environment
  `e2e-sweeper`) runs `go run ./test/e2e-upcloud/sweeper`: one account-wide list of the servers
  labelled `scarpenter-cluster`, those whose value starts with `scarpenter-ci-` and whose
  `scarpenter-created` label is at least two hours old are stopped and deleted with their disks; then the
  server groups, SDN rulesets and networks of such clusters, and unattached disks, when no young or
  age-less server of the cluster is alive. Resources labelled `scarpenter-use` (the templates) and
  anything without the prefix are never touched; the engine is `pkg/vendors/upcloud/sweep`, tested
  against the driver's in-memory UpCloud. `-cluster <name> -max-age 0` is what the teardown uses.
- **Cost per run.** A few `STARTER-1xCPU-1GB` servers (a full run has up to about nine cores at
  once, for about an hour) at an hourly price: cents (price unverified, plan 19 S4). The persistent
  cost is the templates (about 10 GB each, per tier). A full run needs about 10 cores and 12 GB of memory
  of the sub-account's limits.
- **Without UpCloud.** `hack/dev.sh make test-e2e-upcloud-dryrun` (the same checks as the AWS
  target): the manifests against an API server, the sweeper and the canary (with its
  read-only guard) against the in-memory UpCloud, the helpers against a `curl` stub, the planner rules.

#### The Vultr lane

`test/e2e-vultr/` is the same lane for Vultr (plan 20, section 9): the control plane, the tailnet,
the runner and the shape of the scenarios are the AWS ones, the cloud calls are `curl` against
the Vultr API with `VULTR_API_KEY` (passed to curl through a config on a pipe, never in an
argument). It is not in `test/e2e-vendors.txt` until the account's limits are set (see
[Vultr](testing-and-releasing.md#vultr)), so the workflows do not run it by themselves.

The plans the lane launches are `vc2-1c-1gb` (the exact-type scenarios) and `vhf-1c-1gb`
(checked in stock by the preflight and the canary), and `vc2-1c-1gb`, `vc2-1c-2gb` and `vc2-2c-4gb`
for the described-not-named one. There is no arm64, spot, placement-group, storage-profile or quota
scenario: Vultr has none of them in v1. The lane's NodeClasses set `networking.isolation: none`
explicitly (the default, `run`, puts the nodes in a managed VPC).

| Scenario | What it proves |
|---|---|
| `amd64` | exact plan `vultr.vc2-1c-1gb`: Node Ready, Pod Running, `kubectl logs` and `exec`; the labels with the zone equal to the region; the instance's status, plan, tags (cluster, NodeClaim, NodePool, NodeClass, managed-by, the idempotency key, the NodeClass's own), the managed deny-all firewall group with its ownership marker, no VPC; the price and billing-start annotations; the bootstrap token, the serving CSR, the units and the cluster DNS |
| `described` | a Pod that asks for 2 vCPUs and more than 3000 MiB gets `vc2-2c-4gb` |
| `byo` | a firewall group (by ID) and a VPC (by description) created through the API are used, not created or deleted |
| `fw` | the managed firewall group has the rules of `spec.networking.inbound` and is deleted after the last node (`perUse`) |
| `vpc` | `isolation: nodeclass` creates a managed VPC and attaches the instance |
| `negative` | `publicIPv4: false` makes the NodeClass not `VendorReady`; a Pod that needs arm64, and one that selects a storage profile with a `vultr` block, stay Pending with no NodeClaim |
| `drift` | a change of the `vultr` block (inbound rules) replaces the node, with a restart of the controller during the create (the replay finds the first instance by its idempotency tag) |
| `scalezero` | every instance is deleted within two minutes of the Pods' deletion; the snapshot, the managed group and the brought objects are still there |
| `uninstall` | the install how-to's procedure; the managed groups and VPCs go by their lifecycle |

The sweeper (`.github/workflows/e2e-vultr-sweeper.yml`, `go run ./test/e2e-vultr/sweeper`, engine
`pkg/vendors/vultr/sweep`) deletes the instances of every `scarpenter-ci-*` cluster older than two
hours (by their tag), and the firewall groups and VPCs whose description names such a cluster and
that no instance uses; **never a snapshot**. The canary (`test/canary/vultr.go`) checks the key, the
region, the snapshot and the plans in stock, behind a guard that refuses every call that is not a Get
or a List (`test/canary/vultr_guard.go`, proved by reflection). `hack/dev.sh make
test-e2e-vultr-dryrun` runs the manifests against an API server, the sweeper and the canary
against the driver's in-memory Vultr API, and `selftest.sh`.

### The L5 lane of Hetzner Cloud

The second vendor lane (`test/e2e-hcloud/`, `e2e-cloud.yml` with `vendor: hcloud`; plan 15) is not in `test/e2e-vendors.txt` until the project's server limit and usage alert are set (see [Per-vendor cost guards](testing-and-releasing.md#per-vendor-cost-guards)), so the workflows do not run it by themselves. It has the
same shape as the AWS one: a k3s control plane on the runner, nodes that join over the tailnet, the
same scripts and the same tiers, mode, approval and release gate ([Testing and
releasing](testing-and-releasing.md)). What differs is the cloud:

| | Hetzner Cloud |
|---|---|
| Credentials | no federation: the secret `HCLOUD_TOKEN` of the environment `e2e` (a **read-write** token of a dedicated CI project). The workflow puts it in the job's environment and masks it; `install.sh` makes the Secret `hcloud-credentials` the chart reads (`vendors.hcloud.credentials.secret.existingSecret`). |
| Images | `bootstrap.sh` installs `hcloud` and `hcloud-upload-image` with `go install` (the module proxy's checksum database verifies them) and makes **one Flatcar snapshot per architecture** for the pinned version when the project has none, in parallel (about five minutes), with the [label convention](hcloud-images.md); the snapshots are kept. |
| Control plane | on the runner, as in the AWS lane: no server of the project is spent on it. |
| Variables | `E2E_HCLOUD_LOCATIONS` (default `fsn1,nbg1,hel1`: the arm and the cheap types are there; the NodeClasses' `regions`), `TS_OAUTH_CLIENT_ID`, `TS_AUDIENCE`. |
| Types | `cx23` (amd64, the database and its client), `cax11` (arm64), and `cx23` or `cpx22` for the described-not-named scenario. All on-demand: Hetzner has no spot. |
| Cost | about eight servers for one started hour each of the cheapest types (cents), two small volumes, the snapshots kept (about 1 GB, under a cent a month). The whole run's limit is the same 90 minutes. |

**Guards.** A Hetzner token cannot be restricted by type, location or label, so there is no CI policy
to print (`ci-policy.sh` and `canary-policy.sh` have no analogue): the token is the whole project. What
bounds a bug: the **dedicated project** (nothing else in it), a **server limit** requested low in the
Console (the lane needs at least ten: it runs up to nine servers at once; `E2E_HCLOUD_SERVER_LIMIT`
tells the controller), a **usage alert** to a person, the lane's NodePools that list the types
(`cx23`, `cax11`, `cpx22`; `manifests_test.go` keeps them and this page together), `E2E_HCLOUD_LOCATIONS`,
the label `scarpenter.sparecores.com/cluster=scarpenter-ci-<run id>` and `ci-created-at` on every
resource of a run, the sweeper, and the teardown that fails the job when anything labelled remains.

**Scenarios** (`E2E_SCENARIOS`, in this order): `amd64` (`hcloud.cx23`: node Ready, the Pod, `kubectl
logs` and `exec`, node labels with zone = zone ID = location, the server's labels and the
`idempotency-key`, the root filesystem expanded to the type's disk, the time from the create call to
Ready), `arm64` (`hcloud.cax11`, standing in for AWS's spot scenario), `db` (a StatefulSet with the
`db` storage profile and a client Deployment, both with the `spread` placement profile and one run key:
one placement group named and labelled by SCarpenter with both servers in it, a 10 GB volume attached,
mounted by its by-id link and formatted by the first-boot unit; after the deletion the group and the
volume are gone), `described` (a selection by `vcpus` and a `metric-...-pct` threshold within the lane's
types), `byo` (the workflow creates a labelled network with a cloud subnet and a firewall with the
`hcloud` CLI; a NodeClass `use`s them by label, with `networking.isolation: nodeclass`; the server has a
private IP in the network and the firewall is applied, and both survive until the run ends),
`offerings` (the launched type's entry in `GET /server_types` is `available` in its location, and an
offering the API lists as unavailable is not scheduled), `negative` (a server without a public address
is refused; an arm type pinned to a location without it stays Pending with no launch attempt), `drift`
(a profile no node uses drifts nothing; `networking.publicIPv6` replaces the node and the replacement
has an IPv6 address), `scalezero` (servers gone within two minutes of the Pods' deletion, no Primary IP
left, which asserts that Hetzner deletes the Primary IPs it creates with a server, plan 15's open
question 6), and `final` (the uninstall, and nothing labelled with the run remains: servers, volumes,
placement groups, firewalls, networks, Primary IPs). The smoke set is `amd64,scalezero`. The restart-
during-create assertion of the AWS lane has no analogue (the idempotency is the server name).

**Provisioning (outside this repository).** A dedicated Hetzner project (projects cannot be created through
the API) with a low server limit and a usage alert; two tokens, a read-write one for the environments `e2e`
and `e2e-sweeper` and a **read-only** one for `e2e-canary`, all as the secret `HCLOUD_TOKEN`; the optional
variable `E2E_HCLOUD_LOCATIONS`; the tailnet's tags as for AWS.

**The sweeper** (`e2e-hcloud-sweeper.yml`, every three hours, environment `e2e-sweeper`, from `main` only)
runs `go run ./test/e2e-hcloud/sweeper`: it lists servers, volumes, placement groups, firewalls and Primary
IPs by the presence of the cluster label (the API cannot match a prefix), keeps those of clusters named
`scarpenter-ci-*` that are older than two hours by the API's own creation time, and deletes them in the
order servers (waited for), volumes, placement groups, firewalls, Primary IPs, a failed deletion not
stopping the others. The Flatcar snapshots are never listed. The teardown runs it for its own cluster with
`-cluster <name> -max-age 0`, then deletes the brought network with the CLI (the driver's API has no network
delete). `-dry-run` reports only. A Primary IP is found only if it carries the cluster label, which Hetzner
may not copy from the server (unverified): the teardown and the `scalezero` scenario check the IDs instead.

**Without Hetzner.** `hack/dev.sh make test-e2e-hcloud-dryrun` (CI runs it): `manifests_test.go` renders
every manifest of `test/e2e-hcloud/manifests` and sends it to an envtest API server (CRDs of SCarpenter and
Karpenter core); `selftest.sh` runs the helpers of `lib.sh` against stubs (redaction includes Hetzner tokens),
checks that every scenario has a function, that every action is pinned, that the lane holds no key or local
path, and renders the chart with the lane's values; the sweeper's tests run against the in-memory API of the
driver; `test/canary` runs the Hetzner canary against it too and checks both read-only guards.

### The L5 lane of OVHcloud

`test/e2e-ovh/` is the OVH lane (`vendor: ovh` of the same reusable workflow; plan 18), statically
validated only until its first live run, and **not in `test/e2e-vendors.txt`** until the guards of [Per-vendor cost guards](testing-and-releasing.md#per-vendor-cost-guards) are in place (below), so the workflows do not run it by themselves: nodes on OVHcloud Public Cloud (Nova servers on the
public `Ext-Net` network) that join over Tailscale, in one region (`OVH_REGION`, a Keystone catalog
name, default `GRA11`). The scenarios are `amd64` (an exact flavor, on-demand: node labels, server
metadata and tags, the managed security group and its marker, config drive, `kubectl logs` and `exec`,
the units on the node, the `metadata-from-pods` hardening), `db` (an anti-affinity server group and a
`high-speed` data volume mounted by the first-boot unit), `described` (the flavor picked from
vCPU, memory and a benchmark percentile), `byo` (a security group the workflow creates is `use`d and
SCarpenter never deletes it), `negative` (`publicIPv4: false` is not ready and refused by the API
server's CEL rule; a profile that asks for local storage stays Pending without a NodeClaim), `quota` (Nova's
absolute limits equal the controller's `scarpenter_quota_limit`, and `used` follows launch and delete),
`drift` (a root disk that becomes a volume replaces the node), `poweroff` (a guest that powers itself off
leaves a `SHUTOFF` server that the garbage collector deletes: the OVH-specific case), `scalezero` and
`uninstall`. There is no arm64, no spot and no local-storage scenario: OVH has none of them.

- **Flavors** (`defaults.sh`): `d2-2` (shared CPU, the cheapest) for the probes, `b3-8` for the database and its
  client, and `d2-2`, `d2-4` and `b3-8` for the described scenario. No IAM condition limits flavors, so the guard is
  the lane's own types plus the project's core and instance quota as the ceiling.
- **Identity**: a dedicated OpenStack user of the lane's own Public Cloud project with the five operator roles
  ([OVH credentials](ovh-credentials.md)): the environment secrets `OVH_OS_USERNAME` and `OVH_OS_PASSWORD` (OVH
  documents no GitHub OIDC trust, so the password is stored), the variables `OVH_PROJECT_ID`, `OVH_REGION` and
  optionally `OVH_AUTH_URL`. The lane's scripts use the `openstack` CLI (the workflow installs `python-openstackclient`
  pinned, with every dependency pinned by the OpenStack upper constraints of its release, 2025.2).
- **Run tag**: the server tag and metadata `scarpenter-cluster=scarpenter-ci-<run id>`, the managed security group's
  description marker (it records its creation time) and the server groups' name prefix.
- **Sweeper**: `e2e-ovh-sweeper.yml`, every three hours, environment `e2e-sweeper`: `pkg/vendors/ovh/sweep` deletes the
  servers of `scarpenter-ci-*` clusters older than two hours (by the server's own creation time), then the managed
  security groups (by marker and age) and server groups (by name, and only when the cluster has no server left and its oldest group is
  old enough), never images. `go run ./test/e2e-ovh/sweeper -dry-run` lists what it would delete. Volumes are not swept: the driver's
  volumes go with their server, and a leaked one is found by the teardown's check.
- **Canary**: `test/canary/ovh.go` (login and the catalog's regions and zones, `Ext-Net`, the pinned image resolved to exactly one,
  Nova's cores and instances limits against `E2E_MIN_VCPU_OD` and `E2E_MIN_INSTANCES`, the flavors offered and mapped to a quota),
  behind `ovh_guard.go`, which refuses every write (a reflection test over the four OpenStack interfaces), with a second OpenStack
  user in the Infrastructure Supervisor (`infrastructureSupervisor`) role, read-only: the secrets `OVH_OS_CANARY_USERNAME` and
  `OVH_OS_CANARY_PASSWORD` of the environment `e2e-canary`. Whether that role can list images, networks and flavors is spike S6.
- **Cost**: about $0.15 per full run, an estimate from Keeper's `b3-8` price of 0.0656 USD per hour for a handful of small
  servers for minutes plus a few volumes (the `d2-2` price is not in this repository); EU regions bill per second, a US region
  may round up to the hour (plan 18, "Billing period"). Images stay in the project (cents per month).
- **Without OVH**: `hack/dev.sh make test-e2e-ovh-dryrun` renders every manifest against an envtest API server, runs `selftest.sh`
  (helpers, redaction of OpenStack IDs and tokens, the chart with the lane's values), the sweeper's and the canary's tests
  against the driver's in-memory OpenStack.
- **Before the lane is listed**: no flavor deny is possible, so the lane's flavors and the project's core and instance quota are the ceiling,
  and the project's spending alert is unverified. The operator sets the quotas low, creates the alert and checks it fires (plan 18), and
  then adds `ovh` to `test/e2e-vendors.txt`.
- **Provisioned outside this repository**: the project (one active region, low quotas, a spending alert), the two users, the
  images of [Upload the Flatcar image to OVH](ovh-images.md) in the lane's region, the environment secrets and variables above.
### The L5 lane of Azure

`test/e2e-azure/` has the same script names as `test/e2e-aws/`, with Azure's own scenarios (the proximity
placement group and Premium data disk of `db`, the NIC, public IP and disk check of `scalezero`, the
marketplace terms in the preflight), a sweeper (`pkg/vendors/azure/sweep`, `.github/workflows/e2e-azure-sweeper.yml`)
and a canary (`test/canary/azure.go`, read-only through a guard that refuses every write).
`make test-e2e-azure-dryrun` runs what needs no cloud: the manifests against the CRD, the sweeper and the canary against the
in-memory ARM, the selftest. The identity is GitHub's OIDC straight to Azure, with the federated token refreshed in
the background for the controller. **The lane is not in `test/e2e-vendors.txt`**: a line may be added only when every
cost guard (an Allowed-SKU and an Allowed-locations policy on the lane's resource group, the tags, the sweeper, the
quota ceiling and a budget alert, the read-only canary role) is true on a live account, and nobody has verified that yet
(see [Azure](testing-and-releasing.md#azure)); the lane's selftest checks that the line is absent.
Nothing in the lane has run against Azure.

### The L5 lane on Google Cloud

The GCP lane is the same lane as the AWS one (a k3s control plane on the runner, nodes that join over
Tailscale, the reusable `e2e-cloud.yml`, the same pins of Kubernetes, k3s, Tailscale and Flatcar) with the
cloud calls of Google Compute Engine; everything it adds lives in `test/e2e-gcp/`, `pkg/vendors/gcp/sweep`,
`test/canary/gcp.go` and `.github/workflows/e2e-gcp-sweeper.yml`. **It is not in `test/e2e-vendors.txt` yet**:
the line is added only when the cost guards of [Testing and releasing](testing-and-releasing.md#per-vendor-cost-guards)
are true on a live project, which is provisioned outside this repository, and nobody has verified them yet (what to
check first: [GCP](testing-and-releasing.md#gcp)); the lane's selftest checks that the line is absent. Everything else is ready, so that the
single line `gcp` in that file is enough: the planner, the canary matrix and the release gate read it,
and `test/e2e-plan-selftest.sh` and `test/e2e-gcp/selftest.sh` check that nothing else is missing (the
credentials steps of both reusable workflows, the sweeper workflow, the canary's vendor part, the smoke set).
Until then it runs by hand against a project of one's own:

```bash
export GCP_PROJECT=my-project GCP_REGION=us-central1     # the lane's variables; the project has no default
test/canary/run.sh gcp                                    # the free check (read-only identity)
go run ./test/e2e-gcp/sweeper -project my-project -dry-run -regions us-central1
hack/dev.sh make test-e2e-gcp-dryrun                      # everything that needs no project
```

**What differs from AWS.**

- **A dedicated project.** An IAM condition on Compute can test the resource name and type, **not labels** and not
  the machine type, so the AWS statements "only with the cluster tag" and "only these instance types" have no
  equivalent. The wall is the driver's own ownership check plus a project that holds nothing else, the CI
  identity's write permissions limited to the lane's region and to names that start with `scarpenter-`, and the CPU
  quotas of the project as the ceiling. That every Compute method honours such a name condition is **unverified**; the
  first live run reads the denials in the audit log.
- **Credentials.** The CI identity is a GitHub OIDC token exchanged through workload identity federation
  (`google-github-actions/auth`, the provider and the service account in environment secrets; no key file).
  `install.sh` puts the external-account credential file the action wrote into the Secret `gcp-credentials`
  (`vendors.gcp.credentials.mode: secret`); the cluster's own ServiceAccount issuer is not public, so the chart's
  workload identity mode is a manual walk, as AWS web identity is. The file asks GitHub for a fresh OIDC token whenever it needs one (the runner's token endpoint, reachable from the cluster that runs on the same runner), so there is no hour-long credential to expire; that this holds for the whole run is **unverified** until the first live run.
- **Images.** The pinned Flatcar release is read **by name** from `kinvolk-public` (`flatcar-<channel>-<version with dashes>`);
  the images cannot be listed. There is no official arm64 image on GCE: the `arm64` scenario and the canary's
  arm64 check run only with `E2E_ARM64=true` and `E2E_ARM64_IMAGE_PROJECT` (the project that holds the Spare Cores image).
- **Types.** `gcp.e2-small` (amd64, on demand and Spot), `gcp.e2-medium` and `gcp.e2-standard-2` (the described scenario),
  `gcp.c3-standard-4` (the database: it takes Hyperdisk Balanced and compact placement, and needs the `GVNIC` guest OS
  feature on the image, which the preflight and the canary read), and `gcp.t2a-standard-1` (arm64, optional). The pools
  of `test/e2e-gcp/manifests/nodepools.yaml` name the same types, and a test keeps this page and them together.
- **Quotas.** About 24 vCPUs of `CPUS` and 4 of `PREEMPTIBLE_CPUS` in the region (`E2E_MIN_VCPU_OD`, `E2E_MIN_VCPU_SPOT`).
  A region without preemptible quota (limit 0) runs Spot VMs on `CPUS`, so then the preflight and the canary want both
  added up. GCP reports the usage with the limit, so the `quota` scenario compares the controller's buckets with
  `regions.get` and nothing is computed from instances.
- **Ownership marker.** Instances and disks carry the label `scarpenter-cluster=scarpenter-ci-<run id>` (and
  `ci-created-at`, `ci-run` from the NodeClass's `spec.tags`). Networks, subnetworks, firewall rules and resource
  policies have no labels: they are SCarpenter's when the name starts with `scarpenter-<cluster>-` **and** the
  description is the ownership marker (`scarpenter: cluster=<c> nodeclass=<n> managed-by=scarpenter
  resource-kind=<kind>`). What the workflow brings itself for `byo` is named `scarpenter-e2e-byo-<run id>*` and described
  `scarpenter-e2e-byo: cluster=<cluster>`, a marker the controller's garbage collector does not recognize and the sweeper does.

| Scenario | Proves |
|---|---|
| preflight | the pinned Flatcar image exists, is `READY`, amd64, with `GVNIC`; every lane type is offered in the lane's zone; the `CPUS` and `PREEMPTIBLE_CPUS` quotas suffice; whether the project has a default network (the `default` scenario needs it); the NodeClasses are `Ready`, `VendorReady` and `ImagesReady` |
| `amd64` | on demand, exact type `gcp.e2-small`: Node Ready, Pod Running, `kubectl logs` and `exec`; node labels of overview section 5.5 (the zone name and the numeric zone ID consistent with `gcloud compute zones describe`); the instance's labels (cluster, nodeclaim, nodepool, nodeclass, managed-by, the idempotency key equal to the NodeClaim's UID, the NodeClass's `spec.tags`); the metadata keys of plan 16 section 3 (`user-data`, `block-project-ssh-keys=TRUE`, `enable-oslogin=FALSE`, no `ssh-keys`), no service account, an external IPv4; the root disk (`pd-balanced`, 20 GB, from the pinned image, labelled, deleted with the instance); the price annotation; the bootstrap token is gone, the serving CSR approved; on the node, the checks of L4 scenarios Q09 and Q10; the metadata server answers on the node (the user-data is the Ignition) and no login user exists (project SSH keys and OS Login are off); the answer from a Pod is recorded as a note, the exposure that the reference documents |
| `spot` | `gcp.e2-small` Spot: `provisioningModel: SPOT`, **`instanceTerminationAction: DELETE`** (the API's default is `STOP`, which leaves a stopped instance and its disks), no automatic restart, `onHostMaintenance: TERMINATE` |
| `arm64` | optional (`E2E_ARM64=true`): `gcp.t2a-standard-1` Spot on the Spare Cores arm64 image; the image's architecture is `ARM64`, the join units are active |
| `db` | a `c3-standard-4` StatefulSet with the `db-hd` storage profile (a 10 GB Hyperdisk Balanced data disk named `pgdata`) and a client Deployment, both `colocated` with one run key: **one compact resource policy**, named `scarpenter-<cluster>-...` (at most 63 characters) and described by SCarpenter, both instances in it and in one zone; the data disk has the device name `pgdata`, `autoDelete: true` (not GCE's default), the cluster label, and is found at `/dev/disk/by-id/google-pgdata`, mounted as xfs at `/var/lib/pgdata`; the labels of the applied profiles; `C3_CPUS` usage covers the two instances; after deleting both workloads the policy and the disk are gone |
| `described` | a Deployment that selects by `vcpus`, `memory-mib` ranges and a `metric-single-core-pct` threshold within the allowed test types gets a type that satisfies all of them (checked against `machine-types describe`) |
| `byo` | a custom network, a subnet and a firewall rule that the workflow creates with `gcloud` are `use`d by a second NodeClass (the subnet by its resource ID, the rule by its network tag): the node launches into them, carries the tag, and after the workload, the pool and the NodeClass are gone SCarpenter has not touched them (the rule is unchanged); the teardown (the sweeper) deletes them |
| `create` | no `use`: SCarpenter creates a custom-mode network `scarpenter-<cluster>-...`, a subnet of the region from the pool `10.240.0.0/12` and firewall rules for the NodeClass's network tag, all described with the ownership marker; the node uses them; once the NodeClass is deleted its rules go by their lifecycle |
| `default` | only when the project has a `default` network: a NodeClass without a resources block launches into the `default` network and its subnet of the region, and leaves the network alone |
| `negative` | a NodeClass with another project than the controller's is refused (`VendorReady=False`, the message names the project); what `publicIPv4: false` does is recorded as a note for the first live run; no instance is launched for either |
| `quota` | the buckets the controller reports (`scarpenter_quota_limit`, `scarpenter_quota_used` for `CPUS`, `PREEMPTIBLE_CPUS`, the per-series metrics and `INSTANCES`) equal `regions.get`; `CPUS` usage covers the running e2 instances; the in-flight reservations are released; the launched types are in `machine-types` of their zones |
| `drift` | as on AWS with the root disk size (`vendors.gcp.storage.root.sizeGb`): a change to a profile no node uses drifts nothing, a change to the block replaces the node (new root size, old instance deleted, NodeClaim and Node gone, the Pod moves), the controller Pod is killed hard while the replacement's `Create` is in flight (opportunistic replay assertion). Always asserted: no orphan, and **at most one successful `instances.insert` operation for the replacement claim** (deleted instances vanish from the API, but the project's operations are listed) |
| `scalezero` | the exit criterion: after the workloads are deleted every instance of the cluster is gone within 2 minutes; no node, NodeClaim, bootstrap token or **disk** is left; the quota usage equals `regions.get` again |
| `uninstall` | the procedure of [Uninstall](install.md#uninstall): the NodePools, the SCNodeClasses (the managed firewall rules, subnets and networks fall under `managedResources.defaultTTL`), then `helm uninstall` |

**Provisioning (outside this repository).** A maintainer provisions once, with whatever tool they use; the repository holds none of it:

- a project for the lane and nothing else, with a budget alert, the Compute Engine API, and the quotas above in the lane's region
  (new projects start low; the project's other regions can stay at their defaults);
- a workload identity pool and a GitHub OIDC provider (`https://token.actions.githubusercontent.com`) with an attribute mapping of
  `assertion.repository` and `assertion.environment` and an attribute condition that names the repository and the environments
  `e2e`, `e2e-sweeper` and `e2e-canary`, and two service accounts: the CI one, which the principal set of the environments `e2e` and
  `e2e-sweeper` may impersonate (`roles/iam.workloadIdentityUser` on the service account), and the canary one, for `e2e-canary` only;
- the roles: `test/e2e-gcp/ci-policy.sh bindings <project> serviceAccount:<ci service account> <region>` prints the `gcloud` commands for
  three custom roles (the reads; the regional writes, bound with a condition on the resource name `projects/<project>/zones/<region>-*` and
  `projects/<project>/regions/<region>/*`; the global network writes, bound with a condition on names that start with `scarpenter-`), and
  `ci-policy.sh role reads|regional|global` the YAML role definitions; `test/e2e-gcp/canary-policy.sh bindings <project> serviceAccount:<canary service account>`
  one read-only role. No predefined role is used (the instance admin role is wider). Nothing grants `iam.serviceAccounts.actAs`: the nodes carry no service account;
- the GitHub environments: `e2e` (required reviewers), `e2e-sweeper` (limited to `main`, no reviewers) and `e2e-canary` (limited to `main`) with the
  variables `GCP_PROJECT` and `GCP_REGION` (the region is optional, `us-central1`) and the secrets `GCP_WORKLOAD_IDENTITY_PROVIDER` (the provider's resource
  name) and `GCP_SERVICE_ACCOUNT` (the service account's e-mail address; the canary's own in `e2e-canary`); the Tailscale variables and tags are the AWS lane's;
- for `byo`, nothing: the workflow creates and deletes its own network; for `default`, a project that still has its `default` network (an organization
  policy often removes it; the scenario then skips itself).

**The sweeper.** `.github/workflows/e2e-gcp-sweeper.yml` runs every three hours and on demand in the environment `e2e-sweeper`. It
deletes, in dependency order, the instances, the resource policies, the firewall rules, the subnetworks and the networks of every cluster whose
name starts with `scarpenter-ci-` and that is more than two hours old, in the lane's region (`-regions` takes a comma list or `all`). The age is the
creation timestamp (every one of these resources has it). `pkg/vendors/gcp/sweep` is tested against the in-memory Compute API (old and young
runs, another cluster's resources, a name without its marker and a marker without its name, the regions narrowing with a network kept alive by a subnet of another
region, the byo resources by their own marker, a resource still in use retried, a failed operation, a zone that does not answer, a region without access, a dry
run); `go run ./test/e2e-gcp/sweeper -dry-run` lists what a sweep would delete and `-cluster <name> -max-age 0 -regions <region>` is what the teardown uses.
Disks are not swept: the driver creates every disk with `autoDelete`, `ComputeAPI` has no disk calls, and the teardown lists disks by the cluster
label with `gcloud` and fails the job on a leftover (which it then deletes). Every call of the sweeper and of the canary runs under a deadline
(`gcp.WithTimeouts` and a context deadline for the whole run).

**Without a project.** `hack/dev.sh make test-e2e-gcp-dryrun` renders every manifest of `test/e2e-gcp/manifests` and sends it to an envtest API
server as a server-side dry run, runs the sweeper's and the canary's tests against the in-memory Compute API, and `test/e2e-gcp/selftest.sh`: the helpers
against stubs and fixtures (including the redaction of project ids, Google tokens and private keys), the scenario list, the workflows (federated credentials, no key file,
every action pinned by SHA), the byo names shared by the scripts and the sweeper, the roles `ci-policy.sh` and `canary-policy.sh` print (the driver's
permissions all there, no IAM, no overlap between the three, only reads in the canary's), that the lane is ready for its line in `test/e2e-vendors.txt`, and the chart
with the lane's values once the chart knows `vendors.gcp`. The canary's test compares the permissions of the calls it made with `canary-policy.sh role`.

### The L5 lane on OCI

The OCI lane is the same lane as the AWS one (a k3s control plane on the runner, nodes that join over
Tailscale, the reusable `e2e-cloud.yml`, the same pins of Kubernetes, k3s, Tailscale and Flatcar) with the
cloud calls of Oracle Cloud Infrastructure; everything it adds lives in `test/e2e-oci/`, `pkg/vendors/oci/sweep`,
`test/canary/oci.go` and `.github/workflows/e2e-oci-sweeper.yml`. **It is not in `test/e2e-vendors.txt` yet**:
the line is added only when the cost guards of [Testing and releasing](testing-and-releasing.md#per-vendor-cost-guards)
are true on a live tenancy, which is provisioned outside this repository, and nobody has verified them yet (what to
check first: [OCI](testing-and-releasing.md#oci)); the lane's selftest checks that the line is absent. **Nothing in the lane has run
against Oracle yet** (a static validation only: the lane is written against the driver's fake, the SDK source and the documentation, and
the first live run corrects it). Everything else is ready, so that the single line `oci` in that file is enough: the planner, the canary
matrix and the release gate read it, and `test/e2e-plan-selftest.sh` and `test/e2e-oci/selftest.sh` check that nothing else is missing
(the credentials steps of both reusable workflows, the sweeper workflow, the canary's vendor part, the smoke set). Until then it runs by
hand against a tenancy of one's own:

```bash
export OCI_COMPARTMENT=ocid1.compartment... OCI_TENANCY=ocid1.tenancy...      # the lane's variables; no account has a default
# optional: export OCI_IMAGE_COMPARTMENT=ocid1.compartment...  # where the Flatcar images are imported, if not OCI_COMPARTMENT
export OCI_CLI_CONFIG_FILE=~/.oci/config OCI_CLI_PROFILE=DEFAULT OCI_REGION=eu-frankfurt-1
test/canary/run.sh oci                                          # the free check (read-only identity)
go run ./test/e2e-oci/sweeper -dry-run -regions eu-frankfurt-1  # what a sweep would delete
hack/dev.sh make test-e2e-oci-dryrun                            # everything that needs no tenancy
```

**What differs from AWS.**

- **A dedicated compartment.** A policy condition cannot test the freeform tags of an instance on every verb, and cannot
  deny a shape, so the AWS statements "only with the cluster tag" and "only these instance types" have no equivalent. The wall
  is the driver's own ownership check plus a compartment that holds nothing else, a **compartment quota** (the Quotas service) that
  caps compute cores and memory at what the pins need (the hard ceiling), and the CI policy's condition on the lane's region
  (unverified for which verbs).
- **Credentials: the keyless token exchange.** The CI identity is GitHub's OIDC token exchanged at an IAM identity domain (a
  confidential application, a trust for this repository's environments, a group with the policy) for a short-lived user principal
  session token; **no API key exists anywhere**. `test/e2e-oci/oci-session.sh` does it for the `oci` CLI of the scripts (a profile with a session token and
  a key pair it makes), keeps the OIDC token file fresh in the background (GitHub's token lives minutes) and puts that file into the Secret
  `oci-federated-token`; the controller's `workloadIdentityFederation` mode exchanges it itself (`install.sh`; the cluster's own ServiceAccount issuer is
  not public, so the chart's projected token could not be trusted by the domain: the lane sets the mode `none`, which renders no OCI credential flag, and passes
  the flags with `extraArgs`; the selftest renders the chart and checks that they are the only ones. The lane tests the driver's mode, not the chart's projected token). The sweeper and the canary (Go) exchange the same
  file through `test/e2e-oci/ocienv`. The one stored secret is the application's client secret (`OCI_CLIENT_SECRET`, an environment secret).
  The audience the trust expects, the exact fields of the token endpoint's answer and that the CLI's `security_token` profile needs no fingerprint are **unverified**
  until the first live run (spike S11).
- **Images.** The lane's operator imports Flatcar's own `oraclecloud` image into the compartment (or into the compartment `OCI_IMAGE_COMPARTMENT` names, which the lane passes to the controller as `vendors.oci.imageCompartment` and to the preflight and the canary), per architecture, named
  `flatcar-<channel>-<version>-<arch>` with the freeform tags of [Flatcar images on OCI](oci-images.md); the preflight and the canary read them
  and their shape compatibility. arm64 runs only with `E2E_ARM64=true` (the image needs a capability schema and the Arm shapes added).
- **Types.** `oci.VM.Standard.E5.Flex.1-4` (amd64, on demand and preemptible; 1 OCPU is 2 vCPUs, 4 GB), `VM.Standard.E5.Flex.1-8` and
  `VM.Standard.E5.Flex.2-8` (the described scenario), `VM.Standard.A1.Flex.1-2` and `VM.Standard.A2.Flex.1-8` (arm64, optional: the pool takes whichever has capacity).
  The pools of `test/e2e-oci/manifests/nodepools.yaml` name the same types, and a test keeps this page and them together. No bare metal (cost and boot time).
- **Limits.** About 8 OCPUs and 48 GB of the E5 family in the region (`E2E_MIN_OCPU`, `E2E_MIN_MEMORY_GB`). The Limits service reports `used` and `available` per availability
  domain, so the `quota` scenario compares the controller's buckets (`standard-e5-core-count`, `standard-e5-memory-count`: usage is the sum over the domains,
  the limit is that plus the largest availability) with what `oci limits resource-availability get` says; preemptible capacity is counted like any other until
  the first live run says otherwise (spike S9).
- **Ownership marker.** Instances carry the freeform tags `scarpenter-cluster=scarpenter-ci-<run id>` (and `ci-created-at`, `ci-run` from the NodeClass's `spec.tags`).
  The VCN, the internet gateway, the route table, the security list, the subnet and the groups SCarpenter creates carry `scarpenter-cluster`, `scarpenter-managed-by`,
  `scarpenter-nodeclass` and `scarpenter-resource-kind` **and** a name that starts with `scarpenter-<cluster>-`. What the workflow brings itself for `byo` is named
  `scarpenter-e2e-byo-<run id>*` and tagged `e2e-byo-cluster=<cluster>`, a tag the controller's garbage collector does not recognize and the sweeper does.
- **No default network, no data volumes.** There is no `default` scenario (a tenancy has no default VCN) and no `db` scenario (no data volumes or profiles in v1);
  the `drift` scenario changes the boot volume size only.

| Scenario | Proves |
|---|---|
| preflight | the region is subscribed (`READY`) and has availability domains; the pinned Flatcar image exists in the image compartment (`OCI_IMAGE_COMPARTMENT`, default the compartment; `AVAILABLE`, its `flatcar-version` and `arch` tags) and is registered for the lane's shapes; every lane shape is offered in an availability domain; the E5 limits suffice; the NodeClasses are `Ready`, `VendorReady` and `ImagesReady` |
| `amd64` | on demand, exact type `oci.VM.Standard.E5.Flex.1-4`: Node Ready, Pod Running, `kubectl logs` and `exec`; node labels of overview section 5.5 (the zone name without the tenancy's prefix, the Keeper zone ID `<region>-ad-<N>`, `vcpus` 2, `memory-mib` 4096); the instance's freeform tags (cluster, nodeclaim, nodepool, nodeclass, managed-by, the idempotency key equal to the NodeClaim's UID, the NodeClass's `spec.tags`); `shapeConfig` equal to the name; the metadata keys of plan 25 section 5 (`user_data`, no `ssh_authorized_keys`), the legacy metadata endpoints disabled, a public address and a group on the VNIC; the pinned image; the boot volume (size and VPUs); the price annotation; the bootstrap token is gone, the serving CSR approved; on the node, the checks of L4 scenarios Q09 and Q10; the metadata service answers on the node (the `user_data` is the Ignition) and no login user exists; the answer from a Pod (OCI has no hop limit) and from the legacy endpoint are recorded as notes |
| `spot` | the same size preemptible: `preemptibleInstanceConfig` with the action `TERMINATE` and the boot volume not preserved |
| `arm64` | optional (`E2E_ARM64=true`): `A1` or, where its capacity is short, `A2` on the arm64 image; the five launch options of a Flatcar arm64 image (`UEFI_64`, paravirtualized network and volumes, consistent volume naming); the join units are active |
| `described` | a Deployment that selects by `vcpus` and `memory-mib` ranges within the allowed sizes gets `VM.Standard.E5.Flex.1-8` (the only size with 2 vCPUs and more than 6000 MiB; checked against the instance's `shapeConfig`) |
| `byo` | a VCN, an internet gateway, a route table, a subnet and a network security group that the workflow creates with the `oci` CLI are `use`d by a second NodeClass (by their OCIDs): the node launches into the subnet with that group and no other, and after the workload, the pool and the NodeClass are gone SCarpenter has not touched them (the subnet and the group, rules included, are unchanged); the teardown (the sweeper) deletes them |
| `create` | no `use`: SCarpenter creates a VCN `scarpenter-<cluster>-...` of `10.240.0.0/16`, an internet gateway, a route table, a security list, one regional public subnet (the first `/20`) and a group for the NodeClass, all with the ownership tags; the node uses them; once the NodeClass is deleted its group goes by its lifecycle |
| `negative` | a NodeClass with a region the driver cannot use and one with `publicIPv4: false` (the managed network has no NAT gateway) are refused (`VendorReady=False`, the messages name the region and `publicIPv4`); no instance is launched for either |
| `quota` | the buckets the controller reports (`scarpenter_quota_limit`, `scarpenter_quota_used` for the E5 core and memory limits) equal the Limits service; the usage covers the running nodes; the in-flight reservations are released; the launched shape is in the shape table of its availability domain |
| `drift` | a change to the boot volume size replaces the node (new size, old instance terminated, NodeClaim and Node gone, the Pod moves), the controller Pod is killed hard while the replacement's `Create` is in flight (opportunistic replay assertion). Always asserted: no orphan, and **at most one instance ever launched for the replacement claim** (a terminated instance stays listed for about twelve hours) |
| `scalezero` | the exit criterion: after the workloads are deleted every instance of the cluster is terminated within 2 minutes; no node, NodeClaim, bootstrap token or **boot volume created by the run** is left; the quota usage equals the Limits service again |
| `uninstall` | the procedure of [Uninstall](install.md#uninstall): the NodePools, the SCNodeClasses (the managed groups, subnets, route tables, gateways, security lists and VCNs fall under `managedResources.defaultTTL`), then `helm uninstall` |

**Provisioning (outside this repository).** A maintainer provisions once, with whatever tool they use; the repository holds none of it. Nothing below has been created
or checked yet:

- a **dedicated compartment** and nothing else in it, with a **compartment quota** (the Quotas service) that caps the E5 and Arm compute cores and memory at what the pins need, a budget alert, and the
  subscription of the lane's region;
- the **Flatcar images** of the pinned release in the compartment of the lane's region ([oci-images.md](oci-images.md): imported, named, tagged, with their shape compatibility, and for arm64 the capability schema);
- an **IAM identity domain** with a **confidential application** (client credentials) and a **trust** for GitHub's OIDC issuer (`https://token.actions.githubusercontent.com`) that maps the subjects of this repository's environments
  `e2e`, `e2e-sweeper` and `e2e-canary` to identity domain users, in a CI group (the first two) and a canary group (the last); a second application for the canary is allowed;
- the **policies**: `test/e2e-oci/ci-policy.sh policy <group> <compartment> [region] [image compartment]` prints the CI group's statements (the driver's, `manage volume-family` for the sweeper's boot volume deletion, and the region condition),
  `test/e2e-oci/canary-policy.sh policy <group> <compartment> [image compartment]` the canary's read-only ones (both add `read instance-images` in the optional image compartment);
- the GitHub environments: `e2e` (required reviewers), `e2e-sweeper` (limited to `main`, no reviewers) and `e2e-canary` (limited to `main`), each with the **variables** `OCI_COMPARTMENT`, `OCI_TENANCY`
  (OCIDs), `OCI_IDENTITY_DOMAIN_URL` (`https://idcs-<id>.identity.oraclecloud.com`), `OCI_CLIENT_ID`, and optionally `OCI_REGION` (default `eu-frankfurt-1`), `OCI_IMAGE_COMPARTMENT` (OCID; empty means `OCI_COMPARTMENT`; the sweeper ignores it and never touches images), `OCI_HOME_REGION` (default the region),
  `OCI_OIDC_AUDIENCE` (default `oci-federation`; what the trust expects), and the **secret** `OCI_CLIENT_SECRET` (the application's client secret; the canary's own in `e2e-canary`); the Tailscale variables and tags are the AWS lane's;
- for `byo`, nothing: the workflow creates and deletes its own network.

**The sweeper.** `.github/workflows/e2e-oci-sweeper.yml` runs every three hours and on demand in the environment `e2e-sweeper`. It terminates the instances (and waits: their VNICs
must be gone), then deletes, in dependency order, the network security groups, the subnets, the route tables, the internet gateways, the security lists and the VCNs of every cluster whose name starts
with `scarpenter-ci-` and that is more than two hours old, in the lane's region (`-regions` takes a comma list or `all`), and the available boot volumes of those clusters that are that old
(`-boot-volumes`: a terminate deletes the boot volume; one that stays is swept only when it carries the cluster tag, which OCI copies from the instance at launch if at all, and a volume that is still attached is refused by OCI and skipped). The age is the creation time. A resource is SCarpenter's only when its tags **and** its name say so
(`pkg/vendors/oci/sweep`), a brought resource of the lane when its name starts with `scarpenter-e2e-byo-` **and** it carries `e2e-byo-cluster=<cluster>`; anything untagged, an imported image included,
is never touched. A VCN with a subnet the sweep does not select (a user's, or one that is too young) is kept. `pkg/vendors/oci/sweep` is tested against the driver's in-memory API (old and young runs, another cluster's resources,
a marker without its name and a name without its marker, a user's subnet in a managed VCN, the byo resources by their own tag, a resource still in use retried, an instance that stays `TERMINATING`, a region without access, boot volumes,
a dry run); `go run ./test/e2e-oci/sweeper -dry-run` lists what a sweep would delete and `-cluster <name> -max-age 0 -regions <region>` is what the teardown uses. The teardown also lists the boot volumes created since the run began
with the CLI and fails the job on a leftover; it deletes none (without the run's tag nothing proves the volume is the run's), a person does. Every call runs under a deadline (the SDK adapter's, and the whole run's).

**Without a tenancy.** `hack/dev.sh make test-e2e-oci-dryrun` renders every manifest of `test/e2e-oci/manifests` and sends it to an envtest API
server as a server-side dry run, runs the sweeper's and the canary's tests against the in-memory OCI API, and `test/e2e-oci/selftest.sh`: the helpers
against stubs of the CLI and fixtures (including the redaction of tenancy and compartment OCIDs, tokens, signatures and private keys), the session script against a stub of
`curl` (the exchange, the modes of the files, the masks, no secret on a command line), the scenario list, the workflows (keyless credentials, a pinned CLI, no API key, every action pinned by SHA),
the byo names shared by the scripts and the sweeper, the policies `ci-policy.sh` and `canary-policy.sh` print (the driver's statements all there, nothing tenancy-wide that writes, only reads in the canary's),
that the lane is ready for its line in `test/e2e-vendors.txt` and absent from it, and the chart with the lane's values (the keyless flags, and only those). The canary's test compares the statements of the calls it made with `canary-policy.sh statements`.

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
set "Require approval for all outside collaborators" for fork pull requests.
`.github/CODEOWNERS` names the owners of `.github/workflows/`, `test/`, `hack/` and itself, who
are requested to review every pull request that touches them; the review is requested, not
enforced (the ruleset on `main` requires the `e2e-result` check only, and administrators
bypass it). The L4
workflow runs on GitHub-hosted runners. If a self-hosted runner is adopted for it
(overview section 15, Q6), it must be for `schedule` and `workflow_dispatch` only,
never for pull requests: a fork's pull request would run arbitrary code on it.

The real-cloud lane is the exception that costs money, so it has its own rules
([Testing and releasing](testing-and-releasing.md)): `e2e.yml` plans from the changed files of a pull
request which vendors run what (a vendor's full lane, or a smoke of two scenarios on every vendor
when shared node code changed, or nothing for documentation), the job that holds the cloud role waits
for the required reviewers of the environment `e2e`, forks and drafts never run it (their
`e2e-result` fails when the plan needs L5), and the check `e2e-result` is the one to require. The nightly canary (`e2e-canary.yml`) is free and
read-only. Neither runs on a timer that spends money.

## Releases

`.github/workflows/release.yml` publishes on a tag `v<semver>` pushed by a
maintainer: a production image (`ghcr.io/sparecores/scarpenter`, linux/amd64 and
arm64, the drivers of the supported vendors only), a development image
(`scarpenter-dev`, with the fake and qemu drivers, for the lanes), the OCI chart
(`ghcr.io/sparecores/charts/scarpenter`), cosign keyless signatures of the image
and chart digests, an SBOM and provenance per image, and a GitHub release. The
version comes from the tag only (the Dockerfile stamps it, the chart is packaged
with it as `version` and `appVersion`); nothing in the repository carries one.
Before anything is published the tagged commit must be on `main`, pass the CI
gates and pass the **full real-cloud lane (L5) of every vendor**, a required job of
the workflow that the publishing jobs `needs:` (it waits for the approval of the environment
`e2e`). The process step by step, and why the gate is part of the workflow, are in
[Testing and releasing](testing-and-releasing.md#releasing). The build steps are in `hack/release.sh`; the workflow's `workflow_dispatch`
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
