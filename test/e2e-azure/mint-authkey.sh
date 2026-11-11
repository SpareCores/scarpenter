#!/usr/bin/env bash
# L5 step 2: the auth key of the nodes, minted through the Tailscale API with the
# federated identity (no stored secret): ephemeral (the node disappears from the tailnet when it is gone),
# pre-authorized, reusable (one key serves every node of the run), tagged
# tag:scarpenter-ci-node, valid for an hour. The key and its ID land in files of mode
# 0600 in E2E_OUT; install.sh puts the key into the Secret the NodeClasses read, and
# the teardown revokes it by its ID.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-azure/lib.sh
source "${here}/lib.sh"
# shellcheck source=test/e2e-azure/tailscale-api.sh
source "${here}/tailscale-api.sh"
need TS_OAUTH_CLIENT_ID TS_AUDIENCE E2E_CLUSTER E2E_OUT
mkdir -p "${E2E_OUT}"
ts_mint_key "${E2E_TAG_NODE}" "${E2E_CLUSTER}" "${E2E_OUT}/ts-authkey" "${E2E_OUT}/ts-authkey.id"
echo "minted an auth key for ${E2E_TAG_NODE} (ephemeral, pre-authorized, reusable, 1 h)"
