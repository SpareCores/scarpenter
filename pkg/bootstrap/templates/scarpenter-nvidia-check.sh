#!/bin/bash
# Usage: scarpenter-nvidia-check <driver version> <GPU count>
#
# The node's proof for its driver labels (plan 12, D4): the loaded NVIDIA driver is the
# version the type's labels name, and nvidia-smi lists every GPU of the type. The kubelet
# requires this unit, so a node that fails it never joins the cluster; Karpenter then
# deletes the NodeClaim after its registration timeout and tries again.
#
# The GPUs may need a moment after the modules load, so the check is retried.
set -u
want_version=$1
want_gpus=$2
fail=""
for _ in 1 2 3 4 5 6 7 8 9 10; do
  fail=""
  if ! grep -qF " $want_version " /proc/driver/nvidia/version 2>/dev/null; then
    fail="the loaded NVIDIA driver is not $want_version ($(head -n 1 /proc/driver/nvidia/version 2>&1))"
  else
    gpus=$(nvidia-smi -L 2>&1 | grep -c '^GPU ')
    if [ "$gpus" -ne "$want_gpus" ]; then
      fail="nvidia-smi lists $gpus GPUs, the instance type has $want_gpus"
    fi
  fi
  if [ -z "$fail" ]; then
    exit 0
  fi
  sleep 3
done
echo "scarpenter-nvidia-check: $fail; the node does not join the cluster" >&2
exit 1
