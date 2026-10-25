#!/usr/bin/env bash
# Exercise the actual dispatch guard: a branch, replay or injected confirmation
# must never reach checkout or the credential-bearing step.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
workflow="$root/.github/workflows/verify-alert-publisher-app.yaml"
fail() { printf 'alert App proof guard: %s\n' "$1" >&2; exit 1; }
[[ -f "$workflow" ]] || fail 'reviewed dispatch workflow is missing'
[[ "$(yq -o=json -I=0 '.on | keys' "$workflow")" == '["workflow_dispatch"]' ]] || fail 'only manual dispatch is permitted'
[[ "$(yq -o=json -I=0 '.jobs.proof.permissions' "$workflow")" == '{"contents":"read"}' ]] || fail 'workflow token must be read only'
[[ "$(yq -r '.jobs.proof.steps[0].id' "$workflow")" == guard ]] || fail 'dispatch guard must run before checkout'
guard="$(yq -r '.jobs.proof.steps[0].run' "$workflow")"
run_guard() {
  env GITHUB_EVENT_NAME="$1" GITHUB_REF="$2" GITHUB_REPOSITORY="$3" GITHUB_RUN_ATTEMPT="$4" CONFIRM="$5" bash -c "$guard"
}
run_guard workflow_dispatch refs/heads/main devantler-tech/platform 1 verify-alert-publisher-read-only || fail 'explicit main dispatch was refused'
if run_guard workflow_dispatch refs/heads/feature devantler-tech/platform 1 verify-alert-publisher-read-only; then fail 'feature branch was accepted'; fi
if run_guard pull_request refs/heads/main devantler-tech/platform 1 verify-alert-publisher-read-only; then fail 'pull request was accepted'; fi
if run_guard workflow_dispatch refs/heads/main another/platform 1 verify-alert-publisher-read-only; then fail 'another repository was accepted'; fi
if run_guard workflow_dispatch refs/heads/main devantler-tech/platform 2 verify-alert-publisher-read-only; then fail 'automatic replay was accepted'; fi
if run_guard workflow_dispatch refs/heads/main devantler-tech/platform 1 ''; then fail 'empty confirmation was accepted'; fi
if run_guard workflow_dispatch refs/heads/main devantler-tech/platform 1 'verify-alert-publisher-read-only; exit 0'; then fail 'injected confirmation was accepted'; fi
printf 'alert App proof guard: seven dispatch cases passed\n'
