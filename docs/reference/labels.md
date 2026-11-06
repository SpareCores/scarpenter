# Labels and annotations

The labels and annotations SCarpenter owns, all in the `scarpenter.sparecores.com`
domain. The Go constants are in `pkg/apis/v1alpha1/labels.go`. The domain is a
restricted label domain of Karpenter core (ADR-015): a NodePool template cannot
set labels in it, and a typo is rejected instead of silently matching nothing.
The labels below are registered as well-known labels, so NodePools and Pods may
select on them.

The catalog (plan 03) defines every selectable key on every instance type, with
`DoesNotExist` where the value is unknown, and stamps them on NodeClaims and
Nodes. What each value means and where it comes from is described with the
catalog; this page is the list of names.

## Labels

Prefix `scarpenter.sparecores.com/` omitted in the first column.

| Label | Example | Kind |
|---|---|---|
| `vendor` | `aws` | enum |
| `api-reference` | `m7i.large` | enum |
| `family` | `m7i` | enum |
| `zone-id` | `use1-az1` | enum |
| `cpu-manufacturer`, `cpu-family`, `cpu-allocation` | `AMD`, `EPYC`, `Dedicated` | enum |
| `accelerator-count`, `accelerator-memory-mib` | `1`, `24576` | integer |
| `accelerator-manufacturer`, `accelerator-model`, `accelerator-type` | `NVIDIA`, `L4`, `GPU` | enum |
| `vcpus`, `cpu-cores` | `8` | integer |
| `memory-mib`, `memory-per-vcpu-mib` | `32768` | integer |
| `cpu-clock-mhz`, `cpu-l2-per-core-kib`, `cpu-l3-mib` | `3700` | integer |
| `hyperthreading` | `false` | boolean |
| `network-baseline-mbps`, `network-peak-mbps` | `12500` | integer |
| `local-storage-gb`, `local-storage-count` | `474`, `1` | integer |
| `cpu-flag-<flag>` | `true` | boolean, for the flags in `spec.catalog.cpuFlagLabels` |
| `metric-<name>`, `metric-<name>-pct` | `2150`, `93` | integer, for the metrics of `spec.metrics`; `-pct` is the percentile (100 is best) |
| `metric-<name>-per-usd` | `5400` | integer, on offerings: score per USD per hour |
| `selection` | `fast-single-core` | membership in `spec.selections` |
| `storage-profile`, `network-profile`, `placement-profile` | `db-io2`, `default` | profile names; `default` means no profile applies |
| `placement-group` | `pg-bench-1` | run key: free-form, not defined on instance types |
| `country`, `continent`, `city`, `state` | `de`, `europe` | on offerings |
| `green-energy` | `true` | on offerings |

Karpenter's own labels (`node.kubernetes.io/instance-type`,
`topology.kubernetes.io/region`, `topology.kubernetes.io/zone`,
`karpenter.sh/capacity-type`, `kubernetes.io/arch`) are used as usual; instance
types are named `<vendor>.<api_reference>` (ADR-008).

The per-NodeClass keys (`metric-*`, `cpu-flag-*`) must be registered with
Karpenter core too: core rejects keys of a restricted domain that are not
well-known, in NodePool requirements and in Pod requirements alike. The static
labels above are registered by the API package; the dynamic ones are registered
by the catalog (plan 03), not yet in plan 02:

- `cpu-flag-<flag>` comes from a closed, curated set of CPU flags: the items of
  `spec.catalog.cpuFlagLabels` are an enum in the CRD.
- `metric-<name>`, `metric-<name>-pct` and `metric-<name>-per-usd` are
  registered at controller start-up, before core's controllers start, from the
  SCNodeClasses that exist then. A metric added or renamed afterwards is not
  selectable until the controller restarts; the SCNodeClass reports a
  `RestartRequired` condition.

## Annotations on NodeClaims

| Annotation | Meaning |
|---|---|
| `scarpenter.sparecores.com/instance-ref` | The driver's opaque instance reference, for example `us-east-1/i-0abc`. |
| `scarpenter.sparecores.com/nodeclass-hash` | The vendor's drift hash at launch. |
| `scarpenter.sparecores.com/nodeclass-hash-version` | Version of the hash computation. |
| `scarpenter.sparecores.com/price` | Hourly price at launch, USD. |
| `scarpenter.sparecores.com/price-incomplete` | `"true"` when the price misses components the data does not have. |
