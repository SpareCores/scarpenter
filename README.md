# SCarpenter

A multi-vendor [Karpenter](https://karpenter.sh) cloud provider. SCarpenter
launches **any instance type of any supported vendor** into **any conformant
Kubernetes cluster**, picking the cheapest matching offering from the
[Spare Cores](https://sparecores.com) catalog.

- **No extra infrastructure.** One Deployment in your cluster. It needs the
  Spare Cores Keeper API (the public one takes an API key) and statically
  published artifacts (Flatcar images, Flatcar sysexts, SCarpenter's own image
  and chart).
- **Spare Cores catalog.** Instance types, regions, prices and benchmark scores
  come from Keeper, not from tables in the code. A pending Pod gets the cheapest
  matching offering; node selection, node settings and profiles are described
  in [`SCNodeClass`](docs/reference/api.md). Why a type is or is not offered is
  visible in the class status ([guide](docs/how-to-guides/which-types-does-a-class-offer.md)).
- **One OS everywhere.** Nodes run [Flatcar Container Linux](https://www.flatcar.org),
  configured with Ignition ([node bootstrap](docs/reference/node-bootstrap.md)).
- **Distribution-agnostic join.** Nodes join through standard kubelet TLS
  bootstrap: k3s, kubeadm, RKE2 or any other conformant control plane. Nodes
  reach the cluster over Tailscale or directly
  ([network templates](docs/how-to-guides/choose-a-network-template.md)).
- **Karpenter semantics.** Pending Pods drive provisioning; offerings carry real
  prices; capacity errors fall back to the next offering; disruption, expiry and
  drift come from Karpenter core, v1.15 (Kubernetes 1.31 or newer). SCarpenter
  builds against a [patched fork](docs/explanation/DECISIONS.md#adr-033-a-patched-karpenter-core-with-a-configurable-registration-timeout-accepted-2026-11-24)
  of Karpenter core that adds a configurable registration timeout.
- **Launch control.** Per-class [launch policy](docs/reference/controller.md#launch-policy)
  (attempts, time budget, capacity back-off) and an
  [initialization timeout](docs/reference/controller.md#initialization-timeout)
  that removes nodes that register but never become Ready.
- **Isolation.** `networking.isolation` (`none`, `nodeclass` or `run`) decides who
  may reach a node privately; see [node settings](docs/reference/node-settings.md).
- **Spot and GPUs.** Spot or preemptible capacity where the vendor has it, with
  [interruption handling](docs/how-to-guides/handle-spot-interruptions.md), and
  [NVIDIA GPU nodes](docs/how-to-guides/run-gpu-workloads.md).
- **Image copies (opt-in).** The controller can copy the Flatcar image into
  regions that have none, only when a launch needs it
  ([guide](docs/how-to-guides/copy-images-on-demand.md)). Off by default; AWS only.

Releases are published on GitHub; the Helm chart is at
`oci://ghcr.io/sparecores/charts/scarpenter`.

## Cloud providers

All drivers are built into the same controller image; enable the ones you need in
the Helm chart. Each guide takes you from an account to instances joining your
cluster: the Flatcar image, the credentials, the chart values and a first
NodeClass.

| Provider | Status | Spot | Set-up guide |
|---|---|---|---|
| Amazon Web Services (AWS) | Supported | Spot | [Set up AWS](docs/how-to-guides/set-up-aws.md) |
| Oracle Cloud Infrastructure (OCI) | Supported | Preemptible | [Set up OCI](docs/how-to-guides/set-up-oci.md) |
| OVHcloud Public Cloud | Preview: first real-cloud smoke run passed | No | [Set up OVHcloud](docs/how-to-guides/set-up-ovh.md) |
| Google Cloud (GCP) | Preview: first real-cloud smoke run passed | Spot VMs | [Set up Google Cloud](docs/how-to-guides/set-up-gcp.md) |
| Microsoft Azure | Preview: first real-cloud smoke run passed | Spot | [Set up Azure](docs/how-to-guides/set-up-azure.md) |
| Alibaba Cloud | Preview: not yet run against the real cloud | Spot | [Set up Alibaba Cloud](docs/how-to-guides/set-up-alicloud.md) |
| Hetzner Cloud | Preview: not yet run against the real cloud | No | [Set up Hetzner Cloud](docs/how-to-guides/set-up-hcloud.md) |
| UpCloud | Preview: not yet run against the real cloud | No | [Set up UpCloud](docs/how-to-guides/set-up-upcloud.md) |
| Vultr | Preview: not yet run against the real cloud | No | [Set up Vultr](docs/how-to-guides/set-up-vultr.md) |

**Supported**: the real-cloud test lane runs on pull requests that touch the
provider, nightly, and before every release. **Preview**: implemented, reviewed
and covered by unit and offline tests, but not yet proven by a full real-cloud
run; expect rough edges and please report them. Each guide ends with what the
provider does not support; the details are in its driver reference under
[docs/reference/](docs/reference/README.md). Interruptions are handled on every vendor
with spot (OCI has no graceful drain window yet). Vultr bare metal (`vbm`) plans are offered only
when `karpenter.registrationTimeout` is raised to 25 minutes or more
([Vultr set-up](docs/how-to-guides/set-up-vultr.md)).

## Getting started

1. [Install SCarpenter](docs/how-to-guides/install.md): the cluster prerequisites
   (bootstrap tokens, an API server address the nodes can reach, a CNI that runs on
   remote nodes), a Keeper API key, and the Helm chart.
2. Set up one or more providers from the table above.
3. [Choose a network template](docs/how-to-guides/choose-a-network-template.md)
   for how nodes reach your cluster (`tailscale` or `direct`).

## Documentation

Docs follow [Diátaxis](https://diataxis.fr/); start at [docs/](docs/README.md).
The v1 design is [docs/plans/v1/wip/00-overview.md](docs/plans/v1/wip/00-overview.md).
To develop and test, see [Test and develop](docs/how-to-guides/testing.md) (every
command runs in the dev container: `hack/dev.sh make lint test`); real-cloud tests
and the release process are in [Testing and releasing](docs/how-to-guides/testing-and-releasing.md).

## License

[MPL-2.0](LICENSE).
