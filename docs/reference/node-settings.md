# Node settings

What a node is made of (disks, network features, placement), how the settings
of a node are resolved from four layers, what profiles are, which labels say
what was applied, and when a change drifts nodes. Design: ADR-015, ADR-017,
ADR-020 and the node options design (`docs/plans/v1/wip/00a-node-options.md`).
The vendor resources a node depends on are in
[Vendor resources](vendor-resources.md); how to write profiles is in
[Write node profiles](../how-to-guides/write-profiles.md).

## Table of Contents

- [Layers](#layers)
- [Resolution](#resolution)
- [Profiles](#profiles)
- [Labels workloads select by](#labels-workloads-select-by)
- [Which profiles a type can host](#which-profiles-a-type-can-host)
- [What was applied](#what-was-applied)
- [Pricing](#pricing)
- [Drift](#drift)
- [Network addresses and egress](#network-addresses-and-egress)
- [Extra volumes and local storage](#extra-volumes-and-local-storage)
- [Kubelet reservations](#kubelet-reservations)
- [Billing periods](#billing-periods)

## Layers

Settings whose meaning differs between vendors (disk types and their tunables,
extra volumes, NIC features, placement constructs, tenancy) exist only in the
vendor's own terms. Four layers, each overriding the previous one, per vendor:

| Layer | Where | Holds | Set by |
|---|---|---|---|
| 1. Built-in defaults | the driver (documented per vendor) | a sensible node: root disk type and size, network settings | SCarpenter |
| 2. NodeClass vendor block | `spec.vendors.<vendor>` (`storage`, `networking`, ...) | overrides of the defaults for every node of the vendor | admin |
| 3. Profiles | `spec.profiles.{storage,network,placement}.<name>.<vendor>` | named, explicit setups workloads opt into | admin |
| 4. Labels | the pod's `nodeSelector` / affinity | exact instance, selection, **profile names**, the placement run key | workload |

A workload never sets a vendor parameter; it picks a profile by name. The
generic settings, identical in meaning on every vendor, are `spec.networking`
(public IPv4 and IPv6, inbound CIDR rules, isolation), the guest side of an
extra volume (`name`, `filesystem`, `mountPath`) and the placement run key.

## Resolution

For a launch, SCarpenter resolves the settings of the vendor in the order of the
table, deterministically:

1. the driver's `Defaults()` (the shape of the vendor block);
2. overlaid with the vendor block (`regions`, `resources` and `networking` are
   not part of the settings: the generic networking is `spec.networking` with the
   block's `networking` override applied);
3. the selected storage profile overlaid on the `storage` member; the selected
   network and placement profiles are passed to the driver as their own blocks.

The overlay is JSON merge: objects merge key by key, scalars and **lists replace**
(a profile's `data` list replaces the vendor block's list; it does not extend
it). The driver decodes the result (`CreateRequest.Settings`) and validates it.
The same function resolves the NodeClass defaults for the catalog (the root disk
the scheduler assumes as ephemeral storage, the price of the defaults). The
ephemeral storage is computed per instance type: a vendor whose root disk is fixed by
the type (hcloud, Vultr and UpCloud plans, OVH flavors with a local root disk) answers
for the type, and a storage profile cannot change it (ADR-017); otherwise it is the
resolved settings' root disk, and when the driver says nothing, the catalog's storage
size of the type (Keeper `storage_size`, read as GB of 10^9 bytes, unverified until
plan 15 checks the unit against a real vendor).

## Profiles

A profile is a name with one block per vendor. On a vendor without a block the
profile **does not exist** (ADR-017): a workload that requires it finds no
instance type and stays Pending; there is no best-effort substitution. The name
`default` is reserved for the vendor defaults.

```yaml
spec:
  profiles:
    storage:
      db-io2:
        aws: {root: {volumeSize: 64Gi}, data: [{name: pgdata, volumeType: io2, volumeSize: 500Gi, iops: 32000, filesystem: xfs, mountPath: /var/lib/pgdata}]}
    placement:
      colocated: {aws: {placementGroup: {strategy: cluster}}}
```

The profile kinds are `storage` (root and extra volumes, local storage),
`network` (NIC features) and `placement` (placement construct, tenancy). A node
has at most one profile of each kind.

## Labels workloads select by

| Label | Kind | Values | Meaning |
|---|---|---|---|
| `scarpenter.sparecores.com/storage-profile` | enum | profile names, `default` | the storage profile |
| `scarpenter.sparecores.com/network-profile` | enum | profile names, `default` | the network profile |
| `scarpenter.sparecores.com/placement-profile` | enum | profile names, `default` | the placement profile |
| `scarpenter.sparecores.com/placement-group` | name | a label value (at most 63 characters) | the run key: nodes with the same key and placement profile share one native placement group |

The three profile labels are defined on every instance type; the run key is the
one key that is not (every value is allowed). All four are registered with
Karpenter core as well-known labels of a restricted domain, so a typo is
rejected. The registry (`pkg/options`) lists them; a test keeps it in step with
the API package.

A pod that does not mention a kind gets the default. A pod that allows several
profiles of a kind gets `default` when it is among them, else the first by name.
The run key must be exactly one value; a placement profile that creates a group
(for example an AWS placement group) needs it, and a launch without one fails
with an error that names the missing run key (an Event `LaunchFailed` on the
NodeClass). Pods with different run keys never share a NodeClaim; a pod that
does not mention the key can still join one. Run keys should be unique per run.

## Which profiles a type can host

The driver decides, per instance type and zone (`NodeOptions.Supported`), which
of the NodeClass's profiles are valid: for example a profile that mounts local
storage is valid only on types that have local disks. The catalog stamps, on
every instance type and offering, `storage-profile In [default, <valid
profiles>]`, likewise for the other kinds, so Karpenter's scheduler rejects
a profile a type cannot host before anything is launched. When
`spec.pricing.allowIncomplete` is `false`, a profile whose price is incomplete is
not valid either (see [Pricing](#pricing)). Changes of the driver's answers
(`EpochReporter`) rebuild the catalog.

## What was applied

Every node carries the three profile labels with the applied name, or `default`,
and the run key as `placement-group` when it has one. The NodeClaim carries the
price (`price`, `price-incomplete`) and the hashes of the profiles it uses
(`profile-hashes`, see [Labels and annotations](labels.md)). The user-data of
the node is rendered per launch attempt, with the offering's zone and instance
type and the device hints of the launch.

## Pricing

An offering's price is the instance price plus the price delta of the resolved
settings: volumes that are not bundled with the instance (Keeper's storage price
per GB-month divided by 730) and public IPv4 where billed. The catalog prices
offerings with the NodeClass defaults; `Create` orders its candidates by the price
with the selected profiles. A component missing from the data (provisioned IOPS
and throughput, dedicated tenancy, paid bandwidth) makes the price
**incomplete**: the price excludes it and the NodeClaim gets the annotation
`scarpenter.sparecores.com/price-incomplete: "true"`. Such offerings are used by
default; `spec.pricing.allowIncomplete: false` takes them out of the catalog
(and the profiles with an incomplete price out of the valid profiles).
Managed shared resources are not attributed to offerings.

## Drift

Per vendor, `status.vendors.<vendor>.hash` covers the shared fields that change
what a node is and the vendor's block (without `regions` and `resources`); see
[the NodeClass controller](node-bootstrap.md#the-nodeclass-controller). Profiles
are hashed separately: at launch the NodeClaim records the hash of each profile
block it uses (`profile-hashes`), and a node drifts when one of them differs from
the profile's current block for the vendor, or the profile was removed or lost
its block for the vendor. Changing one profile therefore drifts only the nodes
that use it; changing another vendor's block or profile drifts nothing.

## Network addresses and egress

`spec.networking.publicIPv4` (default on) and `publicIPv6` (default off) give nodes
public addresses; a vendor block may override them. Generic meaning:

- **Nodes need egress**: they fetch sysexts (GitHub releases, or a mirror) and
  container images at boot. A public IPv4 address gives it on vendors whose default
  network has an internet gateway and no NAT. A driver refuses `publicIPv4: false`
  where the network SCarpenter would use has no other way out (the NodeClass is not
  ready, with the fix in the message); with subnets or networks the user brings
  (`use`), the driver cannot see a NAT or a proxy and allows it: egress is the
  user's job.
- **IPv6 needs IPv6 in the network.** `publicIPv6` is satisfied only by a subnet with
  an IPv6 block; a zone whose subnets have none is unavailable with the reason, it
  does not fail launches. A network SCarpenter creates has IPv6.
- **IPv6-only** (`publicIPv4: false`, `publicIPv6: true`) is accepted with brought
  subnets only. The sysext-bakery's release host (GitHub) has no IPv6: a NodeClass
  with IPv6-only nodes and the default sysext source gets the Warning Event
  `IPv6OnlyNeedsMirror`; set `--sysext-base-url` to a mirror reachable over IPv6.

The AWS specifics are in [the AWS driver](aws-driver.md#network-addresses-and-egress).

## Extra volumes and local storage

Extra volumes (`data` in a storage profile or block) and instance-local disks
(`localStorage: {policy: mount | raid0, mountPath}`; `ignore` leaves them alone)
reach the guest through a first-boot unit that Ignition installs. The driver's
`Prepare` returns a *device hint* per volume: either the stable by-id path (when
the vendor knows it before the instance exists) or a serial pattern and the size
(when the ID exists only after the create), plus the guest side from the profile.
The unit, rendered only for nodes that have volumes or local storage:

- `scarpenter-volume-<name>.service` (oneshot, before the mount): waits up to
  three minutes for the device, finds it, and creates the filesystem (`ext4`,
  `xfs` or `btrfs`) with the label `scv-<8 hex of the name>`. It creates a
  filesystem only on a device without any signature; a device that already
  carries the label (a reboot) is left alone; **any other signature is an error**,
  and the node does not come up. A device found by serial and size is a whole,
  unmounted, unpartitioned disk without a signature that does not carry the label
  of another volume of SCarpenter (`scv-*`, `scl-*`): a disk with a foreign
  filesystem is skipped for the next one, and a disk that already carries the
  volume's label wins over a blank one (a reboot). The units of several volumes run in
  parallel; choosing the device and creating the filesystem happen under one lock
  (`flock`), so two volumes of the same size never take the same disk.
- `<escaped-mount-path>.mount` mounts `/dev/disk/by-label/<label>` at the mount
  path, wanted by `local-fs.target` and ordered after the preparing service
  (no `nofail`). The mounts are **weak** for the boot (a failed volume does not
  stop it: the node stays reachable by ssh, with the failed unit and its journal
  to look at) but **required by the kubelet**: a drop-in
  (`40-scarpenter-volumes.conf`) gives `kubelet.service` `RequiresMountsFor=` all
  the mount paths, so a node whose volume failed never starts its kubelet and
  never registers; Pods do not write to the root disk instead of the volume they
  asked for, and core replaces the NodeClaim that does not register. (Plan 07
  decided this with the first boots on real Flatcar, scenario Q08 of the L4
  lane: making the mounts required for `local-fs.target` would have put the
  node into emergency mode instead, unreachable.) The preparing services have an
  explicit `TimeoutStartSec=210`, above the scripts' own 180 s wait for the device.
  The mount also waits for the device unit of its label
  (`dev-disk-by\x2dlabel-<label>.device`), whose start job times out after
  `DefaultDeviceTimeoutSec` (90 s) counted from the start of the boot transaction,
  not from the end of the preparing service: a volume that attaches late would
  get its label after the job had failed and the mount, and with it the kubelet,
  would fail although the script was still within its 180 s. Each label's device
  unit therefore gets a drop-in with `JobRunningTimeoutSec=240` (what fstab's
  `x-systemd.device-timeout=` generates); the L4 lane checks the setting on the
  device unit of every volume.
- With local storage, `scarpenter-local-storage.service`: `mount` puts an ext4
  filesystem labelled `scl-<n>` on every local disk (one disk at `mountPath`, more
  at `mountPath`, `mountPath-1`, ...); `raid0` stripes them into
  `/dev/md/scarpenter-local` (ext4, label `scl-raid`) mounted at `mountPath`; it
  creates the array only from blank disks, assembles it when all members are
  members of `scarpenter-local`, and refuses a disk with another signature or
  another array. Fewer
  disks than the type has is an error.

Mount paths must be plain absolute paths (letters, digits, `.`, `_`, `-`, `/`)
and are checked when the user-data is rendered. The scripts are
`pkg/bootstrap/templates/scarpenter-volume.sh` and `scarpenter-local-storage.sh`.
They add about 2.5 KiB (compressed and encoded) and 0.3 KiB per volume to the
user-data: eight volumes and four local disks on the tailscale template are about
11 KiB of the 16 KiB limit (the default templates themselves are held to 12 KiB).
The NodeClass controller's trial rendering includes every storage setup each
vendor allows (the vendor block's `storage` and each storage profile on top of it,
with local disks counted as 24, the most a type has), so a NodeClass whose user Butane leaves
no room for its volumes is `ValidationSucceeded=False` (`TemplateInvalid`) before
any launch.

The first-boot scripts were run on real Flatcar (L4 scenarios Q01 and Q02): the
tools they use (`blkid -p`, `lsblk -dnbp -o PATH,SIZE,TYPE,SERIAL`, `findmnt`,
`flock`, `mdadm`, `mkfs.ext4`, `mkfs.xfs`, `mkfs.btrfs`) all exist on Flatcar;
two data disks of one size are told apart under the lock; a disk that carries a
volume's label is taken before a blank one, and a reboot neither wipes nor
creates anything twice (checked with the UUIDs); the raid0 array keeps its
identity across a reboot (`--homehost=any` keeps `/dev/md/scarpenter-local`
stable, the script assembles it from the members' metadata).

## Kubelet reservations

The catalog assumes the AWS provider's kube-reserved formula for every instance
type (CPU by the number of vCPUs, memory by the pod count, 1 GiB of ephemeral
storage), and `spec.kubelet.kubeReserved` replaces single resources. The
user-data is rendered per launch attempt with exactly that reservation of the
chosen instance type in the kubelet's `kubeReserved`, so the node's allocatable
equals what the scheduler assumed (a user-data that carried only
`spec.kubelet.kubeReserved` would leave the kubelet reserving less).

## Billing periods

`spec.billing` is an opt-in that keeps nodes of vendors that bill by the started period (the hour on
hcloud, UpCloud and Vultr) until the end of their paid period, so that Karpenter reuses them. It is a
NodeClass setting and not a node setting: it does not change a node, so it is not drift. How it works, when it
helps and its limits are in
[Reuse nodes within the billing period](../how-to-guides/reuse-nodes-within-billing-period.md)
(ADR-024, plan 23; not released yet).
