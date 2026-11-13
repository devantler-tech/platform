#!/usr/bin/env bash
#
# Pins scripts/await-idle-merge-queue.sh against stub `gh`, `timeout` and `sleep` commands.
#
# The stub `gh` answers each queue read with the next GraphQL response of a scripted
# sequence, and each check-run read from a file named after the commit and the job, both
# filtered by the script's own --jq program, so the programs are exercised as well as the
# loop. It refuses a job name it does not know, and fails once a case reads more than 50
# times, so a loop that never answers fails its case instead of hanging the suite.
#
# `state <tip> <queued>` builds a queue response and `runs <status> <conclusion>` a
# check-run response. `given <deploy|wait> <commit> <response>...` scripts what the reads of
# that commit's job return, in order; the last response repeats, and a commit that was given
# nothing has no such check run.

set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="$repo_root/scripts/await-idle-merge-queue.sh"
workflow="$repo_root/.github/workflows/validate-main.yaml"
ci_workflow="$repo_root/.github/workflows/ci.yaml"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

failures=0
assertions=0

[ -x "$script" ] || {
  printf 'FAIL: %s is not executable\n' "$script"
  exit 1
}
command -v jq >/dev/null 2>&1 || {
  printf 'FAIL: jq is required to run the script'"'"'s own filters\n'
  exit 1
}

subject="$(printf 'a%.0s' {1..40})"
newer="$(printf 'b%.0s' {1..40})"
newest="$(printf 'c%.0s' {1..40})"
deploy_job='🚀 Deploy to Prod'
wait_job='🚦 Wait for an idle merge queue'

mkdir -p "$scratch/bin" "$scratch/checks"
cat >"$scratch/bin/gh" <<'EOF'
#!/usr/bin/env bash
printf 'gh %s\n' "$*" >>"$STUB_LOG"
total=$(($(cat "$STUB_DIR/total") + 1))
printf '%s\n' "$total" >"$STUB_DIR/total"
[ "$total" -le 50 ] || exit 95
[ "${1-}" = api ] || exit 99
shift
filter="" check="" path=""
case "${1-}" in
graphql) kind=queue ;;
--method)
  [ "${2-}" = GET ] || exit 94
  kind=check
  path="${3-}"
  ;;
*) exit 99 ;;
esac
while [ "$#" -gt 0 ]; do
  case "$1" in
  --jq)
    filter="$2"
    shift
    ;;
  check_name=*) check="${1#check_name=}" ;;
  esac
  shift
done
[ -n "$filter" ] || exit 98

if [ "$kind" = queue ]; then
  [ "${STUB_QUEUE_RC:-0}" = 0 ] || exit "$STUB_QUEUE_RC"
  calls=$(($(cat "$STUB_DIR/queue.calls") + 1))
  printf '%s\n' "$calls" >"$STUB_DIR/queue.calls"
  line="$(sed -n "${calls}p" "$STUB_DIR/queue")"
  # Past the end of the sequence, keep repeating the last response.
  [ -n "$line" ] || line="$(tail -n 1 "$STUB_DIR/queue")"
  printf '%s\n' "$line" | jq -r "$filter"
  exit
fi

commit="${path#repos/devantler-tech/platform/commits/}"
commit="${commit%/check-runs}"
case "$check" in
"$STUB_DEPLOY_JOB") job=deploy ;;
"$STUB_WAIT_JOB") job=wait ;;
*) exit 97 ;;
esac
responses="$STUB_DIR/checks/${job}.${commit}"
calls=$(($(cat "$responses.calls" 2>/dev/null || printf 0) + 1))
printf '%s\n' "$calls" >"$responses.calls"
if [ ! -f "$responses" ]; then
  line='{"check_runs":[]}'
else
  line="$(sed -n "${calls}p" "$responses")"
  [ -n "$line" ] || line="$(tail -n 1 "$responses")"
