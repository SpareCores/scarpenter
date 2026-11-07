# 09 — Packaging: Helm chart, release, install docs

Depends on: 08. Read `docs/plans/v1/CONVENTIONS.md` first.
Spec: overview §12, §8.5 (RBAC), §11 (credentials), F11.

## Goal

SCarpenter installable with one Helm command, released as a signed
multi-arch image and OCI chart, with the install and credentials how-tos and
the controller reference.

## Scope

- **Chart** `charts/scarpenter/` (structure ported from
  `sc-karpenter-nodepool`'s chart and `chart-sync.sh`): CRDs, Deployment (one
  replica by default, leader election for two; non-root, read-only root fs,
  seccomp `RuntimeDefault`; resources sized from the plan 04 scale numbers),
  ServiceAccount (annotations and a projected token volume for AWS web
  identity: `AWS_ROLE_ARN`, `AWS_WEB_IDENTITY_TOKEN_FILE`), RBAC from
  `config/rbac` plus the three join ClusterRoleBindings, PodDisruptionBudget,
  Service + optional ServiceMonitor, values for every flag of overview §12
  (including `--resource-default-ttl`), example NodeClass and NodePools
  (one per enabled vendor) as optional chart resources,
  `vendors.aws.enabled`, credential modes (`webIdentity` | `secret` |
  `none`), Keeper API key Secret reference, extra env, node selector and
  tolerations (SCarpenter must never run on a node it manages: default
  affinity away from `karpenter.sh/nodepool`). `values.schema.json`.
  NOTES.txt checks required values.
- **Chart tests**: `helm lint`, `helm template` golden for default and
  web-identity values, schema negative cases, install into kind (the L3
  lane switches from kustomize to the chart).
- **Release workflow** (port): on tag `v*`: multi-arch image (amd64, arm64)
  to GHCR with the production driver set only (no `scarpenter_dev`), a
  separate `-dev` image with the dev drivers for the lanes, cosign keyless
  signatures, SBOM, OCI chart push, GitHub release notes. A version package
  (`pkg/version`, port) stamped at build.
- **Docs**: `docs/how-to-guides/install.md` (prerequisites from plans 05 and 07:
  bootstrap-token auth, CNI/kube-proxy as DaemonSets reaching remote nodes,
  API endpoint reachable from nodes; values; upgrade; uninstall and what is
  left behind), `docs/how-to-guides/aws-credentials.md` (publishing the
  cluster's ServiceAccount issuer for AWS OIDC federation, the role and the
  IAM policy, static keys as the fallback), `docs/reference/controller.md`
  (flags/env, Helm values, conditions and reasons, Events, metrics,
  permissions).

## Carried over from the plan 05 review

- Chart RBAC from `config/rbac`: no cluster-wide Secrets, the namespaced Roles,
  and the recipe for an auth key Secret in another namespace (Role plus
  RoleBinding there).
- Helm values for `--cluster-dns` (default from the `kube-dns` Service
  ClusterIP), `--cluster-domain`, `--cluster-ca-file`, `--api-server-endpoint`
  and `--bootstrap-token-ttl` (at least 20m).
- The `POD_NAMESPACE` (where `kube-root-ca.crt` is read) and
  `SCARPENTER_DEPLOYMENT_NAME` environment variables of the Deployment.
- Make L3 keep using the chart's RBAC (it applies `config/rbac` today).

## Carried over from the plan 06 review

- Helm values and flags: `--resource-per-use-grace` (2m), `--resource-check-interval`
  (30s), `--resource-default-ttl` (24h), `--resource-full-sweep-every` (10).
- Metrics `scarpenter_resource_operations_total` and
  `scarpenter_resources_managed`; Events `ResourceCreated`, `ResourceAdopted`,
  `ResourceDeleted`, `ResourceCreateFailed`, `ResourceDeleteFailed` and
  `ResourcesUnavailable` for the complete controller reference. No new RBAC and no
  state store were needed.
- Uninstall cleanup of managed resources (00a section 9.3: "or SCarpenter is
  uninstalled with cleanup") needs a design (a hook or a Job).

## Carried over from the plan 07 review

- N10: `--kubelet-client-ca-file` is a file flag, which an in-cluster install on
  k3s cannot satisfy (the controller cannot read `<data-dir>/server/tls/client-ca.crt`
  of the k3s host). Offer a chart value that mounts a user-supplied Secret or
  ConfigMap, and consider an opt-in source `kube-system/extension-apiserver-authentication`
  key `client-ca-file` (the API server's `--client-ca-file`: the client CA on k3s, the
  cluster CA on kubeadm and EKS; needs a RoleBinding to the built-in
  `extension-apiserver-authentication-reader`). Document that kubeadm and EKS need
  no value, and that on k3s without it `kubectl logs` and `exec` fail with 401.
- The install how-to links the k3s prerequisites of `docs/reference/node-bootstrap.md`
  (production-like flags, the kubelet client CA) and the chart's controller reference
  lists `--kubelet-client-ca-file`.
- If a self-hosted runner is adopted for the L4 lane (overview section 15, Q6): only for
  `schedule` and `workflow_dispatch`, never for pull requests (a fork's pull request
  would run arbitrary code on it), and "require approval for fork workflows" in the
  repository settings.

## Gates

CONVENTIONS gates 1–4, plus `hack/dev.sh make test-chart`; the release
workflow passes `actionlint` and a dry run of its build steps without
pushing (`workflow_dispatch` input `dryRun: true`, run locally with the same
script).
