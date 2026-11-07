#!/bin/bash
# First-boot preparation of one extra volume (docs/reference/node-settings.md,
# 00a section 11): wait for the device, find it by its stable by-id path or by
# vendor serial pattern and size, and create the filesystem with the label the
# mount unit mounts by. It creates a filesystem only on a device that has no
# signature at all and never touches one it did not create: a device that
# already carries the label is left alone (a reboot), any other signature is an
# error.
set -euo pipefail

label="" fs="" byid="" serial="" size=0 timeout=180
while [ $# -gt 0 ]; do
  case "$1" in
    --label) label="$2" ;;
    --fs) fs="$2" ;;
    --by-id) byid="$2" ;;
    --serial) serial="$2" ;;
    --size) size="$2" ;;
    --timeout) timeout="$2" ;;
    *) echo "scarpenter-volume: unknown argument $1" >&2; exit 2 ;;
  esac
  shift 2
done
if [ -z "$label" ] || [ -z "$fs" ] || { [ -z "$byid" ] && [ -z "$serial" ]; }; then
  echo "scarpenter-volume: --label, --fs and --by-id or --serial are required" >&2
  exit 2
fi

# signature prints the filesystem or other signature type of a device, empty
# when it has none.
signature() { blkid -p -o value -s TYPE "$1" 2>/dev/null || true; }
devlabel() { blkid -p -o value -s LABEL "$1" 2>/dev/null || true; }

# candidates prints the whole disks that match the serial pattern and size, are
# not mounted, have no partitions and carry no signature of someone else, one
# per line (0 for the disk that already carries this volume's label, 1 for a
# blank one), in path order. A disk labelled for another volume of ours (scv-*,
# scl-*) is not a candidate either, and neither is a foreign filesystem: with
# two volumes of the same size the other one must not be taken.
candidates() {
  local path sz typ ser cl
  lsblk -dnbp -o PATH,SIZE,TYPE,SERIAL | sort | while read -r path sz typ ser; do
    [ "$typ" = disk ] || continue
    # shellcheck disable=SC2254 # the serial is a glob pattern on purpose
    case "$ser" in $serial) ;; *) continue ;; esac
    if [ "$size" -gt 0 ] && [ "$sz" -ne "$size" ]; then continue; fi
    if [ "$(lsblk -nr -o NAME "$path" | wc -l)" -gt 1 ]; then continue; fi
    if findmnt -rn -S "$path" >/dev/null; then continue; fi
    cl="$(devlabel "$path")"
    if [ "$cl" = "$label" ]; then echo "0 $path"; continue; fi
    case "$cl" in scv-*|scl-*) continue ;; esac
    if [ -n "$(signature "$path")" ]; then continue; fi
    echo "1 $path"
  done
}

# The units of several volumes run in parallel: the choice of the device and
# the creation of the filesystem happen under one lock, and the label is read
# again once the lock is held, so that two volumes of the same size never take
# the same disk.
lockfile="${SCARPENTER_VOLUME_LOCK:-/run/scarpenter-volume.lock}"
exec 9>"$lockfile"

deadline=$((SECONDS + timeout))
dev=""
while :; do
  flock 9
  if [ -n "$byid" ]; then
    if [ -e "$byid" ]; then dev="$(readlink -f "$byid")"; fi
  else
    # A disk that already carries this volume's label comes before a blank one.
    dev="$(candidates | sort -s -k1,1 | sed -n 1p | cut -d" " -f2)"
  fi
  [ -z "$dev" ] || break
  flock -u 9
  if [ "$SECONDS" -ge "$deadline" ]; then
    echo "scarpenter-volume: no device for ${byid:-serial $serial size $size} after ${timeout}s" >&2
    exit 1
  fi
  sleep 2
done

current="$(devlabel "$dev")"
if [ "$current" = "$label" ]; then
  echo "scarpenter-volume: $dev already carries the label $label"
  exit 0
fi
sig="$(signature "$dev")"
if [ -n "$sig" ]; then
  echo "scarpenter-volume: $dev has a $sig signature that scarpenter did not create, refusing to overwrite it" >&2
  exit 1
fi
case "$fs" in
  ext4) mkfs.ext4 -q -F -L "$label" "$dev" ;;
  xfs) mkfs.xfs -q -L "$label" "$dev" ;;
  btrfs) mkfs.btrfs -q -L "$label" "$dev" ;;
  *) echo "scarpenter-volume: unsupported filesystem $fs" >&2; exit 2 ;;
esac
echo "scarpenter-volume: created $fs with the label $label on $dev"