fi
[ "$line" != FAILED ] || exit 1
printf '%s\n' "$line" | jq -r "$filter"
EOF
cat >"$scratch/bin/timeout" <<'EOF'
#!/usr/bin/env bash
printf 'timeout %s\n' "$1" >>"$STUB_LOG"
shift
exec "$@"
EOF
cat >"$scratch/bin/sleep" <<'EOF'
#!/usr/bin/env bash
printf 'sleep %s\n' "$*" >>"$STUB_LOG"
EOF
chmod +x "$scratch/bin/gh" "$scratch/bin/timeout" "$scratch/bin/sleep"

state() {
  printf '{"data":{"repository":{"ref":{"target":{"oid":"%s"}},"mergeQueue":{"entries":{"totalCount":%s}}}}}' "$1" "$2"
}

# <status> <conclusion>: one check run. GitHub reports an unfinished run's conclusion as null.
runs() {
  if [ "$2" = null ]; then
    printf '{"check_runs":[{"status":"%s","conclusion":null}]}' "$1"
  else
    printf '{"check_runs":[{"status":"%s","conclusion":"%s"}]}' "$1" "$2"
  fi
}

given() {
  local job="$1" commit="$2"
  shift 2
  printf '%s\n' "$@" >"$scratch/checks/${job}.${commit}"
}

# How often the last case read <deploy|wait> of <commit>.
check_reads() {
  cat "$scratch/checks/$1.$2.calls" 2>/dev/null || printf 0
}

