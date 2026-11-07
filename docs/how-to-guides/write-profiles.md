# Write node profiles

Profiles are named, explicit, vendor-native node setups that workloads opt into
with a label. This guide writes a storage and a placement profile, lets a pod
use them, and checks what was applied. The model is in
[Node settings](../reference/node-settings.md).

## Before you start

You need a working SCarpenter with an `SCNodeClass` for the vendor, and
workloads that can carry a `nodeSelector`. A profile applies to the vendors it has
a block for; on the others it does not exist, and a workload that requires it
stays Pending instead of getting a node that is not what it asked for.

## 1. Choose what is a default and what is a profile

What every node of a vendor should have goes into the vendor block
(`spec.vendors.<vendor>.storage`), which overrides the driver's built-in
defaults. What only some workloads need goes into a profile. A profile is not
cumulative: a node has at most one storage, one network and one placement
profile, and a storage profile overrides the block key by key.

## 2. Write the profile

A database that needs a fast data volume and a client that must sit next to it:

```yaml
apiVersion: scarpenter.sparecores.com/v1alpha1
kind: SCNodeClass
metadata: {name: bench}
spec:
  # flatcar, kubernetes and network omitted
  vendors:
    aws:
      storage:
        root: {volumeType: gp3, volumeSize: 128Gi}     # layer 2: every AWS node
  profiles:
    storage:
      db-io2:
        aws:
          data:
            - name: pgdata
              volumeType: io2
              volumeSize: 500Gi
              iops: 32000
              filesystem: xfs
              mountPath: /var/lib/pgdata
    placement:
      colocated:
        aws: {placementGroup: {strategy: cluster}}
```

Fields of the vendor blocks are in the vendor's terms (the
[API reference](../reference/api.md) lists them). Notes:

- `data` volumes always have a `name`, a `filesystem` (`ext4`, `xfs` or `btrfs`)
  and an absolute `mountPath` made of letters, digits, `.`, `_`, `-` and `/`.
  SCarpenter creates the filesystem once, on a device that has no signature, and
  mounts it by label; it never overwrites a filesystem it did not create.
- A list in a profile **replaces** the list of the vendor block (`data` does not
  extend it).
- `localStorage: {policy: mount, mountPath: /mnt/local}` uses the instance-local
  disks of types that have them (`raid0` stripes them); the profile is valid only
  on those types.
- A placement profile that creates groups (a placement group) needs a run key:
  the pods that belong together set the same `placement-group` label.
- Profile names are lower-case label slugs; `default` is reserved.

A profile for a vendor that has no block in it (here: any vendor but AWS) does
not exist there. To give the other vendors something, add their blocks:
`aws: {...}` and `gcp: {...}` side by side.

## 3. Use it from a workload

Standard Kubernetes scheduling; nothing else is needed in the pod-creation path:

```yaml
spec:
  template:
    spec:
      nodeSelector:
        node.kubernetes.io/instance-type: aws.r7i.2xlarge
        karpenter.sh/capacity-type: on-demand
        scarpenter.sparecores.com/storage-profile: db-io2
        scarpenter.sparecores.com/placement-profile: colocated
        scarpenter.sparecores.com/placement-group: pg-bench-1     # the run key
```

A pod and its client with the same profile and run key share one native group,
which is created on the first launch and deleted when both are gone. Make the
zone deterministic by pinning `topology.kubernetes.io/zone` on both, or with a
pod affinity on the zone. There is no gang scheduling: if one node cannot launch,
the other runs anyway. Use a fresh run key per run.

## 4. Check what was applied

```bash
kubectl get node <node> -L scarpenter.sparecores.com/storage-profile,scarpenter.sparecores.com/placement-profile
kubectl get nodeclaim <claim> -o jsonpath='{.metadata.annotations}'
```

The profile labels say what was applied (`default` when none), the annotation
`scarpenter.sparecores.com/price` is the hourly price with the profile, and
`price-incomplete: "true"` says that something in it is not priced (provisioned
IOPS, dedicated tenancy): set `spec.pricing.allowIncomplete: false` to refuse such
offerings.

## If the pod stays Pending

Karpenter's scheduling event names the label that no instance type satisfies.
Typical causes: the profile has no block for the vendor the pod may use; the
instance type cannot host the profile (for example a local-storage profile on a
type without local disks); the profile name
is misspelled (a typo in a label of the restricted domain is rejected, a typo in
the value matches nothing).

## Changing a profile

Changing a profile drifts only the nodes that use it, and only through core's
disruption budgets of the NodePool; changing the vendor block drifts every node
of the vendor. See [Drift](../reference/node-settings.md#drift).
