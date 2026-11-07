#!/usr/bin/env bash
# The Tailscale API calls of the L5 lane, as functions: an OAuth access token, an
# auth key for the nodes and its revocation. The OAuth client (TS_OAUTH_CLIENT_ID,
# TS_OAUTH_SECRET, repository secrets) needs the scope auth_keys (write), for the
# tags tag:scarpenter-ci and tag:scarpenter-ci-node. Source it; secrets stay in
# the environment and in files of mode 0600, never in a command line or the log.

ts_api="${TS_API:-https://api.tailscale.com/api/v2}"

# ts_token: prints an OAuth access token (client credentials grant).
ts_token() {
  local resp
  resp="$(printf 'client_id=%s&client_secret=%s&grant_type=client_credentials' "${TS_OAUTH_CLIENT_ID}" "${TS_OAUTH_SECRET}" |
    curl -fsS --retry 3 --data @- "${ts_api}/oauth/token")" || return 1
  jq -er '.access_token' <<<"${resp}"
}

# ts_mint_key <tag> <description> <key file> <id file>: an ephemeral, pre-authorized,
# reusable key tagged <tag>, valid for an hour (overview section 13.4, step 2).
ts_mint_key() {
  local tag="$1" desc="$2" keyfile="$3" idfile="$4" token resp
  token="$(ts_token)" || {
    echo "could not get an OAuth token from Tailscale" >&2
    return 1
  }
  echo "::add-mask::${token}"
  resp="$(jq -n --arg tag "${tag}" --arg desc "${desc}" '{
      capabilities: {devices: {create: {reusable: true, ephemeral: true, preauthorized: true, tags: [$tag]}}},
      expirySeconds: 3600, description: $desc}' |
    curl -fsS --retry 3 -H "Authorization: Bearer ${token}" -H 'Content-Type: application/json' \
      --data @- "${ts_api}/tailnet/-/keys")" || {
    echo "could not create the auth key" >&2
    return 1
  }
  local key
  key="$(jq -er '.key' <<<"${resp}")" || return 1
  echo "::add-mask::${key}"
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
  echo "::add-mask::${token}"
  curl -fsS --retry 3 -X DELETE -H "Authorization: Bearer ${token}" "${ts_api}/tailnet/-/keys/${id}" >/dev/null
}
