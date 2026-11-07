#!/bin/bash
# First-boot preparation of the instance-local disks (docs/reference/node-settings.md,
# 00a section 6.1): mount puts an ext4 filesystem labelled scl-<n> on each disk,
# raid0 stripes them into /dev/md/scarpenter-local with the label scl-raid. Like
# scarpenter-volume it creates filesystems only on devices without any signature.
set -euo pipefail

policy="" pattern="" count=0 timeout=180
while [ $# -gt 0 ]; do
  case "$1" in
    --policy) policy="$2" ;;
    --pattern) pattern="$2" ;;
    --count) count="$2" ;;
    --timeout) timeout="$2" ;;
    *) echo "scarpenter-local-storage: unknown argument $1" >&2; exit 2 ;;
  esac
  shift 2
done
if [ "$policy" != mount ] && [ "$policy" != raid0 ]; then
  echo "scarpenter-local-storage: --policy is mount or raid0" >&2
  exit 2
fi
[ -n "$pattern" ] || { echo "scarpenter-local-storage: --pattern is required" >&2; exit 2; }

signature() { blkid -p -o value -s TYPE "$1" 2>/dev/null || true; }
devlabel() { blkid -p -o value -s LABEL "$1" 2>/dev/null || true; }

# disks prints the resolved, de-duplicated local disks, in path order.
disks() {
  local f
  for f in "${SCARPENTER_BY_ID_DIR:-/dev/disk/by-id}"/$pattern; do
    [ -e "$f" ] || continue
    case "$f" in *-part*) continue ;; esac
    readlink -f "$f"
  done | sort -u
}

deadline=$((SECONDS + timeout))
while :; do
  mapfile -t devs < <(disks)
  # Without a count, whatever is there once the first disk shows up.
  if [ "${#devs[@]}" -gt 0 ] && [ "${#devs[@]}" -ge "$count" ]; then break; fi
  if [ "$SECONDS" -ge "$deadline" ]; then
    echo "scarpenter-local-storage: found ${#devs[@]} of $count local disks after ${timeout}s" >&2
    exit 1
  fi
  sleep 2
done

format() { # format <device> <label>
  local dev="$1" label="$2" sig
  if [ "$(devlabel "$dev")" = "$label" ]; then return 0; fi
  sig="$(signature "$dev")"
  if [ -n "$sig" ]; then
    echo "scarpenter-local-storage: $dev has a $sig signature that scarpenter did not create, refusing to overwrite it" >&2
    exit 1
  fi
  mkfs.ext4 -q -F -L "$label" "$dev"
}

if [ "$policy" = mount ]; then
  i=0
  for dev in "${devs[@]}"; do
    format "$dev" "scl-$i"
    i=$((i + 1))
  done
  exit 0
fi

# The array: members must be blank or already members of our array; anything
# else is someone else's data and is never overwritten.
if [ ! -e /dev/md/scarpenter-local ]; then
  existing=0
  fresh=0
  for dev in "${devs[@]}"; do
    sig="$(signature "$dev")"
    if [ -z "$sig" ]; then
      fresh=$((fresh + 1))
    elif [ "$sig" = linux_raid_member ] && mdadm --examine "$dev" 2>/dev/null | grep -Eq '^[[:space:]]*Name[[:space:]]*:.*scarpenter-local'; then
      existing=$((existing + 1))
    else
      echo "scarpenter-local-storage: $dev has a $sig signature that scarpenter did not create, refusing to use it in the array" >&2
      exit 1
    fi
  done
  if [ "$existing" -gt 0 ] && [ "$fresh" -gt 0 ]; then
    echo "scarpenter-local-storage: some local disks belong to the array and some are blank, refusing to guess" >&2
    exit 1
  fi
  if [ "$existing" -gt 0 ]; then
    mdadm --assemble /dev/md/scarpenter-local --name=scarpenter-local --run "${devs[@]}"
  else
    mdadm --create /dev/md/scarpenter-local --name=scarpenter-local --homehost=any --run --level=0 \
      --raid-devices="${#devs[@]}" "${devs[@]}"
  fi
fi
format /dev/md/scarpenter-local scl-raid
