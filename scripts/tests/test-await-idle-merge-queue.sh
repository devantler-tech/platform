#!/usr/bin/env bash
#
# Pins scripts/await-idle-merge-queue.sh against a stub `gh` command that answers each read
# with the next GraphQL response of a scripted sequence, filtered by the script's own --jq
# program, so the program is exercised as well as the loop.
#
# A stub line is one JSON response. `state <tip> <queued>` below builds the usual one.

set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="$repo_root/scripts/await-idle-merge-queue.sh"
workflow="$repo_root/.github/workflows/validate-main.yaml"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

failures=0
assertions=0

[ -x "$script" ] || {
  printf 'FAIL: %s is not executable\n' "$script"
  exit 1
}
command -v jq >/dev/null 2>&1 || {
  printf 'FAIL: jq is required to run the script'"'"'s own filter\n'
  exit 1
}

subject="$(printf 'a%.0s' {1..40})"
newer="$(printf 'b%.0s' {1..40})"

mkdir -p "$scratch/bin"
cat >"$scratch/bin/gh" <<'EOF'
#!/usr/bin/env bash
printf 'gh %s\n' "$*" >>"$STUB_LOG"
[ "$1 $2" = "api graphql" ] || exit 99
[ "${STUB_RC:-0}" = 0 ] || exit "$STUB_RC"
filter=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--jq" ]; then
    filter="$2"
    break
  fi
  shift
done
[ -n "$filter" ] || exit 98
calls=$(($(cat "$STUB_CALLS") + 1))
printf '%s\n' "$calls" >"$STUB_CALLS"
line="$(sed -n "${calls}p" "$STUB_STATES")"
# Past the end of the script, keep repeating the last response.
[ -n "$line" ] || line="$(tail -n 1 "$STUB_STATES")"
printf '%s\n' "$line" | jq -r "$filter"
EOF
chmod +x "$scratch/bin/gh"

state() {
  printf '{"data":{"repository":{"ref":{"target":{"oid":"%s"}},"mergeQueue":{"entries":{"totalCount":%s}}}}}' "$1" "$2"
}

# <label> <expected-rc> <expected-output> <expected-reads|-> <stub rc> <response>...
run_case() {
  local label="$1" want_rc="$2" want_output="$3" want_reads="$4" stub_rc="$5"
  shift 5
  local out rc got_output reads
  : >"$scratch/log"
  : >"$scratch/output"
  printf '0\n' >"$scratch/calls"
  printf '%s\n' "$@" >"$scratch/states"
  if out="$(env PATH="$scratch/bin:$PATH" STUB_LOG="$scratch/log" STUB_STATES="$scratch/states" \
    STUB_CALLS="$scratch/calls" STUB_RC="$stub_rc" \
    IDLE_POLL_SECONDS="${CASE_POLL_SECONDS-0}" IDLE_MAX_POLLS="${CASE_MAX_POLLS-5}" \
    IDLE_REPOSITORY="${CASE_REPOSITORY-devantler-tech/platform}" IDLE_BRANCH="${CASE_BRANCH-main}" \
    IDLE_SHA="${CASE_SHA-$subject}" GITHUB_OUTPUT="${CASE_OUTPUT-$scratch/output}" \
    "$script" 2>&1)"; then rc=0; else rc=$?; fi
  printf '%s\n' "$out" >"$scratch/stdout"
  got_output="$(cat "$scratch/output")"
  reads="$(cat "$scratch/calls")"

  assertions=$((assertions + 1))
  if [ "$rc" = "$want_rc" ] && [ "$got_output" = "$want_output" ] &&
    { [ "$want_reads" = - ] || [ "$reads" = "$want_reads" ]; }; then
    printf '  ok   %s (exit %s, output=%s, reads=%s)\n' "$label" "$rc" "${got_output:-<none>}" "$reads"
  else
    printf '  FAIL %s: expected exit %s, output=%s, reads=%s; got exit %s, output=%s, reads=%s:\n%s\n' \
      "$label" "$want_rc" "${want_output:-<none>}" "$want_reads" "$rc" "${got_output:-<none>}" "$reads" "$out"
    failures=$((failures + 1))
  fi
}

