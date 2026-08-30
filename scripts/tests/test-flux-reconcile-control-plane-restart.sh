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
  # Fail EXACTLY one call, so a snapshot comes back PARTIAL rather than empty.
  if [[ -n "${GEN_FAIL_ONLY_CALL:-}" && "${calls}" == "${GEN_FAIL_ONLY_CALL}" ]]; then
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
  local gen_fail_only_call="${6:-}"
  local baseline_file="${7:-}"
  : >"${call_log}"
  rm -f "${tmp_dir}/genstate" "${tmp_dir}/callcount"
  set +e
  GEN_STATE="${tmp_dir}/genstate" \
    GEN_READ_FAILS="${gen_read_fails:-0}" \
    GEN_FAIL_ONLY_CALL="${gen_fail_only_call:-}" \
    CALL_COUNT="${tmp_dir}/callcount" \
    GEN_BEFORE="${gen_before}" \
    GEN_AFTER="${gen_after}" \
    RECONCILE_EXITS="${exits}" \
    CALL_LOG="${call_log}" \
    KUBECTL="${fake_kubectl}" \
    RECONCILE_BIN="${fake_reconcile}" \
    FLUX_CONTROL_PLANE_BASELINE_FILE="${baseline_file}" \
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

# 5b. PARTIAL snapshot: the second snapshot's labelled query fails while the
#     flux-operator query succeeds, so it comes back NON-EMPTY but incomplete.
#     The emptiness guard cannot catch this shape — the partial differs from the
#     complete first snapshot for a reason that is not a restart, so checking
#     each query's own status is the only thing between it and a free retry.
run_case partial 1 2 "1 0" 0 3
[[ "${case_status}" -ne 0 ]] ||
  fail 'a partial second snapshot must not buy a free retry'
[[ "${case_calls}" -eq 1 ]] ||
  fail "a partial snapshot must still run the reconcile exactly once (ran ${case_calls})"

# 5c. Both snapshot queries must carry a FINITE --request-timeout. kubectl's
#     default is 0, i.e. wait forever, and these reads run at precisely the
#     moment this deploy may be restarting the API server's clients — so an
#     unbounded read would hang the deploy rather than fail it. Structural,
#     because the fake kubectl cannot observe a real client-side timeout.
snapshot_queries="$(grep -c -- '--request-timeout="${kubectl_request_timeout}"' "${script}")"
[[ "${snapshot_queries}" -eq 2 ]] ||
  fail "both generation queries must bound their read (found ${snapshot_queries} of 2)"
grep -Eq '^readonly kubectl_request_timeout=.*:-[0-9]+[smh]\}"$' "${script}" ||
  fail 'the snapshot read timeout must default to a finite duration'

# 5d. THE PRE-PUBLISH BASELINE. Flux may observe the newly published revision
#     before this wrapper even starts — the deploy composite says so where it
#     suspends autoscaling "before the mutable artifact is published" — so a
#     control-plane rollout can already have advanced the generation by the time
#     the wrapper takes its own "before" snapshot. Both of the wrapper's
#     snapshots then show the SAME advanced generation, and the cancellation this
#     deploy inflicted on itself is reported as a genuine failure: exactly the
#     outage this wrapper exists to prevent. A baseline captured before the tag
#     moved is what closes that window.
printf 'flux-operator=1\nkustomize-controller=1\nsource-controller=1\n' >"${tmp_dir}/baseline"
run_case baseline 2 2 "1 0" 0 "" "${tmp_dir}/baseline"
[[ "${case_status}" -eq 0 ]] ||
  fail "a rollout that began before the wrapper started must still be recognised from the pre-publish baseline (got ${case_status})"
[[ "${case_calls}" -eq 2 ]] ||
  fail "the pre-publish-baseline case must reconcile exactly twice (ran ${case_calls})"

# 5e. NEGATIVE CONTROL for that path: a baseline MATCHING the post-reconcile
#     snapshot is not restart evidence and must not buy a retry. Without it,
#     "prefer the baseline" could degenerate into "always retry" and still pass.
printf 'flux-operator=1\nkustomize-controller=1\nsource-controller=2\n' >"${tmp_dir}/baseline-same"
run_case baseline_same 2 2 "1 0" 0 "" "${tmp_dir}/baseline-same"
[[ "${case_status}" -ne 0 ]] ||
  fail 'a baseline equal to the post-reconcile snapshot is not restart evidence and must not buy a retry'
[[ "${case_calls}" -eq 1 ]] ||
  fail "the matching-baseline case must not be retried (ran ${case_calls})"

