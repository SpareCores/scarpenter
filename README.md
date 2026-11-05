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

> Status: **design / v1 planning**. No release yet. The first vendor is AWS;
> more vendors follow as drivers in the same binary.

## Documentation

Docs follow [Diátaxis](https://diataxis.fr/); start at [docs/](docs/README.md).
The v1 design is [docs/plans/v1/wip/00-overview.md](docs/plans/v1/wip/00-overview.md).

## License

[MPL-2.0](LICENSE).
