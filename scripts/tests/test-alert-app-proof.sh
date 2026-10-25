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

# Run the real credential step with synthetic external tools. A child that
# prints a private diagnostic must not leak it on either success or failure.
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
mkdir -p "$scratch/source/scripts" "$scratch/bin" "$scratch/temp"
yq -r '.jobs.proof.steps[-1].run' "$workflow" >"$scratch/run-proof.sh"
cat >"$scratch/source/scripts/use-prod-stable-api-endpoint.sh" <<'SH'
#!/usr/bin/env bash
[[ -f "$KUBECONFIG" && -n "$HCLOUD_TOKEN" ]] || exit 4
printf '%s\n' "$PROOF_CANARY"
[[ "${PROOF_ENDPOINT_FAIL:-0}" == 0 ]]
SH
cat >"$scratch/bin/kubectl" <<'SH'
#!/usr/bin/env bash
[[ "$*" == '--request-timeout=10s --namespace github-config get secret github-app-credentials --output=jsonpath={.data.credentials}' ]] || exit 5
if [[ "${PROOF_READ_FAIL:-0}" == 1 ]]; then printf '%s\n' "$PROOF_CANARY" >&2; exit 6; fi
printf '%s' '{"fixture":"private"}' | base64
SH
cat >"$scratch/temp/alert-app-proof" <<'SH'
#!/usr/bin/env bash
[[ -z "${KUBE_CONFIG+x}" && -z "${HCLOUD_TOKEN+x}" && -z "${KUBECONFIG+x}" ]] || exit 7
[[ -f "$ALERT_APP_PROOF_CREDENTIAL_FILE" ]] || exit 8
mode=$(stat -c '%a' "$ALERT_APP_PROOF_CREDENTIAL_FILE" 2>/dev/null || stat -f '%Lp' "$ALERT_APP_PROOF_CREDENTIAL_FILE")
[[ "$mode" == 600 ]] || exit 9
printf '%s\n' "$PROOF_CANARY"
printf '%s\n' "$PROOF_CANARY" >&2
[[ "${PROOF_PROBE_FAIL:-0}" == 0 ]]
SH
chmod +x "$scratch/bin/kubectl" "$scratch/temp/alert-app-proof" "$scratch/source/scripts/use-prod-stable-api-endpoint.sh"
check_step() {
  local expected=$1 endpoint_fail=$2 read_fail=$3 probe_fail=$4 kube_config=$5 status=0
  (
    cd "$scratch/source"
    env PATH="$scratch/bin:$PATH" RUNNER_TEMP="$scratch/temp" \
      KUBE_CONFIG="$kube_config" HCLOUD_TOKEN=private-cloud-fixture \
      PROOF_CANARY=private-proof-fixture-canary PROOF_ENDPOINT_FAIL="$endpoint_fail" \
      PROOF_READ_FAIL="$read_fail" PROOF_PROBE_FAIL="$probe_fail" bash "$scratch/run-proof.sh"
  ) >"$scratch/step.log" 2>&1 || status=$?
  [[ "$status" == "$expected" ]] || fail 'credential step returned the wrong outcome'
  if grep -Eq 'private-proof-fixture-canary|private-cloud-fixture|private-kube-fixture' "$scratch/step.log"; then
    fail 'credential step emitted a private child diagnostic'
  fi
  [[ -z "$(find "$scratch/temp" -type d -name 'alert-app-proof.*' -print)" ]] || fail 'private credential directory survived cleanup'
}
check_step 0 0 0 0 private-kube-fixture
check_step 1 1 0 0 private-kube-fixture
check_step 1 0 1 0 private-kube-fixture
check_step 1 0 0 1 private-kube-fixture
check_step 1 0 0 0 ''
printf 'alert App credential boundary: five execution cases passed\n'
