# scarpenter-crd

The CRDs of SCarpenter as chart templates. Helm never upgrades the `crds/`
directory of the `scarpenter` chart; this chart does. The templates are generated
by `make chart-crds`; do not edit them. How to install and adopt existing CRDs:
[docs/how-to-guides/install.md](../../docs/how-to-guides/install.md#upgrade).
