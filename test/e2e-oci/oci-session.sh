#!/usr/bin/env bash
# The credentials step of the OCI lane: the keyless token exchange with GitHub's OIDC token
# (plan 25 sections 4 and 11). No key and no long-lived secret of the account is stored: the job's
# OIDC token, requested with the audience the identity domain's trust expects, is exchanged at the
# identity domain's token endpoint (the confidential application's client ID and secret, a
# urn:oci:token-type:oci-upst request carrying the public key of a key pair made here) for a user
# principal session token. Three consumers, one directory:
#
#   the oci CLI of the scripts   OCI_CLI_AUTH=security_token with the profile CI of <dir>/config
#                                (the session token and the key pair made here)
#   the Go programs              OCI_TOKEN_FILE (the OIDC token itself) with the identity domain's
#   (sweeper, canary)            variables: oci.NewConfigurationProvider exchanges it as the
#                                controller does (test/e2e-oci/ocienv)
#   the controller in k3s        the same token file, as the Secret oci-federated-token, and the
#                                client secret as the Secret oci-client (install.sh): its
#                                workloadIdentityFederation mode exchanges the token itself
#
# GitHub's OIDC token lives minutes and a run outlives it, so a background loop writes a fresh one
# to the file every few minutes, updates the controller's Secret, and exchanges again for the CLI's
# session token now and then.
#
#   oci-session.sh once  <dir>    fetch a token, exchange it, write everything (mode 0600)
#   oci-session.sh loop  <dir>    renew until stopped or E2E_OCI_SESSION_MAX_SECONDS
#   oci-session.sh start <dir>    once, export the variables to GITHUB_ENV, detach a loop (<dir>/loop.pid)
#   oci-session.sh stop  <dir>    end the loop recorded in <dir>/loop.pid and remove <dir> (the key, the
#                                tokens and the client secret)
#
# Environment: ACTIONS_ID_TOKEN_REQUEST_URL and ACTIONS_ID_TOKEN_REQUEST_TOKEN (the job's permission
# id-token: write); OCI_TENANCY, OCI_REGION, OCI_IDENTITY_DOMAIN_URL (https://idcs-<id>.identity.oraclecloud.com),
# OCI_CLIENT_ID, OCI_CLIENT_SECRET (the workflow reads it from the environment's secret), OCI_OIDC_AUDIENCE;
# optional OCI_TOKEN_SECRET=<namespace>/<name> (the loop applies the Secret, key "token", whenever the
# cluster answers). Tokens never reach the log: nothing here prints one; the request token goes in a header
# file (a process substitution), the client secret and the subject token in no command line.
#
# Nothing here has run against a real tenancy (UNVERIFIED, spike S11): the audience, the exact fields of
# the token endpoint's answer, that a profile without a fingerprint works for the CLI's security_token
# authentication, and the lifetime of the session token.
set -euo pipefail
: "${E2E_OCI_TOKEN_INTERVAL:=240}"
: "${E2E_OCI_SESSION_INTERVAL:=1200}"
: "${E2E_OCI_SESSION_MAX_SECONDS:=7200}"
profile=CI

# oidc_token: prints a GitHub OIDC token for the audience of the identity domain's trust.
oidc_token() {
  local url resp
  [[ -n "${ACTIONS_ID_TOKEN_REQUEST_URL:-}" && -n "${ACTIONS_ID_TOKEN_REQUEST_TOKEN:-}" ]] || {
    echo "no OIDC token request URL: the job needs the permission id-token: write" >&2
    return 1
  }
  url="${ACTIONS_ID_TOKEN_REQUEST_URL}&audience=$(jq -rn --arg a "${OCI_OIDC_AUDIENCE}" '$a | @uri')"
  resp="$(curl -fsS --retry 3 -H @<(printf 'Authorization: Bearer %s\n' "${ACTIONS_ID_TOKEN_REQUEST_TOKEN}") "${url}")" || return 1
  jq -er '.value' <<<"${resp}"
}

# write_private <file> <content from stdin>: mode 0600, replaced atomically (a reader never sees half a token).
write_private() {
  local tmp
  tmp="$(umask 077 && mktemp "$1.XXXXXX")"
  cat >"${tmp}"
  mv "${tmp}" "$1"
}

# write_oidc <dir>: a fresh OIDC token into <dir>/oidc-token.
write_oidc() {
  local tok
  tok="$(oidc_token)" || return 1
  [[ -n "${tok}" ]] || return 1
  printf '%s' "${tok}" | write_private "$1/oidc-token"
  # The caller masks it in the job log when it runs in a step.
  [[ "${OCI_SESSION_MASK:-}" != true ]] || echo "::add-mask::${tok}"
}

# exchange <dir>: the OIDC token of the directory for a session token bound to <dir>/session.pem's
# public key, into <dir>/session-token.
exchange() {
  local dir="$1" pub body resp tok basic
  pub="$(openssl rsa -in "${dir}/session.pem" -pubout -outform DER 2>/dev/null | base64 -w0)"
  basic="$(printf '%s:%s' "${OCI_CLIENT_ID}" "${OCI_CLIENT_SECRET}" | base64 -w0)"
  body="$(jq -rn --rawfile t "${dir}/oidc-token" --arg p "${pub}" '[
      "grant_type=" + ("urn:ietf:params:oauth:grant-type:token-exchange" | @uri),
      "requested_token_type=" + ("urn:oci:token-type:oci-upst" | @uri),
      "subject_token_type=jwt",
      "subject_token=" + ($t | rtrimstr("\n") | @uri),
      "public_key=" + ($p | @uri)] | join("&")')"
  # printf, not a here-string: --data-binary sends the bytes as they are, and a here-string's
  # newline would end up in the value of the last field (the public key).
  resp="$(printf '%s' "${body}" | curl -fsS --retry 3 -H @<(printf 'Authorization: Basic %s\n' "${basic}") \
    -H 'Content-Type: application/x-www-form-urlencoded' --data-binary @- \
    "${OCI_IDENTITY_DOMAIN_URL%/}/oauth2/v1/token")" || return 1
  tok="$(jq -er '.token // .access_token' <<<"${resp}")" || return 1
  printf '%s' "${tok}" | write_private "${dir}/session-token"
  [[ "${OCI_SESSION_MASK:-}" != true ]] || echo "::add-mask::${tok}"
}

