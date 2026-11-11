#!/usr/bin/env bash
# The workflows' credentials step of the Alibaba Cloud lane: writes a GitHub OIDC token to a
# file and exports the three variables the credentials library of Alibaba Cloud (and ali_login
# of lib.sh) reads to assume a RAM role through a RAM OIDC identity provider:
#
#   ALIBABA_CLOUD_ROLE_ARN, ALIBABA_CLOUD_OIDC_PROVIDER_ARN, ALIBABA_CLOUD_OIDC_TOKEN_FILE
#
# No third-party action: the token comes from the job's own token endpoint
# (ACTIONS_ID_TOKEN_REQUEST_URL and _TOKEN, which the job gets with `permissions: id-token:
# write`). The token lives minutes, so the lane calls this again before the teardown, and the
# step that needs the role runs right after it. Called with the role and provider ARNs in the
# environment (the workflows read them from the environment's secrets: they are never in the
# repository) and the audience of the RAM OIDC provider:
#
#   ALICLOUD_ROLE_ARN, ALICLOUD_OIDC_PROVIDER_ARN, ALICLOUD_OIDC_AUDIENCE, ALICLOUD_SESSION_NAME
#
# Nothing here has run against a real account (unverified: the audience the provider expects,
# and that the session name is accepted as given).
set -euo pipefail
for v in ALICLOUD_ROLE_ARN ALICLOUD_OIDC_PROVIDER_ARN ALICLOUD_OIDC_AUDIENCE ACTIONS_ID_TOKEN_REQUEST_URL ACTIONS_ID_TOKEN_REQUEST_TOKEN; do
  [[ -n "${!v:-}" ]] || {
    echo "::error::${v} is not set (the job needs id-token: write, and the environment's secret for the role)" >&2
    exit 1
  }
done
file="${RUNNER_TEMP:?RUNNER_TEMP is required}/alicloud-oidc-token"
# The request token goes in a header file (a process substitution), never in a command line.
url="${ACTIONS_ID_TOKEN_REQUEST_URL}&audience=$(jq -rn --arg a "${ALICLOUD_OIDC_AUDIENCE}" '$a | @uri')"
resp="$(curl -fsS --retry 3 -H @<(printf 'Authorization: Bearer %s\n' "${ACTIONS_ID_TOKEN_REQUEST_TOKEN}") "${url}")"
token="$(jq -er '.value' <<<"${resp}")"
echo "::add-mask::${token}"
(
  umask 077
  printf '%s' "${token}" >"${file}"
)
{
  echo "ALIBABA_CLOUD_ROLE_ARN=${ALICLOUD_ROLE_ARN}"
  echo "ALIBABA_CLOUD_OIDC_PROVIDER_ARN=${ALICLOUD_OIDC_PROVIDER_ARN}"
  echo "ALIBABA_CLOUD_OIDC_TOKEN_FILE=${file}"
  echo "ALIBABA_CLOUD_ROLE_SESSION_NAME=${ALICLOUD_SESSION_NAME:-scarpenter-ci}"
} >>"${GITHUB_ENV:?GITHUB_ENV is required}"
