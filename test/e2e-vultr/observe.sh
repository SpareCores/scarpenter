#!/usr/bin/env bash
# What the L5 lane shows while it waits and when a scenario fails: a compact progress
# block every E2E_PROGRESS_INTERVAL seconds during a node wait, and one snapshot at a
# failure. Sourced by lib.sh (redact(), the constants and the defaults are defined there).
#
# The job log of a public repository is public: EVERYTHING printed here goes through
# redact(). Only fields that are chosen by name are printed (never `-o yaml` of an
# object, never a Secret, a ConfigMap's data, a pod's environment or user-data), and
# nothing here runs under `set -x`.
# shellcheck shell=bash
# shellcheck disable=SC2154  # ns, E2E_LAUNCH_TIMEOUT and E2E_PROGRESS_INTERVAL come from lib.sh and defaults.sh

# k <kubectl arguments>: kubectl that cannot hang the wait on a dead API server.
k() { kubectl --request-timeout=20s "$@"; }

# selector_pools <pod label selector>: the NodePools (karpenter.sh/nodepool) the matching
# pods ask for, from their node selector or required node affinity, one per line.
selector_pools() {
  k get pods -l "$1" -o json 2>/dev/null | jq -r '
    [.items[] | (.spec.nodeSelector["karpenter.sh/nodepool"] // empty),
      (.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[]?.matchExpressions[]?
        | select(.key == "karpenter.sh/nodepool") | .values[]?)] | unique | .[]' 2>/dev/null || true
}

# pending_age <pod label selector>: the age in seconds of the oldest Pending pod of the
# selector; empty when there is none.
pending_age() {
  k get pods -l "$1" -o json 2>/dev/null | jq -r '
    [.items[] | select(.status.phase == "Pending") | now - (.metadata.creationTimestamp | fromdateiso8601)]
    | if length == 0 then empty else (max | floor) end' 2>/dev/null || true
}

# claims_launched <pools, one per line> <claim to ignore>: prints "<launched> <total>" for
# the NodeClaims (not being deleted) of the pools; every pool when the list is empty. The
# signal is core v1's Launched condition (True once the cloud provider's Create returned) or
# status.providerID, which Create's result sets; Registered and Initialized come later.
claims_launched() {
  k get nodeclaims -o json 2>/dev/null | jq -r --arg pools "$1" --arg ignore "${2:-}" '
    ($pools | split("\n") | map(select(. != ""))) as $p
    | [.items[] | select(.metadata.deletionTimestamp == null and .metadata.name != $ignore)
       | select(($p | length) == 0 or (.metadata.labels["karpenter.sh/nodepool"] as $n | any($p[]; . == $n)))] as $c
    | "\([$c[] | select((.status.providerID // "") != ""
          or any(.status.conditions[]?; .type == "Launched" and .status == "True"))] | length) \($c | length)"' 2>/dev/null || true
}

# ---- the sections (raw: the callers redact) -----------------------------------

