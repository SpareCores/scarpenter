# Vendor capabilities: storage, networking, placement, tenancy, location

What the eight target vendors can do at instance-create time, and what the
Spare Cores data already knows about it. This is the factual basis for the
node-options design ([`../plans/v1/wip/00a-node-options.md`](../plans/v1/wip/00a-node-options.md)).

Researched 2026-11-05. Facts come from vendor API docs, the vendors' Go SDKs
(`hcloud-go`, `upcloud-go-api`, `govultr`, `gophercloud`), the Karpenter
providers surveyed in ADR-001, and the Spare Cores code at
[sc-crawler@f598594](https://github.com/SpareCores/sc-crawler/tree/f5985945618a30da13c5455e3357b8ae90d75eac),
[sc-keeper@0cc3273](https://github.com/SpareCores/sc-keeper/tree/0cc327303bc44d7ffed3934d9ee454782e3eb78a),
[sc-runner@0c210d3](https://github.com/SpareCores/sc-runner/tree/0c210d3f6118823f9d64d148af3824621ce473a6)
and
[sc-inspector@c583123](https://github.com/SpareCores/sc-inspector/tree/c583123c85b44c65bbf6fa0ec0b67418469e4abf).
Cells marked † are from general vendor knowledge and were not re-verified;
check them before a driver depends on them.

## Table of Contents

- [Prerequisite resources](#prerequisite-resources)
- [Storage](#storage)
- [Networking](#networking)
- [Placement](#placement)
- [Tenancy and reservations](#tenancy-and-reservations)
- [Location metadata](#location-metadata)
- [What the Spare Cores data covers](#what-the-spare-cores-data-covers)
- [What sc-runner and sc-inspector use today](#what-sc-runner-and-sc-inspector-use-today)

## Prerequisite resources

What must exist in the account before an instance can be created, and what
a usable default exists for. Every resource here can be pre-created by the
user (e.g. with Pulumi or Terraform, in every region) and referenced, or created by
SCarpenter on demand (design: [00a §9](../plans/v1/wip/00a-node-options.md#9-vendor-resources-bring-your-own-or-managed)).

| Vendor | Required for every instance | Usual default | Needed for options |
|---|---|---|---|
| aws | VPC, subnet per zone (with route to an internet gateway for egress), security group | default VPC with a default subnet per zone and a default security group (may have been deleted) | placement groups; separate volumes not needed (block device mappings) |
| gcp | network, subnet per region, firewall rules for anything beyond the implied deny-inbound | `default` network (often removed by org policy) | resource policies (placement) |
| azure | resource group, VNet, subnet, NIC per VM; public IP per VM for egress on new VNets (or a NAT gateway) | **none** | NSG / application security groups, proximity placement groups, separate disks for PremiumV2/Ultra |
| alicloud | VPC, vSwitch per zone, security group | default VPC and vSwitches often exist | deployment sets |
| hcloud | none | public network | firewall (inbound is open without one), private network, placement group, volumes, primary IPs |
| upcloud | none | public + utility interfaces | SDN network and router, server group |
| ovh | none (possibly an SSH key object †) | Ext-Net public network | private network, subnet and gateway, server group, volumes |
| vultr | none | public network | firewall group (inbound is open without one), VPC, block storage |

SSH keys do not need vendor objects on Flatcar: Ignition installs them.
Vendors that insist on a key object at create time get one through the same
mechanism as any other prerequisite.

## Storage

| Capability | aws | gcp | azure | alicloud | hcloud | upcloud | ovh | vultr |
|---|---|---|---|---|---|---|---|---|
| Root size configurable | yes | yes | yes (billing-tier rounded) | yes | **no** (server type) | yes (template clone, 10–4096 GB) | only by booting from a volume; else fixed by flavor | **no** (plan); diskless VX1 plans boot from block storage |
| Root type choice | gp2, gp3, io1, io2, standard | pd-\*, hyperdisk-\* (**some series are Hyperdisk-only**: c4, n4, a3, …) | Standard, StandardSSD, Premium; PremiumV2/Ultra only as a separately created disk | cloud\*, ESSD PL0–3, `cloud_auto`, `cloud_essd_entry` (per-zone availability) | — | maxiops, standard, hdd | with boot-from-volume: classic, high-speed, high-speed-gen2 | — |
| IOPS/throughput tunable | gp3 (both), io1/io2 (IOPS) | hyperdisk (both) | PremiumV2, Ultra | ESSD performance level, AutoPL provisioned IOPS | — | — | — (scales with size) | — |
| Extra volumes in the create call | yes (block device mappings) | yes (`disks[]`) | yes (`dataDisks[]`), not PremiumV2/Ultra | yes (`DataDisk.1..16`) | attach **pre-created** volumes (`volumes[]`) | yes (`storage_device` with `action: create`) | yes (Nova block device mapping, `source_type: blank`) | **no**: create, then attach |
| Deleted with the instance by default | root yes, mapped extras yes | **no** (`autoDelete` must be set) | **no** (`deleteOption` defaults to Detach) | yes | **no** (volumes outlive servers) | root yes; extras only when deleting with storages † | **no** (`delete_on_termination` defaults false) | **no** |
| Attachment scope | zone | zone | zone (zonal VMs) | zone | location | zone | zone in 3-AZ regions, else region | region |
| Max extra volumes | per type, shared with NICs † | up to 128 (series-dependent) † | per VM size (MaxDataDiskCount) | 16 | 16 | ~15 † | ~25 † | unknown |
| Local / instance storage | NVMe instance store, unformatted | local SSD, NVMe, 375 GiB units | temp disk, local NVMe on some sizes | local disks on i/d families | root only | none | NVMe on some flavors | root only (bare metal: more) |
| Encryption | flag + KMS | always; CMEK optional | always; DES / encryption-at-host optional | flag + KMS | none | flag | limited † | none |
| Stable device path for Ignition | NVMe by-id with the volume ID (unknown before create) | `/dev/disk/by-id/google-<deviceName>` (we choose the name) | `/dev/disk/azure/scsi1/lun<N>` (we choose the LUN) | virtio by-id from the disk ID † | `/dev/disk/by-id/scsi-0HC_Volume_<id>` (volume pre-created, ID known) | virtio by-id from the storage UUID † | virtio by-id from the volume UUID † | virtio † |

Consequences:

- **Two lifecycles.** On aws, gcp, azure, alicloud, upcloud and ovh, extra
  volumes are part of the create call, as long as delete-with-instance is
  always set explicitly. On hcloud and vultr, volumes are separate resources:
  create them, then create the server, then delete them after the server is
  gone. These need SCarpenter-owned garbage collection.
- **Device identity drives ordering.** Where the stable path contains a
  volume ID that only exists after creation, either the volumes are created
  first and the user-data rendered afterwards, or a first-boot unit finds the
  device by serial or size. GCP and Azure let us name the device up front.
- **Fixed root disks** (hcloud, vultr, most OVH flavors): the root size is a
  property of the instance type. sc-runner's Vultr workaround swaps to a
  bigger sibling plan. That would change the instance type, which a benchmark
  must never do.
- **Hyperdisk-only GCP series** and per-zone Alibaba disk categories make the
  allowed disk type a property of the (instance type, zone), not of the
  NodeClass alone.

## Networking

| Capability | aws | gcp | azure | alicloud | hcloud | upcloud | ovh | vultr |
|---|---|---|---|---|---|---|---|---|
| Usable without creating a network | default VPC (if not deleted) | `default` network (often removed by policy) | **no**: resource group, VNet and subnet needed | default VPC/vSwitch often exist; Spare Cores uses a shared pre-created VPC | yes (public) | yes (public + utility) | yes (Ext-Net) | yes (public) |
| Subnet required, scope | yes, zone | yes, region | yes, region | vSwitch, zone | no | no | no | no |
| Public IPv4 can be omitted | yes | yes | yes, but new VNets (API versions after 2026-03-31) have no default outbound access | yes (`InternetMaxBandwidthOut=0`) | yes | likely † | yes (needs a gateway for egress) | yes |
| IPv6 | VPC + subnet CIDR, then a count | dual-stack subnet | dual-stack VNet + second IP config | IPv6 CIDR + gateway | create-time flag | on the public interface | usually on † | create-time flag |
| Public IPv4 cost | ~$0.005/h | ~$0.005/h | ~$0.005/h | free (bandwidth or traffic billed) | €0.50/month | per IP, hourly | included | included (reserved IPs extra) |
| Firewall model | security group per ENI | VPC rules by tag | NSG + application security groups | security group per instance | firewall per server or by **label selector** (public interface only) | per-server rules (public + utility) | OpenStack security groups (coverage of public ports unverified) | firewall group (public interface only) |
| Inbound default | deny | deny | deny (except from the VNet) | deny | **open** without a firewall | on; rule default unverified | open † | **open** without a group |
| "Only nodes of the same run may connect" | SG self-reference | tag → tag rule | ASG | SG reference | private network per run | private network per run | private network per run | VPC per run |
| Create-time performance options | ENA Express, EFA, cluster placement | gVNIC, Tier_1 egress | accelerated networking | internet bandwidth + charge type, eRDMA | — | — | — (per flavor) | — |
| Free egress | 100 GB/month | 200 GB/month † | 100 GB/month | none (pay per GB) | 20 TB EU / 1 TB US per server | per plan | unlimited (APAC: 1 TiB) | pooled per plan |

Consequences:

- "Deny all inbound" is free on the hyperscalers but needs an attached
  firewall on hcloud, vultr, upcloud and (probably) ovh. On hcloud one
  permanent label-selector firewall covers every node of the cluster.
- "Same run only" is a reference rule on the hyperscalers and a per-run
  private network everywhere else. Benchmarks between nodes of a run should
  use the private addresses anyway.
- Alibaba's sc-runner default of 5 Mbit/s outbound bandwidth caps any
  internet-facing benchmark.

## Placement

| Strategy | aws | gcp | azure | alicloud | hcloud | upcloud | ovh | vultr |
|---|---|---|---|---|---|---|---|---|
| Cluster (co-locate) | placement group `cluster` (one zone) | compact resource policy (22 VMs without `maxDistance`) | proximity placement group (pinned by the first VM) | deployment set `LowLatency` (one zone, 20) | — | — | server group `affinity` | — |
| Spread | placement group `spread` (7 per zone per group) | spread resource policy † | availability zones / sets | deployment set `Availability` (20 per zone) | placement group `spread` (10 servers, 50 groups) | server group anti-affinity (strict or soft, enforced at start) | server group `anti-affinity` (bounded by hypervisors and quota) | — |
| Partition | placement group `partition` (7 per zone) | — | — | deployment set `AvailabilityGroup` (1–7 groups) | — | — | — | — |

Every construct is a separate resource that must exist before the instance
is created, is free, and can only be deleted when empty. Exceeding a group's
size limit fails like a capacity error (hcloud returns `placement_error`).
The AWS Karpenter provider supports one pre-existing placement group per
NodeClass and explicitly does not create groups; none of the surveyed
providers creates groups per workload.

## Tenancy and reservations

| | aws | gcp | azure | alicloud | hcloud, upcloud, ovh, vultr |
|---|---|---|---|---|---|
| Dedicated instance | `tenancy=dedicated` | — | — (isolated sizes are instance types) | — | — (dedicated-vCPU plans are instance types) |
| Dedicated host | Dedicated Hosts | sole-tenant node groups | dedicated hosts | dedicated hosts (DDH) | — |
| Reservations | ODCR, capacity blocks | reservations | capacity reservation groups | capacity reservations, elasticity assurance | — |

The Spare Cores catalog prices shared tenancy only (the AWS crawler stores
`Shared` only; the GCP crawler drops sole-tenancy SKUs). Dedicated prices are
unknown to SCarpenter.

## Location metadata

Per region, the Spare Cores data has `country_id` (ISO alpha-2), `state`,
`city`, `lat`, `lon`, `founding_year`, `aliases` and a tri-state
`green_energy` (null = unknown; AWS and hcloud are always true, Alibaba and
OVH unknown). Countries map to an English continent name (39 countries, no
codes). Zones carry no location data. Keeper serves these as raw rows
(`/table/region`, `/table/zone`, `/table/country`) to be joined by the
client.

Zone identities differ: AWS zone IDs are portable (`use1-az1`) while names are
per account; GCP uses numeric IDs and zone names; Azure uses `1`, `2`, `3`
(`0` without zones); hcloud, UpCloud and Vultr use the region as a dummy zone.

## What the Spare Cores data covers

| Topic | Present | Missing |
|---|---|---|
| Storage | one row per volume type (type, max IOPS, max throughput, min/max size); GB-month price per region; per-server local disks for aws, gcp, azure, ovh | tunable-IOPS/throughput ranges and their prices; bootable flag; scope; delete default; `compatible_storage_ids` (declared, never filled); local disks for hcloud, vultr, upcloud, alicloud; alicloud `cloud_auto` price; gcp `pd-extreme`; max volumes per instance |
| Network | per-server baseline/max speed (aws, alicloud, ovh, vultr), free traffic, IPv4 count; IPv4 and traffic prices per region | speed for gcp, azure, hcloud, upcloud; NIC feature flags (ENA Express, EFA, gVNIC/Tier_1, accelerated networking, eRDMA); IPv6 and paid-bandwidth prices |
| Placement | — | which instance families support which strategy |
| Tenancy | shared prices | dedicated instance and host prices |
| Location | region location fields, country → continent name | continent codes, zone locations |

## What sc-runner and sc-inspector use today

- **Disks:** root size everywhere it is configurable (128 GiB for
  benchmarks); disk type, IOPS and throughput only for the database VM of
  multi-VM runs (gp3/io2 on AWS, pd-ssd or hyperdisk-balanced on GCP,
  PremiumV2 on Azure). No extra volumes.
- **Networks:** default or shared networks for single VMs; a private network
  per stack for multi-VM runs on every vendor (hcloud's network zone is
  hard-coded to `eu-central`). Firewalls are allow-all or absent.
- **Multi-VM runs:** two VMs of different types in the same zone, connected
  by private IP.
- **Spot:** AWS tries spot in every region, then on-demand; GCP uses spot for
  `e2` and GPU machines.
- **Not used:** placement groups, tenancy, reservations, network performance
  options.
