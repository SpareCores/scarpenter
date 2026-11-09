#!/usr/bin/env bash
# The Tailscale API calls of the L5 lane, as functions: an API access token, an
# auth key for the nodes and its revocation. No long-lived secret: the job's GitHub
# OIDC token (requested with the audience TS_AUDIENCE) is exchanged for a short-lived
# access token through workload identity federation. The federated identity
# (TS_OAUTH_CLIENT_ID, TS_AUDIENCE, environment variables, not sensitive) needs the
# scope auth_keys (write; nothing here or in the join action calls a device API) and the
# tag tag:scarpenter-ci only, which owns tag:scarpenter-ci-node in the tailnet policy, so
# it can mint the nodes' key too. Source it; tokens and keys stay in the environment and in
# files of mode 0600, never in a command line or the log. It needs the job's
# permission id-token: write (ACTIONS_ID_TOKEN_REQUEST_URL and _TOKEN).

ts_api="${TS_API:-https://api.tailscale.com/api/v2}"

# ts_mask <value>: registers a GitHub Actions mask. On stderr: the functions that call it
# run inside $(...), where a line on stdout would become part of the captured value
# instead of reaching the runner (which reads workflow commands on both streams).
ts_mask() { [[ -z "${1:-}" ]] || echo "::add-mask::$1" >&2; }

# ts_auth_header <token>: an Authorization header for curl's -H @file, through a
# process substitution, so that no token is ever on a command line (/proc/*/cmdline).
ts_auth_header() { printf 'Authorization: Bearer %s\n' "$1"; }

# ts_id_token: prints a GitHub OIDC token for the audience TS_AUDIENCE.
ts_id_token() {
  local url resp
  [[ -n "${ACTIONS_ID_TOKEN_REQUEST_URL:-}" && -n "${ACTIONS_ID_TOKEN_REQUEST_TOKEN:-}" ]] || {
    echo "no OIDC token request URL: the job needs the permission id-token: write" >&2
    return 1
  }
  url="${ACTIONS_ID_TOKEN_REQUEST_URL}&audience=$(jq -rn --arg a "${TS_AUDIENCE}" '$a | @uri')"
  resp="$(curl -fsS --retry 3 -H @<(ts_auth_header "${ACTIONS_ID_TOKEN_REQUEST_TOKEN}") "${url}")" || return 1
  jq -er '.value' <<<"${resp}"
}

# ts_token: prints a Tailscale API access token: POST ${ts_api}/oauth/token-exchange
# with the form fields client_id and jwt (the OIDC token); the answer is JSON with
# access_token (the exchange of identityfederation.go in tailscale-client-go-v2, the library of
# the Terraform provider; https://tailscale.com/kb/1581/workload-identity-federation).
ts_token() {
  local idtoken resp access
  idtoken="$(ts_id_token)" || return 1
  ts_mask "${idtoken}"
  resp="$(jq -rn --arg c "${TS_OAUTH_CLIENT_ID}" --arg j "${idtoken}" '"client_id=\($c | @uri)&jwt=\($j | @uri)"' |
    curl -fsS --retry 3 --data @- "${ts_api}/oauth/token-exchange")" || return 1
  access="$(jq -er '.access_token' <<<"${resp}")" || return 1
  ts_mask "${access}"
  printf '%s\n' "${access}"
}

# ts_mint_key <tag> <description> <key file> <id file>: an ephemeral, pre-authorized,
# reusable key tagged <tag>, valid for an hour (overview section 13.4, step 2).
ts_mint_key() {
  local tag="$1" desc="$2" keyfile="$3" idfile="$4" token resp
  token="$(ts_token)" || {
    echo "could not exchange the OIDC token for a Tailscale access token" >&2
    return 1
  }
  ts_mask "${token}"
  resp="$(jq -n --arg tag "${tag}" --arg desc "${desc}" '{
      capabilities: {devices: {create: {reusable: true, ephemeral: true, preauthorized: true, tags: [$tag]}}},
      expirySeconds: 3600, description: $desc}' |
    curl -fsS --retry 3 -H @<(ts_auth_header "${token}") -H 'Content-Type: application/json' \
      --data @- "${ts_api}/tailnet/-/keys")" || {
    echo "could not create the auth key" >&2
    return 1
  }
  local key
  key="$(jq -er '.key' <<<"${resp}")" || return 1
  ts_mask "${key}"
  (
    umask 077
    printf '%s' "${key}" >"${keyfile}"
    jq -er '.id' <<<"${resp}" >"${idfile}"
  )
}

# ts_revoke_key <id file>: deletes the key (a no-op for one already gone).
ts_revoke_key() {
  local id token
  [[ -s "$1" ]] || return 0
  id="$(cat "$1")"
  token="$(ts_token)" || return 1
  ts_mask "${token}"
  curl -fsS --retry 3 -X DELETE -H @<(ts_auth_header "${token}") "${ts_api}/tailnet/-/keys/${id}" >/dev/null
}
