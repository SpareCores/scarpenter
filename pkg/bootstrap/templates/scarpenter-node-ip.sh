#!/bin/bash
# Picks the node's IPv4 address inside a CIDR and hands it to the kubelet as
# --node-ip (docs/reference/node-bootstrap.md, Network templates): the direct
# template on a node that has a private network next to its public interface.
# The kubelet unit reads SCARPENTER_NODE_IP_ARGS from the file this writes. It
# waits for the interface to have the address (DHCP can take a moment after
# network-online) and fails after --timeout seconds, so that the kubelet does not
# start and register with the wrong address. IPv4 only.
set -euo pipefail

cidr="" timeout=120
while [ $# -gt 0 ]; do
  case "$1" in
    --cidr) cidr="${2:-}" ;;
    --timeout) timeout="${2:-}" ;;
    *) echo "scarpenter-node-ip: unknown argument $1" >&2; exit 2 ;;
  esac
  shift 2
done
env_file="${SCARPENTER_NODE_IP_ENV:-/run/scarpenter/node-ip.env}"

if ! [[ "$cidr" =~ ^([0-9]+\.[0-9]+\.[0-9]+\.[0-9]+)/([0-9]+)$ ]]; then
  echo "scarpenter-node-ip: --cidr must be an IPv4 prefix like 10.0.0.0/24, got '$cidr'" >&2
  exit 2
fi
net="${BASH_REMATCH[1]}" bits="${BASH_REMATCH[2]}"
if [ "$bits" -gt 32 ]; then
  echo "scarpenter-node-ip: --cidr has a prefix length over 32: $cidr" >&2
  exit 2
fi

to_int() { # to_int a.b.c.d prints the address as an integer
  local IFS=. a b c d
  read -r a b c d <<<"$1"
  echo $(((10#$a << 24) + (10#$b << 16) + (10#$c << 8) + 10#$d))
}
mask=$(((0xFFFFFFFF << (32 - bits)) & 0xFFFFFFFF))
want=$(($(to_int "$net") & mask))

# pick prints the first global IPv4 address of the node inside the CIDR.
pick() {
  local addr
  while read -r addr; do
    if [ $(($(to_int "$addr") & mask)) -eq "$want" ]; then
      echo "$addr"
      return 0
    fi
  done < <(ip -4 -o addr show scope global | awk '{ sub("/.*", "", $4); print $4 }')
  return 1
}

deadline=$((SECONDS + timeout))
while :; do
  if ip="$(pick)"; then break; fi
  if [ "$SECONDS" -ge "$deadline" ]; then
    echo "scarpenter-node-ip: no IPv4 address inside $cidr after ${timeout}s; the kubelet will not start" >&2
    ip -4 -o addr show >&2 || true
    exit 1
  fi
  sleep 1
done

mkdir -p "$(dirname "$env_file")"
printf 'SCARPENTER_NODE_IP_ARGS=--node-ip=%s\n' "$ip" >"$env_file.tmp"
mv "$env_file.tmp" "$env_file"
echo "scarpenter-node-ip: node IP $ip (inside $cidr)"