assert_stdout() {
  local label="$1" needle="$2"
  assertions=$((assertions + 1))
  if grep -qF -- "$needle" "$scratch/stdout"; then
    printf '  ok   %s\n' "$label"
  else
    printf '  FAIL %s: expected the output to contain "%s":\n%s\n' "$label" "$needle" "$(cat "$scratch/stdout")"
    failures=$((failures + 1))
  fi
}

printf 'await-idle-merge-queue.sh\n'

run_case 'an empty queue observes at once' 0 'observe=true' 1 0 "$(state "$subject" 0)"
assert_stdout 'and says why, as a notice' '::notice title=Production observation::no change is waiting in the merge queue after 1 read(s)'

run_case 'a busy queue is waited out, then observed' 0 'observe=true' 4 0 \
  "$(state "$subject" 3)" "$(state "$subject" 2)" "$(state "$subject" 1)" "$(state "$subject" 0)"

run_case 'a newer push takes over the observation' 0 'observe=false' 3 0 \
  "$(state "$subject" 2)" "$(state "$subject" 2)" "$(state "$newer" 1)"
assert_stdout 'and names both commits' "main has moved on from ${subject:0:12} to ${newer:0:12}"

# The newer push's own run observes, so this one stands down even when nothing is queued.
run_case 'a newer push takes over even with an empty queue' 0 'observe=false' 1 0 "$(state "$newer" 0)"

run_case 'a queue that never drains is observed after the bound' 0 'observe=true' 5 0 "$(state "$subject" 2)"
assert_stdout 'and warns that the bound was reached' '::warning title=Production observation::2 change(s) were still queued after 5 reads'

# Every state the script cannot read falls back to observing, which is what happened
# before it existed. None of them may stand the observer down.
run_case 'a failed read observes' 0 'observe=true' - 1 "$(state "$subject" 2)"
assert_stdout 'and says the queue could not be read' 'the merge queue could not be read'
assert_stdout 'and marks the fallback as a warning' '::warning title=Production observation::the merge queue could not be read'
run_case 'a response that is not JSON observes' 0 'observe=true' 1 0 'not json'
run_case 'a response without a repository observes' 0 'observe=true' 1 0 '{"data":{"repository":null}}'
run_case 'a response without the branch observes' 0 'observe=true' 1 0 \
  '{"data":{"repository":{"ref":null,"mergeQueue":{"entries":{"totalCount":2}}}}}'
run_case 'a queue without a count observes' 0 'observe=true' 1 0 \
  "{\"data\":{\"repository\":{\"ref\":{\"target\":{\"oid\":\"$subject\"}},\"mergeQueue\":{\"entries\":{}}}}}"
run_case 'a negative count observes' 0 'observe=true' 1 0 "$(state "$subject" -1)"
run_case 'a short commit id observes' 0 'observe=true' 1 0 "$(state abc123 2)"

# A branch that has no merge queue has nothing queued.
run_case 'a branch without a merge queue observes at once' 0 'observe=true' 1 0 \
  "{\"data\":{\"repository\":{\"ref\":{\"target\":{\"oid\":\"$subject\"}},\"mergeQueue\":null}}}"

# The read names this repository's branch and nothing else, and the script never writes.
run_case 'the read is scoped to the branch' 0 'observe=true' 1 0 "$(state "$subject" 0)"
assertions=$((assertions + 1))
if grep -qF -- '-f owner=devantler-tech -f name=platform -f branch=main -f ref=refs/heads/main' "$scratch/log" &&
  [ "$(grep -c '^gh ' "$scratch/log")" = 1 ] && ! grep -qE 'mutation|--method|-X ' "$scratch/log"; then
  printf '  ok   one read-only query for main\n'
