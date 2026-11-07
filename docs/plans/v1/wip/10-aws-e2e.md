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

GitHub environment `aws-e2e` (deployments from `main` only); repository
secrets `AWS_ROLE_ARN`, `TS_OAUTH_CLIENT_ID`, `TS_OAUTH_SECRET`; variable
`AWS_REGION` (`us-east-2`). The role carries overview §11.6's policy for
cluster names `scarpenter-ci-*`, an explicit deny for instance types other
than the small test types, a region limit, and the optional VPC-resource
statement of §11.6 (the `create` scenario) plus permission for the workflow
itself to create and delete one tagged security group and subnet (the `use`
scenario). The tailnet ACL knows
`tag:scarpenter-ci` (runner) and `tag:scarpenter-ci-node` (nodes). Never put
real values into the repository; reference the secrets.

## Scope

- **`.github/workflows/e2e-aws.yml`**: `workflow_dispatch` (inputs:
  `scenarios`, `keep: false`) and weekly schedule; one job,
  `environment: aws-e2e`, `concurrency` group, `timeout-minutes: 60`; steps
  of overview §13.4 (tailnet join with the official action and the OAuth
  client, k3s server with TLS SAN = tailscale IP and production-like flags,
  flannel `--iface=tailscale0` and kube-proxy DaemonSets, auth-key minting
  through the Tailscale API with `ephemeral`, `preauthorized`, `reusable`,
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
- **Teardown** (`if: always()` unless `keep`): delete NodePools, wait for
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
  stays for L5 is Tailscale SaaS with the OAuth-minted ephemeral, tagged key (the
  optional L4 scenario Q11 takes a key of your own) and the real AWS nodes.
- Reuse the unit-level checks of L4 scenario Q10 on real nodes: the tailscale sysext
  merged, `tailscaled` upheld, the kubelet ordered after and bound to the join unit.
- Expect `CatalogNotReady` retries after a controller restart (core retries; bounded
  by its 5 minute launch timeout). A restart scenario must assert that the replay of
  `Create` ran (the qemu driver's `--qemu-launch-delay` is the model: the instance
  exists, `Create` has not returned), and that no instance is orphaned.
- Volumes attached at or after launch: check on a real node that the label's device
  unit has `JobRunningTimeoutSec=240` and that a slow attach (about 100 s) does not
  fail the mount (plan 07 review N1; the lane's qemu disks exist at once and cannot
  show it).

## Gates

CONVENTIONS gates 1–4; `actionlint` and `shellcheck` clean on the workflows
and scripts; the scenario runner's non-AWS parts (manifests, waits, label
assertions) run against the L4 lane or kind where possible
(`make test-e2e-aws-dryrun`). Report the expected cost per run.
