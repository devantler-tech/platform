#!/usr/bin/env bash
# Behaviour and wiring tests for scripts/dr-rebuild-supersession-guard.sh.
#
# The behaviour half drives the guard's decision through its payload seam, so
# every branch is exercised without a token or network. The wiring half asserts
# the guard is actually reached: a guard the workflow does not call, or calls in
# a job the destructive job does not depend on, protects nothing.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly guard="${root_dir}/scripts/dr-rebuild-supersession-guard.sh"
readonly dr_workflow="${root_dir}/.github/workflows/dr-rebuild.yaml"

readonly run_id=4242
readonly created="2026-07-28T12:00:00Z"

pass_count=0

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

ok() {
  pass_count=$((pass_count + 1))
  printf 'ok — %s\n' "$1"
}

work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT

# run_guard <payload-json> — echoes the decided `superseded` value and returns
# the guard's exit status. Output is captured so a failing case can be asserted
# on its status rather than its noise.
run_guard() {
  local payload="$1"
  local payload_file="${work_dir}/runs.json"
  local output_file="${work_dir}/github_output"
  local status=0

  printf '%s' "${payload}" >"${payload_file}"
  : >"${output_file}"

  GITHUB_RUN_ID="${run_id}" \
    GITHUB_REPOSITORY="devantler-tech/platform" \
    GITHUB_OUTPUT="${output_file}" \
    DR_SUPERSESSION_RUNS_JSON="${payload_file}" \
    "${guard}" >"${work_dir}/stdout" 2>"${work_dir}/stderr" || status=$?

  sed -n 's/^superseded=//p' "${output_file}"

  return "${status}"
}

# me is always present in the payload; extra runs are appended by each case.
me="{\"id\": ${run_id}, \"created_at\": \"${created}\", \"status\": \"in_progress\", \"conclusion\": null, \"updated_at\": \"${created}\", \"html_url\": \"u/me\"}"

other() {
  # other <id> <status> <conclusion> <updated_at>
  printf '{"id": %s, "created_at": "2026-07-28T11:00:00Z", "status": "%s", "conclusion": %s, "updated_at": "%s", "html_url": "u/%s"}' \
    "$1" "$2" "$(if [ "$3" = "null" ]; then printf 'null'; else printf '"%s"' "$3"; fi)" "$4" "$1"
}

payload() {
  printf '{"workflow_runs": [%s]}' "$(
    local IFS=,
    printf '%s' "$*"
  )"
}

# --- behaviour -------------------------------------------------------------

# The ordinary case: this is the only run, so there is nothing to be superseded by.
[ "$(run_guard "$(payload "${me}")")" = "false" ] ||
  fail 'a lone run must proceed'
ok 'a lone run proceeds'

# The reported P1. An earlier dispatch held the dr-rebuild slot while waiting on
# prod-deploy and finished AFTER this run was created; rebuilding again would
# tear down the cluster it just recovered.
[ "$(run_guard "$(payload "${me}" "$(other 1 completed success 2026-07-28T12:30:00Z)")")" = "true" ] ||
  fail 'a rebuild that completed successfully after this run was created must supersede it'
ok 'a successful rebuild completing after this run was created supersedes it'

# The genuine second incident: the earlier rebuild finished BEFORE the operator
# pressed the button again, so it cannot have satisfied this request.
[ "$(run_guard "$(payload "${me}" "$(other 1 completed success 2026-07-28T11:30:00Z)")")" = "false" ] ||
  fail 'a rebuild that finished before this run was created must not supersede it'
ok 'a rebuild finishing before this run was created does not supersede it'

# Retry after a failure must still rebuild — this is the path an incident
# actually depends on.
[ "$(run_guard "$(payload "${me}" "$(other 1 completed failure 2026-07-28T12:30:00Z)")")" = "false" ] ||
  fail 'a FAILED earlier rebuild must not supersede this one'
ok 'a failed earlier rebuild does not supersede this one'

[ "$(run_guard "$(payload "${me}" "$(other 1 completed cancelled 2026-07-28T12:30:00Z)")")" = "false" ] ||
  fail 'a CANCELLED earlier rebuild must not supersede this one'
ok 'a cancelled earlier rebuild does not supersede this one'

