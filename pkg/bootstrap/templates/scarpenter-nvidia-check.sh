#!/bin/bash
# Usage: scarpenter-nvidia-check <driver version> <GPU count> <node name> [fabric]
#
# The node's proof for its driver labels (plan 12, D4): the loaded NVIDIA driver is the
# version the type's labels name, and nvidia-smi lists every GPU of the type. The kubelet
# does not wait for this unit (ADR-031): the node registers with the taint
# scarpenter.sparecores.com/gpu-pending, and on success this script labels the Node
# scarpenter.sparecores.com/gpu-ready=true with the kubelet's credentials, which makes the
# controller remove the taint (a node may label itself but not change its taints). A node
# that fails labels the Node gpu-ready=false, and the controller deletes its NodeClaim.
#
# With "fabric" (NVSwitch systems, plan 12, D9) the check then also waits for Fabric Manager
# to report that it configured the fabric, read from the journal of the current run of its
# unit, so that the device plugin never advertises GPUs that cannot initialize CUDA. Its
# success lines differ by GPU generation ("... all the available NVSwitches to route GPU
# NVLink traffic." on A100, "... all the available GPUs and NVSwitches to route NVLink
# traffic." where it also trains the GPUs' links, as on H100) and share this prefix in every
# pinned version; its failure lines start with "failed to".
#
# The GPUs may need a moment after the modules load, so the check is retried: for 30 s, and
# for 10 more minutes for the fabric (which includes the download of its archive).
set -u
want_version=$1
want_gpus=$2
node=$3
fabric=${4:-}
fm_unit=scarpenter-nvidia-fabricmanager.service

# The lines of the unit's current run: the fabric state, and why it is not ready.
fm_journal() {
  journalctl -o cat --no-pager "_SYSTEMD_INVOCATION_ID=$(systemctl show -p InvocationID --value "$fm_unit")" 2>&1
}

# retry runs a check until it prints nothing or the tries are used up, and fails with
# its last message.
retry() {
  local tries=$1 fail
  shift
  for _ in $(seq "$tries"); do
    fail=$("$@")
    [ -z "$fail" ] && return 0
    sleep 3
  done
  echo "scarpenter-nvidia-check: $fail; the node is labelled gpu-ready=false" >&2
  return 1
}

check_gpus() {
  if ! grep -qF " $want_version " /proc/driver/nvidia/version 2>/dev/null; then
    echo "the loaded NVIDIA driver is not $want_version ($(head -n 1 /proc/driver/nvidia/version 2>&1))"
    return
  fi
  local gpus
  gpus=$(nvidia-smi -L 2>&1 | grep -c '^GPU ')
  if [ "$gpus" -ne "$want_gpus" ]; then
    echo "nvidia-smi lists $gpus GPUs, the instance type has $want_gpus"
  fi
}

check_fabric() {
  if ! fm_journal | grep -qF "Successfully configured all the available "; then
    echo "Fabric Manager has not configured the NVSwitches ($(fm_journal | tail -n 1))"
  fi
}

# The kubelet registers the Node once its certificate is approved, which can take minutes:
# retry until the unit's timeout stops the script.
set_ready() {
  until kubectl --kubeconfig /etc/kubernetes/kubelet.conf --request-timeout=10s label node "$node" \
    "scarpenter.sparecores.com/gpu-ready=$1" --overwrite; do
    sleep 3
  done
}

if ! { retry 10 check_gpus && { [ -z "$fabric" ] || retry 200 check_fabric; }; }; then
  set_ready false
  exit 1
fi
set_ready true
