# 26 — Spot and preemptible interruption handling

Depends on: 04 (CloudProvider, the fake driver and clock, the L3 KWOK lane), 05 (boot fragments, per-attempt
rendering), 07 (the L4 qemu lane with a real kubelet), 14 (contract groundwork), 23 (the retention controller's use of
`state.Cluster`, the model for a SCarpenter controller next to core), 25 (the OCI driver), and [ADR-025](../../../explanation/DECISIONS.md#adr-025-instance-metadata-reaches-only-the-nodes-own-components-accepted-2026-11-19) for the
node agent (steps 4 to 8): the allowlist and its `scarpenter-metadata.slice` are on main (see "Dependencies"). Read `docs/plans/v1/CONVENTIONS.md` first. Spec: this plan; overview §5.6 (the unavailable
cache), §7 (driver contract), §10 (garbage collection); ADR-011 (lists interruption handling as not planned), ADR-017,
ADR-024 (interruption is forceful and ignores retention holds), ADR-025, ADR-026; `docs/reference/driver-contract.md`
(instance states, the stopped-node rule), `docs/reference/node-bootstrap.md` (boot fragments, the size budget,
instance metadata access).

Written 2026-11-19 from public vendor documentation and the Karpenter core v1.15.1 source in the module cache. No
cloud API was called. Everything marked **unverified** is settled by a spike (section 9) or a live run.

## Goal

When a vendor reclaims a spot or preemptible node, Karpenter drains it **as early in the notice window as possible**,
so every Pod gets its SIGTERM with time to finish: the first workload (benchmarks) needs about 5 s to flush and sets
`terminationGracePeriodSeconds` to about 20 s, so a 30 s notice (Azure) must be enough. The reason is visible on the
NodeClaim and the Node and is distinguishable from a crash, so a workload can count interruptions per instance type.
A reclaim without notice is still cleaned up and recorded with its own reason. The interrupted spot offering (type,
zone) is kept out of scheduling for a TTL. Spot is offered and launched correctly per vendor.

Order of vendors (decided by the maintainer 2026-11-19): **OCI preemptible first** (the first benchmarks run there), then AWS, then GCP,
Azure, Alibaba. Hetzner, Vultr, OVH have no spot (confirmed in step 9). UpCloud (GPU spot only, ACPI-only notice) is
deferred.

## 1. Mechanisms (decided)

Two notice paths, one reaction:

- **In-VM path (AWS, GCP, Azure, Alibaba): the node agent.** A small systemd unit on spot nodes polls the vendor's
  in-VM notice and, on a notice, annotates **its own Node** with the kubelet's credentials. No cloud plumbing, no
  IAM, no queues, the full lead time the vendor gives. It runs in `scarpenter-metadata.slice` (ADR-025), so the
  metadata service stays closed to Pods and other host processes.
- **Controller path (OCI, decided by the maintainer 2026-11-19): the interruption poller.** OCI documents no in-VM
  signal; its two-minute `instancepreemptionaction` is an Events-service event, and event-service plumbing is ruled out. The
  controller polls the OCI API, one poller per region shared by all instances (section 4). If spike S1 finds an
  in-VM preemption signal, OCI uses the node agent for the lead time and the API poller stays as a backstop
  (decided 2026-11-19, section 4).
- **Reaction: the interruption controller** (`pkg/controllers/interruption`). Both paths end in the same Node
  annotation or an equivalent in-memory notice; the controller tightens the termination deadline, records the
  reason, marks the offering unavailable and deletes the NodeClaim. Karpenter core does the rest (taint
  `karpenter.sh/disrupted:NoSchedule`, drain, instance delete).
- **Silent reclaim** (no notice, or a missed one): core's NodeClaim garbage collection (section 5), plus a reason
  SCarpenter records where it can tell.

Rejected: vendor event streams (AWS EventBridge and SQS, Azure Event Grid, GCP log sinks, OCI Events, Alibaba
CloudMonitor): a queue, rules and permissions per vendor and per account, owned by the operator's IaC, with no lead
time over the in-VM poll (they may be added later as another notice source). Core's node repair
(`RepairPolicies`): alpha, off by default, paced by disruption budgets: too slow for a 30 s notice. A Pod (DaemonSet)
agent: it would need the metadata service open to Pods, which ADR-025 closes, and RBAC to patch Nodes.

## 2. The node agent (steps 4 to 8)

**A shell script, not a Go binary** (decided). It is rendered into the user-data as a boot fragment, the same way
ADR-025's firewall script is: nothing to build, sign, publish, mirror or pin per release, no sysext, nothing for the
operator's IaC to host; it is readable on the node. The cost is user-data: the generic loop plus one vendor's check
must stay under 1.5 KiB (golden size test; the default templates are held to 12 KiB of AWS's 16 KiB). A Go binary
would need a release artifact with a sha256 pin and a mirror flag like the sysexts; reconsider only if the script
grows past that budget.

- **Rendering.** Only on **spot launches** (the user-data is rendered per attempt and the attempt knows its capacity
  type). New optional driver capability `InterruptionNotices` returning `NoticeCheck{Script string, Interval,
  MinNotice, MaxNotice time.Duration}`: `Script` is a POSIX `sh` function body `scarpenter_check` that prints
  `<reason> <deadline RFC 3339>` and returns 0 when a notice is present, returns 1 when not, 2 on a transient error.
  The core (`vendors.InterruptionAgent(d)`, next to `vendors.RestrictMetadata`) wraps it in the generic loop and the
  units; the core never branches on a vendor. A driver without the capability gets no agent.
- **Units.** `scarpenter-interruption.service`: `Slice=scarpenter-metadata.slice`, `Wants=` and `After=`
  `scarpenter-metadata-firewall.service` (when the restriction is rendered), `After=network-online.target`,
  `Restart=always`, `RestartSec=1`. It starts at boot, before the kubelet has a client certificate: Azure's opt-in
  needs the first metadata call early (section 3). With `instanceMetadata.access: Open` the restriction fragment is
  absent; systemd creates the slice implicitly and the agent works the same.
- **Loop.** Run `scarpenter_check` every `Interval` (AWS 2 s, Azure 1 s, Alibaba 5 s; GCP blocks in
  `wait_for_change`). On a notice: clamp the deadline to `[now, now + MaxNotice]` (a missing or unparsable deadline
  is `now + MinNotice`, the vendor's documented minimum, a field of `NoticeCheck`), then patch the Node
  until a 2xx, then keep running without patching again (idempotent across restarts: it reads the Node's annotation
  first). Log one line per state change to the journal.
- **Report.** A JSON merge patch of `/api/v1/nodes/<.NodeName>` with `curl`, the API server from
  `.APIServerEndpoint`, `/etc/kubernetes/pki/ca.crt`, and the kubelet's client certificate
  (`/var/lib/kubelet/pki/kubelet-client-current.pem`, certificate and key in one file; wait until it exists).
  Annotations (also the reference names): `scarpenter.sparecores.com/interruption` = reason (`SpotInterrupted`),
  `scarpenter.sparecores.com/interruption-deadline` = RFC 3339, `scarpenter.sparecores.com/interruption-source` =
  `instance-metadata`.
- **Credential: the kubelet's** (decided). NodeRestriction limits a `system:node:<name>` identity to its own Node
  and forbids taints and most labels but not annotations (kubernetes master, `admitNode`; to verify at the pinned
  Kubernetes in L4). A separate identity would need its own bootstrap and is not node-bound under NodeRestriction.
  The agent runs as root on the host, the kubelet's trust level already. A node that lies can only delete itself
  and mark its own type and zone unavailable for one TTL (section 3 bounds it).
- **Graceful node shutdown backstop** (decided by the maintainer 2026-11-19): spot launches render
  `shutdownGracePeriod: 25s`, `shutdownGracePeriodCriticalPods: 5s` in the kubelet configuration, so an ACPI
  shutdown with no usable notice (GCP with a 0 s notice, a missed notice, UpCloud later) still sends SIGTERM.
  Verify in L4 that Flatcar's logind honours the kubelet's inhibitor (`InhibitDelayMaxSec`).

## 3. The interruption controller (step 1)

Watches Nodes with the `interruption` annotation (predicate on the annotation key) and NodeClaims; time from
`clock.Clock`; built with the operator's `state.Cluster` like the retention controller. Per Node:

1. Find the NodeClaim by provider ID; skip if not managed by SCarpenter. Skip with a Warning Event
   `InterruptionIgnored` (once) when the NodeClaim is not spot (`karpenter.sh/capacity-type`), the reason is not
   known, or the deadline is not parsable (then use `now + the driver's MinNotice`).
2. Clamp the deadline to `[now, now + driver MaxNotice]`.
3. **Tighten the deadline**: `nodeclaimutils.TightenTerminationTimestampAnnotation(ctx, kube, claim, deadline -
   margin)` (verified in core v1.15.1, `pkg/utils/nodeclaim/nodeclaim.go`: sets
   `karpenter.sh/nodeclaim-termination-timestamp` only when earlier than an existing one). The termination
   controller then force-deletes, with the grace clamped to the time left (minimum 1 s), every Pod whose grace would
   outlast the deadline, PDBs and `do-not-disrupt` included (`terminator/eviction.go` `needsForceDelete`,
   `forceDelete`), and stops waiting for the drain at the deadline. `--interruption-margin` (default 5 s) is the time
   the instance needs after the kubelet stops containers. In the same metadata patch: copy the three annotations to
   the NodeClaim.
4. Status patch of the NodeClaim: condition `Interrupted` = True, reason `SpotInterrupted` (SCarpenter's own
   type: core's `ClearNodeClaimsCondition` may clear `DisruptionReason` on nodes it does not consider disrupting), and
   `DisruptionReason` = `SpotInterrupted` too, so core's eviction Events and `karpenter_pods_drained_total` carry the
   reason (`terminator/eviction.go` `evictionReason`). Once the NodeClaim is deleting, core counts it as disrupting
   (`StateNode.Deleted()`), so its disruption controller no longer clears the condition.
5. **Delete the NodeClaim.** Core's node termination taints and drains at once (`MinDrainTime` 5 s only delays the
   `Drained` condition after the drain, verified in `node/termination/controller.go`).
6. Mark the offering unavailable: new class in `pkg/unavailable`, `MarkInterrupted(vendor, instanceType, zone,
   capacityType)` with its own TTL `--interruption-unavailable-ttl` (default 10 m, decided 2026-11-19; a type just reclaimed is likely to be
   reclaimed again), matched in
   `IsUnavailable` like `InsufficientCapacity`, bumping the sequence so the next scheduling round sees it. SCarpenter
   already has its own cache (`pkg/unavailable`, consulted by `pkg/catalog/catalog.go` when it builds offerings);
   this is not core's job (the AWS provider keeps its own too).
7. Events `SpotInterrupted` (Warning) on the Node and the NodeClaim, with type, zone, deadline and lead time in the
   message; metrics below. Steps 3 to 6 are idempotent: a restart or a second reconcile changes nothing.

Steps 3 to 5 run before anything slow; the target is under 1 s from the Node update to the NodeClaim delete.

The poller path (section 4) calls the same handler with the notice in memory and also writes the annotations to the
Node (source `vendor-api`), so the record looks the same whichever path found it.

**Metrics:** `scarpenter_interruptions_total{vendor,instance_type,zone,reason,source}` (the durable count a
workload should use: Events expire after an hour), `scarpenter_interruption_lead_seconds{vendor,source}`
(histogram, deadline minus the time the controller saw the notice), `scarpenter_interruption_reaction_seconds`
(notice seen to NodeClaim delete), `scarpenter_interruption_poll_errors_total{vendor,region}`.

RBAC: `nodes` get, list, watch, patch (held); `nodeclaims` get, list, watch, patch, delete; `nodeclaims/status`
patch; `events` create. Flag `--interruption` (default on) turns the controller and the poller off.

## 4. The interruption poller (step 2) and OCI (step 3)

New optional capability `InterruptionPoller`: `PollInterruptions(ctx, region string) ([]Notice, error)` and
`PollInterval() time.Duration`, `Notice{Ref InstanceRef, Reason string, Deadline time.Time}`. A core controller runs
one poller per (vendor, region) that has at least one spot NodeClaim, and stops when there is none. It maps a
notice to the NodeClaim by provider ID and calls the handler of section 3. Errors back off (429 honoured) and are
counted; one failed poll never fails the controller.

**OCI** (driver side, step 3). Which API shows a preemption before termination is **unverified**: the spike
(section 9, S1) checks in order (a) `GetInstance`/`ListInstances` fields during the window (`lifecycleState`, a
preemption or maintenance field); (b) Work Requests (`ListWorkRequests` for the compartment, by resource);
(c) `ListInstanceMaintenanceEvents`; (d) the Audit service (latency likely too high); and, outside the decision but
cheap to observe, (e) the instance metadata (`/opc/v2/instance/`) during the window: an Oracle blog says OKE's node
termination handler polls IMDS every 2 s, while an Oracle forum post finds no preemption field. **Decided
2026-11-19:** if (e) shows a preemption signal, OCI gets a `scarpenter_check` script and the node agent (section 2)
for the full lead time, and the poller stays on as a backstop that reports `TERMINATING` at once; otherwise the
poller alone.
The poll is one paged list call per compartment and region per interval, default 10 s (`--oci-interruption-poll`),
so about 6 calls a minute per region whatever the number of nodes; OCI does not publish its Compute API rate limits
(spike records the 429 behaviour). With a 120 s notice and a 10 s poll, Pods keep about 105 s. If only the
`TERMINATING` state is visible (no lead time), the poller still reports it at once (reason `SpotInterrupted`,
deadline `now`): faster than garbage collection, which waits for the Node to be NotReady.

## 5. Silent reclaim (step 2)

Verified in core v1.15.1 (`controllers/nodeclaim/garbagecollection/controller.go`): every 2 minutes it lists the
cloud provider's instances and deletes a Registered NodeClaim whose instance is gone **once its Node is not Ready**
(a Ready Node is skipped), counting `karpenter_nodeclaims_disrupted_total{reason="garbage_collected"}`; it publishes
no Event. SCarpenter's `Get`/`List` already treat `terminating` and `terminated` as gone (driver contract, instance
states), and every driver launches spot with delete-on-reclaim (section 7), so a reclaim is seen. Latency: the
node-monitor grace period (about 40 to 50 s) plus up to 2 minutes.

The reason: SCarpenter's `CloudProvider.Delete` and `Get` see the instance gone; for a NodeClaim without the
`interruption` annotation they publish `ReclaimedWithoutNotice` (spot) or `InstanceVanished` (on-demand) on the
NodeClaim, set the `Interrupted` condition with that reason (spot only), count the metric with `source=gc` and, for
spot, mark the offering unavailable. A driver that can tell a reclaim from a crash sets the new optional
`Instance.GoneReason` (AWS `StateReason.Code` `Server.SpotInstanceTermination`, Alibaba `OperationLocks` `Recycling`,
GCP the zone operation `compute.instances.preempted`; others empty): then the reason is `SpotInterrupted` with
`source=vendor-api`. A crashed spot node whose instance still exists is not touched (node repair is core's).

## 6. Per-vendor notice

| Vendor | Notice source | Lead | Poll | Deadline | Status |
|---|---|---|---|---|---|
| OCI | API poller (section 4) | 120 s before termination begins (Events doc) | 10 s | from the signal, else `now` | step 3 |
| AWS | IMDSv2 `meta-data/spot/instance-action` (404 when none; `{"action":"terminate","time":...}`) | 120 s, best effort | 2 s (AWS advises 5 s) | `time` | step 5 |
| GCP | `computeMetadata/v1/instance/preempted?wait_for_change=true` (`Metadata-Flavor: Google`), then ACPI G2 | 0 s (default) or 120 s set at create, then up to 30 s shutdown | blocking | `now + notice` (no time in the response) | step 6 |
| Azure | Scheduled Events `?api-version=2020-07-01` (`Metadata: true`), `EventType: Preempt` with own VM name in `Resources` | 30 s minimum, best effort | 1 s | `NotBefore` | step 7 |
| Alibaba | `100.100.100.200/latest/meta-data/instance/spot/termination-time` with the metadata token | 5 min (product news; unverified) | 5 s | the timestamp | step 8 |
| UpCloud | ACPI only, 30 s, then forced off | 30 s | — | — | deferred; the shutdown backstop covers it when added |
| Hetzner, Vultr, OVH | no spot | — | — | — | step 9 confirms |

Azure: the first call opts the VM in (up to 2 minutes; events can start up to 5 minutes later), so the agent starts at
boot; a VM evicted inside that window has no notice and falls to section 5. No `StartRequests` acknowledgement for
`Preempt`: approving an event ends its window early. AWS rebalance recommendations and Azure
`SpotRebalanceRecommendation` are ignored (decided 2026-11-19).

## 7. Spot in the catalog and at launch

- **Catalog**: spot offerings come only from Keeper's spot prices (overview §5.2); `pkg/catalog` builds them with
  `karpenter.sh/capacity-type: spot` and the spot price, and the NodeClaim gets the label from
  `Instance.CapacityType` (`pkg/cloudprovider/instances.go`). Step 9 checks Keeper's snapshot per vendor (spot rows for
  AWS, GCP, Azure, Alibaba, OCI; none for Hetzner, Vultr, OVH; UpCloud GPU) with the committed snapshots.
- **Launch, already done by the drivers** (read 2026-11-19): AWS one-time spot, interruption behaviour `terminate`;
  GCP `SPOT`, `instanceTerminationAction: DELETE`, `onHostMaintenance: TERMINATE` (**missing: the 120 s notice**,
  step 6, requested by default when S3 shows it is free; field name unverified); Azure `Spot`, eviction policy `Delete`, max price -1; OCI
  `preemptibleInstanceConfig` terminate with `preserveBootVolume: false`; Alibaba `SpotAsPriceGo`, `SpotDuration: 0`,
  `SpotInterruptionBehavior: Terminate` (whether `SpotDuration: 0` keeps the 5-minute notice: unverified).
- **Vendors without spot**: Vultr refuses a spot request; hcloud, OVH and UpCloud ignore the capacity type and would
  launch on-demand under a spot NodeClaim if Keeper ever priced spot for them (ADR-017: never degrade silently).
  Step 9 makes them return `Unsupported` and adds a conformance case.

## 8. Steps (one agent each, each ending green)

1. **Core reaction.** `pkg/controllers/interruption` (section 3), `MarkInterrupted` in `pkg/unavailable`, flags,
   RBAC, Helm values, metrics, Events, condition; fake driver hooks: `MinNotice`/`MaxNotice` per impersonated
   vendor. L2 envtest with a fake clock: annotation → termination timestamp `deadline - margin`, an existing earlier
   timestamp kept, conditions, Events, delete, cache entry and its expiry, on-demand ignored once, unparsable and
   far-future deadlines clamped, restart idempotent. Write the ADR (next free number) and update ADR-011's "not
   planned" line and the overview. No vendor work.
   **Done.** `pkg/controllers/interruption`, `unavailable.MarkInterrupted` (class `Interrupted`, 10 m, `--interruption-unavailable-ttl`),
   flags `--interruption` and `--interruption-margin`, Helm values, the metrics `scarpenter_interruptions_total`,
   `scarpenter_interruption_lead_seconds` and `scarpenter_interruption_reaction_seconds`, Events `SpotInterrupted` and
   `InterruptionIgnored`, the `Interrupted` condition, ADR-027, the ADR-011 and overview updates; the fake declares the notice window of the
   vendor it impersonates. Deviations from the text above: (a) the notice window comes from a small optional driver capability
   `InterruptionWindow` (`NoticeWindow{MinNotice, MaxNotice}`), not from step 4's `NoticeCheck`, which can embed it and also serves
   pollers; (b) the controller does not use `state.Cluster` (it finds the claim by the name in the provider ID and needs no
   cluster state) and needs no new RBAC (core's role already patches and deletes NodeClaims, patches their status and creates Events;
   SCarpenter's patches Nodes); (c) an unparsable or missing deadline is acted on with `now + MinNotice` and no `InterruptionIgnored` Event
   (that Event is for a non-spot node or an unknown reason); (d) the effective (clamped) deadline is written back to the Node and the NodeClaim;
   (e) the controller refuses a Node whose provider ID or name the NodeClaim does not confirm, and a source other than `instance-metadata` or
   `vendor-api` is read as `instance-metadata`; (f) a NodeClaim already being deleted (consolidation) still gets the tightened deadline
   and the condition. Not done here: the how-to "Handle spot interruptions" (it describes what a workload sees, which needs a notice
   path: it comes with the first vendor step), and `scarpenter_interruption_poll_errors_total` (step 2).
2. **Poller and silent reclaim.** `InterruptionPoller` in the contract and the core poller controller (section 4);
   `Instance.GoneReason` and the reasons of section 5; fake: `Interrupt(ref, deadline)` (poller path) and
   `Reclaim(ref)` (vanish). L3 KWOK scenarios (a) to (f) of section 10.
   **Done.** `vendors.InterruptionPoller` and `Notice`, `Instance.GoneReason`, the poller (`interruption.Controller.Poller`, a manager
   runnable: one loop per (vendor, region) with a spot NodeClaim, it writes the Node annotations with source `vendor-api` and the controller
   reacts as to the agent's annotation), `Controller.InstanceGone` called from `CloudProvider.Delete` (reasons `ReclaimedWithoutNotice`,
   `InstanceVanished`, and `SpotInterrupted` from `GoneReason`; source `gc` or `vendor-api`), `scarpenter_interruption_poll_errors_total`,
   the fake's `Interrupt`, `Reclaim`, `ReclaimWithReason` and the `interrupt` and `reclaim` steering keys, a conformance subtest, L2 tests, and the
   L3 scenarios I01 to I06 for (a) to (f). Deviations from the text above: (a) the silent-reclaim record is made in `CloudProvider.Delete` only, not in
   `Get` (core's termination calls `Get` with a provider ID and no NodeClaim to record on); `Delete` runs after the drain, so the reason appears late; only
   the first `Delete` of a NodeClaim reports (core's `InstanceTerminating` condition, stored on the NodeClaim, marks a later one, so a restart does
   not turn SCarpenter's own delete into a loss; provider IDs it deleted are also kept in memory for an hour), and a NodeClaim with a
   `DisruptionReason` (core disrupting on purpose) is skipped; a terminating instance is reported too, with or without a `GoneReason`; (b) `InstanceGone` is wired through `CloudProvider.Options.OnInstanceGone`, and the controller
   is built in `NewComponents` (`Components.Interruption`); (c) the poller adds no flag: it checks the regions every 5 s, backs off from twice the poll
   interval to 5 minutes on errors (any error class, 429 included, drivers give no retry-after); (d) a notice without a deadline is written without one
   and the controller assumes the vendor's shortest notice; (e) the scenarios are named I01 to I06 (S22 may be taken by plan 12) and a finalizer on the
   Pod keeps the drained Pod visible. The silent-reclaim condition is only visible until the NodeClaim is gone; the Event and the count last.
3. **OCI.** Spike S1 (credentials from the coordinator), then the driver's `InterruptionPoller` on what S1 found (plus a
   `scarpenter_check` script when S1 finds an in-VM signal),
   its fake, conformance, the OCI reference; L5 scenario if S1 found a way to trigger a preemption (S1).
   **Done** (unverified until the first real preemption in a real run). Maintainer decisions 2026-11-20: spike S1 is skipped (OCI cannot
   simulate a preemption); OCI preemption is found by polling the OCI API; accepted for now that the poller sees a preemption only once the
   instance is `STOPPING`, `STOPPED` or `TERMINATING`, with no lead time (the two-minute notice exists only as an OCI Events message); the next
   step (not this one) is an in-VM instance metadata check, verified on the first real preemption. So OCI today: the reason is recorded, the
   node is cleaned up fast, and there is no graceful drain window yet. What exists: one paged `ListInstances` of the compartment per region and
   interval (`--oci-interruption-poll`, chart `vendors.oci.interruptionPoll`, 10 s; the SDK's `Instance` has no preemption field and no work
   request or maintenance event is documented for one); a preemptible instance of the cluster in one of those states is a `SpotInterrupted`
   notice without a deadline, except SCarpenter's own terminations (the driver remembers them for an hour in memory; the core poller skips a
   NodeClaim with core's stored `InstanceTerminating` condition, which survives a restart); `NoticeWindow` 5 s to 2 min, so the controller's
   deadline is now + 5 s and, with the 5 s margin, Pods are deleted at once with a 1 s grace; `GoneReason` `SpotInterrupted` for a preemptible
   instance that is terminating or terminated; the fake's `Preempt(id)`; the conformance fixture launches preemptible instances in the fake
   (the live lane is unchanged: on-demand, plus its existing preemptible probe); the how-to "Handle spot interruptions" and the OCI reference.
   Deviation from section 4: no deadline "signal time + 120 s", only the state after the Events message is visible. No L5 scenario (no way to
   trigger a preemption).
4. **Node agent (core).** `InterruptionNotices` capability, `vendors.InterruptionAgent`, rendering for spot
   attempts only, the kubelet shutdown settings for spot, golden and size tests; the qemu driver implements the
   capability against a notice server the L4 harness runs on the host bridge, so L4 checks the real path: agent in
   the slice, kubelet credential, NodeRestriction lets it annotate its own Node and not another, the controller
   deletes the NodeClaim, Pods get SIGTERM, graceful shutdown on ACPI.
   **Done.** `vendors.InterruptionNotices` (`NoticeCheck{Script, Interval, NoticeWindow}`), `vendors.InterruptionAgent(d, restricted)` and
   `BootFragmentsOf(d, restrictMetadata, spot)` (the agent fragment `interruption-agent` on spot launches only), `RenderRequest.Spot` and the kubelet's
   `shutdownGracePeriod: 25s` and `shutdownGracePeriodCriticalPods: 5s` on spot launches, the qemu driver's spot type `qemu.spot` and its check
   (`--qemu-notice-url`), L1 tests that run the script with `sh` against a local HTTP fixture, a golden fragment, the size tests, and the L4
   scenarios Q16 (agent, slice, NodeRestriction both ways, notice to Pod SIGTERM) and Q17 (graceful shutdown), both in the default L4 set (no
   extra download). Deviations from the text above: (a) the agent does not clamp the deadline: the controller already clamps it into the notice
   window and treats a missing or unparsable one as `now + MinNotice` (step 1), and the clamping code cost about 0.4 KiB of user-data; the agent
   only reduces the reason and the deadline to safe characters; (b) the agent adds about 1.2 KiB to the user-data (it shares the metadata
   restriction's part), which the 12 KiB golden worst case cannot hold: spot nodes are held to 12 KiB plus `SpotAgentBytes` (1.5 KiB). The largest case,
   which AWS's p4d and p5 spot types can reach, is 16.1 KiB of the 16 KiB limit (held 256 bytes under it); a launch over the limit is refused for its
   offering alone (`InvalidOffering`, `UserDataTooLarge`), and the NodeClass's trial rendering reports that combination as a Warning Event, not a
   failure; (c) the node name and the API server reach the script through `Environment=` of its unit, written from the template variables, so the
   script is static and testable; (d) a graceful
   shutdown test (Q17) runs `systemctl poweroff` because the lane's VMs have no QEMU monitor for the ACPI power button. Verified in L4: NodeRestriction
   on k3s v1.37.1 allows the kubelet's identity to patch its own Node's annotations and refuses another Node (403); the kubelet holds a delay
   lock of logind on Flatcar 4757.2.3, and a Pod gets SIGTERM within a second of `systemctl poweroff`. `jq` is not needed (the agent does not parse JSON).
5. **AWS** check script, `GoneReason`, spike S2, opt-in L5 `interruption` scenario.
6. **GCP** check script, the 120 s notice at create by default if S3 shows no extra cost (else the 30 s ACPI window and the shutdown backstop), `GoneReason`, opt-in L5 scenario.
   **Done.** Spike S3 has not run, so the step is built for "S3 unknown": Spot VMs are launched without a notice (the 30 s ACPI window,
   the kubelet's graceful shutdown as the backstop), and the 120 s notice is one flag away: `--gcp-preemption-notice` (chart
   `vendors.gcp.preemptionNotice`, default 0, at most 2 minutes) sets `scheduling.preemptionNoticeDuration`, the field of the Compute API's
   `Scheduling` (name and limit unverified), and the check prints `now + N` as the deadline. The maintainer turns it on, or changes the
   default, after S3 shows that it costs nothing extra. The check long-polls `instance/preempted` from the ETag of a first read and reads
   the value again; the interval is 1 s, not 0, so that a failing long poll cannot spin. Deviations: (a) `GoneReason` is `SpotInterrupted` for
   any Spot VM that is stopping, stopped or terminated, not read from the zone operation `compute.instances.preempted` (that would add an
   API method and a permission; a stop by hand gets the same reason, as on OCI); (b) the L5 scenario `interruption` (manual only, in
   `manual_scenarios` of `test/e2e-gcp`) preempts with `gcloud compute instances simulate-maintenance-event` and asserts the annotation,
   the `Interrupted` condition, the Pod's SIGTERM before the deadline, the instance gone and the metric; it does not assert the replacement,
   which the 10-minute unavailability of the one pool type may delay. Size: a GCP spot node's worst case is 13.0 KiB of user-data (limit 256 KB).
7. **Azure** check script (opt-in at boot, VM name filter), opt-in L5 scenario with Simulate Eviction (S4).
8. **Alibaba** check script with the token, `GoneReason`, S5, opt-in L5 scenario if it can be triggered.
9. **Spot catalog and spot-less vendors** (independent, any time): section 7 checks, `Unsupported` for spot on
   hcloud, OVH, UpCloud, conformance case, docs say "no spot" per vendor reference after S6.
   **Done** (UpCloud left out by the maintainer: its GPU-SPOT plans stay not launched). Spot offerings come from Keeper's spot rows
   only, no vendor list: `TestSpotOfferingsComeFromKeepersSpotRows` over the committed snapshot (spot for AWS, GCP, Azure, Alibaba;
   none for Hetzner, OVH, Vultr; OCI in `keeper_oci_test.go`) and a price check. The launch fields of AWS, GCP, Azure, OCI and Alibaba
   match section 7 (read in code and golden tests; the GCP 120 s notice stays step 6). `vendors.NoSpot` (`Unsupported`, code `NoSpot`,
   `PreLaunch`) in hcloud, OVH and Vultr, conformance subtest `no spot` (fixture `NoSpot`), the vendor references. S6 (the vendors' own docs) is not run.

Each step updates in the same change: the controller reference (flags, RBAC, metrics, Events, conditions), the labels
reference (annotations), the driver contract (capabilities, `GoneReason`), node bootstrap (agent, shutdown
settings, size), the vendor reference, and a how-to "Handle spot interruptions" (what a workload sees, how to count
per instance type, `terminationGracePeriodSeconds` against each vendor's notice).

## 9. Spikes and unverified facts

- **S1 OCI**: which API (section 4, a to d) changes during the 120 s window, and when; whether the IMDS shows it (e);
  the Compute API rate limits for `ListInstances`/`ListWorkRequests`; whether a preemption can be triggered (none is
  documented; otherwise a preemptible fleet left running until one is reclaimed); the delay from the event to
  `TERMINATING` (an Oracle blog reports 5 minutes in logs, the docs say 2).
- **S2 AWS**: AWS FIS action `aws:ec2:send-spot-instance-interruptions` sends the notice and terminates after the
  set time (the permissions the lane needs go into the hand-off contract, IaC-neutral); IMDS response format;
  `StateReason.Code` after a reclaim.
- **S3 GCP**: the `scheduling` field for the 120 s notice (not on the Spot page) and whether it is billed;
  `gcloud compute instances simulate-maintenance-event` on a Spot VM triggers a preemption with `preempted=TRUE`;
  the zone operation timing.
- **S4 Azure**: Simulate Eviction (`az vm simulate-eviction`) produces a `Preempt` Scheduled Event with 30 s;
  first-call latency on Flatcar.
- **S5 Alibaba**: the notice length (5 min) and whether `SpotDuration: 0` changes it; any way to simulate;
  `OperationLocks` timing.
- **S6**: Hetzner, Vultr, OVH have no spot in their own docs; UpCloud GPU spot API field.
- **Node (L4)**: NodeRestriction allows a node to patch its own Node's annotations at the pinned Kubernetes (k3s
  enables NodeRestriction: unverified); `jq` is on Flatcar (else parse with `sed` on the vendors' fixed JSON shapes);
  kubelet graceful shutdown works on Flatcar.

## 10. Tests

- **L1**: each check script against recorded responses (no notice, notice, malformed, 5xx), run with `sh` and a
  local HTTP fixture; golden fragments per vendor; the size test.
- **L2** (step 1): as listed there; the cache TTL on the fake clock.
- **L3 KWOK** (step 2; the harness writes the annotation as an agent would): (a) a Node annotated with a deadline
  60 s ahead: the NodeClaim has `Interrupted`/`SpotInterrupted`, the termination timestamp is `deadline - 5 s`,
  Events on both, and the Pod's `deletionTimestamp` is set **within 5 s** of the annotation; (b) with a 30 s
  deadline and a Pod with `terminationGracePeriodSeconds: 20`, the Pod is deleted with grace 20 s or the time left;
  (c) the replacement NodeClaim avoids the interrupted type and zone as spot until the shortened TTL ends, then may
  use it again; (d) the fake poller's notice gives the same record with `source=vendor-api`; (e) a silent reclaim
  ends in `ReclaimedWithoutNotice` and the metric; (f) an on-demand Node annotated: ignored, one Event.
- **L4** (step 4): section 8 step 4.
- **L5** (steps 3, 5 to 8): an opt-in `interruption` scenario per vendor that triggers a real interruption where the
  vendor can, asserting the reason, the lead time left at the delete, the Pod's SIGTERM before the deadline (the Pod
  writes a line on SIGTERM; the lane reads its log) and the replacement. Each run needs the maintainer's approval.

## Not here

Vendor event streams as a notice source; rebalance recommendations; capacity-aware spot choice (placement scores);
UpCloud GPU spot; scheduled maintenance events for on-demand nodes; a faster gone-instance poller for vendors other
than OCI (core's GC is the backstop).

## Dependencies

- **[ADR-025](../../../explanation/DECISIONS.md#adr-025-instance-metadata-reaches-only-the-nodes-own-components-accepted-2026-11-19) (on main)**: steps 4 to 8 use the slice and the
  `InstanceMetadata` fragment. Steps 1 to 3 and 9 do not need them.
- Plans 04, 05, 07, 14, 23, 25 (done). The GPU plan (12) runs in parallel; UpCloud GPU spot waits for both.

## Decisions of the maintainer (2026-11-19)

All six open questions were answered by taking the recommendation. Also decided: OCI preemption is found by polling
the OCI API (section 1).

2026-11-20 (OCI, step 3): spike S1 is skipped; the poller sees a preemption only once the instance is `STOPPING`,
`STOPPED` or `TERMINATING` (no lead time: the two-minute notice exists only as an OCI Events message), accepted for now:
the reason is recorded and the node cleaned up fast, with no graceful drain window yet. The next step is an in-VM
instance metadata check, verified on the first real preemption; decision 1 below applies to it instead of S1.

1. **OCI in-VM signal.** Decided by the maintainer 2026-11-19: if S1 finds a preemption signal in the instance
   metadata, OCI uses the node agent for the lead time and the API poller stays as a backstop for `TERMINATING`
   (section 4).
2. **TTL of an interrupted spot offering.** Decided by the maintainer 2026-11-19: 10 minutes, flag-configurable
   (section 3, step 6).
3. **Graceful node shutdown on spot nodes.** Decided by the maintainer 2026-11-19: on, `shutdownGracePeriod: 25s`
   with 5 s for critical Pods, spot launches only (section 2).
4. **GCP notice.** Decided by the maintainer 2026-11-19: Spot VMs get the 120 s notice by default if S3 confirms the
   field and that it costs nothing extra; otherwise the 30 s ACPI window and the shutdown backstop (step 6).
5. **Rebalance recommendations.** Decided by the maintainer 2026-11-19: ignored in this plan (section 6).
6. **Vendor order.** Decided by the maintainer 2026-11-19: OCI, AWS, GCP, Azure, Alibaba.

## Gates

CONVENTIONS gates 1 to 4 for every step, gate 5 (L4) for step 4; `hack/check-docs.py` clean; the reference docs of
section 8 in the same change; the user-data size test under the 12 KiB template budget with the agent; the
`docs/plans/v1/README.md` status `review`. Report per vendor step: the spike results, the measured lead time left at
the NodeClaim delete and at the Pod's SIGTERM, and any core behaviour that differed from section 3.
