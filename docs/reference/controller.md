# Controller reference

The SCarpenter controller is one binary (`scarpenter-controller`) that runs
Karpenter core's operator with SCarpenter's CloudProvider. This page lists what
the controller accepts and reports: flags and environment variables, the
[Helm values](#helm-values) that set them, conditions, Events, metrics and
[permissions](#permissions). `scarpenter-controller --version` prints the version
and commit of the build and exits.

## Flags and environment variables

Every flag has a `SCARPENTER_*` environment fallback; a flag on the command line
wins. Karpenter core's own flags and feature gates (`--log-level`,
`--metrics-port`, `--feature-gates`, ...) are accepted on the same command line
with their upstream names and environment variables (`LOG_LEVEL`,
`METRICS_PORT`, ...); see the
[Karpenter documentation](https://karpenter.sh/docs/reference/settings/).

The last column names the plan that introduced the flag.

| Flag | Environment | Default | Meaning | Used from |
|---|---|---|---|---|
| `--cluster-name` | `SCARPENTER_CLUSTER_NAME` | required | Tag value on vendor resources and part of their names. Letters, digits, `-` and `_`, at most 63 characters, starting with a letter or digit. | plan 04 |
| `--api-server-endpoint` | `SCARPENTER_API_SERVER_ENDPOINT` | required | `https://` URL nodes use to reach the API server. | plan 05 |
| `--tailnet-prefixes` | `SCARPENTER_TAILNET_PREFIXES` | empty (`100.64.0.0/10`, `fd7a:115c:a1e0::/48`) | Comma-separated CIDRs of the tailnet. With the `tailscale` template a kubelet serving certificate may name a node address only inside them (plus the instance's own addresses); set it for a Headscale with other prefixes. | plan 08 |
| `--cluster-dns` | `SCARPENTER_CLUSTER_DNS` | empty | Comma-separated IPs of the cluster DNS service, set as the kubelet's `clusterDNS` on nodes. **Empty: the controller reads the ClusterIPs of the DNS Service of `kube-system` itself** (the Service labelled `k8s-app=kube-dns`, else the one named `kube-dns`; cached for 5 minutes), and an SCNodeClass is `ValidationSucceeded=False` with reason `ClusterDNSUnknown`, naming this flag and the Helm value, while it finds none. `none` leaves `clusterDNS` unset on purpose: Pods then use the node's resolver. | plan 05, 09 |
| `--cluster-domain` | `SCARPENTER_CLUSTER_DOMAIN` | `cluster.local` | The kubelet's `clusterDomain` on nodes. | plan 05 |
| `--cluster-ca-file` | `SCARPENTER_CLUSTER_CA_FILE` | empty | File with the cluster CA bundle given to nodes. Empty means the `kube-root-ca.crt` ConfigMap of the controller's namespace (`POD_NAMESPACE`; `kube-system` without it). | plan 05 |
| `--kubelet-client-ca-file` | `SCARPENTER_KUBELET_CLIENT_CA_FILE` | empty | File with the CA bundle that signs the API server's kubelet client certificates, for control planes where it is not the cluster CA (k3s: `<data-dir>/server/tls/client-ca.crt`). Written to the nodes as `/etc/kubernetes/pki/kubelet-client-ca.crt` and used as the kubelet's `authentication.x509.clientCAFile`; without it the kubelet trusts the cluster CA, and `kubectl logs` and `exec` fail with 401 on k3s. Read at every rendering. | plan 07 |
| `--vendors` | `SCARPENTER_VENDORS` | `aws` | Comma-separated list of enabled vendor drivers. | plan 04 |
| `--keeper-url` | `SCARPENTER_KEEPER_URL` | `https://keeper.sparecores.net` | Base URL of the Spare Cores Keeper API. | plan 03 |
| `--keeper-api-key-file` | `SCARPENTER_KEEPER_API_KEY_FILE` | empty | File with an optional Keeper API key. | plan 03 |
| `--catalog-refresh-interval` | `SCARPENTER_CATALOG_REFRESH_INTERVAL` | `1h` | How often the catalog is refreshed (with 10 % jitter). | plan 03 |
| `--catalog-max-age` | `SCARPENTER_CATALOG_MAX_AGE` | `24h` | Snapshot age after which SCNodeClasses report `CatalogReady=False`. Must not be shorter than the refresh interval. | plan 03 |
| `--sysext-base-url` | `SCARPENTER_SYSEXT_BASE_URL` | empty (upstream) | Mirror of the Flatcar sysext-bakery releases: `<base>/<name>-<version>/...` as upstream. | plan 05 |
| `--flatcar-base-url` | `SCARPENTER_FLATCAR_BASE_URL` | empty (upstream) | Mirror of the Flatcar release files (`<base>/<arch>-usr/<version>/...`; `{channel}` in the base is replaced by the channel). | plan 05 |
| `--bootstrap-token-ttl` | `SCARPENTER_BOOTSTRAP_TOKEN_TTL` | `1h` | Lifetime of a node's bootstrap token; at least `20m`, because core gives a NodeClaim 15 minutes to register and the token must outlive that. | plan 05 |
| `--max-launch-attempts` | `SCARPENTER_MAX_LAUNCH_ATTEMPTS` | `3` | Vendor calls one NodeClaim creation may make; candidates skipped for lack of quota headroom do not count. | plan 04 |
| `--quota-refresh-interval` | `SCARPENTER_QUOTA_REFRESH_INTERVAL` | `2m` | How often quota usage is refreshed per vendor and region in scope. | plan 04 |
| `--quota-limits-refresh-interval` | `SCARPENTER_QUOTA_LIMITS_REFRESH_INTERVAL` | `15m` | How often quota limits are refreshed. | plan 04 |
| `--quota-max-staleness` | `SCARPENTER_QUOTA_MAX_STALENESS` | `15m` | Age after which a region whose quota refreshes fail counts as unknown quota. | plan 04 |
| `--quota-launchable-refresh-interval` | `SCARPENTER_QUOTA_LAUNCHABLE_REFRESH_INTERVAL` | `1h` | How often the instance types the account can launch per zone (`OfferingLister`) are refreshed. | plan 04 |
| `--gc-interval` | `SCARPENTER_GC_INTERVAL` | `2m` | How often the garbage collector looks for orphans. | plan 04 |
| `--gc-grace-period` | `SCARPENTER_GC_GRACE_PERIOD` | `5m` | Age an instance (or bootstrap token) without a NodeClaim must have before it is deleted. | plan 04 |
| `--terminating-grace` | `SCARPENTER_TERMINATING_GRACE` | `10m` | How long an instance may be seen `terminating` before it is deleted again, by the garbage collector (no NodeClaim) and by `Delete`. The first sighting is kept in memory, so a restart restarts the clock. A stopped or powered-off instance is `terminating` for ever; one the vendor is deleting disappears well inside the grace. | plan 14 |
| `--gc-dry-run` | `SCARPENTER_GC_DRY_RUN` | `false` | Log (and count, `result="dry-run"`) what the garbage collector would delete without deleting it. | plan 04 |
| `--unavailable-insufficient-capacity-ttl` | `SCARPENTER_UNAVAILABLE_INSUFFICIENT_CAPACITY_TTL` | `0` (3m) | How long an offering stays unavailable after `InsufficientCapacity`. | plan 04 |
| `--unavailable-unsupported-ttl` | `SCARPENTER_UNAVAILABLE_UNSUPPORTED_TTL` | `0` (24h) | How long a type stays unavailable in a zone after `Unsupported`. | plan 04 |
| `--unavailable-quota-exceeded-ttl` | `SCARPENTER_UNAVAILABLE_QUOTA_EXCEEDED_TTL` | `0` (30m) | How long a region and capacity type stay unavailable after `QuotaExceeded` of a vendor without a quota API; every region of the vendor when the error is account-wide. | plan 04 |
| `--unavailable-zone-unusable-ttl` | `SCARPENTER_UNAVAILABLE_ZONE_UNUSABLE_TTL` | `0` (10m) | How long a zone stays unavailable after `ZoneUnusable`. | plan 04 |
| `--keeper-prices-regions-per-request` | `SCARPENTER_KEEPER_PRICES_REGIONS_PER_REQUEST` | `0` (client default) | Regions per Keeper price request. The replay server of the test lanes has one region per recorded request and needs `1`. | plan 04 |
| `--resource-default-ttl` | `SCARPENTER_RESOURCE_DEFAULT_TTL` | `24h` | `idleTTL` of managed shared resources when an SCNodeClass does not set one. | plan 06 |
| `--resource-per-use-grace` | `SCARPENTER_RESOURCE_PER_USE_GRACE` | `2m` | How long an unused `perUse` managed resource is kept for launches in flight. | plan 06 |
| `--resource-full-sweep-every` | `SCARPENTER_RESOURCE_FULL_SWEEP_EVERY` | `10` | Every this many garbage collection sweeps (and after a NodeClass was deleted) the managed resources of every region of the driver are swept, not only of the regions in use. **Cost:** a full sweep asks the vendor, for every region the account can use, which managed resources exist (for AWS: a few `Describe*` calls per resource kind and region): read-only and free, but rate limited, and it runs on the leader only. At the default `--gc-interval` of 2m it happens every 20 minutes; raise the value to make it rarer, `1` makes every sweep a full one. | plan 06 |
| `--resource-check-interval` | `SCARPENTER_RESOURCE_CHECK_INTERVAL` | `30s` | How often the vendor resources of each SCNodeClass are checked (`status.vendors.<vendor>.resources`, `ResourcesReady`, preCreate). | plan 06 |
| (AWS only) | `SCARPENTER_AWS_IPV4_PRICE` | `0.005` | Price of a public IPv4 address per hour in USD, used in the price delta of AWS offerings. **An approximation, not a Keeper price** (Keeper has no IPv4 price): AWS charges 0.005 in every commercial region since 2024-02-01. `0` means free; negative, NaN and infinite values are refused at startup. The driver logs the value it uses. Environment only. | plan 08 |
| `--aws-home-region` | `SCARPENTER_AWS_HOME_REGION` | `us-east-1` | AWS region the account's region list (`DescribeRegions`) is read from; the AWS driver's other calls go to the region they concern ([AWS driver](aws-driver.md)). | plan 08 |

All problems of the command line are reported together, and the controller
refuses to start on any of them. Durations are Go durations (`90s`, `2h30m`) and
must be positive.

### Other environment variables

| Variable | Meaning |
|---|---|
| `POD_NAMESPACE` | The controller's namespace: where `kube-root-ca.crt` is read when `--cluster-ca-file` is empty (`kube-system` without it) and the namespace of the Deployment its Events go to. The chart sets it from the Pod's namespace. |
| `SCARPENTER_DEPLOYMENT_NAME` | The Deployment (in `POD_NAMESPACE`) that the Events of the garbage collector are attached to when an orphan has no NodeClass. The chart sets it to the release's full name. |
| `LEADER_ELECTION_NAMESPACE`, `LEADER_ELECTION_NAME` | Namespace and name of core's leader election lease (default name `karpenter-leader-election`). The chart sets the namespace from the Pod's namespace, the name to `<release>-leader-election` (so that another Karpenter in the same namespace does not share the lease) and gives the controller a Role for it. |
| `MEMORY_LIMIT`, `CPU_REQUESTS` | Core's: the container's memory limit in bytes (the garbage collector's soft limit is 90 % of it) and CPU request in millicores (sizes core's concurrency). The chart derives both from the container's resources. |
| `DISABLE_LEADER_ELECTION` | Core's: `true` runs without leader election; the chart sets it with `leaderElection.enabled=false`. |
| `AWS_REGION`, `AWS_ROLE_ARN`, `AWS_WEB_IDENTITY_TOKEN_FILE`, `AWS_ACCESS_KEY_ID`, ... | The AWS SDK's own: the [credential modes](../how-to-guides/aws-credentials.md) of the chart set them. Without `AWS_REGION` the driver uses `--aws-home-region` as the SDK's default region. |

Core's other flags and environment variables (`--log-level`/`LOG_LEVEL`,
`--metrics-port`/`METRICS_PORT` (8080), `--health-probe-port`/`HEALTH_PROBE_PORT`
(8081), `--batch-max-duration`, `--batch-idle-duration`, `--feature-gates`, ...) are
those of Karpenter core v1.15; the chart has values for the ones above and
`extraArgs` for the rest. The controller serves `/healthz` and `/readyz` on the
health port and Prometheus metrics on the metrics port (plain HTTP).

## Helm values

The chart is `charts/scarpenter` (installed from `oci://ghcr.io/sparecores/charts/scarpenter`,
[install guide](../how-to-guides/install.md)); `values.yaml` documents every value
and `values.schema.json` rejects unknown keys and malformed values. A value that is
empty or `null` leaves its flag out, so the controller's default above applies.
`test/chart` checks that every flag of the table above can be set from the chart
and that the controller's own parser accepts what the chart renders.

| Value | Flag or environment | Notes |
|---|---|---|
| `cluster.name` | `--cluster-name` | Required. |
| `cluster.apiServerEndpoint` | `--api-server-endpoint` | Required, `https://`. |
| `cluster.ca.existingConfigMap`, `existingSecret`, `key` | `--cluster-ca-file` | Mounts the object (of the release namespace) and points the flag at the key. Unset: the `kube-root-ca.crt` ConfigMap. |
| `cluster.dns` | `--cluster-dns` | A list of IPs, passed as it is. **Empty (the default): the controller finds the DNS Service itself** (the chart looks nothing up); `[none]` gives the nodes no `clusterDNS`. |
| `cluster.domain` | `--cluster-domain` | |
| `kubeletClientCA.source`, `inline`, `configMap.*`, `secret.*` | `--kubelet-client-ca-file` | `none`, `inline`, an existing `configMap` or `secret`, or `extensionApiserverAuthentication` (copies key `client-ca-file` of `kube-system/extension-apiserver-authentication` into a ConfigMap of the release with Helm's `lookup`; the API server's client CA: the k3s client CA, the cluster CA on kubeadm and EKS). kubeadm and EKS need none; on k3s without it `kubectl logs` and `exec` fail with 401. |
| `vendors.aws.enabled`, `vendors.extra` | `--vendors` | `aws` if enabled, then the extra drivers (the development image's `fake` and `qemu`); at least one is required. |
| `vendors.aws.homeRegion` | `--aws-home-region` | |
| `vendors.aws.region` | `AWS_REGION` | Default: the home region, else `us-east-1`. |
| `vendors.aws.ipv4Price` | `SCARPENTER_AWS_IPV4_PRICE` | An approximation, see the table above. |
| `vendors.aws.credentials.mode`, `webIdentity.*`, `secret.*` | `AWS_ROLE_ARN`, `AWS_WEB_IDENTITY_TOKEN_FILE`, a projected ServiceAccount token, `AWS_ACCESS_KEY_ID`, ... | [AWS credentials](../how-to-guides/aws-credentials.md). `webIdentity` needs `roleArn`, `secret` an `existingSecret`. |
| `keeper.url` | `--keeper-url` | |
| `keeper.apiKey.existingSecret`, `key` | `--keeper-api-key-file` | The Secret's key is mounted as a file. |
| `keeper.pricesRegionsPerRequest` | `--keeper-prices-regions-per-request` | |
| `catalog.refreshInterval`, `maxAge` | `--catalog-refresh-interval`, `--catalog-max-age` | |
| `artifacts.sysextBaseURL`, `flatcarBaseURL` | `--sysext-base-url`, `--flatcar-base-url` | |
| `bootstrap.tokenTTL` | `--bootstrap-token-ttl` | At least 20m; the chart refuses less. |
| `bootstrap.tailnetPrefixes` | `--tailnet-prefixes` | A list of CIDRs. |
| `launch.maxAttempts` | `--max-launch-attempts` | |
| `quota.refreshInterval`, `limitsRefreshInterval`, `maxStaleness`, `launchableRefreshInterval` | `--quota-*` | |
| `gc.interval`, `gracePeriod`, `terminatingGrace`, `dryRun` | `--gc-interval`, `--gc-grace-period`, `--terminating-grace`, `--gc-dry-run` | |
| `unavailable.insufficientCapacityTTL`, `unsupportedTTL`, `quotaExceededTTL`, `zoneUnusableTTL` | `--unavailable-*-ttl` | |
| `managedResources.defaultTTL`, `perUseGrace`, `checkInterval`, `fullSweepEvery` | `--resource-default-ttl`, `--resource-per-use-grace`, `--resource-check-interval`, `--resource-full-sweep-every` | See the cost note of the full sweep. |
| `karpenter.logLevel`, `batchMaxDuration`, `batchIdleDuration`, `featureGates`, `metricsPort`, `healthProbePort` | `--log-level`, `--batch-max-duration`, `--batch-idle-duration`, `--feature-gates`, `--metrics-port`, `--health-probe-port` | Core's. |
| `leaderElection.enabled` | `DISABLE_LEADER_ELECTION` | With more than one replica leave it on. |
| `replicaCount`, `podDisruptionBudget.*` | | The PodDisruptionBudget is rendered with more than one replica. |
| `image.repository`, `tag`, `digest`, `pullPolicy`, `imagePullSecrets` | | `ghcr.io/sparecores/scarpenter` (production drivers only), tag = the chart's `appVersion`. The development image is `ghcr.io/sparecores/scarpenter-dev`. The chart needs Kubernetes 1.31 or newer. |
| `serviceAccount.*`, `rbac.*` | | `rbac.authKeySecrets`: a Role (`get` on that one Secret) and a RoleBinding for tailscale auth key Secrets in other namespaces; `rbac.extraRules` are appended to the controller's ClusterRole. |
| `metrics.*`, `serviceMonitor.*` | | A Service for the metrics port and an optional ServiceMonitor (needs the Prometheus Operator CRD). |
| `examples.*` | | An example `SCNodeClass` and `NodePool` per enabled vendor. |
| `resources`, `affinity`, `nodeSelector`, `tolerations`, `topologySpreadConstraints`, `priorityClassName`, `podSecurityContext`, `securityContext`, probes, `podAnnotations`, `podLabels`, `extraArgs`, `extraEnv`, `extraVolumes`, `extraVolumeMounts` | | The default affinity prefers nodes without the `karpenter.sh/nodepool` label (the controller must not run on a node it manages). The default resources come from the L3 scale measurement: about 300 MiB peak with three regions, 475 MiB with every AWS region. |

## Permissions

The chart's RBAC is generated from `config/rbac` (SCarpenter) and
`config/rbac-core` (Karpenter core) by `make chart-rbac`; `make test-chart` renders the
chart and fails when its rules or bindings differ from those files. Kubernetes
objects, all bound to the controller's ServiceAccount:

| Object | Grants | For |
|---|---|---|
| ClusterRole `scarpenter` | `scnodeclasses` get, list, watch, update, patch and `scnodeclasses/status` get, update, patch; `nodeclaims`, `nodes`, `certificatesigningrequests` get, list, watch; `update` on `certificatesigningrequests/approval`; `approve` on the signer `kubernetes.io/kubelet-serving` only; `get` on ConfigMaps (references name their namespace) | the NodeClass controllers (server-side apply of the status), the serving certificate approver (it reads the NodeClaims, the Nodes and, through the driver registry, the SCNodeClasses), ConfigMap references and the cluster CA |
| ClusterRole `scarpenter-core` | Karpenter core's: NodePools, NodeClaims and NodeOverlays (full), CapacityBuffers, pods, volumes, namespaces, services, events, storage classes, CSI nodes, volume attachments, workloads, PodDisruptionBudgets, CRDs (read), `pods/eviction` create, `pods` delete, Nodes get, list, watch, patch, update, delete, Events create and patch, leases get, list, watch | scheduling and disruption |
| Role `scarpenter-bootstrap-tokens` in `kube-system` | Secrets get, list, create, delete | the bootstrap tokens (RBAC cannot limit `get` and `list` by label: the controller can read every Secret of `kube-system`) |
| Role `scarpenter-cluster-dns` in `kube-system` | Services get, list | the cluster DNS Service, read when `--cluster-dns` is not set |
| Role `scarpenter-auth-key` in the release namespace | Secrets get | the tailscale auth key Secret (`rbac.authKeySecrets` for other namespaces) |
| Role `scarpenter-leader-election` in the release namespace | leases (full) | core's leader election |
| ClusterRoleBindings `scarpenter-node-bootstrapper`, `scarpenter-auto-approve-node-client-csrs`, `scarpenter-auto-approve-node-client-renewals` | Kubernetes' `system:node-bootstrapper` and the `nodeclient` and `selfnodeclient` CSR approver roles for the group `system:bootstrappers:scarpenter` and `system:nodes` | the node join ([node bootstrap](node-bootstrap.md#rbac)) |

**No ClusterRole grants access to Secrets.** SCarpenter creates no Nodes (the
kubelets register them), and no state store is needed: its state is the cluster
objects, the vendor's resources and their tags.

The controller's network access is outbound only: the Kubernetes API, the Keeper
API (HTTPS; the catalog and the AWS EBS prices from `/storage_prices`), the
vendors' APIs and the artifact hosts (or their mirrors). It listens on the metrics
and health ports only; no webhook, no admission controller.

## SCNodeClass status conditions

| Condition | Meaning | Set by |
|---|---|---|
| `Ready` | Root condition: true when `CatalogReady`, `ArtifactsReady` and `ValidationSucceeded` are. A vendor that is not ready only makes its own offerings unavailable. | derived (operatorpkg) |
| `CatalogReady` | The catalog snapshot for the SCNodeClass is available and not older than `--catalog-max-age`. The catalog computes it (`Catalog.Status`: `NoSnapshot`, `ScopePending`, `Stale`); the NodeClass controller writes it to the status. | plan 03 (status), plan 05 (condition) |
| `ArtifactsReady` | The sysext images for the pinned versions are resolved (`status.resolved`). Reasons: `Resolved`, `ReleaseNotFound`, `ResolutionFailed`. | plan 05 |
| `ValidationSucceeded` | The semantic checks pass: kubelet quantities parse, the Kubernetes version is within the skew of the API server (at most three minors older, never newer), the referenced ConfigMaps and Secret exist, a trial rendering of the user-data succeeds within the size budget, and the cluster DNS is known (`--cluster-dns`, or found). Reasons: `Valid`, `InvalidKubeletSettings`, `KubernetesVersionNotSupported`, `ReferenceNotFound`, `ReferenceUnreadable`, `TemplateInvalid`, `ClusterDNSUnknown`, `InvalidResourceUse`. A `use` block of a vendor resource that selects nothing (an empty `use`, which an old object stored before a field was renamed has after the API server pruned the field) is `InvalidResourceUse`, never "nothing given": the controller does not fall back to the vendor's default or to creating the resource. | plan 05, plan 14 |
| `RestartRequired` | Informational, not a dependent of `Ready`: a metric was added or renamed after start-up; its labels are selectable only after the controller restarts (see [labels](labels.md)). The catalog names the keys (`Catalog.RestartRequired`) and stops stamping them; the NodeClass controller writes the condition. | plan 03 (keys), plan 05 (condition) |

Per vendor, `status.vendors.<vendor>.conditions`:


| Condition | Meaning | Set by |
|---|---|---|
| `VendorReady` | The vendor is enabled, its regions are ones the driver can use and the driver's `ValidateNodeClass` accepts the block. Reasons: `Ready`, `VendorNotEnabled`, `UnknownRegions`, `ValidationFailed`. | plan 05 |
| `ImagesReady` | Boot images exist per region and architecture, as the driver's optional `ImageReporter` reports them (`status.vendors.<vendor>.images`); a driver without it resolves images at launch and counts as ready. `False` only when a region the NodeClass names has no image or when no region has one; otherwise `True` with reason `ImagesPartial` naming the regions without. An image that is not the pinned Flatcar release (the `images` selector or the convention found an image that says another version, or none) makes it `False` with the image's own reason, `ImageVersionMismatch` or `ImageVersionUnknown`, naming the region and the image, whether or not the NodeClass names the region: the image is what the NodeClass chose, and it is never booted ([ADR-017](../explanation/DECISIONS.md#adr-017-never-degrade-an-option-silently-accepted-2026-11-05)). The offerings of a region in which no image is ready are unavailable; the NodeClass stays `Ready`. | plan 05 (hook), plan 08 (AWS), plan 14 (A1) |
| `ResourcesReady` | `False` (`ResourcesUnavailable`) when a region lacks a resource the NodeClass needs (a `use` selector matching nothing, or more than its `select` allows (the message carries the candidates after each stage, see [resource selectors](resource-selectors.md#resolution-status-and-caching)), a `forbid` kind without a vendor default, a preCreate resource that cannot be created): the offerings of that region are unavailable, the message names region and kind; `Unknown` (`CheckFailed`) when the vendor could not be asked. Informational for `Ready`. See [vendor resources](vendor-resources.md#status). | plan 06 |
| `QuotaAvailable` | Informational: `False` when a quota bucket has no headroom. | plan 04 |

The NodeClass controller (plan 05; see [node bootstrap](node-bootstrap.md#the-nodeclass-controller))
writes these conditions, the resolved artifacts and the per-vendor drift hash with
server-side apply under the field manager `scarpenter-nodeclass`; the quota status
controller sets `QuotaAvailable` (see [Quota](#quota)) under `scarpenter-quota`, the
resource status controller `ResourcesReady` and `resources` (see
[vendor resources](vendor-resources.md#status)) under `scarpenter-resources`.
`VendorReady` also requires that the vendor block and every profile merge with the
driver's defaults (`ValidationFailed` otherwise).
Development builds run the same controller; the stand-in of plan 04 is gone.

`Create` needs `Ready=True` with the current `observedGeneration`. A vendor that
is not ready offers no instance types; a `Create` fails with `NodeClassNotReady`
only when every vendor the NodeClaim allows is not ready (when another, ready
vendor simply has no capacity, it is `InsufficientCapacity`).

## Catalog

The catalog reads Keeper for the vendors and regions of the SCNodeClasses
(`--keeper-url`, `--keeper-api-key-file`, `--catalog-refresh-interval`,
`--catalog-max-age`) and is described in
[Labels and offerings](labels-and-offerings.md). `Operator.NewComponents` builds it
from those flags with the driver facts (zones, root disk, profiles, launchable
types), the quota tracker and the unavailable-offerings cache, and runs it in the
manager.

Metrics (Prometheus, on the metrics port):

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `scarpenter_catalog_age_seconds` | gauge | `vendor` | Seconds since the catalog snapshot was read from Keeper. |
| `scarpenter_catalog_refresh_errors_total` | counter | | Failed refreshes; the last good snapshot stays in use. |
| `scarpenter_catalog_instance_types` | gauge | `vendor` | Instance types with a price in scope. |
| `scarpenter_catalog_offerings` | gauge | `vendor` | Priced offerings (price rows in scope, before the driver's zone mapping). |

At start-up, before core's controllers are built, the controller lists the
SCNodeClasses and registers their `metric-<m>` keys, and all `cpu-flag-<flag>`
keys, with core (`Operator.RegisterLabelKeys`, logged as `registered the label
keys of the SCNodeClasses`). The controller needs permission to list
SCNodeClasses.

## Development builds

Binaries built with `-tags scarpenter_dev` add the fake driver and its flags
(all `SCARPENTER_*` environment fallbacks): `--fake-catalog-vendor` (default
`aws`: the Keeper vendor whose catalog the fake serves, under that vendor ID),
`--fake-config-namespace` (`scarpenter-system`), `--fake-config-name`
(`scarpenter-fake`), `--fake-state-name` (`scarpenter-fake-state`), `--fake-resources-name`
(`scarpenter-fake-resources`) and `--fake-launch-delay` (`0`). Details: [the fake driver](driver-contract.md#the-fake-driver).
The qemu driver adds `--qemu-state-dir`, `--qemu-cache-dir`, `--qemu-log-dir`,
`--qemu-bridge` and `--qemu-launch-delay` (below). The NodeClass controller is the same as in production. Development builds also
expose the gauges `scarpenter_fake_state_revision` and
`scarpenter_fake_state_published_revision` (the revision of the fake's in-memory
state and of its state ConfigMap).

### qemu driver flags

| Flag | Environment | Default | Meaning |
|---|---|---|---|
| `--qemu-state-dir` | `SCARPENTER_QEMU_STATE_DIR` | `<user cache dir>/scarpenter-qemu/state` | One directory per instance (record, disk images, Ignition file, serial log) and the records of managed networks. State survives restarts of the controller. |
| `--qemu-cache-dir` | `SCARPENTER_QEMU_CACHE_DIR` | `<user cache dir>/scarpenter-qemu/cache` | Downloaded Flatcar releases (`flatcar/<channel>/<arch>/<version>`), verified against the release's digest file; honours `--flatcar-base-url`. |
| `--qemu-log-dir` | `SCARPENTER_QEMU_LOG_DIR` | empty | If set, the serial log of every deleted instance is moved here (`<nodeclaim>-<id>.serial.log`). |
| `--qemu-bridge` | `SCARPENTER_QEMU_BRIDGE` | `scbr0` | The bridge of the default network. |
| `--qemu-launch-delay` | `SCARPENTER_QEMU_LAUNCH_DELAY` | `0` | Test aid: how long `CreateInstance` waits after the VM has started before it returns (the L4 scenario that kills the controller in that window). |

## CloudProvider

The CloudProvider is named `scarpenter` and supports the `SCNodeClass` kind. It
is vendor-neutral on top of [the driver contract](driver-contract.md).

`Create` resolves the vendor settings of each candidate (driver defaults, the
vendor block, the profiles the NodeClaim's requirements select; see [node
settings](node-settings.md)), orders the candidates by price plus the price delta
of the settings, and for each attempt runs the driver's `Prepare` (vendor
resources, placement groups, volumes, with the placement run key) before the
vendor call, renders the user-data for that attempt (zone, instance type, the
type's kube-reserved, the device hints), and stamps the profile labels, the run
key, the price (`price-incomplete` where the price misses components) and the
profile hashes on the NodeClaim. A vendor resource that is missing or forbidden
fails the candidate (`Unsupported`, code `ResourceMissing` or `ResourceForbidden`)
without marking the type or the zone, skips the other candidates of the region
and makes the region's offerings unavailable. A replayed idempotency key that
returns another instance than the attempted offering moves the quota reservation
to the returned instance.

- `GetInstanceTypes` returns the catalog's instance types for the NodePool's
  SCNodeClass, pre-filtered by the NodePool's requirements on vendor, region,
  zone, architecture, capacity type and instance type. Offerings are
  unavailable when the unavailable-offerings cache or the quota tracker says so.
- `Create` computes the candidate offerings of the NodeClaim, orders them by
  price (ties: larger quota headroom, instance type name, zone, region, capacity
  type), skips those without quota headroom without calling the vendor, and
  tries the others until one launches or `--max-launch-attempts` vendor calls
  failed. Error classes: `InsufficientCapacity` marks the offering unavailable
  and continues; `Unsupported` marks the type in the zone, `ZoneUnusable` the
  zone; `QuotaExceeded` triggers a quota refresh of the region (vendor with
  quota knowledge for the type) or marks region and capacity type unavailable
  for the TTL (vendor without), and the rest of that region and capacity type is
  skipped (an account-wide quota error, `Error.AccountWide`, marks the capacity type
  unavailable in every region of the vendor); `Retryable` is returned as is (core retries); `Fatal` returns a
  `CreateError` and an Event. When nothing launched it returns
  `InsufficientCapacityError` and core deletes the NodeClaim, except when there are
  no candidates because the catalog is not ready (a controller that has just
  restarted and has not read its first snapshot, a stale one): that is a
  `CreateError` (`CatalogNotReady`) that core retries, because a replayed launch
  of a NodeClaim whose instance exists must not delete the NodeClaim and orphan
  the instance (found by the L4 scenario Q04). The NodeClaim UID
  is the idempotency key. The bootstrap token is minted once per NodeClaim (a retry
  gets the same one) and the user-data rendered once per vendor, region and
  architecture ([node bootstrap](node-bootstrap.md)).
- Provider ID: `scarpenter://<vendor>/<region>/<nodeclaim>`. Annotations on the
  launched NodeClaim: `scarpenter.sparecores.com/instance-ref`, `nodeclass-hash`,
  `nodeclass-hash-version`, `price` (the `price-incomplete` annotation comes with
  the pricing deltas of plan 06). Labels: every single-valued requirement of the
  instance type and offering, the node name as `kubernetes.io/hostname`, and the
  three profile labels (`default` until plan 06).
- `Get` and `List` read through a 30 s cache of the cluster's instances
  (`Create` and `Delete` update it); a miss asks the driver by NodeClaim tag.
  Terminated and terminating instances are `NodeClaimNotFound`.
- `Delete` terminates by the `instance-ref` annotation, else by tag lookup, and
  removes the bootstrap token. It returns `NodeClaimNotFound` once the instance
  is gone. An instance that is terminating (the vendor is deleting it, or it is
  stopped or powered off, which every driver reports as terminating) is left alone
  for `--terminating-grace` from the first time `Delete` saw it, then deleted again
  (`DeleteInstance` is idempotent), and again after each further grace.
- `IsDrifted` returns `NodeClassDrift` when the NodeClaim's `nodeclass-hash`
  differs from `status.vendors.<vendor>.hash`.

Node repair (when core's `NodeRepair` feature gate is on) treats `Ready=False`
and `Ready=Unknown` for ten minutes as unhealthy.

Metrics:

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `scarpenter_launches_total` | counter | `vendor`, `region`, `capacity_type`, `result` | Vendor launch calls; `result` is `success` or the error class. |
| `scarpenter_launch_skipped_total` | counter | `vendor`, `region`, `capacity_type`, `reason` | Candidates passed over without a vendor call (`no-quota`, `unavailable`, `quota-exceeded`, `image`). |
| `scarpenter_launch_attempts` | histogram | `result` | Vendor calls per `Create`. |
| `scarpenter_create_duration_seconds` | histogram | `result` | Duration of `Create`. |

Events (on the SCNodeClass): `LaunchFailed` (a `Fatal` vendor error),
`ImageResolutionFailed`. The NodeClass controller, the bootstrap token controller
and the serving certificate approver add `ArtifactsNotResolved`,
`ValidationFailed`, `VendorNotReady`, `RestartRequired`, `MetricInvalid`,
`IPv6OnlyNeedsMirror` (Warning: IPv6-only nodes and the default sysext source, which
has no IPv6; set `--sysext-base-url`) (on the SCNodeClass), `BootstrapTokenDeleted` (on the NodeClaim; also `GarbageCollected` on
the NodeClass for orphan tokens) and `ServingCertificateApproved` /
`ServingCertificateDenied` (on the certificate signing request); see
[node bootstrap](node-bootstrap.md).

## Quota

The quota tracker reads, for drivers with the `QuotaReporter` capability, limits
and usage of every bucket per region in scope (the regions the SCNodeClasses
allow, or every region of the driver) and account-wide, and for drivers with
`OfferingLister` the launchable types per zone. An offering is unavailable when a
bucket it consumes has less headroom than one instance needs; launchable
types are dropped from the offerings. Headroom is `limit - used - reserved`;
`reserved` is the in-flight ledger of launches not yet visible in `used` (released
on failure, when a refresh counts the instance, or after 10 minutes). Refreshes
run every `--quota-refresh-interval` (limits every `--quota-limits-refresh-interval`),
and, debounced to 10 s, after a quota error and after an own launch or
termination. A region whose refreshes fail for `--quota-max-staleness` counts as
unknown quota (the reactive path). Types the driver cannot map have unknown
quota and are never blocked.

When every allowed offering of a (vendor, region, capacity type) is blocked, the
controller sets `status.vendors.<vendor>.quota` (region, capacity type, the
limiting bucket, used, limit, reason) and the condition `QuotaAvailable=False`
with reason `QuotaExhausted`, or `QuotaZero` when every listed limit is 0 (it
resolves only with a quota increase); otherwise `QuotaAvailable=True`. Warning
Events `QuotaExhausted` and `QuotaZero` (deduplicated for 10 minutes) go to the
SCNodeClass. With one allowed region and no headroom the pods stay Pending and
nothing is launched; when a refresh shows headroom, core's next scheduling round
(it re-evaluates pending pods every 10 s) provisions them, with no nudge from
SCarpenter. The status is written by server-side apply with field manager
`scarpenter-quota`, which owns only these fields.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `scarpenter_quota_limit` | gauge | `vendor`, `region`, `bucket` | Limit of the bucket (`region` is empty for account-wide buckets). |
| `scarpenter_quota_used` | gauge | `vendor`, `region`, `bucket` | Usage of the whole account. |
| `scarpenter_quota_reserved` | gauge | `vendor`, `region`, `bucket` | In-flight reservations. |
| `scarpenter_quota_refresh_age_seconds` | gauge | `vendor`, `region` | Seconds since the last successful refresh. |
| `scarpenter_quota_refresh_errors_total` | counter | `vendor`, `region` | Failed quota or launchable refreshes. |

## Garbage collection

Every `--gc-interval` (leader only) the collector lists the instances tagged with
this cluster in the regions in use (allowed by a NodeClass, or holding a
NodeClaim) and deletes those whose NodeClaim does not exist (or that lack the
`nodeclaim` tag) and that are older than `--gc-grace-period`. An instance without a
NodeClaim that has been `terminating` for longer than `--terminating-grace` (a
stopped or powered-off instance, which no vendor deletes by itself) is deleted again,
with the reason `Terminating`; while its NodeClaim exists, Karpenter core deletes the
NodeClaim (`Get` does not return it) and `Delete` deletes the instance after the
grace. Bootstrap-token
Secrets (`kube-system`, label `scarpenter.sparecores.com/nodeclaim`) of missing
NodeClaims are deleted the same way, with a `GarbageCollected` Event on the
SCNodeClass of the token's `scarpenter.sparecores.com/nodeclass` label. Managed
vendor resources are swept after the instances and tokens by their lifecycle
(`perUse`, `idleTTL`, `retain`) in the driver's dependency order, only when they
carry both the deterministic name and the ownership marker, never those of `use`;
the metrics and Events are in [vendor resources](vendor-resources.md#garbage-collection). Every deletion is logged with its reason and published as the Normal Event
`GarbageCollected` on the SCNodeClass of the instance's `nodeclass` tag, else on
the controller Deployment (`SCARPENTER_DEPLOYMENT_NAME`, `POD_NAMESPACE`).

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `scarpenter_gc_deletions_total` | counter | `vendor`, `kind`, `reason`, `result` | `kind`: `instance` or `bootstrap-token`; `reason`: `NodeClaimNotFound`, `NoNodeClaimTag` or `Terminating`; `result`: `deleted`, `dry-run`, `error`. |
| `scarpenter_gc_sweep_duration_seconds` | histogram | | Duration of a sweep. |
| `scarpenter_gc_last_sweep_timestamp_seconds` | gauge | | Time of the last sweep. |

## Events

| Reason | Type | On | Meaning |
|---|---|---|---|
| `LaunchFailed` | Warning | SCNodeClass | A `Fatal` vendor error ended a launch. |
| `ImageResolutionFailed` | Warning | SCNodeClass | A boot image could not be resolved. |
| `ArtifactsNotResolved`, `ValidationFailed`, `VendorNotReady`, `MetricInvalid`, `RestartRequired` | Warning / Normal | SCNodeClass | The NodeClass controller's findings ([conditions](#scnodeclass-status-conditions)). |
| `IPv6OnlyNeedsMirror` | Warning | SCNodeClass | IPv6-only nodes and the default sysext source, which has no IPv6: set `--sysext-base-url`. |
| `QuotaExhausted`, `QuotaZero` | Warning | SCNodeClass | No headroom in a quota bucket (deduplicated for 10 minutes). |
| `BootstrapTokenDeleted` | Normal | NodeClaim | The node registered; its token was deleted. |
| `GarbageCollected` | Normal | SCNodeClass, else the Deployment | An orphan instance, bootstrap token or resource was deleted ([garbage collection](#garbage-collection)). |
| `ServingCertificateApproved`, `ServingCertificateDenied` | Normal / Warning | CertificateSigningRequest | The serving certificate approver's decision ([node bootstrap](node-bootstrap.md#the-serving-certificate-approver)). |
| `ResourceCreated`, `ResourceAdopted`, `ResourceDeleted` | Normal | SCNodeClass, else the Deployment | A managed vendor resource was created, adopted after a restart, or deleted ([vendor resources](vendor-resources.md#metrics-and-events)). |
| `ResourceCreateFailed`, `ResourceDeleteFailed` | Warning (deduplicated for 10 minutes) | SCNodeClass, else the Deployment | A creation or deletion failed; deletions retry with backoff. |
| `ResourcesUnavailable` | Warning | SCNodeClass | A region lacks a resource the NodeClass needs, so its offerings are unavailable. |

## Metrics

Besides the metrics of Karpenter core, the controller serves, on the metrics port:

| Metric | Section |
|---|---|
| `scarpenter_catalog_age_seconds`, `scarpenter_catalog_refresh_errors_total`, `scarpenter_catalog_instance_types`, `scarpenter_catalog_offerings` | [Catalog](#catalog) |
| `scarpenter_launches_total`, `scarpenter_launch_skipped_total`, `scarpenter_launch_attempts`, `scarpenter_create_duration_seconds` | [CloudProvider](#cloudprovider) |
| `scarpenter_quota_limit`, `scarpenter_quota_used`, `scarpenter_quota_reserved`, `scarpenter_quota_refresh_age_seconds`, `scarpenter_quota_refresh_errors_total` | [Quota](#quota) |
| `scarpenter_gc_deletions_total`, `scarpenter_gc_sweep_duration_seconds`, `scarpenter_gc_last_sweep_timestamp_seconds` | [Garbage collection](#garbage-collection) |
| `scarpenter_resource_operations_total` (`vendor`, `kind`, `region`, `operation` `create`, `adopt` or `delete`, `result` `ok`, `error` or `dry-run`) and `scarpenter_resources_managed` (`vendor`, `kind`, `region`: the managed resources the last sweep found) | [Vendor resources](vendor-resources.md#metrics-and-events) |

Development builds add the `scarpenter_fake_state_*` gauges.
