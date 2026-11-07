# 07 — qemu driver and the L4 lane

Depends on: 06. Read `docs/plans/v1/CONVENTIONS.md` first.
Spec: overview §13.3, §15 Q3, Q4, Q6; 00a §11.

## Goal

Real Flatcar VMs, booted from the official QEMU image with SCarpenter's
rendered Ignition, join a local k3s control plane through the full Karpenter
path. This is the first time the bootstrap runs on a real OS; it settles the
riskiest assumptions without a cloud account.

## Scope

- **`pkg/vendors/qemu`** (build tag `scarpenter_dev`): static catalog
  (overview §13.3) through `pkg/catalog/static`; `ResolveImage` downloads
  `flatcar_production_qemu_image.img` for the pinned version and arch from
  the Flatcar release base, verifies it against the release's digest file,
  caches it in a configurable directory; `CreateInstance` creates a qcow2
  overlay, writes the user-data to a file, starts
  `qemu-system-<arch>` with KVM (`-fw_cfg name=opt/org.flatcar-linux/config`),
  a tap on the lane's bridge, a serial log file; `Delete` kills the process
  and removes files; state survives controller restarts (a state directory
  with one JSON file per instance; `List` reconciles with running PIDs).
  Passes the conformance suite. SCarpenter runs out-of-cluster in this lane
  (the driver needs the host's KVM and bridge).
- **Lane** (`test/qemu/`, `make test-qemu`, run with
  `hack/dev.sh --privileged`): in a private network namespace create a
  bridge with NAT and dnsmasq (DHCP); start a k3s server on the bridge address
  configured like a production k3s (`--flannel-backend=none`,
  `--disable-kube-proxy`, `--disable-network-policy`,
  `--egress-selector-mode=disabled`, bootstrap-token auth, `tokencleaner`);
  install flannel and kube-proxy DaemonSets; serve sysexts from a local HTTP
  mirror (download once, cached across runs); run SCarpenter with
  `--vendors=qemu --api-server-endpoint=https://<bridge-ip>:6443`; apply a
  NodeClass (`direct` template) and NodePool.
- **Storage**: the qemu driver implements `NodeOptions` for one storage
  profile kind (extra qcow2 data disks with a virtio serial), so the L4 lane
  exercises the first-boot volume unit of plan 06 on a real Flatcar.
- **Scenarios**: Pod with `qemu.small` → VM boots → Node Ready; record
  boot-to-Ready time; `kubectl logs` and `kubectl exec` work (serving CSR
  approved by SCarpenter); the bootstrap token Secret is gone after
  registration; a pod with a storage profile gets its data disk formatted and
  mounted at the profile's mount path (and an existing filesystem is never
  wiped after a reboot); delete the Pod → node and VM gone; change `kubernetes.version`
  or the qemu vendor block's root disk size → drift replacement; kill SCarpenter while a VM boots →
  after restart no duplicate VM (idempotency) and the node still joins;
  a NodeClass with an unreachable sysext URL → the node never joins, the
  NodeClaim times out and is replaced by core (asserted only as "no Ready
  node", not timing).
- **§15 Q4**: confirm k3s accepts the bootstrap token and the client CSR flow
  with its default flags plus the ones above, and that `kube-root-ca.crt`
  holds the CA nodes need; record any required k3s flag in the install
  how-to prerequisites.
- **§15 Q6**: try the lane on a GitHub-hosted `ubuntu-latest` runner (KVM via
  the udev rule; bridge inside a privileged container). If it works within
  20 minutes, add a CI job (on PRs touching `pkg/bootstrap`,
  `pkg/vendors/qemu`, `test/qemu`, and nightly). If not, document the local
  run and leave a nightly job for a self-hosted runner disabled with a clear
  comment.
- **Docs**: testing how-to (L4 section, requirements: `/dev/kvm`).

## Carried over from the plan 05 review

- N2: the kubelet client CA. `authentication.x509.clientCAFile` is the CA of
  `kube-root-ca.crt` or `--cluster-ca-file` (the serving CA). On k3s the API
  server's kubelet client certificate is signed by `client-ca`, so `kubectl
  logs/exec` would fail with 401; add `--kubelet-client-ca-file` or document the
  value, and cover it in the L4 check.
- Boot proof of what plan 05 only renders: gzip `data:` merges in
  `ignition.config.merge` (Q3), sysext activation, `Upholds=` starting the
  kubelet and tailscaled, the tailscale unit ordering, and the kubelet reading
  the CA file named in the bootstrap kubeconfig (`certificate-authority`).
- Q4: bootstrap-token auth on k3s and `kube-root-ca.crt` as the CA nodes need.
- `kubectl logs/exec` end to end, which proves the serving certificate approval.
- Run one NodeClass with an auth-key or custom-template Secret/ConfigMap through
  the namespaced Role of `config/rbac` (L3 does not exercise it).

## Carried over from the plan 06 review

- Boot the first-boot volume unit (`pkg/bootstrap/templates/`) on a real
  Flatcar, which plan 06 tested only against stubbed tools: two data disks of
  the same size matched by serial pattern (the `flock` and its `/run` lock file),
  a disk that already carries the volume's label next to a new blank one, raid0
  of local disks across a reboot (whether `--homehost=any` keeps
  `/dev/md/scarpenter-local` stable), and that nothing is created twice on a
  reboot.
- Check the tools the scripts use exist on Flatcar: `blkid -p`,
  `lsblk -dnbp -o PATH,SIZE,TYPE,SERIAL`, `findmnt`, `flock`, `mdadm`,
  `mkfs.xfs`, `mkfs.btrfs`.
- Decide whether the volume mounts are required: they are `WantedBy` (weak)
  `local-fs.target` today, so a failed volume does not stop the boot. Make them
  required, or give the kubelet `RequiresMountsFor=`; align the default 90 s
  device timeout of the mount with the script's 180 s wait; fix
  `docs/reference/node-settings.md` accordingly.
- The NodeClass controller's trial rendering omits the volume unit (about 2.5 KiB
  plus 0.3 KiB per volume): render the worst-case storage profile in
  `VendorReady` so that the 16 KiB budget is checked before any launch.
- Give the qemu driver `NodeOptions` and `ManagedResources` (bridge networks) with
  its own vendor ID, a data-disk profile and local-disk hints.

## Gates

CONVENTIONS gates 1–5. Report boot-to-Ready times, Q3/Q4/Q6 findings, and
the serial log excerpt of one successful boot (no secrets: the token is gone
by then, and the lane's token is local).