# 5f. An empty or unreadable baseline must fall back to the wrapper's own read
#     rather than becoming a new way for the deploy to fail.
: >"${tmp_dir}/baseline-empty"
run_case baseline_empty 1 2 "1 0" 0 "" "${tmp_dir}/baseline-empty"
[[ "${case_status}" -eq 0 ]] ||
  fail "an empty baseline must fall back to reading the generations in-wrapper (got ${case_status})"

# 5g. `--snapshot-baseline` must print the snapshot and exit WITHOUT reconciling,
#     or the pre-publish step would trigger the very reconcile it precedes.
: >"${call_log}"
rm -f "${tmp_dir}/genstate" "${tmp_dir}/callcount"
set +e
GEN_STATE="${tmp_dir}/genstate" CALL_COUNT="${tmp_dir}/callcount" \
  GEN_BEFORE=1 GEN_AFTER=1 RECONCILE_EXITS="0" CALL_LOG="${call_log}" \
  KUBECTL="${fake_kubectl}" RECONCILE_BIN="${fake_reconcile}" \
  "${script}" --snapshot-baseline >"${tmp_dir}/out.snapshot" 2>&1
snapshot_status=$?
set -e
[[ "${snapshot_status}" -eq 0 ]] ||
  fail "--snapshot-baseline must exit 0 (got ${snapshot_status})"
[[ "$(grep -c . "${call_log}" || true)" -eq 0 ]] ||
  fail '--snapshot-baseline must not run a reconcile'
grep -Fq 'source-controller=1' "${tmp_dir}/out.snapshot" ||
  fail '--snapshot-baseline must print the control-plane generations on stdout'

# 6. Wiring: the deploy composite must reconcile THROUGH the wrapper, so the
#    tolerance actually reaches the merge-queue deploy rather than only existing.
grep -Fq 'run: ./scripts/reconcile-flux-workloads.sh' "${deploy_action}" ||
  fail 'the deploy composite must trigger reconciliation through the wrapper'
grep -Fq 'run: ./scripts/run-ksail-prod-with-pull-auth.sh workload reconcile' "${deploy_action}" &&
  fail 'the deploy composite must not still call the bare reconcile, or the wrapper is bypassed'

# 6b. ORDERING: the baseline must be captured BEFORE the mutable tag is
#     published. Capturing it anywhere after publish leaves the window open,
#     because Flux can observe the new revision the moment the tag moves — which
#     is why a wrapper-local snapshot was not enough in the first place.
baseline_line="$(awk '/--snapshot-baseline/{print NR; exit}' "${deploy_action}")"
publish_line="$(awk '/publish-platform-manifests/{print NR; exit}' "${deploy_action}")"
[[ -n "${baseline_line}" ]] ||
  fail 'the deploy composite must capture the control-plane baseline before publishing'
[[ -n "${publish_line}" ]] ||
  fail 'could not locate the publish step in the deploy composite'
[[ "${baseline_line}" -lt "${publish_line}" ]] ||
  fail "the baseline must be captured BEFORE the publish step (baseline at line ${baseline_line}, publish at ${publish_line})"

# 6c. The baseline is only useful if the RECONCILE step is handed it. Assert that
#     structurally, per step: an earlier draft counted references to the variable
#     across the whole file, which the capture step alone already satisfies — so
#     it passed unchanged when the reconcile step's env was deleted, reading as
#     enforcement while enforcing nothing.
baseline_env_path='.runs.steps[] | select(.run | test("reconcile-flux-workloads\.sh")) | select(.run | test("--snapshot-baseline") | not) | .env.FLUX_CONTROL_PLANE_BASELINE_FILE // "MISSING"'
reconcile_baseline="$(yq -r "${baseline_env_path}" "${deploy_action}")"
[[ -n "${reconcile_baseline}" && "${reconcile_baseline}" != "MISSING" ]] ||
  fail 'the reconcile step must receive FLUX_CONTROL_PLANE_BASELINE_FILE, or the captured baseline never reaches the wrapper'

capture_env_path='.runs.steps[] | select(.run | test("--snapshot-baseline")) | .env.FLUX_CONTROL_PLANE_BASELINE_FILE // "MISSING"'
capture_baseline="$(yq -r "${capture_env_path}" "${deploy_action}")"
[[ "${capture_baseline}" == "${reconcile_baseline}" ]] ||
  fail "the capture and the read must name the SAME baseline file (capture=${capture_baseline}, reconcile=${reconcile_baseline})"

printf 'PASS: %s\n' "${0##*/}"