obs_claims() {
  echo "--- NodeClaims: name nodepool type capacity conditions age"
  k get nodeclaims -o json 2>&1 | jq -r '
    (.items // [])[] | [.metadata.name,
      (.metadata.labels["karpenter.sh/nodepool"] // "-"),
      (.metadata.labels["node.kubernetes.io/instance-type"] // "-"),
      (.metadata.labels["karpenter.sh/capacity-type"] // "-"),
      ([.status.conditions[]? | "\(.type)=\(.status)" + (if .reason then "/\(.reason)" else "" end)] | join(",")),
      (((now - (.metadata.creationTimestamp | fromdateiso8601)) | floor | tostring) + "s")] | join("  ")' 2>&1 || true
}

obs_pools() {
  echo "--- NodePools: name Ready resources limits"
  k get nodepools -o json 2>&1 | jq -r '
    (.items // [])[] | ([.status.conditions[]? | select(.type == "Ready") | "Ready=\(.status)/\(.reason // "")"] | join(",")) as $r
    | [.metadata.name, (if $r == "" then "Ready=unknown" else $r end),
       "resources=\((.status.resources // {}) | tostring)", "limits=\((.spec.limits // {}) | tostring)"] | join("  ")' 2>&1 || true
}

# obs_pending <pod label selector> <events json file>
obs_pending() {
  echo "--- pending pods of ${1}: name phase last event"
  local out
  out="$(k get pods -l "$1" -o json 2>&1 | jq -r --slurpfile ev "$2" '
    ($ev[0].items // []) as $e
    | (.items // [])[] | select(.status.phase == "Pending") | . as $p
    | ([$e[] | select(.involvedObject.kind == "Pod" and .involvedObject.name == $p.metadata.name
        and .involvedObject.namespace == $p.metadata.namespace)]
       | sort_by(.lastTimestamp // .eventTime // .metadata.creationTimestamp) | last) as $l
    | "\($p.metadata.namespace)/\($p.metadata.name)  \($p.status.phase)  "
      + (if $l then "\($l.reason): \(($l.message // "")[:300])" else "no events" end)' 2>&1)" || true
  echo "${out:-(none)}"
}

obs_nodeclasses() {
  echo "--- SCNodeClasses: conditions"
  k get scnodeclasses -o json 2>&1 | jq -r '
    (.items // [])[] | .metadata.name as $n
    | ((.status.conditions // [])[] | "\($n)  \(.type)=\(.status)  \(.reason // ""): \((.message // "")[:300])"),
      ((.status.vendors // {}) | to_entries[] | .key as $v | (.value.conditions // [])[]
        | "\($n)  \($v)/\(.type)=\(.status)  \(.reason // ""): \((.message // "")[:300])")' 2>&1 || true
}

# obs_ctrl_log <since seconds> <lines> <all|errors>
obs_ctrl_log() {
  echo "--- controller log, last $2 lines of the last ${1}s ($3)"
  local lines
  lines="$(k -n "${ns}" logs deploy/scarpenter --since="${1}s" --tail=-1 2>&1 | cut -c1-800)" || true
  if [[ "$3" == errors ]]; then
    lines="$(grep -iE '"level" *: *"(warn|warning|error|dpanic|panic|fatal)"|level=(warn|error)|(^|[^[:alnum:]])(warn|warning|error|err)([^[:alnum:]]|$)' <<<"${lines}" || true)"
  fi
  tail -n "$2" <<<"${lines}"
}

# obs_warnings <since seconds> <events json file>
obs_warnings() {
  echo "--- Warning events of the last ${1}s (all namespaces)"
  jq -r --argjson since "$1" '
    [(.items // [])[] | select(.type == "Warning")
      | ((.lastTimestamp // .eventTime // .metadata.creationTimestamp) | sub("\\.[0-9]+Z$"; "Z")) as $t
      | select(now - ($t | fromdateiso8601) <= $since)
      | {t: $t, s: "\($t)  \(.reason)  \(.involvedObject.kind)/\(.involvedObject.namespace // "-")/\(.involvedObject.name): \((.message // "")[:300])"}]
    | sort_by(.t) | .[-20:][] | .s' "$2" 2>&1 || true
}

# progress_block <pod label selector> <seconds since the previous block>
progress_block() {
  local selector="$1" since="$2" tmp
  tmp="$(mktemp -d)"
  k get events -A -o json >"${tmp}/events.json" 2>/dev/null || true
  echo "::group::L5 progress at $(date -u +%H:%M:%SZ) (${selector})"
  {
    obs_claims
    obs_pools
    obs_pending "${selector}" "${tmp}/events.json"
    obs_nodeclasses
    obs_ctrl_log "${since}" 20 errors
    obs_warnings "${since}" "${tmp}/events.json"
  } 2>&1 | redact
  echo "::endgroup::"
  rm -rf "${tmp}"
}

# describe_filtered: stdin to stdout without the sections that can hold what must not
# be printed: the container environments and the last-applied configuration.
describe_filtered() {
  awk '
    /^    Environment( Variables from)?:/ { print "    Environment: <omitted>"; skip = 1; next }
    skip && /^      / { next }
    { skip = 0 }
    /last-applied-configuration/ { next }
    { print }'
}

# failure_snapshot <scenario> <pod label selector>: once, when a scenario failed.
failure_snapshot() {
  local name="$1" selector="$2" tmp pools pod
  local -a claim_sel=()
  tmp="$(mktemp -d)"
  k get events -A -o json >"${tmp}/events.json" 2>/dev/null || true
  pools="$(selector_pools "${selector}" | paste -sd, -)"
  [[ -z "${pools}" ]] || claim_sel=(-l "karpenter.sh/nodepool in (${pools})")
  echo "::group::L5 failure snapshot of ${name} at $(date -u +%H:%M:%SZ) (${selector})"
  {
    obs_claims
    obs_pools
    obs_pending "${selector}" "${tmp}/events.json"
    obs_nodeclasses
    obs_warnings 900 "${tmp}/events.json"
    echo "--- kubectl describe nodeclaims ${pools:+of the NodePools ${pools}}"
    k describe nodeclaims "${claim_sel[@]}" 2>&1 | describe_filtered || true
    for pod in $(k get pods -l "${selector}" -o json 2>/dev/null |
      jq -r '(.items // [])[] | select(.status.phase == "Pending") | "\(.metadata.namespace)/\(.metadata.name)"' 2>/dev/null); do
      echo "--- kubectl describe pod ${pod}"
      k -n "${pod%%/*}" describe pod "${pod#*/}" 2>&1 | describe_filtered || true
    done
    obs_ctrl_log 86400 150 all
  } 2>&1 | redact
  echo "::endgroup::"
  rm -rf "${tmp}"
}

# wait_node <what> <timeout seconds> <pod label selector> <NodeClaim to ignore or ""> <command...>
#
# wait_until for the wait of a node: polls the command every 3 s and prints a progress
# block every E2E_PROGRESS_INTERVAL seconds. Two distinct failures:
#   - never launched: E2E_LAUNCH_TIMEOUT s after the start of the wait (or the age of the oldest
#     pending pod of the selector, when that is older) no NodeClaim of the pods' NodePools is
#     launched (core's Launched condition or a providerID): SCarpenter never created an instance;
#   - launched, but the command did not succeed within the timeout (the boot or the join).
# The claim to ignore is one that existed before (the drifted node a replacement is awaited for).
wait_node() {
  local what="$1" timeout="$2" selector="$3" ignore="$4" start deadline next prev last pools out age elapsed launched=0
  shift 4
  start="${SECONDS}"
  deadline=$((start + timeout))
  next=$((start + E2E_PROGRESS_INTERVAL))
  prev="${start}"
  if [[ -n "${E2E_OUT:-}" && -n "${E2E_CURRENT_SCENARIO:-}" ]]; then
    mkdir -p "${E2E_OUT}/state"
    printf '%s' "${selector}" >"${E2E_OUT}/state/${E2E_CURRENT_SCENARIO}.selector"
  fi
  while true; do
    if last="$("$@" 2>&1)"; then
      echo "ok: ${what} (after $((SECONDS - start))s)"
      return 0
    fi
    if [[ "${launched}" -eq 0 ]]; then
      elapsed=$((SECONDS - start))
      age="$(pending_age "${selector}")"
      [[ -z "${age}" || "${age}" -le "${elapsed}" ]] || elapsed="${age}"
      if [[ "${elapsed}" -ge "${E2E_LAUNCH_TIMEOUT}" || "${SECONDS}" -ge "${deadline}" ]]; then
        pools="$(selector_pools "${selector}" | paste -sd, -)"
        out="$(claims_launched "$(tr ',' '\n' <<<"${pools}")" "${ignore}")"
        if [[ -z "${out}" ]]; then
          : # the API server did not answer: no verdict at this poll
        elif [[ "${out%% *}" -gt 0 ]]; then
          launched=1
        else
          echo "last output: ${last}" | redact >&2
          fail "never launched: ${what}: none of the ${out#* } NodeClaim(s) of the NodePool(s) '${pools:-any}' is launched (no Launched=True, no providerID) ${elapsed}s after the wait began; SCarpenter did not create an instance (E2E_LAUNCH_TIMEOUT=${E2E_LAUNCH_TIMEOUT}s)"
        fi
      fi
    fi
    if [[ "${SECONDS}" -ge "${deadline}" ]]; then
      echo "last output: ${last}" | redact >&2
      [[ "${launched}" -eq 1 ]] || fail "timed out after ${timeout}s waiting for ${what}; whether a NodeClaim was launched is unknown (the API server did not answer)"
      fail "launched but not ready: timed out after ${timeout}s waiting for ${what}; the instance was launched, so the boot, the join or the Pod is what failed"
    fi
    if [[ "${SECONDS}" -ge "${next}" ]]; then
      progress_block "${selector}" $((SECONDS - prev))
      prev="${SECONDS}"
      next=$((SECONDS + E2E_PROGRESS_INTERVAL))
    fi
    sleep 3
  done
}
