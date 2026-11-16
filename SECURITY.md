# Security policy

## Reporting a vulnerability

Please do not open a public issue for a security problem. Report it privately
through GitHub: on the repository's **Security** tab, choose **Report a
vulnerability** (a private security advisory). Only the maintainers see it.

Include what you can of:

- the affected component (the controller, a vendor driver, the Helm chart, the
  node bootstrap, a workflow) and the version, image digest or commit;
- how to reproduce it, and what an attacker gains;
- whether it is already public.

We acknowledge a report within a few working days, keep you informed while we
work on a fix, and credit you in the advisory unless you prefer otherwise.

## Supported versions

SCarpenter has no release yet. Until v1, fixes go to `main` and into the next
release; only the latest release is supported.

## Scope

In scope: this repository's code, chart and published artifacts
(`ghcr.io/sparecores/scarpenter`, `ghcr.io/sparecores/scarpenter-dev`,
`ghcr.io/sparecores/charts/scarpenter`) and its GitHub workflows. Problems in a
dependency (Karpenter core, a cloud SDK, Flatcar) belong to that project; tell us
too if SCarpenter's use of it makes the problem exploitable.

## Verifying releases

Release images and the chart are signed with cosign (keyless, from
`.github/workflows/release.yml`) and come with an SBOM and provenance; see
[Releases](docs/how-to-guides/testing.md#releases).
