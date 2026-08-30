#!/usr/bin/env bash

# A deploy that changes the Flux control plane restarts the very controllers the
# reconcile depends on, so the reconcile it triggered is cancelled mid-flight and
# the deploy fails over a cluster that converges correctly moments later
# (platform#3478). The reconcile wrapper grants exactly one extra attempt, and
# only on positive evidence that THIS deploy restarted the control plane — a
# genuine failure must still fail on the first attempt.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly script="${root_dir}/scripts/reconcile-flux-workloads.sh"
readonly deploy_action="${root_dir}/.github/actions/deploy-prod/action.yml"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

[[ -x "${script}" ]] ||
  fail 'the reconcile-with-control-plane-restart wrapper must be an executable script'

tmp_dir="$(mktemp -d)"
readonly tmp_dir
cleanup() { rm -rf "${tmp_dir}"; }
trap cleanup EXIT

readonly fake_kubectl="${tmp_dir}/kubectl"
readonly fake_reconcile="${tmp_dir}/reconcile"
readonly call_log="${tmp_dir}/calls"

# Fake kubectl. GEN_BEFORE/GEN_AFTER let a case decide whether the control plane
# appears to have been restarted between the two snapshots.
cat >"${fake_kubectl}" <<'FAKE_KUBECTL'
#!/usr/bin/env bash
set -euo pipefail
args="$*"
case "${args}" in
*"rollout status"*)
  printf 'deployment rollout complete\n'
  exit 0
  ;;
*"get deploy"*)
  snapshot_file="${GEN_STATE}"
  # Count kubectl calls. One snapshot costs two calls (label query + operator),
  # so failing from call 3 onward fails the SECOND snapshot only, leaving the
  # first readable — the asymmetry the emptiness guard exists for.
  calls=0
  [[ -f "${CALL_COUNT}" ]] && calls="$(cat "${CALL_COUNT}")"
  calls=$((calls + 1))
  printf '%s' "${calls}" >"${CALL_COUNT}"
  if [[ "${GEN_READ_FAILS:-0}" == "1" && "${calls}" -ge 3 ]]; then
    printf 'the server could not find the requested resource\n' >&2
    exit 1
  fi
  n=0
  [[ -f "${snapshot_file}" ]] && n="$(cat "${snapshot_file}")"
  if [[ "${args}" == *"flux-operator"* ]]; then
    printf 'flux-operator=1\n'
    exit 0
  fi
  # First snapshot returns GEN_BEFORE, every later one returns GEN_AFTER.
  if [[ "${n}" -eq 0 ]]; then
    printf '1\n' >"${snapshot_file}"
    printf 'source-controller=%s\nkustomize-controller=1\n' "${GEN_BEFORE}"
  else
    printf 'source-controller=%s\nkustomize-controller=1\n' "${GEN_AFTER}"
  fi
  exit 0
  ;;
esac
exit 0
FAKE_KUBECTL
chmod +x "${fake_kubectl}"

# Fake reconcile. Appends to the call log and exits per RECONCILE_EXITS (one
# space-separated status per attempt).
cat >"${fake_reconcile}" <<'FAKE_RECONCILE'
#!/usr/bin/env bash
set -euo pipefail
printf 'reconcile %s\n' "$*" >>"${CALL_LOG}"
attempt="$(grep -c . "${CALL_LOG}")"
status="$(printf '%s\n' ${RECONCILE_EXITS} | sed -n "${attempt}p")"
[[ -n "${status}" ]] || status=0
exit "${status}"
FAKE_RECONCILE
chmod +x "${fake_reconcile}"

run_case() {
  local name="$1" gen_before="$2" gen_after="$3" exits="$4"
  local gen_read_fails="${5:-0}"
  : >"${call_log}"
  rm -f "${tmp_dir}/genstate" "${tmp_dir}/callcount"
  set +e
  GEN_STATE="${tmp_dir}/genstate" \
    GEN_READ_FAILS="${gen_read_fails:-0}" \
    CALL_COUNT="${tmp_dir}/callcount" \
    GEN_BEFORE="${gen_before}" \
    GEN_AFTER="${gen_after}" \
    RECONCILE_EXITS="${exits}" \
    CALL_LOG="${call_log}" \
    KUBECTL="${fake_kubectl}" \
    RECONCILE_BIN="${fake_reconcile}" \
    FLUX_CONTROL_PLANE_ROLLOUT_TIMEOUT=1s \
    "${script}" >"${tmp_dir}/out.${name}" 2>&1
  case_status=$?
  set -e
  case_calls="$(grep -c . "${call_log}" || true)"
}

# 1. Happy path: reconcile succeeds first time, no retry, no restart evidence needed.
run_case happy 1 1 "0"
[[ "${case_status}" -eq 0 ]] ||
  fail "a reconcile that succeeds must exit 0 (got ${case_status})"
[[ "${case_calls}" -eq 1 ]] ||
  fail "a successful reconcile must run exactly once (ran ${case_calls})"

# 2. NEGATIVE CONTROL: reconcile fails and the control plane did NOT restart.
#    This is a genuine failure and must fail immediately, with no second attempt.
run_case genuine 1 1 "1 0"
[[ "${case_status}" -ne 0 ]] ||
  fail 'a genuine reconcile failure must still fail the deploy'
[[ "${case_calls}" -eq 1 ]] ||
  fail "a genuine failure must NOT be retried (ran ${case_calls} times)"

# 3. Self-inflicted: reconcile fails, control-plane generation advanced, retry succeeds.
run_case restart 1 2 "1 0"
[[ "${case_status}" -eq 0 ]] ||
  fail "a reconcile cancelled by this deploy's own control-plane restart must be retried to success (got ${case_status})"
[[ "${case_calls}" -eq 2 ]] ||
  fail "the restart case must reconcile exactly twice (ran ${case_calls})"

# 4. The retry is bounded: a failure that persists across the retry still fails.
run_case persistent 1 2 "1 1"
[[ "${case_status}" -ne 0 ]] ||
  fail 'a failure persisting through the retry must still fail the deploy'
[[ "${case_calls}" -eq 2 ]] ||
  fail "the retry must be granted exactly once (ran ${case_calls})"

# 5. Asymmetric snapshot: the FIRST read succeeds and the second fails, so the
#    two snapshots differ only because one is missing. That is not evidence of a
#    restart, so it must not buy a retry — and, equally, reading generations must
#    never become a NEW way for the deploy to fail before the reconcile even runs.
#    Both-empty is already caught by the equality check; only this shape isolates
#    the emptiness guard.
run_case unreadable 1 2 "1 0" 1
[[ "${case_status}" -ne 0 ]] ||
  fail 'an unreadable second snapshot must not buy a free retry'
[[ "${case_calls}" -eq 1 ]] ||
  fail "an unreadable snapshot must still run the reconcile exactly once (ran ${case_calls})"

# 6. Wiring: the deploy composite must reconcile THROUGH the wrapper, so the
#    tolerance actually reaches the merge-queue deploy rather than only existing.
grep -Fq 'run: ./scripts/reconcile-flux-workloads.sh' "${deploy_action}" ||
  fail 'the deploy composite must trigger reconciliation through the wrapper'
grep -Fq 'run: ./scripts/run-ksail-prod-with-pull-auth.sh workload reconcile' "${deploy_action}" &&
  fail 'the deploy composite must not still call the bare reconcile, or the wrapper is bypassed'

printf 'PASS: %s\n' "${0##*/}"
