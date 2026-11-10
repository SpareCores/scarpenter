# 10 — AWS e2e over Tailscale (L5) and sweeper

Depends on: 09. Read `docs/plans/v1/CONVENTIONS.md` first.
Spec: overview §13.4, §8.3 (tailscale template), §11.

## Goal

A workflow that proves SCarpenter end to end on real AWS with a k3s control
plane on the CI runner and nodes joining over Tailscale, plus a scheduled
sweeper. This plan writes and statically validates them; it does **not** run
them (that costs money and needs credentials; the coordinator triggers the
first run and iterates).

## Prerequisites (provisioned outside this repository, do not change)

GitHub environment `e2e` (deployments from `main` only, no required reviewers;
shared by the cloud providers' lanes); variables `AWS_REGION` (`us-east-2`),
`TS_OAUTH_CLIENT_ID` and `TS_AUDIENCE`; secret `AWS_ROLE_ARN`. The Tailscale side is a
federated identity (workload identity federation) with the issuer GitHub Actions, the
subject `repo:SpareCores@152076439/scarpenter@1447426340:environment:e2e` (GitHub
issues immutable subject claims for this repository), the scope `auth_keys` (write)
and the tag `tag:scarpenter-ci` only (it owns `tag:scarpenter-ci-node` in the
policy, so the identity can also mint the nodes' key, while the runner's
single-tag request still matches the credential's tags); there is no
OAuth client secret. The role carries overview §11.6's policy for
cluster names `scarpenter-ci-*`, an explicit deny for instance types other
than the small test types, a region limit, and the optional VPC-resource
statement of §11.6 (the `create` scenario) plus permission for the workflow
itself to create and delete one tagged security group and subnet (the `use`
scenario). The tailnet ACL knows
`tag:scarpenter-ci` (runner) and `tag:scarpenter-ci-node` (nodes). Never put
real values into the repository; reference the secrets.

## Scope

- **`.github/workflows/e2e-aws.yml`**: `workflow_dispatch` (inputs:
  `scenarios`, and pins for the minimum-version run) and weekly schedule; one job,
  `environment: e2e`, `concurrency` group, `timeout-minutes: 90` (the
  Scenarios step has its own limit, 55 min, so that at least 20 min stay for
  diagnostics and teardown); steps
  of overview §13.4 (tailnet join with the official action and the
  federated identity, k3s server with TLS SAN = tailscale IP and production-like flags,
  flannel `--iface=tailscale0` and kube-proxy DaemonSets, auth-key minting
  through the Tailscale API (the GitHub OIDC token exchanged at
  `/api/v2/oauth/token-exchange`) with `ephemeral`, `preauthorized`, `reusable`,
  tag, 1 h expiry, OIDC role assumption, credentials Secret, image build and
  `k3s ctr images import`, chart install with
  `clusterName=scarpenter-ci-${{ github.run_id }}`).
- **`test/e2e-aws/`**: scripts and manifests (NodeClasses for amd64 and
  arm64 with the `tailscale` template and pinned versions, NodePools with
  `consolidationPolicy: WhenEmpty`, `consolidateAfter: 0s`, `limits`), and a
  scenario runner (bash with `kubectl wait` and explicit deadlines) for:
  1. amd64 on-demand exact type (`aws.t3a.small`): node Ready, Pod Running,
     `kubectl logs` and `exec` work, node labels as overview §5.5 (zone name
     and zone ID consistent), instance tags and IMDS options as §11.4
     (checked with `aws ec2 describe-instances`).
  2. arm64 spot exact type (`aws.t4g.small`, spot).
  3. Scale to zero: delete workloads → instances `terminated` within
     2 minutes of Pod deletion (the roadmap exit criterion); bootstrap tokens
     gone.
  4. Drift: change `vendors.aws.storage.root.volumeSize` → replacement node,
     old instance gone; changing a profile no running node uses drifts nothing.
  4a. Quota discovery (overview §13.4, §11.8): the Standard class limits
     reported by SCarpenter's metrics equal `aws service-quotas
     get-service-quota`; after a launch the running instance's default vCPUs
     appear in `used` within one refresh and its ledger reservation is gone;
     after termination they leave `used` within one refresh (no lingering
     spot-request count); the launched type is in
     `DescribeInstanceTypeOfferings` for its zone. (The counting rules
     themselves were verified by hand on 2026-11-05; do not re-test them
     here.)
  4b. Database + client in one placement group (00a §8.2): an `r`-family
     StatefulSet with the `db-io2` storage profile (small io2 volume, low
     IOPS to keep the cost down) and a client Deployment, both with the
     `colocated` placement profile and one run key; assert one AWS cluster
     placement group named and tagged by SCarpenter, both instances in it and
     in one zone, the io2 volume attached and mounted at the profile's path,
     node labels for the applied profiles; after deleting both workloads the
     group is deleted.
  4c. Described, not named: a Deployment selecting by `vcpus` range and a
     `metric-…-pct` threshold within the allowed test types; assert the
     chosen type satisfies them.
  4d. Bring your own resources: the workflow creates a tagged security group
     and subnet with the AWS CLI, a second NodeClass `use`s them by tag; a
     node launches into them; after the run SCarpenter has not deleted them
     (the workflow deletes them in teardown).
  5. Final check: no instance, security group or ENI tagged with the run's
     cluster remains after teardown.
- **Teardown** (`if: always()`): delete NodePools, wait for
  NodeClaims to go, then force-terminate anything tagged with the run's
  cluster, delete managed security groups, remove the runner from the
  tailnet; dump diagnostics on failure (SCarpenter logs, events, NodeClaims,
  serial console output of failed instances via `get-console-output`).
- **`.github/workflows/e2e-aws-sweeper.yml`**: every 3 hours and
  `workflow_dispatch`; terminate instances and delete security groups tagged
  `scarpenter.sparecores.com/cluster` = `scarpenter-ci-*` older than 2 hours,
  in every enabled region of the account.
- **Docs**: testing how-to (L5 section: what it costs, how to trigger, how
  to read failures).

## Carried over from the plan 07 review

- The tailscale template is proven against a Headscale in the L4 lane (scenario Q09:
  tailnet join, `--node-ip`, node Ready over the tailnet, `kubectl logs/exec`). What
  stays for L5 is Tailscale SaaS with the federation-minted ephemeral, tagged key (the
  optional L4 scenario Q11 takes a key of your own) and the real AWS nodes.
- Reuse the unit-level checks of L4 scenario Q10 on real nodes: the tailscale sysext
  merged, `tailscaled` upheld, the kubelet ordered after and bound to the join unit.
- Expect `CatalogNotReady` retries after a controller restart (core retries; bounded
  by its 5 minute launch timeout). A restart scenario must assert that the replay of
  `Create` ran (the qemu driver's `--qemu-launch-delay` is the model: the instance
  exists, `Create` has not returned), and that no instance is orphaned.
  (Decided in the plan 10 fix round: on AWS the window is narrow and no test-only driver
  flag is added, so the replay assertions are opportunistic: a missed window is noted,
  never a failure.)
- Volumes attached at or after launch: check on a real node that the label's device
  unit has `JobRunningTimeoutSec=240` and that a slow attach (about 100 s) does not
  fail the mount (plan 07 review N1; the lane's qemu disks exist at once and cannot
  show it).

## Carried over from the plan 08 review

Live verification list (everything in plan 08 ran against the in-memory fake only):

1. Flatcar AMI names `Flatcar-<channel>-<version>-hvm` / `-arm64-hvm`, owner
   `075585003325`, stable and lts, exactly one match per region and architecture;
   `ImagesPartial` and the gated region in an account whose scope includes
   `ap-northeast-3`.
2. Spot `RunInstances` under the documented policy, with the tagged spot request;
   every IAM condition: `RunInstances` used resources, `CreateTags` with
   `ec2:CreateAction`, `CreateSecurityGroup` on `vpc/*`, the managed-network
   statements (incl. `CreateRouteTable` on `vpc/*` and `ReplaceRoute`), the KMS
   statements with a customer key.
3. `DeleteInstance` refuses an instance without the cluster tag under broad
   credentials.
4. Web identity without `AWS_REGION` (the home region is the SDK default).
5. Real `RunInstances` details: encrypted root from the unencrypted Flatcar snapshot,
   KMS alias and ARN; ENA Express with and without UDP; IPv6 count; dedicated
   tenancy; cluster, spread and partition groups and the real code of the spread
   limit; IMDS hop limit 1 in practice; `vol*` NVMe serial matching; the
   instance-store by-id pattern and count.
6. `IdempotentParameterMismatch` with changed user-data; the `client-token` filter on
   a terminated instance.
7. Serving CSRs approved for `tailscale` and `direct` nodes; a forged IP denied under
   `direct`; a VPC IP the Node claims denied under `tailscale`.
8. A complete `PriceDelta` from `/storage_prices`; the IPv4 constant against the bill.
9. Quota: `GetServiceQuota` for all 18 codes in a fresh region; `stopping` and
   `shutting-down` counting; unused ODCR counting; a spot request `active` after
   termination; the API volume of the 2-minute full `DescribeInstances` and of
   `ReportImages`.
10. The `Unsupported` / `InvalidParameterCombination` message for a type missing in a
    zone.
11. A region without a default VPC: create, delete, recreate within 24 h; a gateway
    attach failure; a missing or stale default route; a half-finished delete followed
    by a new Ensure.
12. User-data size with an EKS-size CA bundle (16 KiB).
13. Filtered `DescribeInstances` pages that are short or empty.
14. `isolation: run` without a run key: nodes of a NodeClass reach each other privately
    with the `direct` template; a run key isolates.
15. Slow EBS attach versus the volume unit's device timeout (240 s): time from boot to
    the NVMe device of a data volume and of instance store, under load and with large
    volumes.
16. IPv6 in the managed network: the Amazon-provided `/56`, a `/64` per subnet, the
    `::/0` route; a node with `publicIPv6: true` gets a global address and reaches
    the internet over IPv6; whether a security group created by `CreateSecurityGroup`
    in an IPv6 VPC has the default `::/0` egress rule (otherwise add it); an older
    IPv4-only managed VPC upgraded by `Ensure` (including the `VpcIPv6Pending`
    wait); deleting a managed network with IPv6.
17. `publicIPv6` with the default VPC without IPv6: the zones are gated with the
    reason and the region is not ready; with `use` subnets that have an IPv6 block
    the launch works and the serving certificate (IPv6 SAN) is approved.
18. `publicIPv4: false`: refused on the default VPC and the managed network;
    accepted with `use` private subnets (with a NAT gateway: the node joins; without
    egress: it does not, as documented); IPv6-only with `use` subnets and a sysext
    mirror reachable over IPv6, and the `IPv6OnlyNeedsMirror` Event without one.

Sweeper: tags `scarpenter.sparecores.com/cluster` and `resource-kind`; tagged spot
requests; the managed VPC layout (`10.240.0.0/16`, name
`scarpenter-<cluster>-subnets-<region>`, children `-<zone>`, `-igw`, `-rt`); unattached
tagged gateways. Run one region without a default VPC.

## Carried over from the plan 09 review

- Install the chart (`charts/scarpenter`), not manifests, on the L5 cluster with a real
  `webIdentity` role (trust policy with the issuer path, `aud` and `sub`); then once
  with EKS Pod Identity (`vendors.aws.credentials.mode: none`) and once with static
  keys (`mode: secret`). Confirm the "Missing Region" behaviour, and that the EKS pod
  identity webhook does not override the chart's explicit environment when
  `annotateServiceAccount` is on.
- The self-managed OIDC issuer steps of `docs/how-to-guides/aws-credentials.md`
  (`--service-account-issuer`, publishing the discovery documents, the IAM provider and
  its thumbprint remark, trust-policy condition keys with a path, the Pod Identity trust
  policy) are untested: walk them once.
- Run the uninstall procedure of `docs/how-to-guides/install.md#uninstall` (SCarpenter's
  NodePools selected by `nodeClassRef.group`, then the SCNodeClasses, then
  `helm uninstall`) on the live account and confirm that nothing tagged
  `scarpenter.sparecores.com/cluster=<name>` remains.
- Check the cluster DNS on real nodes: the controller discovers the DNS Service itself
  when `cluster.dns` is empty; a node's kubelet gets `clusterDNS` and a Pod resolves a
  cluster Service.
- Install the chart at the minimum Kubernetes version (1.31) once.

## Gates

CONVENTIONS gates 1–4; `actionlint` and `shellcheck` clean on the workflows
and scripts; the scenario runner's non-AWS parts (manifests, waits, label
assertions) run against the L4 lane or kind where possible
(`make test-e2e-aws-dryrun`). Report the expected cost per run.

## Outcome

The first fully green live run was GitHub Actions run 42792527317 on 2026-11-10
(commit 0c4196d): 22 minutes wall-clock (09:39:22Z to 10:01:28Z), every scenario
passing, teardown clean.

| scenario | result | time |
|---|---|---|
| preflight | pass | 8 s |
| amd64 | pass | 144 s |
| arm64 | pass | 5 s |
| db | pass | 138 s |
| described | pass | 2 s |
| byo | pass | 177 s |
| local | skipped (`E2E_LOCAL_STORAGE_TYPE` unset) | n/a |
| negative | pass | 4 s |
| quota | pass | 21 s |
| drift | pass | 392 s |
| scalezero | pass | 61 s |
| uninstall | pass | 68 s |

Fixes found by the earlier live runs: the driver's facts skip regions the
credentials cannot read (8d0ced6, 66b1d91); the db scenario's resource-kind tag
expectation (0c4196d); fail-fast, live progress and the never-launched abort
(bdec5ef, b155a37).
