# OCI bare metal

Why Flatcar nodes do not boot on OCI bare metal shapes, what the boot test showed, and what is
missing. Tested on 2026-11-25 with Flatcar stable 4757.2.3 (Linux 6.12.114) on `BM.Standard.E2.64`
in `eu-frankfurt-1`. The OCI driver itself is described in the [OCI driver reference](../reference/oci-driver.md).

## Table of Contents

- [Status](#status)
- [The boot test](#the-boot-test)
- [What the console shows](#what-the-console-shows)
- [Why it fails](#why-it-fails)
- [Upstream support](#upstream-support)
- [Ways forward](#ways-forward)

## Status

Flatcar's `oraclecloud` image does not boot on OCI bare metal. The kernel starts, but it never
finds the boot volume, so the node never registers. VM shapes are not affected.

The driver launches only the shapes on an image's compatibility list (see
[Launch](../reference/oci-driver.md#launch)): a bare metal shape that is not on the list is
`InvalidOffering` (`ImageNotCompatible`) without a launch call. So keep bare metal shapes off the
compatibility lists of the Flatcar images. A bare metal shape on the list launches an instance that is
billed until the registration timeout deletes it, and the provisioner then tries the shape again.

## The boot test

- The image was Flatcar's own `oraclecloud` image, imported as a custom image. Its compatibility list had
  `BM.Standard.E2.64` added and no other bare metal shape.
- A pod that selected `oci.BM.Standard.E2.64` in `eu-frankfurt-1` made SCarpenter launch one node. OCI
  accepted the launch, and the instance reached `Running`.
- The node never registered. After the 25 minute registration timeout the NodeClaim was deleted
  together with its instance, and the provisioner launched a second instance, which failed the same way.
- The serial console history of the second instance was captured while it was running
  (`oci compute console-history capture`, then `get-content`).

## What the console shows

- **Firmware:** the machine boots through iPXE, whose banner lists
  `Features: DNS HTTP HTTPS iSCSI TFTP AoE EFI Menu`. It reaches the boot volume over iSCSI, and
  GRUB shows its `Flatcar default` entry.
- **Kernel:** Linux 6.12.114-flatcar starts with
  `mount.usr=/dev/mapper/usr verity.usr=PARTUUID=… root=LABEL=ROOT console=ttyS0,115200n8 console=tty0 flatcar.first_boot=detected flatcar.oem.id=oraclecloud`.
  There is no `rd.iscsi.*` argument.
- **Hardware detection** ends about 15 s after boot. There is no NVMe device, no `Attached SCSI disk`
  line and no network driver line. The only disk controller found is an AHCI controller whose one port
  reports `SATA link down`.
- **Userspace:** nothing from the initramfs, systemd or Ignition reaches the serial console. With
  `console=tty0` last, `/dev/console` is the screen, and the serial console gets only kernel messages.
- **Reboot:** about 6 minutes after boot (378 s) the kernel logs `reboot: Restarting system`, a clean
  restart. The firmware and GRUB then start again.

## Why it fails

OCI attaches the boot volume of a bare metal instance over iSCSI. The firmware logs in to the target and loads
GRUB and the kernel from it, but its iSCSI session ends when the kernel starts. The operating system must log in
again from its initramfs. It does this from the iSCSI Boot Firmware Table (iBFT), which the firmware leaves behind.

Flatcar already ships the parts for that. Its initramfs is built with dracut's `iscsi` module
([bootengine `update-bootengine`](https://github.com/flatcar/bootengine/blob/main/update-bootengine):
`--add "flatcar-network i18n iscsi systemd-coredump"`). The amd64 kernel of the 4757 branch has `ISCSI_IBFT=y`,
`ISCSI_IBFT_FIND=y` and `ISCSI_TCP=m`, and builds the NIC drivers `BNXT` and `MLX5_CORE` as modules
([commonconfig-6.12](https://github.com/flatcar/scripts/blob/flatcar-4757/sdk_container/src/third_party/coreos-overlay/sys-kernel/coreos-modules/files/commonconfig-6.12),
[amd64_defconfig-6.12](https://github.com/flatcar/scripts/blob/flatcar-4757/sdk_container/src/third_party/coreos-overlay/sys-kernel/coreos-modules/files/amd64_defconfig-6.12)).

dracut's `iscsi` module acts only on `root=iscsi:`, `netroot=` or `rd.iscsi.*` arguments. Flatcar's command line
has none: the GRUB configuration of the `oraclecloud` OEM only sets `oem_id`
([grub.cfg](https://github.com/flatcar/scripts/blob/main/build_library/oem/oraclecloud/grub.cfg)). So the
initramfs never logs in, the root and `/usr` partitions never appear, and the initramfs fails and reboots.

On VM shapes the boot volume is a paravirtualized disk that Linux sees directly, so the same image boots.

The initramfs output went to the screen and was not captured, so this reading is inferred. It has not been
tested with the iSCSI arguments added.

## Upstream support

- **Oracle** does not support custom images on bare metal: "Bringing your own image to a bare metal instance
  is not supported" ([Bring your own image](https://docs.oracle.com/en-us/iaas/Content/Compute/References/bringyourownimage.htm)).
  Oracle's guidance for making an imported image boot on bare metal anyway is an iSCSI initiator in the
  initramfs plus `rd.iscsi.ibft=1 rd.iscsi.firmware=1`. That guidance was published on Oracle's blog, which was
  not reachable at the time of writing; it was read in a copy.
- **Flatcar** documents iSCSI only for data volumes
  ([iSCSI](https://www.flatcar.org/docs/latest/os-config/storage/iscsi/)). Its issue tracker has no issue or
  pull request about an iSCSI root, iBFT or OCI bare metal.
- **Fedora CoreOS** installs to iSCSI disks with `rd.iscsi.firmware=1 ip=ibft`
  ([bare metal](https://docs.fedoraproject.org/en-US/fedora-coreos/bare-metal/)). Its OCI guide covers VM
  shapes only. Red Hat tracked bare metal iSCSI boot of RHCOS on OCI
  ([OCPSTRAT-749](https://issues.redhat.com/browse/OCPSTRAT-749)), and has an open bug where DNS is missing on
  the first boot from an OCI iSCSI boot volume with `ip=dhcp` or `ip=ibft`
  ([OCPBUGS-26580](https://issues.redhat.com/browse/OCPBUGS-26580)). Neither issue was read in full.
- **Oracle's Kubernetes service (OKE)** runs bare metal workers only on its Oracle Linux node images
  ([images and shapes](https://docs.cloud.oracle.com/iaas/Content/ContEng/Reference/contengimagesshapes.htm),
  not read in full).
- **Azure** is not an example to copy. SCarpenter runs Flatcar's own Marketplace image there, and another OS only
  on confidential sizes, where Flatcar has no image (see [Flatcar images](flatcar-images.md)). Azure has no bare
  metal.

No OCI option was found that attaches a bare metal boot volume without iSCSI, and no source says whether the local
NVMe of a DenseIO shape can be the boot disk.

## Ways forward

1. **Add the iBFT arguments to the image:** append `rd.iscsi.firmware=1 rd.iscsi.ibft=1 ip=ibft` in the
   `oraclecloud` OEM GRUB configuration of the imported image, and repeat the boot test on one shape. Put
   `console=ttyS0,115200n8` last for that test, so the initramfs and Ignition messages reach the serial
   console. This is the least work. The risk is the network in the initramfs: `ip=ibft` with Flatcar's
   systemd-networkd initramfs, and the DNS problem Red Hat saw on OCI. The arguments must also be checked
   on a VM shape.
2. **Ask Flatcar upstream:** open an issue with the console capture. This is needed if the initramfs network
   fails with the arguments.
3. **Use a vendor OS on bare metal:** Oracle Linux (OKE images) or Ubuntu. These are known to boot on bare metal,
   but they need a second bootstrap path next to Flatcar and Ignition.
4. **Leave bare metal out:** OCI stays on VM shapes. This is the current state.
