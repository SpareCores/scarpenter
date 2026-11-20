#!/bin/bash
# Usage: scarpenter-nvidia-check <driver version> <GPU count> [fabric]
#
# The node's proof for its driver labels (plan 12, D4): the loaded NVIDIA driver is the
# version the type's labels name, and nvidia-smi lists every GPU of the type. The kubelet
# requires this unit, so a node that fails it never joins the cluster; Karpenter then
# deletes the NodeClaim after its registration timeout and tries again.
#
# With "fabric" (NVSwitch systems, plan 12, D9) the check then also waits for Fabric Manager
# to report that it configured the NVSwitches (its log line, which every driver version of the
# table has, read from the journal of the current run of its unit), so that the device plugin
# never advertises GPUs that cannot initialize CUDA.
#
# The GPUs may need a moment after the modules load, so the check is retried: for 30 s, and
# for 3 more minutes for the fabric.
set -u
want_version=$1
want_gpus=$2
fabric=${3:-}
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
  echo "scarpenter-nvidia-check: $fail; the node does not join the cluster" >&2
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
  if ! fm_journal | grep -qF "Successfully configured all the available NVSwitches"; then
    echo "Fabric Manager has not configured the NVSwitches ($(fm_journal | tail -n 1))"
  fi
}

retry 10 check_gpus || exit 1
if [ -n "$fabric" ]; then
  retry 60 check_fabric || exit 1
fi
