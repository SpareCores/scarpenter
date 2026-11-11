#!/usr/bin/env bash
# The federated token the controller in k3s reads from AZURE_FEDERATED_TOKEN_FILE, and its
# renewal. No secret is stored anywhere: the job's GitHub OIDC token, requested with the
# audience api://AzureADTokenExchange, is what the controller's WorkloadIdentityCredential
# exchanges for an Azure access token (the federated credential of the CI identity trusts
# the immutable subject of the environment e2e). The OIDC token is short-lived and a
# scenario run outlives it, so a background loop writes a fresh one to the file every few
# minutes and updates the Secret the controller Pod mounts; the credential re-reads the
# file whenever it needs a new access token. The loop also logs the runner's own az session
# in again now and then (its access token lasts about an hour; a federated login cannot
# refresh it).
#
#   azure-token.sh once  <file>    fetch one token, write it (mode 0600, atomically)
#   azure-token.sh loop  <file>    renew until stopped or E2E_AZURE_TOKEN_MAX_SECONDS
#   azure-token.sh start <file>    detach a loop and record its process ID in <file>.pid
#   azure-token.sh stop  <file>    end the loop recorded in <file>.pid
#
# Environment: ACTIONS_ID_TOKEN_REQUEST_URL and ACTIONS_ID_TOKEN_REQUEST_TOKEN (the job's
# permission id-token: write); optional AZURE_TOKEN_SECRET=<namespace>/<name> (the loop
# applies the Secret, key "token", whenever the cluster answers); optional AZURE_RELOGIN=true
# with AZURE_CLIENT_ID, AZURE_TENANT_ID and AZURE_SUBSCRIPTION_ID (the az login renewal; the
# token is on az's command line for that call: the runner is ephemeral and the token is
# short-lived and audience-bound). Tokens never reach the log: nothing here prints one.
set -euo pipefail
: "${E2E_AZURE_AUDIENCE:=api://AzureADTokenExchange}"
: "${E2E_AZURE_TOKEN_INTERVAL:=240}"
: "${E2E_AZURE_RELOGIN_INTERVAL:=1800}"
: "${E2E_AZURE_TOKEN_MAX_SECONDS:=7200}"

# azure_oidc_token: prints a GitHub OIDC token for the audience of the federated credential.
azure_oidc_token() {
  local url resp
  [[ -n "${ACTIONS_ID_TOKEN_REQUEST_URL:-}" && -n "${ACTIONS_ID_TOKEN_REQUEST_TOKEN:-}" ]] || {
    echo "no OIDC token request URL: the job needs the permission id-token: write" >&2
    return 1
  }
  url="${ACTIONS_ID_TOKEN_REQUEST_URL}&audience=$(jq -rn --arg a "${E2E_AZURE_AUDIENCE}" '$a | @uri')"
  # The request token goes in through a process substitution, never on a command line.
  resp="$(curl -fsS --retry 3 -H @<(printf 'Authorization: Bearer %s\n' "${ACTIONS_ID_TOKEN_REQUEST_TOKEN}") "${url}")" || return 1
  jq -er '.value' <<<"${resp}"
}

# azure_write_token <file>: a fresh token into the file, mode 0600, replaced atomically (the
# reader never sees half a token; a Secret volume would be updated from this file).
azure_write_token() {
  local file="$1" tok tmp
  tok="$(azure_oidc_token)" || return 1
  [[ -n "${tok}" ]] || return 1
  mkdir -p "$(dirname "${file}")"
  tmp="$(umask 077 && mktemp "${file}.XXXXXX")"
  printf '%s' "${tok}" >"${tmp}"
  mv "${tmp}" "${file}"
  # The caller masks it in the job log when it runs in a step.
  [[ "${AZURE_TOKEN_MASK:-}" != true ]] || echo "::add-mask::${tok}"
}

# azure_apply_secret <file>: the Secret <namespace>/<name> of AZURE_TOKEN_SECRET holds the
# token (key token); a cluster that does not answer yet is not an error (the next round).
azure_apply_secret() {
  local file="$1" sns sname
  [[ -n "${AZURE_TOKEN_SECRET:-}" ]] || return 0
  sns="${AZURE_TOKEN_SECRET%%/*}"
  sname="${AZURE_TOKEN_SECRET##*/}"
  kubectl --request-timeout=20s get namespace "${sns}" >/dev/null 2>&1 || return 0
  kubectl --request-timeout=20s -n "${sns}" create secret generic "${sname}" --from-file=token="${file}" \
    --dry-run=client -o yaml | kubectl --request-timeout=20s apply -f - >/dev/null
}

# azure_relogin <file>: az login with the token of the file (the federated login).
azure_relogin() {
  [[ "${AZURE_RELOGIN:-}" == true ]] || return 0
  az login --service-principal --username "${AZURE_CLIENT_ID}" --tenant "${AZURE_TENANT_ID}" \
    --federated-token "$(cat "$1")" --allow-no-subscriptions >/dev/null &&
    az account set --subscription "${AZURE_SUBSCRIPTION_ID}"
}

cmd="${1:-}"
file="${2:-}"
[[ -n "${cmd}" && -n "${file}" ]] || {
  echo "usage: azure-token.sh once|loop|start|stop <token file>" >&2
  exit 2
}

case "${cmd}" in
  once)
    AZURE_TOKEN_MASK=true azure_write_token "${file}"
    ;;
  loop)
    end=$((SECONDS + E2E_AZURE_TOKEN_MAX_SECONDS))
    last_login="${SECONDS}"
    while [[ "${SECONDS}" -lt "${end}" ]]; do
      sleep "${E2E_AZURE_TOKEN_INTERVAL}"
      if azure_write_token "${file}"; then
        azure_apply_secret "${file}" || echo "the Secret was not updated (next round)" >&2
        if [[ $((SECONDS - last_login)) -ge "${E2E_AZURE_RELOGIN_INTERVAL}" ]]; then
          azure_relogin "${file}" && last_login="${SECONDS}" || echo "az login again failed (next round)" >&2
        fi
      else
        echo "could not renew the federated token (next round)" >&2
      fi
    done
    ;;
  start)
    AZURE_TOKEN_MASK=true azure_write_token "${file}" || exit 1 # the token is masked in this step's log
    # setsid: the loop outlives the step that started it; its output goes next to the token.
    setsid nohup "$0" loop "${file}" >"${file}.log" 2>&1 </dev/null &
    echo "$!" >"${file}.pid"
    echo "the federated token is renewed every ${E2E_AZURE_TOKEN_INTERVAL}s (process $(cat "${file}.pid"))"
    ;;
  stop)
    if [[ -s "${file}.pid" ]]; then
      pid="$(cat "${file}.pid")"
      # Only a process that is this script's loop (a recycled ID must not be killed).
      if [[ "${pid}" =~ ^[0-9]+$ ]] && tr '\0' ' ' <"/proc/${pid}/cmdline" 2>/dev/null | grep -q "azure-token.sh loop"; then
        kill "${pid}" 2>/dev/null || true
      fi
      rm -f "${file}.pid"
    fi
    ;;
  *)
    echo "unknown command ${cmd}" >&2
    exit 2
    ;;
esac