else
  printf '  FAIL the read is not one read-only query for main:\n%s\n' "$(cat "$scratch/log")"
  failures=$((failures + 1))
fi

# Misconfiguration writes no answer and fails, so the observer does not run on a guess.
CASE_REPOSITORY='platform' run_case 'a repository without an owner is refused' 1 '' 0 0 "$(state "$subject" 0)"
CASE_REPOSITORY='devantler-tech/platform --jq .' run_case 'a repository with an option is refused' 1 '' 0 0 "$(state "$subject" 0)"
CASE_BRANCH='main; rm -rf /' run_case 'a branch with shell syntax is refused' 1 '' 0 0 "$(state "$subject" 0)"
CASE_SHA='main' run_case 'a branch name in place of the commit is refused' 1 '' 0 0 "$(state "$subject" 0)"
CASE_SHA="${subject:0:12}" run_case 'an abbreviated commit is refused' 1 '' 0 0 "$(state "$subject" 0)"
CASE_POLL_SECONDS='soon' run_case 'a poll interval that is not a number is refused' 1 '' 0 0 "$(state "$subject" 0)"
CASE_MAX_POLLS='0' run_case 'a bound of zero reads is refused' 1 '' 0 0 "$(state "$subject" 0)"
CASE_OUTPUT="$scratch/missing/output" run_case 'an unwritable answer fails' 1 '' 1 0 "$(state "$subject" 0)" 2>/dev/null

# The workflow wiring this script relies on.
assert_workflow() {
  local label="$1" expression="$2" expected="$3" got
  assertions=$((assertions + 1))
  got="$(yq -r "$expression" "$workflow" 2>&1)"
  if [ "$got" = "$expected" ]; then
    printf '  ok   %s\n' "$label"
  else
    printf '  FAIL %s: expected "%s", got "%s"\n' "$label" "$expected" "$got"
    failures=$((failures + 1))
  fi
}

printf 'validate-main.yaml\n'
wait_job='.jobs.await-idle-merge-queue'
observer='.jobs.observe-prod-convergence'
assert_workflow 'the wait takes no lock' "$wait_job | has(\"concurrency\")" 'false'
assert_workflow 'the wait runs on pushes to main only' "$wait_job.if" \
  "github.event_name == 'push' && github.ref == 'refs/heads/main'"
assert_workflow 'the wait can only read' "$wait_job.permissions | to_entries | map(.key + \"=\" + .value) | join(\" \")" \
  'contents=read pull-requests=read'
assert_workflow 'the wait runs the script' "$wait_job.steps[-1].run" 'scripts/await-idle-merge-queue.sh'
assert_workflow 'the wait is told this push'"'"'s commit' "$wait_job.steps[-1].env.IDLE_SHA" "\${{ github.sha }}"
assert_workflow 'the wait reads the default branch'"'"'s queue' "$wait_job.steps[-1].env.IDLE_BRANCH" 'main'
assert_workflow 'the wait cannot be skipped over' "$wait_job.steps[-1] | has(\"continue-on-error\")" 'false'
assert_workflow 'the wait outlasts its own polling' "$wait_job.timeout-minutes" '75'
assert_workflow 'the wait exports its answer' "$wait_job.outputs.observe" "\${{ steps.idle.outputs.observe }}"
assert_workflow 'the observer waits for it' "$observer.needs | join(\" \")" 'await-idle-merge-queue'
assert_workflow 'the observer runs only on an explicit yes' "$observer.if" \
  "github.event_name == 'push' && github.ref == 'refs/heads/main' && needs.await-idle-merge-queue.outputs.observe == 'true'"
assert_workflow 'the observer still takes the production lock' "$observer.concurrency.group" 'prod-deploy'

printf '%s assertions, %s failed\n' "$assertions" "$failures"
[ "$failures" = 0 ]
