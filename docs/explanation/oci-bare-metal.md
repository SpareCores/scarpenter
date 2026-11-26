# OCI bare metal

OCI bare metal shapes boot Flatcar when the image is the SpareCores Flatcar iSCSI image. Flatcar's own
`oraclecloud` image does not boot on them. The OCI driver itself is described in the
[OCI driver reference](../reference/oci-driver.md).

## Table of Contents

- [Status](#status)
- [Why the stock image fails](#why-the-stock-image-fails)
- [What the SpareCores image adds](#what-the-sparecores-image-adds)
- [Getting the image](#getting-the-image)
- [Operations](#operations)
- [Limits](#limits)

## Status

The SpareCores image (platform `oraclecloud-iscsi`, from
[SpareCores/flatcar-images](https://github.com/SpareCores/flatcar-images), amd64) booted on `BM.Standard.E2.64`
through two boots across a guest reboot, with the root on iSCSI, `iscsid` running with a 6000 s replacement
timeout, and a `systemd-networkd` restart under I/O. The same image boots unchanged on VM shapes. The upstream
issue is [flatcar/Flatcar#2516](https://github.com/flatcar/Flatcar/issues/2516).

The driver launches only the shapes on the image's compatibility list (see
[Launch](../reference/oci-driver.md#launch)). Add bare metal shapes only to the list of the SpareCores image: a bare
metal shape on the list of the stock image launches an instance that never registers and is billed until the
registration timeout deletes it.

Otherwise SCarpenter treats a bare metal shape like a VM shape, except for these rules:

- Bare metal shapes are fixed shapes: the launch sends no `shapeConfig`.
- Bare metal has no preemptible capacity: it is launched on demand only, and a spot request for it is `Unsupported`.
- A bare metal node needs about 15 minutes from the launch to Ready, so the shape is launched only while Karpenter's
  registration timeout (`--registration-timeout`, chart value `karpenter.registrationTimeout`) is at least 25
  minutes (the same setting as Vultr bare metal). While it is shorter, the catalog drops the shape (`NotLaunchable`)
  and a create is `Unsupported` (`BareMetalRegistrationTimeout`).

## Why the stock image fails

OCI attaches the boot volume of a bare metal instance over iSCSI. The firmware logs in to the target and loads GRUB
and the kernel from it, but its session ends when the kernel starts. The operating system must log in again, using
the iSCSI Boot Firmware Table (iBFT) the firmware leaves behind.

Flatcar's initramfs starts with a minimal busybox early init (`minimal-init` in
[flatcar/bootengine](https://github.com/flatcar/bootengine)). It looks for the `/usr` partition among the kernel's
block devices before it loads the dracut initrd, which has the `iscsi` module. The early init has no iSCSI login, so
with an iSCSI-only boot volume no block device appears, it waits for the drive and reboots. Kernel arguments such as
`rd.iscsi.firmware=1` do not help, because the dracut initrd that reads them is never reached. On VM shapes the boot
volume is a paravirtualized disk that Linux sees directly.

## What the SpareCores image adds

- An extra initrd on the OEM partition that logs in to the iBFT iSCSI boot volume, and only when an iBFT exists, so
  the image is unchanged on VMs.
- `iscsid` kept running after the root is mounted, with the iBFT initiator name and a replacement timeout of 6000 s,
  so a short loss of the path does not fail the root.
- The boot NIC's address kept, so networking does not drop the iSCSI connection of the root.

## Getting the image

Both ways give the same image, the release
[stable-4757.2.3-r5](https://github.com/SpareCores/flatcar-images/releases/tag/stable-4757.2.3-r5) of
SpareCores/flatcar-images. Then add the shapes to the compatibility list and tag the image as in
[Set up OCI](../how-to-guides/set-up-oci.md).

**A. The public copy.** A copy of the image is available in a public Object Storage bucket (it is a convenience, not a
promise that the object stays). Import it into your compartment:

```
oci compute image import from-object-uri \
  --uri https://objectstorage.eu-frankfurt-1.oraclecloud.com/n/frihwb5ywtth/b/sc-flatcar-images-public/o/flatcar-oraclecloud-iscsi-stable-4757.2.3-r5-amd64.qcow2 \
  --source-image-type QCOW2 --launch-mode PARAVIRTUALIZED \
  --operating-system Flatcar --operating-system-version 4757.2.3 \
  --display-name flatcar-stable-4757.2.3-amd64 --compartment-id <your compartment>

oci compute image-shape-compatibility-entry add --image-id <image OCID> --shape-name BM.Standard.E2.64
```

Run the second command once per bare metal shape.

**B. From the release.** Download the asset `flatcar-oraclecloud-iscsi-stable-4757.2.3-amd64.qcow2` from the
release and its sigstore bundle, verify it with `cosign verify-blob` (keyless: the certificate identity is the
`build.yml` workflow of SpareCores/flatcar-images on `main`, issued by GitHub's OIDC provider), upload it to your own
Object Storage bucket and import it from there with the `import from-object-uri` command above.

## Operations

- **Keep Flatcar's auto-updates off.** The extra initrd matches only the kernel the image ships in USR-A, so an
  updated `/usr` would not boot on bare metal. SCarpenter already masks `update-engine` and `locksmithd`; replace
  nodes to move to a new version, with a new image.
- **Expect 5 to 8 minutes of firmware POST** before GRUB. A launch takes about 7 minutes to reach `RUNNING` and about
  1 more to boot.
- **Never administratively set the boot NIC down** (`ip link set <nic> down`): the node loses its default route, and
  the next firmware boot finds no link.
- A missing SSH key in the instance metadata makes `coreos-metadata-sshkeys@core` fail. This is not specific to bare
  metal: it happens on VMs too.

## Limits

- amd64 only. Arm bare metal shapes (A1, A4) are not supported.
- Only the shapes of the image's compatibility list launch, and only while the registration timeout allows it.
