# SCarpenter

A multi-vendor [Karpenter](https://karpenter.sh) cloud provider. SCarpenter
launches **any instance type of any supported vendor** into **any conformant
Kubernetes cluster**, picking the cheapest matching offering from the
[Spare Cores](https://sparecores.com) catalog.

- **No extra infrastructure.** One Deployment in your cluster. It needs only the
  public Spare Cores Keeper API and statically published artifacts (Flatcar
  images, Flatcar sysexts, SCarpenter's own image and chart).
- **One OS everywhere.** Nodes run [Flatcar Container Linux](https://www.flatcar.org),
  configured with Ignition.
- **Distribution-agnostic join.** Nodes join through standard kubelet TLS
  bootstrap: k3s, kubeadm, RKE2 or any other conformant control plane.
- **Karpenter semantics.** Pending Pods drive provisioning; offerings carry real
  prices; capacity errors fall back to the next offering; disruption, expiry and
  drift come from Karpenter core.

> Status: **pre-release**. No version is published yet; the chart and the images
> come with the first release.

## Cloud providers

All drivers are built into the same controller image; enable the ones you need in
the Helm chart. Each guide takes you from an account to instances joining your
cluster: the Flatcar image, the credentials, the chart values and a first
NodeClass.

| Provider | Status | Set-up guide |
|---|---|---|
| Amazon Web Services (AWS) | Supported | [Set up AWS](docs/how-to-guides/set-up-aws.md) |
| Oracle Cloud Infrastructure (OCI) | Supported | [Set up OCI](docs/how-to-guides/set-up-oci.md) |
| OVHcloud Public Cloud | Preview: first real-cloud smoke run passed | [Set up OVHcloud](docs/how-to-guides/set-up-ovh.md) |
| Google Cloud (GCP) | Preview: not yet run against the real cloud | [Set up Google Cloud](docs/how-to-guides/set-up-gcp.md) |
| Microsoft Azure | Preview: not yet run against the real cloud | [Set up Azure](docs/how-to-guides/set-up-azure.md) |
| Alibaba Cloud | Preview: not yet run against the real cloud | [Set up Alibaba Cloud](docs/how-to-guides/set-up-alicloud.md) |
| Hetzner Cloud | Preview: not yet run against the real cloud | [Set up Hetzner Cloud](docs/how-to-guides/set-up-hcloud.md) |
| UpCloud | Preview: not yet run against the real cloud | [Set up UpCloud](docs/how-to-guides/set-up-upcloud.md) |
| Vultr | Preview: not yet run against the real cloud | [Set up Vultr](docs/how-to-guides/set-up-vultr.md) |

**Supported**: the real-cloud test lane runs on pull requests that touch the
provider, nightly, and before every release. **Preview**: implemented, reviewed
and covered by unit and offline tests, but not yet proven by a full real-cloud
run; expect rough edges and please report them. Each guide ends with what the
provider does not support; the details are in its driver reference under
[docs/reference/](docs/reference/README.md).

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

## License

[MPL-2.0](LICENSE).
