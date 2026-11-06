# Controller reference

The SCarpenter controller is one binary (`scarpenter-controller`) that runs
Karpenter core's operator with SCarpenter's CloudProvider. This page lists what
the controller accepts and reports. It grows with the plans that implement the
behaviour; sections say which plan fills them in.

## Flags and environment variables

Every flag has a `SCARPENTER_*` environment fallback; a flag on the command line
wins. Karpenter core's own flags and feature gates (`--log-level`,
`--metrics-port`, `--feature-gates`, ...) are accepted on the same command line
with their upstream names and environment variables (`LOG_LEVEL`,
`METRICS_PORT`, ...); see the
[Karpenter documentation](https://karpenter.sh/docs/reference/settings/).

Flags marked "later" are parsed and validated now and used by the plan named in
the last column.

| Flag | Environment | Default | Meaning | Used from |
|---|---|---|---|---|
| `--cluster-name` | `SCARPENTER_CLUSTER_NAME` | required | Tag value on vendor resources and part of their names. Letters, digits, `-` and `_`, at most 63 characters, starting with a letter or digit. | plan 04 |
| `--api-server-endpoint` | `SCARPENTER_API_SERVER_ENDPOINT` | required | `https://` URL nodes use to reach the API server. | plan 05 |
| `--cluster-ca-file` | `SCARPENTER_CLUSTER_CA_FILE` | empty | File with the cluster CA bundle given to nodes. Empty means the `kube-root-ca.crt` ConfigMap of the controller's namespace. | plan 05 |
| `--vendors` | `SCARPENTER_VENDORS` | `aws` | Comma-separated list of enabled vendor drivers. | plan 04 |
| `--keeper-url` | `SCARPENTER_KEEPER_URL` | `https://keeper.sparecores.net` | Base URL of the Spare Cores Keeper API. | plan 03 |
| `--keeper-api-key-file` | `SCARPENTER_KEEPER_API_KEY_FILE` | empty | File with an optional Keeper API key. | plan 03 |
| `--catalog-refresh-interval` | `SCARPENTER_CATALOG_REFRESH_INTERVAL` | `1h` | How often the catalog is refreshed (with 10 % jitter). | plan 03 |
| `--catalog-max-age` | `SCARPENTER_CATALOG_MAX_AGE` | `24h` | Snapshot age after which SCNodeClasses report `CatalogReady=False`. Must not be shorter than the refresh interval. | plan 03 |
| `--sysext-base-url` | `SCARPENTER_SYSEXT_BASE_URL` | empty (upstream) | Mirror of the Flatcar sysext-bakery releases. | plan 05 |
| `--flatcar-base-url` | `SCARPENTER_FLATCAR_BASE_URL` | empty (upstream) | Mirror of the Flatcar release files. | plan 05 |
| `--bootstrap-token-ttl` | `SCARPENTER_BOOTSTRAP_TOKEN_TTL` | `1h` | Lifetime of a node's bootstrap token. | plan 05 |
| `--max-launch-attempts` | `SCARPENTER_MAX_LAUNCH_ATTEMPTS` | `3` | Vendor calls one NodeClaim creation may make; candidates skipped for lack of quota headroom do not count. | plan 04 |
| `--quota-refresh-interval` | `SCARPENTER_QUOTA_REFRESH_INTERVAL` | `2m` | How often quota usage is refreshed per vendor and region in scope. | plan 04 |
| `--quota-limits-refresh-interval` | `SCARPENTER_QUOTA_LIMITS_REFRESH_INTERVAL` | `15m` | How often quota limits are refreshed. | plan 04 |
| `--quota-max-staleness` | `SCARPENTER_QUOTA_MAX_STALENESS` | `15m` | Age after which a region whose quota refreshes fail counts as unknown quota. | plan 04 |
| `--gc-interval` | `SCARPENTER_GC_INTERVAL` | `2m` | How often the garbage collector looks for orphans. | plan 04 |
| `--resource-default-ttl` | `SCARPENTER_RESOURCE_DEFAULT_TTL` | `24h` | `idleTTL` of managed shared resources when an SCNodeClass does not set one. | plan 06 |
| `--aws-home-region` | `SCARPENTER_AWS_HOME_REGION` | `us-east-1` | AWS region of the global endpoints. | plan 08 |

All problems of the command line are reported together, and the controller
refuses to start on any of them. Durations are Go durations (`90s`, `2h30m`) and
must be positive.

## SCNodeClass status conditions

| Condition | Meaning | Set by |
|---|---|---|
| `Ready` | Root condition: true when `CatalogReady` and `ArtifactsReady` are. A vendor that is not ready only makes its own offerings unavailable. | derived (operatorpkg) |
| `CatalogReady` | The catalog snapshot for the SCNodeClass is available and not older than `--catalog-max-age`. The catalog computes it (`Catalog.Status`: `NoSnapshot`, `ScopePending`, `Stale`); the NodeClass controller writes it to the status. | plan 03 (status), plan 05 (condition) |
| `ArtifactsReady` | The sysext images for the pinned versions are resolved (`status.resolved`). | plan 05 |
| `RestartRequired` | Informational, not a dependent of `Ready`: a metric was added or renamed after start-up; its labels are selectable only after the controller restarts (see [labels](labels.md)). The catalog names the keys (`Catalog.RestartRequired`) and stops stamping them; the NodeClass controller writes the condition. | plan 03 (keys), plan 05 (condition) |

Per vendor, `status.vendors.<vendor>.conditions`:


| Condition | Meaning | Set by |
|---|---|---|
| `VendorReady` | Credentials, regions and the vendor block validate. | plan 04 |
| `ImagesReady` | Boot images exist per region and architecture. | plan 05 |
| `ResourcesReady` | Referenced resources exist and managed ones are in place. | plan 06 |
| `QuotaAvailable` | Informational: `False` when a quota bucket has no headroom. | plan 04 |

Until those plans land, the controller does not set any condition and an
SCNodeClass stays `Ready=Unknown`; the NodePools that reference it are not
ready.

## Catalog

The catalog reads Keeper for the vendors and regions of the SCNodeClasses
(`--keeper-url`, `--keeper-api-key-file`, `--catalog-refresh-interval`,
`--catalog-max-age`) and is described in
[Labels and offerings](labels-and-offerings.md). `Operator.WithCatalog` builds it
from those flags and runs it in the manager; plan 04 wires it into the controller
binary together with the driver facts (zones, root disk, profiles) it needs.

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

## CloudProvider

The CloudProvider is named `scarpenter` and supports the `SCNodeClass` kind.
Plan 02 ships a stub: it offers no instance types, so nothing is provisioned.
Node repair (when core's `NodeRepair` feature gate is on) treats `Ready=False`
and `Ready=Unknown` for ten minutes as unhealthy.

Metrics, Events and Helm values are added here by the plans that introduce them.
