# Which types does a class offer

A benchmark planner, a dashboard or an operator who asks "which instance types will Karpenter get from this NodeClass, which are
available now, and why not the others" has three places to look, from cheap to detailed:

| Place | Gives | Cost |
|---|---|---|
| `kubectl describe scnodeclass` (`status.vendors.<vendor>.offered`) | counts, a count per reason code, up to five sample type names per code | always there |
| the listing ConfigMap | every type by name, every offering's state, the reason and its data | opt-in per class |
| the controller log at V(1) | every change of a reason, with the data of the gate | log level `debug` |

The reason codes, and which gate each one names, are in
[Why a type or an offering is not offered](../reference/labels-and-offerings.md#why-a-type-or-an-offering-is-not-offered).
Design: [plan 28](../plans/v1/done/28-offered-types.md).

## Read the summary

```sh
kubectl get scnodeclass bench -o jsonpath='{.status.vendors.oci.offered}' | jq
```

`dropped` counts the types that are not offered at all (`NoPrice`, `RegionNotAllowed`, the `GPU*` codes, `NoOffering`) and
`unavailable` the offerings of offered types that are not available (`QuotaGate`, `UnavailableCache`, `ZoneNotAvailable`, ...). A
`QuotaGate` count with sample names tells you that a quota bucket blocks those types; the listing has the bucket.

## Publish the listing

Annotate the SCNodeClass; the annotation is not part of the spec and does not drift any node:

```sh
kubectl annotate scnodeclass bench scarpenter.sparecores.com/publish-offered=true
```

The controller then keeps one ConfigMap per vendor of the class, `scarpenter-offered-<class>-<vendor>`, in its own namespace
(`scarpenter-system` with the chart's defaults), owned by the SCNodeClass. It is rewritten when something in it changed (at most every 30
seconds) and at least every 10 minutes, with a new `generatedAt`: treat a listing older than 30 minutes as stale. Remove the annotation and
the ConfigMaps go. The key is `types.json`; the fields are in [the reference](../reference/labels-and-offerings.md#the-offered-listing).

To read it from another namespace, give the reader's ServiceAccount a Role with `get`, `list` and `watch` on `configmaps` in the
controller's namespace (restrict it with `resourceNames`) and a RoleBinding. The chart does not grant it.

A listing past 800 KiB is cut and says `"truncated": true`: the types that are not available come first, so the available ones may be
incomplete. Narrow the class's regions, or read the status.

## Recipes

```sh
cm() { kubectl -n scarpenter-system get cm scarpenter-offered-bench-oci -o jsonpath='{.data.types\.json}'; }
```

Instance types with an available offering, on-demand:

```sh
cm | jq -r '.types | to_entries[] | select(.value.capacityTypes | index("on-demand")) | .key'
```

Why a type is not available:

```sh
cm | jq '.types["oci.VM.Standard4.Ax.Flex.16-64"] | {available, reason, detail, offerings}'
```

The gated types, and the types that are not in the catalog at all, with the code:

```sh
cm | jq -r '.types | to_entries[] | select(.value.available | not) | [.key, .value.reason] | @tsv'
```

Pin a type: the map key is the instance type name. Put it into the NodePool, or the Pod's node selector, as
`node.kubernetes.io/instance-type In [<key>]`, valid for every type. A type with `apiReference` can also be pinned with
`scarpenter.sparecores.com/api-reference`, which is not vendor-qualified; a type without it has no such label, and a pin on it matches
nothing.

## What the listing does not know

It is the class-level view: no NodePool filter and no pinned selection profile, so `Unrankable`, `NodePoolRequirement` and
`NodePoolPreFilter` never appear, and Karpenter-side removals (requests that do not fit, `minValues`, NodePool limits) are not
SCarpenter's to see. A type that the listing shows available can still be refused by a NodePool.
If someone deletes the ConfigMap it comes back with the next change or heartbeat, within 10 minutes.