# <label> <expected-rc> <expected-output> <expected-queue-reads|-> <queue response>...
# Uses what `given` scripted since the previous case, and leaves the read counts behind.
run_case() {
  local label="$1" want_rc="$2" want_output="$3" want_reads="$4"
  shift 4
  local out rc got_output reads
  : >"$scratch/log"
  : >"$scratch/output"
  rm -f "$scratch"/checks/*.calls
  printf '0\n' >"$scratch/total"
  printf '0\n' >"$scratch/queue.calls"
  printf '%s\n' "$@" >"$scratch/queue"
  if out="$(env PATH="$scratch/bin:$PATH" STUB_LOG="$scratch/log" STUB_DIR="$scratch" \
    STUB_QUEUE_RC="${CASE_QUEUE_RC-0}" STUB_DEPLOY_JOB="$deploy_job" STUB_WAIT_JOB="$wait_job" \
    IDLE_REPOSITORY="${CASE_REPOSITORY-devantler-tech/platform}" IDLE_BRANCH="${CASE_BRANCH-main}" \
    IDLE_SHA="${CASE_SHA-$subject}" GITHUB_OUTPUT="${CASE_OUTPUT-$scratch/output}" \
    "${CASE_RUNNER-$script}" 2>&1)"; then rc=0; else rc=$?; fi
  printf '%s\n' "$out" >"$scratch/stdout"
  got_output="$(cat "$scratch/output")"
  reads="$(cat "$scratch/queue.calls")"
  find "$scratch/checks" -type f ! -name '*.calls' -delete

  assertions=$((assertions + 1))
  if [ "$rc" = "$want_rc" ] && [ "$got_output" = "$want_output" ] &&
    { [ "$want_reads" = - ] || [ "$reads" = "$want_reads" ]; }; then
    printf '  ok   %s (exit %s, output=%s, queue reads=%s)\n' "$label" "$rc" "${got_output:-<none>}" "$reads"
  else
    printf '  FAIL %s: expected exit %s, output=%s, queue reads=%s; got exit %s, output=%s, queue reads=%s:\n%s\n' \
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

# <label> <expected> <actual>
assert_equal() {
  assertions=$((assertions + 1))
  if [ "$2" = "$3" ]; then
    printf '  ok   %s\n' "$1"
  else
    printf '  FAIL %s: expected "%s", got "%s"\n%s\n' "$1" "$2" "$3" "$(cat "$scratch/log")"
    failures=$((failures + 1))
  fi
}

printf 'await-idle-merge-queue.sh: this push is still the tip\n'

run_case 'an empty queue observes at once' 0 'observe=true' 1 "$(state "$subject" 0)"
assert_stdout 'and says why, as a notice' '::notice title=Production observation::no change is waiting in the merge queue after 1 read(s)'
assert_equal 'and reads no check run' 1 "$(grep -c '^gh ' "$scratch/log")"

run_case 'a busy queue is waited out, then observed' 0 'observe=true' 4 \
  "$(state "$subject" 3)" "$(state "$subject" 2)" "$(state "$subject" 1)" "$(state "$subject" 0)"
assert_equal 'pausing 30 seconds between reads' 'sleep 30 sleep 30 sleep 30' "$(grep '^sleep ' "$scratch/log" | tr '\n' ' ' | sed 's/ $//')"
assert_equal 'and giving every read 60 seconds' 4 "$(grep -c '^timeout 60$' "$scratch/log")"
assert_equal 'with no read left unbounded' "$(grep -c '^gh ' "$scratch/log")" "$(grep -c '^timeout ' "$scratch/log")"

printf 'await-idle-merge-queue.sh: a later change merged\n'

# The later change deployed before it merged, so production is at or past this push.
given deploy "$newer" "$(runs completed success)"
run_case 'a later change that deployed ends the wait' 0 'observe=false' 3 \
  "$(state "$subject" 2)" "$(state "$subject" 2)" "$(state "$newer" 1)"
assert_stdout 'and names both commits' "main has moved on from ${subject:0:12} to ${newer:0:12}, which deployed"
assert_equal 'without asking who observes next' 0 "$(check_reads wait "$newer")"

given deploy "$newer" "$(runs completed success)"
run_case 'a later change that deployed ends the wait with an empty queue too' 0 'observe=false' 1 "$(state "$newer" 0)"

# A later change that did not deploy changed nothing in production: the wait goes on, and
# once the queue is empty the newest push's own wait observes, so only one run does.
given deploy "$newer" "$(runs completed skipped)"
given wait "$newer" "$(runs in_progress null)"
run_case 'a later change that did not deploy does not end the wait' 0 'observe=false' 4 \
  "$(state "$newer" 2)" "$(state "$newer" 2)" "$(state "$newer" 1)" "$(state "$newer" 0)"
assert_stdout 'and the newest push observes once nothing is queued' "no change is queued and main has moved on from ${subject:0:12} to ${newer:0:12}; that push observes production"
assert_equal 'its deploy is read once, not on every read' 1 "$(check_reads deploy "$newer")"
assert_equal 'its wait is read only once nothing is queued' 1 "$(check_reads wait "$newer")"

for conclusion in failure cancelled timed_out skipped neutral; do
  given deploy "$newer" "$(runs completed "$conclusion")"
  given wait "$newer" "$(runs queued null)"
  run_case "a later deploy that ended ${conclusion} is not a deploy" 0 'observe=false' 2 "$(state "$newer" 1)" "$(state "$newer" 0)"
  assert_stdout "so the newest push observes (${conclusion})" 'that push observes production'
done
given deploy "$newer" "$(runs in_progress null)"
given wait "$newer" "$(runs completed success)"
run_case 'a later deploy that is still running is not a deploy' 0 'observe=false' 2 "$(state "$newer" 1)" "$(state "$newer" 0)"
assert_stdout 'and a newest wait that already succeeded counts' 'that push observes production'

# Each new tip is judged once: the first did not deploy, the second did.
given deploy "$newer" "$(runs completed skipped)"
given deploy "$newest" "$(runs completed success)"
run_case 'a second later change that deployed ends the wait' 0 'observe=false' 3 \
  "$(state "$newer" 2)" "$(state "$newer" 2)" "$(state "$newest" 1)"
assert_stdout 'and is the one named' "to ${newest:0:12}, which deployed"

# The newest push's run may not be listed yet, so its wait is looked for more than once.
given wait "$newer" '{"check_runs":[]}' "$(runs queued null)"
run_case 'a newest push whose wait is listed late still observes' 0 'observe=false' 2 "$(state "$newer" 0)"
assert_equal 'after a second look' 2 "$(check_reads wait "$newer")"

# A push with no run of its own (a skipped workflow, a broken one) observes nothing, so
# this run does. A wait that failed, was cancelled or was skipped observes nothing either.
run_case 'a newest push with no wait of its own is not trusted to observe' 0 'observe=true' 4 "$(state "$newer" 0)"
assert_stdout 'and says so' "the newer push ${newer:0:12} has no wait of its own after 4 reads"
assert_equal 'after four looks' 4 "$(check_reads wait "$newer")"
for conclusion in failure cancelled skipped timed_out; do
  given wait "$newer" "$(runs completed "$conclusion")"
  run_case "a newest wait that ended ${conclusion} is not trusted to observe" 0 'observe=true' 4 "$(state "$newer" 0)"
done

# The looks are counted per tip: a third push starts the count again, so its own wait may
# be listed late too.
given wait "$newest" '{"check_runs":[]}' "$(runs in_progress null)"
run_case 'a third push is looked up afresh' 0 'observe=false' 5 \
  "$(state "$newer" 0)" "$(state "$newer" 0)" "$(state "$newer" 0)" "$(state "$newest" 0)" "$(state "$newest" 0)"
assert_stdout 'and is the one that observes' "to ${newest:0:12}; that push observes production"

printf 'await-idle-merge-queue.sh: the bound\n'

# The bound is this run's own hour, whatever merged meanwhile. The runner starts the script
# with an hour already on the clock.
late="$scratch/late"
printf '#!/usr/bin/env bash\nSECONDS=3600\n. "%s"\n' "$script" >"$late"
chmod +x "$late"
CASE_RUNNER="$late" run_case 'a queue that never drains is observed after an hour' 0 'observe=true' 1 "$(state "$subject" 2)"
assert_stdout 'and warns that the bound was reached' '::warning title=Production observation::an hour passed with 2 change(s) still queued'
given deploy "$newer" "$(runs completed skipped)"
CASE_RUNNER="$late" run_case 'a later change that did not deploy does not restart the hour' 0 'observe=true' 1 "$(state "$newer" 2)"
almost="$scratch/almost"
printf '#!/usr/bin/env bash\nSECONDS=3000\n. "%s"\n' "$script" >"$almost"
chmod +x "$almost"
CASE_RUNNER="$almost" run_case 'fifty minutes are not the bound' 0 'observe=true' 3 \
  "$(state "$subject" 2)" "$(state "$subject" 2)" "$(state "$subject" 0)"
assert_stdout 'so the queue is still waited out' 'no change is waiting in the merge queue after 3 read(s)'

printf 'await-idle-merge-queue.sh: what it cannot read\n'

# Every state the script cannot read falls back to observing, which is what happened
# before it existed. None of them may stand the observer down.
CASE_QUEUE_RC=1 run_case 'a failed queue read observes' 0 'observe=true' - "$(state "$subject" 2)"
assert_stdout 'and marks the fallback as a warning' '::warning title=Production observation::the merge queue could not be read'
CASE_QUEUE_RC=124 run_case 'a queue read that ran out of time observes' 0 'observe=true' - "$(state "$subject" 2)"
run_case 'a response that is not JSON observes' 0 'observe=true' 1 'not json'
run_case 'a response without a repository observes' 0 'observe=true' 1 '{"data":{"repository":null}}'
run_case 'a response without the branch observes' 0 'observe=true' 1 \
  '{"data":{"repository":{"ref":null,"mergeQueue":{"entries":{"totalCount":2}}}}}'
run_case 'a queue without a count observes' 0 'observe=true' 1 \
  "{\"data\":{\"repository\":{\"ref\":{\"target\":{\"oid\":\"$subject\"}},\"mergeQueue\":{\"entries\":{}}}}}"
run_case 'a negative count observes' 0 'observe=true' 1 "$(state "$subject" -1)"
run_case 'a short commit id observes' 0 'observe=true' 1 "$(state abc123 2)"
# A branch whose queue cannot be found is not a branch with nothing queued.
run_case 'a branch without a merge queue observes' 0 'observe=true' 1 \
  "{\"data\":{\"repository\":{\"ref\":{\"target\":{\"oid\":\"$subject\"}},\"mergeQueue\":null}}}"
assert_stdout 'as a warning, not as an idle queue' '::warning title=Production observation::the merge queue answered'

given deploy "$newer" FAILED
run_case 'a later deploy that cannot be read observes' 0 'observe=true' 1 "$(state "$newer" 2)"
assert_stdout 'and says which read failed' "::warning title=Production observation::the deploy of ${newer:0:12} could not be read"
given deploy "$newer" 'not json'
run_case 'a later deploy that is not JSON observes' 0 'observe=true' 1 "$(state "$newer" 2)"
given deploy "$newer" '{"message":"Not Found"}'
run_case 'a later deploy without check runs observes' 0 'observe=true' 1 "$(state "$newer" 2)"
given wait "$newer" FAILED
run_case 'a newest wait that cannot be read observes' 0 'observe=true' 1 "$(state "$newer" 0)"
assert_stdout 'and says which read failed' "::warning title=Production observation::the wait of the newer push ${newer:0:12} could not be read"
given wait "$newer" '{"check_runs":{}}'
run_case 'a newest wait that is not a list observes' 0 'observe=true' 1 "$(state "$newer" 0)"

printf 'await-idle-merge-queue.sh: what it reads\n'

# The reads name this repository, its branch and the two jobs, and the script never writes.
given deploy "$newer" "$(runs completed skipped)"
given wait "$newer" "$(runs in_progress null)"
run_case 'the reads are scoped' 0 'observe=false' 1 "$(state "$newer" 0)"
# shellcheck disable=SC2016 # The GraphQL variables are matched literally.
assert_equal 'one query for the branch and its queue' 1 \
  "$(grep -cF -- 'gh api graphql -f query=query($owner:String!,$name:String!,$branch:String!,$ref:String!)' "$scratch/log")"
assert_equal 'with this repository and branch' 1 \
  "$(grep -cF -- '-f owner=devantler-tech -f name=platform -f branch=main -f ref=refs/heads/main' "$scratch/log")"
assert_equal 'one read of the later change'"'"'s deploy' 1 \
  "$(grep -cF -- "gh api --method GET repos/devantler-tech/platform/commits/${newer}/check-runs -f check_name=${deploy_job} -f filter=latest" "$scratch/log")"
assert_equal 'one read of the newest push'"'"'s wait' 1 \
  "$(grep -cF -- "gh api --method GET repos/devantler-tech/platform/commits/${newer}/check-runs -f check_name=${wait_job} -f filter=latest" "$scratch/log")"
assert_equal 'and nothing else' 3 "$(grep -c '^gh ' "$scratch/log")"
assert_equal 'each given 60 seconds' 3 "$(grep -c '^timeout 60$' "$scratch/log")"
assert_equal 'none of them a write' 0 "$(grep -cE 'mutation|--method (POST|PUT|PATCH|DELETE)|-X ' "$scratch/log")"

printf 'await-idle-merge-queue.sh: misconfiguration\n'

# Misconfiguration writes no answer and fails, so the observer does not run on a guess.
CASE_REPOSITORY='platform' run_case 'a repository without an owner is refused' 1 '' 0 "$(state "$subject" 0)"
CASE_REPOSITORY='devantler-tech/platform --jq .' run_case 'a repository with an option is refused' 1 '' 0 "$(state "$subject" 0)"
CASE_REPOSITORY='' run_case 'a missing repository is refused' 1 '' 0 "$(state "$subject" 0)"
CASE_BRANCH='main; rm -rf /' run_case 'a branch with shell syntax is refused' 1 '' 0 "$(state "$subject" 0)"
CASE_SHA='main' run_case 'a branch name in place of the commit is refused' 1 '' 0 "$(state "$subject" 0)"
CASE_SHA="${subject:0:12}" run_case 'an abbreviated commit is refused' 1 '' 0 "$(state "$subject" 0)"
CASE_OUTPUT="$scratch/missing/output" run_case 'an unwritable answer fails' 1 '' 1 "$(state "$subject" 0)" 2>/dev/null

# The pauses and bounds are the script's own: nothing in the environment changes them.
assertions=$((assertions + 1))
if grep -qE 'IDLE_(POLL|MAX|READ|WAIT)' "$script"; then
  printf '  FAIL the script takes a pause or a bound from its environment\n'
  failures=$((failures + 1))
else
  printf '  ok   the pauses and bounds cannot be set from the environment\n'
fi

# The workflow wiring this script relies on.
assert_workflow() {
  local label="$1" file="$2" expression="$3" expected="$4" got
  assertions=$((assertions + 1))
  got="$(yq -r "$expression" "$file" 2>&1)"
  if [ "$got" = "$expected" ]; then
    printf '  ok   %s\n' "$label"
  else
    printf '  FAIL %s: expected "%s", got "%s"\n' "$label" "$expected" "$got"
    failures=$((failures + 1))
  fi
}

printf 'validate-main.yaml and ci.yaml\n'
waiter='.jobs.await-idle-merge-queue'
observer='.jobs.observe-prod-convergence'
assert_workflow 'the wait takes no lock' "$workflow" "$waiter | has(\"concurrency\")" 'false'
assert_workflow 'the wait runs on pushes to main only' "$workflow" "$waiter.if" \
  "github.event_name == 'push' && github.ref == 'refs/heads/main'"
assert_workflow 'the wait can only read' "$workflow" "$waiter.permissions | to_entries | map(.key + \"=\" + .value) | join(\" \")" \
  'contents=read checks=read'
assert_workflow 'the wait runs the script' "$workflow" "$waiter.steps[-1].run" 'scripts/await-idle-merge-queue.sh'
assert_workflow 'the wait is given this push and nothing to tune' "$workflow" "$waiter.steps[-1].env | keys | sort | join(\" \")" \
  'GH_TOKEN IDLE_BRANCH IDLE_REPOSITORY IDLE_SHA'
assert_workflow 'the wait is told this push'"'"'s commit' "$workflow" "$waiter.steps[-1].env.IDLE_SHA" "\${{ github.sha }}"
assert_workflow 'the wait reads the default branch'"'"'s queue' "$workflow" "$waiter.steps[-1].env.IDLE_BRANCH" 'main'
assert_workflow 'the wait cannot be skipped over' "$workflow" "$waiter.steps[-1] | has(\"continue-on-error\")" 'false'
assert_workflow 'the wait outlasts its own hour' "$workflow" "$waiter.timeout-minutes" '75'
assert_workflow 'the wait exports its answer' "$workflow" "$waiter.outputs.observe" "\${{ steps.idle.outputs.observe }}"
assert_workflow 'the observer waits for it' "$workflow" "$observer.needs | join(\" \")" 'await-idle-merge-queue'
assert_workflow 'the observer runs only on an explicit yes' "$workflow" "$observer.if" \
  "github.event_name == 'push' && github.ref == 'refs/heads/main' && needs.await-idle-merge-queue.outputs.observe == 'true'"
assert_workflow 'the observer still takes the production lock' "$workflow" "$observer.concurrency.group" 'prod-deploy'
# The script finds the two jobs by the names GitHub records them under.
assert_workflow 'the wait job has the name the script looks for' "$workflow" "$waiter.name" "$wait_job"
assert_workflow 'the queue deploy job has the name the script looks for' "$ci_workflow" '.jobs.deploy-prod.name' "$deploy_job"
assert_equal 'the script names the deploy job' 1 "$(grep -cFx -- "deploy_job='${deploy_job}'" "$script")"
assert_equal 'the script names the wait job' 1 "$(grep -cFx -- "wait_job='${wait_job}'" "$script")"

printf '%s assertions, %s failed\n' "$assertions" "$failures"
[ "$failures" = 0 ]
