#!/usr/bin/env bash
# Runs the canary of a vendor (docs/how-to-guides/testing-and-releasing.md#the-canary):
#
#   test/canary/run.sh <vendor>
#
# It loads the lane's pins (test/e2e-<vendor>/defaults.sh, the one source of truth: the
# workflow's "Prepare" step reads the same file), runs `go run ./test/canary`, and prints
# the table through redact() of test/e2e-<vendor>/lib.sh: the job log, the artifact and the
# issue of a public repository are public. The vendor's credentials come from the
# environment (the workflow assumes the read-only role); nothing is written to the cloud.
# The exit status is the canary's. A vendor's lib.sh must define redact.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/../.." && pwd)"
vendor="${1:?usage: run.sh <vendor>}"
[[ "${vendor}" =~ ^[a-z][a-z0-9]*$ && -d "${root}/test/e2e-${vendor}" ]] || {
  echo "run.sh: no lane test/e2e-${vendor}/" >&2
  exit 2
}
# shellcheck source=/dev/null
source "${root}/test/e2e-${vendor}/lib.sh" # defines redact, and sources defaults.sh
cd "${root}"
go run ./test/canary -vendor "${vendor}" 2>&1 | redact
exit "${PIPESTATUS[0]}"