# write_config <dir>: the CLI's config file with the profile of the session token and its key pair.
write_config() {
  local dir="$1"
  write_private "${dir}/config" <<CONFIG
[${profile}]
tenancy=${OCI_TENANCY}
region=${OCI_REGION}
key_file=${dir}/session.pem
security_token_file=${dir}/session-token
CONFIG
}

# apply_secret <dir>: the Secret <namespace>/<name> of OCI_TOKEN_SECRET holds the OIDC token (key
# token); a cluster that does not answer yet is not an error (the next round).
apply_secret() {
  local sns sname
  [[ -n "${OCI_TOKEN_SECRET:-}" ]] || return 0
  sns="${OCI_TOKEN_SECRET%%/*}"
  sname="${OCI_TOKEN_SECRET##*/}"
  kubectl --request-timeout=20s get namespace "${sns}" >/dev/null 2>&1 || return 0
  kubectl --request-timeout=20s -n "${sns}" create secret generic "${sname}" --from-file=token="$1/oidc-token" \
    --dry-run=client -o yaml | kubectl --request-timeout=20s apply -f - >/dev/null
}

cmd="${1:-}"
dir="${2:-}"
[[ -n "${cmd}" && -n "${dir}" ]] || {
  echo "usage: oci-session.sh once|loop|start|stop <directory>" >&2
  exit 2
}

need() {
  local v
  for v in "$@"; do
    [[ -n "${!v:-}" ]] || {
      echo "the variable ${v} is required" >&2
      exit 2
    }
  done
}

case "${cmd}" in
  once | start)
    need OCI_TENANCY OCI_REGION OCI_IDENTITY_DOMAIN_URL OCI_CLIENT_ID OCI_CLIENT_SECRET OCI_OIDC_AUDIENCE
    [[ "${OCI_IDENTITY_DOMAIN_URL}" == https://* ]] || {
      echo "OCI_IDENTITY_DOMAIN_URL must be an https URL" >&2
      exit 2
    }
    (umask 077 && mkdir -p "${dir}")
    [[ -s "${dir}/session.pem" ]] || (umask 077 && openssl genrsa -out "${dir}/session.pem" 2048 2>/dev/null)
    printf '%s' "${OCI_CLIENT_SECRET}" | write_private "${dir}/client-secret"
    # In a step of the workflow the tokens are masked in the job log (the loop has none).
    OCI_SESSION_MASK=true write_oidc "${dir}"
    OCI_SESSION_MASK=true exchange "${dir}"
    write_config "${dir}"
    if [[ "${cmd}" == start ]]; then
      if [[ -n "${GITHUB_ENV:-}" ]]; then
        {
          echo "OCI_CLI_CONFIG_FILE=${dir}/config"
          echo "OCI_CLI_PROFILE=${profile}"
          echo "OCI_CLI_AUTH=security_token"
          echo "OCI_TOKEN_FILE=${dir}/oidc-token"
          echo "OCI_CLIENT_SECRET_FILE=${dir}/client-secret"
          echo "OCI_SESSION_DIR=${dir}"
        } >>"${GITHUB_ENV}"
      fi
      # setsid: the loop outlives the step that started it; its output goes next to the token.
      setsid nohup "$0" loop "${dir}" >"${dir}/loop.log" 2>&1 </dev/null &
      echo "$!" >"${dir}/loop.pid"
      echo "the OIDC token is renewed every ${E2E_OCI_TOKEN_INTERVAL}s and the CLI's session every ${E2E_OCI_SESSION_INTERVAL}s (process $(cat "${dir}/loop.pid"))"
    fi
    ;;
  loop)
    need OCI_TENANCY OCI_REGION OCI_IDENTITY_DOMAIN_URL OCI_CLIENT_ID OCI_OIDC_AUDIENCE
    OCI_CLIENT_SECRET="$(cat "${dir}/client-secret")"
    end=$((SECONDS + E2E_OCI_SESSION_MAX_SECONDS))
    last_session="${SECONDS}"
    while [[ "${SECONDS}" -lt "${end}" ]]; do
      sleep "${E2E_OCI_TOKEN_INTERVAL}"
      if write_oidc "${dir}"; then
        apply_secret "${dir}" || echo "the Secret was not updated (next round)" >&2
        if [[ $((SECONDS - last_session)) -ge "${E2E_OCI_SESSION_INTERVAL}" ]]; then
          exchange "${dir}" && last_session="${SECONDS}" || echo "the exchange failed (next round)" >&2
        fi
      else
        echo "could not renew the OIDC token (next round)" >&2
      fi
    done
    ;;
  stop)
    if [[ -s "${dir}/loop.pid" ]]; then
      pid="$(cat "${dir}/loop.pid")"
      # Only a process that is this script's loop (a recycled ID must not be killed).
      if [[ "${pid}" =~ ^[0-9]+$ ]] && tr '\0' ' ' <"/proc/${pid}/cmdline" 2>/dev/null | grep -q "oci-session.sh loop"; then
        kill "${pid}" 2>/dev/null || true
      fi
    fi
    rm -rf "${dir}"
    ;;
  *)
    echo "unknown command ${cmd}" >&2
    exit 2
    ;;
esac