# An unfinished run has recovered nothing yet, whatever its timestamps say.
[ "$(run_guard "$(payload "${me}" "$(other 1 in_progress null 2026-07-28T12:30:00Z)")")" = "false" ] ||
  fail 'an in-progress run must not supersede this one'
ok 'an in-progress run does not supersede this one'

# --- fail-closed -----------------------------------------------------------

# Not finding this run means the comparison baseline is unknown, so the decision
# is undecidable and must not default to "proceed".
if run_guard "$(payload "$(other 1 completed success 2026-07-28T12:30:00Z)")" >/dev/null 2>&1; then
  fail 'a payload not containing this run must fail closed'
fi
ok 'a payload not containing this run fails closed'

if run_guard '{"workflow_runs": "not-a-list"}' >/dev/null 2>&1; then
  fail 'a malformed payload must fail closed'
fi
ok 'a malformed payload fails closed'

if run_guard 'this is not json' >/dev/null 2>&1; then
  fail 'unparseable JSON must fail closed'
fi
ok 'unparseable JSON fails closed'

status=0
GITHUB_RUN_ID="${run_id}" GITHUB_REPOSITORY="devantler-tech/platform" \
  GITHUB_OUTPUT="${work_dir}/out" \
  DR_SUPERSESSION_RUNS_JSON="${work_dir}/does-not-exist.json" \
  "${guard}" >/dev/null 2>&1 || status=$?
[ "${status}" -ne 0 ] || fail 'an unreadable payload file must fail closed'
ok 'an unreadable payload file fails closed'

# --- wiring ----------------------------------------------------------------
#
# Everything above is inert unless the workflow reaches the guard on the path to
# the destructive job.

[[ -x "${guard}" ]] ||
  fail 'the supersession guard must be executable'
ok 'the guard is executable'

# The guard compares runs of the workflow it names; a rename that misses one
# side would compare an empty run list and silently never supersede.
guard_workflow="$(sed -n 's/^readonly workflow_file="\(.*\)"$/\1/p' "${guard}")"
[ -n "${guard_workflow}" ] || fail 'could not read the workflow file name out of the guard'
[ -f "${root_dir}/.github/workflows/${guard_workflow}" ] ||
  fail "the guard compares runs of ${guard_workflow}, which does not exist"
[ "${root_dir}/.github/workflows/${guard_workflow}" = "${dr_workflow}" ] ||
  fail "the guard names ${guard_workflow}, not the DR rebuild workflow it gates"
ok 'the guard names the workflow it gates, and that workflow exists'

grep -Fq 'run: ./scripts/dr-rebuild-supersession-guard.sh' "${dr_workflow}" ||
  fail 'the DR workflow must invoke the supersession guard'
ok 'the DR workflow invokes the guard'

# The gate must be a dependency of the destructive job, not merely present.
grep -Fq 'needs: supersession-gate' "${dr_workflow}" ||
  fail 'the rebuild job must depend on the supersession gate'
# shellcheck disable=SC2016  # GitHub evaluates this expression; match it literally.
grep -Fq "if: needs.supersession-gate.outputs.superseded != 'true'" "${dr_workflow}" ||
  fail 'the rebuild job must skip when the gate reports it was superseded'
ok 'the rebuild job depends on the gate and skips when superseded'

# The gate cannot list runs without this scope, and the failure would arrive
# during an incident.
awk '/^  supersession-gate:/{g=1} g && /actions: read/{found=1} /^  rebuild:/{g=0} END{exit !found}' "${dr_workflow}" ||
  fail 'the supersession gate job needs actions: read to list workflow runs'
ok 'the gate job holds actions: read'

# The guard must run BEFORE the destructive job is defined to depend on it, and
# the rebuild job must still be the one holding the prod-deploy lock.
gate_line="$(grep -n '^  supersession-gate:' "${dr_workflow}" | cut -d: -f1)"
rebuild_line="$(grep -n '^  rebuild:' "${dr_workflow}" | cut -d: -f1)"
[ -n "${gate_line}" ] && [ -n "${rebuild_line}" ] ||
  fail 'both the gate job and the rebuild job must exist'
((gate_line < rebuild_line)) ||
  fail 'the supersession gate must be declared before the rebuild job'
ok 'the gate is declared before the rebuild job'

printf '\n%d checks passed.\n' "${pass_count}"
