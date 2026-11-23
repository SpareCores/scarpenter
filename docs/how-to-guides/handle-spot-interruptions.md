# Handle spot interruptions

When a vendor reclaims a spot or preemptible node, SCarpenter drains it at once instead of waiting for the node to fail: your Pods get
their SIGTERM with the time the vendor still gives, the reason is recorded on the NodeClaim and the Node, and the interrupted offering
(instance type and zone) is kept out of scheduling for a while. Design: [ADR-027](../explanation/DECISIONS.md#adr-027-spot-interruption-the-notice-is-an-annotation-the-reaction-is-one-controller-accepted-2026-11-19),
[plan 26](../plans/v1/done/26-interruption.md); mechanics: [the controller reference](../reference/controller.md#interruption).

Status: **OCI, AWS, GCP, Alibaba Cloud and Azure**. OCI (preemptible instances, found by polling the OCI API) records the reason and cleans the node up fast, but has
no graceful drain window yet, because the poll sees the preemption only once the instance is stopping or terminating; its path follows the vendor's
documentation and is unverified until the first real preemption: see [OCI interruption](../reference/oci-driver.md#interruption).
AWS, GCP (Spot VMs), Alibaba Cloud and Azure spot nodes run the [node agent](../reference/node-bootstrap.md#the-interruption-agent) (a shell script in the metadata slice that
annotates its own Node). AWS checks IMDSv2's `spot/instance-action`: EC2's two-minute notice reaches the controller within seconds ([AWS interruption](../reference/aws-driver.md#interruption);
unverified until the owner's live check, spike S2). GCP: see [GCP interruption](../reference/gcp-driver.md#interruption). Alibaba Cloud uses the spot termination time of the metadata service; unverified until a real reclaim:
see [Alibaba Cloud interruption](../reference/alicloud-driver.md#interruption).
Every vendor with spot is covered, each with its own check script for that agent; UpCloud (GPU spot only, ACPI-only notice) is intentionally left out.

Azure's agent reads Scheduled Events on a Spot VM and gets at least 30 s of notice, best effort. It is unverified until
the first live eviction (spike S4); see [Azure interruption](../reference/azure-driver.md#interruption).

## What your workload sees

1. The Node gets the annotations `scarpenter.sparecores.com/interruption` (`SpotInterrupted`), `interruption-deadline` and
   `interruption-source` ([labels](../reference/labels.md#interruption-annotations)); the NodeClaim gets them too, and the conditions
   `Interrupted` and `DisruptionReason`, both with the reason `SpotInterrupted`.
2. The NodeClaim is deleted, Karpenter taints the node `karpenter.sh/disrupted:NoSchedule` and evicts the Pods. Pods whose
   `terminationGracePeriodSeconds` would outlast the deadline are deleted with the grace cut to the time left (at least 1 s); PDBs and
   `karpenter.sh/do-not-disrupt` do not hold them back. The drain stops waiting at the deadline minus `--interruption-margin` (5 s).
3. A Warning Event `SpotInterrupted` on the Node and the NodeClaim names the type, the zone, the deadline and the lead time. Events
   expire after an hour; count with the metric instead.
4. The replacement does not use the same type in the same zone as spot for `--interruption-unavailable-ttl` (10 minutes); on-demand
   offerings of that type stay available.

A node that vanished without any notice is cleaned up by Karpenter's garbage collection (about a minute after its Node goes NotReady, plus
up to two minutes) and recorded with the reason `ReclaimedWithoutNotice`; a vendor that tells a reclaim from a crash gives `SpotInterrupted`.

## Count interruptions per instance type

`scarpenter_interruptions_total{vendor,instance_type,zone,reason,source}` is the durable count:

```promql
sum by (instance_type) (increase(scarpenter_interruptions_total{reason="SpotInterrupted"}[1d]))
```

`scarpenter_interruption_lead_seconds{vendor,source}` is the lead time the controller got, and `scarpenter_interruption_reaction_seconds`
the time from seeing a notice to the NodeClaim delete.

## Set `terminationGracePeriodSeconds`

The grace a Pod gets is at most the lead time SCarpenter sees, and on OCI that is none yet: the poller sees the instance already stopping
or terminating, not the two-minute notice before it.

| Vendor | Lead time at the node | Assume for the grace period |
|---|---|---|
| AWS (spot) | 2 minutes, best effort: the `time` of `spot/instance-action`, polled every 2 s | about 100 s; the controller's deadline is the notice's `time` minus the 5 s margin |
| OCI (preemptible) | none seen: about 2 minutes are documented, but only as an Events message; the API poll sees `STOPPING`, `STOPPED` or `TERMINATING` | none: the reason is recorded and the node cleaned up fast, Pods are deleted at once (1 s grace); no graceful drain window yet |
| Alibaba Cloud (spot) | 5 minutes in the product news (*unverified* with `SpotDuration=0`); the agent polls every 5 s | at least 20 s is safe; the deadline is clamped to 5 minutes |
| Azure (Spot) | 30 s at least, best effort (a `Preempt` Scheduled Event; the VM must have registered for events at boot, so an eviction in its first minutes has no notice) | 20 s (a flush of 5 s fits); the controller deletes the Pods at the deadline less 5 s, so a grace above about 25 s is cut |
| GCP (Spot) | 30 s: the ACPI shutdown after the preemption; with `--gcp-preemption-notice=N`, `N` (up to 2 minutes) before it, which is off by default | 30 s by default (the controller takes the shutdown window), `N` seconds with a notice set |

A workload that needs about 5 s to flush (benchmarks) sets `terminationGracePeriodSeconds` to about 20 s for the vendors that give a notice (AWS, GCP, Alibaba Cloud and Azure today).
On OCI, until the in-VM check (plan 26) gives the node the two minutes, write progress early and make the Pod's work restartable: a preempted
node is gone before a flush can finish.
