# Bring your own vendor resources

SCarpenter needs networks, subnets, security groups, placement groups and
similar objects at the vendor. By default it uses the vendor's own defaults
where they exist and creates what is missing. This guide shows how to reference
resources you manage yourself (infrastructure as code), and how to choose the
lifecycle of the ones SCarpenter creates. The model is in
[Vendor resources](../reference/vendor-resources.md). What each vendor needs, and
what sc-infra's inventory gives, is in the
[infrastructure hand-off](../reference/infrastructure-handoff.md); the boot
images, which are brought but never created by SCarpenter, are explained in
[Flatcar images](../explanation/flatcar-images.md).

## Reference your own resources

Under `spec.vendors.<vendor>.resources`, per resource kind, `use` points at
existing resources. Three ways to find them, per region in scope:

```yaml
spec:
  vendors:
    aws:
      regions: [us-east-1, eu-central-1]
      resources:
        subnets:
          use:
            selector: {tags: {scarpenter.sparecores.com/use: "true"}}
        securityGroups:
          use:
            ids: {us-east-1: [sg-0123456789abcdef0], eu-central-1: [sg-0fedcba9876543210]}
```

- `selector: {tags: {...}}`: all resources that carry every listed tag (labels,
  where the vendor has no tags). Tag your subnets with the key once in your
  infrastructure code and every region is covered.
- `ids: {<region>: [...]}`: explicit IDs. A region without an entry is an error,
  not "the default".
- `name: "sc-{region}"`: a name pattern; `{region}` and `{zone}` are replaced.

SCarpenter validates the references per region. The vendor condition
`ResourcesReady` in `status.vendors.<vendor>` says which region misses what
(`kubectl get scnodeclass <name> -o yaml`), and the offerings of that region are
unavailable until the resource exists: pods that need that region stay Pending,
and the others are unaffected. SCarpenter never modifies or deletes a resource it
resolved with `use`.

A pre-created placement group is referenced from the placement profile, not from
the NodeClass: `aws: {placementGroup: {use: {name: my-pg}}}`. It is never
deleted, and the run key then only keeps NodeClaims apart.

## Use what the vendor has, or forbid creating anything

With neither `use` nor `create` for a kind, SCarpenter uses the vendor's own
default resource where it exists (the AWS default VPC and its subnets, GCP's
`default` network) and creates the resource only where there is none. To make
sure nothing is ever created, forbid it:

```yaml
      resources:
        subnets: {forbid: true}
```

Offerings that need the kind are then available only where the vendor has a
default; elsewhere they are unavailable with the reason in `ResourcesReady`.

## Let SCarpenter create resources, with a lifecycle

```yaml
      resources:
        securityGroups:
          create:
            lifecycle: {policy: idleTTL, ttl: 12h}
            timing: {mode: preCreate, regions: [us-east-1]}
        placementGroups:
          create: {lifecycle: {policy: perUse}}
```

- Lifecycle `perUse`: deleted when no instance uses the resource any more, after
  a two-minute grace for launches in flight. Right for per-run resources
  (placement groups) and per-node ones (volumes).
- Lifecycle `idleTTL`: deleted when no instance has used it for `ttl` (default
  24 hours, `--resource-default-ttl`). Right for shared regional infrastructure
  (networks, subnets, security groups); a short TTL means churn on bursty
  workloads.
- Lifecycle `retain`: deleted only when the NodeClass is deleted.
- Timing `onDemand` (default): the first launch in a region creates the resource
  and waits for it. `preCreate` creates it in the listed regions right away and
  keeps it while the region stays listed, which takes the creation latency out of
  the first launch.

Resources SCarpenter creates are named `scarpenter-<cluster>-<kind>-<region>` and
carry the ownership tags `cluster`, `nodeclass`, `managed-by` and `resource-kind`
(plus `spec.tags`); only resources with both the name and the tags are ever
deleted by the garbage collector. After a restart it finds them again instead of
creating duplicates.

## Check

```bash
kubectl get scnodeclass <name> -o jsonpath='{.status.vendors.aws.resources}'
kubectl get events --field-selector involvedObject.name=<name>   # ResourceCreated, ResourceAdopted, ResourceDeleted
```

Metrics `scarpenter_resource_operations_total` and `scarpenter_resources_managed`
count creations, adoptions and deletions. A deletion the vendor refuses (the
resource is still used by something SCarpenter does not own) is retried with
backoff and shows in the status `message` of the resource and in a Warning Event.
