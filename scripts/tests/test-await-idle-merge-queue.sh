#!/usr/bin/env bash
#
# Pins scripts/await-idle-merge-queue.sh against stub `gh`, `timeout` and `sleep` commands.
#
# The stub `gh` answers each queue read with the next GraphQL response of a scripted
# sequence, and each read about a commit (its deploy check runs, its merge-queue runs) from a
# file named after the read and the commit. Every answer is filtered by the script's own
# --jq program, so the programs are exercised as well as the loop. The stub refuses a job or
# workflow it does not know, and fails once a case reads more than 50 times, so a loop that
# never answers fails its case instead of hanging the suite.
#
# `state <tip> <queued>` builds a queue response and `runs <status> <conclusion>` a
# check-run response. `given <deploy|queue-runs> <commit> <response>...` scripts what the
# reads about that commit return, in order; the last response repeats. A commit that was
# given nothing has no deploy check run and no merge-queue run. The words FAILED and EMPTY
# stand for a read that fails and one that succeeds without printing anything.
#
# Unless a case says otherwise, this push is a queued change that deployed.

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
queue_workflow='ci.yaml'

mkdir -p "$scratch/bin" "$scratch/reads"
cat >"$scratch/bin/gh" <<'EOF'
#!/usr/bin/env bash
printf 'gh %s\n' "$*" >>"$STUB_LOG"
total=$(($(cat "$STUB_DIR/total") + 1))
printf '%s\n' "$total" >"$STUB_DIR/total"
[ "$total" -le 50 ] || exit 95
[ "${1-}" = api ] || exit 99
shift
filter="" check="" commit="" path="" kind=""
case "${1-}" in
graphql) kind=queue ;;
--method)
  [ "${2-}" = GET ] || exit 94
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
  head_sha=*) commit="${1#head_sha=}" ;;
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

case "$path" in
repos/devantler-tech/platform/commits/*/check-runs)
  [ "$check" = "$STUB_DEPLOY_JOB" ] || exit 97
  commit="${path#repos/devantler-tech/platform/commits/}"
  commit="${commit%/check-runs}"
  read=deploy
  missing='{"check_runs":[]}'
  ;;
"repos/devantler-tech/platform/actions/workflows/${STUB_QUEUE_WORKFLOW}/runs")
  [ -n "$commit" ] || exit 96
  read=queue-runs
  missing='{"total_count":0}'
  ;;
*) exit 97 ;;
esac
responses="$STUB_DIR/reads/${read}.${commit}"
calls=$(($(cat "$responses.calls" 2>/dev/null || printf 0) + 1))
printf '%s\n' "$calls" >"$responses.calls"
if [ ! -f "$responses" ]; then
  line="$missing"
else
  line="$(sed -n "${calls}p" "$responses")"
  [ -n "$line" ] || line="$(tail -n 1 "$responses")"
fi
[ "$line" != FAILED ] || exit 1
[ "$line" != EMPTY ] || exit 0
printf '%s\n' "$line" | jq -r "$filter"
EOF
# `timeout -k <grace> <limit> <command>...`
cat >"$scratch/bin/timeout" <<'EOF'
#!/usr/bin/env bash
printf 'timeout %s %s %s\n' "$1" "$2" "$3" >>"$STUB_LOG"
shift 3
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
  local read="$1" commit="$2"
  shift 2
  printf '%s\n' "$@" >"$scratch/reads/${read}.${commit}"
}

# What this push is, for the next `run_case`.
merged_without_deploying() {
  given deploy "$subject" "$(runs completed skipped)"
  given queue-runs "$subject" '{"total_count":1}'
}
pushed_past_the_queue() {
  # Its pull request's own run can leave a skipped deploy check run on the commit.
  given deploy "$subject" "$(runs completed skipped)"
  given queue-runs "$subject" '{"total_count":0}'
}

# How often the last case read <deploy|queue-runs> of <commit>.
reads_of() {
  cat "$scratch/reads/$1.$2.calls" 2>/dev/null || printf 0
}

# <label> <expected-rc> <expected-output> <expected-queue-reads|-> <queue response>...
# Uses what `given` scripted since the previous case, and leaves the read counts behind.
run_case() {
  local label="$1" want_rc="$2" want_output="$3" want_reads="$4"
  shift 4
  local out rc got_output reads
  : >"$scratch/log"
  : >"$scratch/output"
  rm -f "$scratch"/reads/*.calls
  [ -f "$scratch/reads/deploy.$subject" ] || given deploy "$subject" "$(runs completed success)"
  printf '0\n' >"$scratch/total"
  printf '0\n' >"$scratch/queue.calls"
  printf '%s\n' "$@" >"$scratch/queue"
  if out="$(env PATH="$scratch/bin:$PATH" STUB_LOG="$scratch/log" STUB_DIR="$scratch" \
    STUB_QUEUE_RC="${CASE_QUEUE_RC-0}" STUB_DEPLOY_JOB="$deploy_job" STUB_QUEUE_WORKFLOW="$queue_workflow" \
    IDLE_REPOSITORY="${CASE_REPOSITORY-devantler-tech/platform}" IDLE_BRANCH="${CASE_BRANCH-main}" \
    IDLE_SHA="${CASE_SHA-$subject}" GITHUB_OUTPUT="${CASE_OUTPUT-$scratch/output}" \
    "${CASE_RUNNER-$script}" 2>&1)"; then rc=0; else rc=$?; fi
  printf '%s\n' "$out" >"$scratch/stdout"
  got_output="$(cat "$scratch/output")"
  reads="$(cat "$scratch/queue.calls")"
  find "$scratch/reads" -type f ! -name '*.calls' -delete

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

printf 'await-idle-merge-queue.sh: a change that deployed, and is still the tip\n'

run_case 'an empty queue observes at once' 0 'observe=true' 1 "$(state "$subject" 0)"
assert_stdout 'and says why, as a notice' '::notice title=Production observation::no change is waiting in the merge queue after 1 read(s)'
assert_equal 'having asked only whether it deployed' 0 "$(reads_of queue-runs "$subject")"

run_case 'a busy queue is waited out, then observed' 0 'observe=true' 4 \
  "$(state "$subject" 3)" "$(state "$subject" 2)" "$(state "$subject" 1)" "$(state "$subject" 0)"
assert_equal 'pausing 30 seconds between reads' 'sleep 30 sleep 30 sleep 30' "$(grep '^sleep ' "$scratch/log" | tr '\n' ' ' | sed 's/ $//')"
assert_equal 'and giving every read 60 seconds, then 5 to stop' 5 "$(grep -c '^timeout -k 5 60$' "$scratch/log")"
assert_equal 'with no read left unbounded' "$(grep -c '^gh ' "$scratch/log")" "$(grep -c '^timeout ' "$scratch/log")"

printf 'await-idle-merge-queue.sh: a change that deployed, and a later change merged\n'

# The later change's deploy carried this push to production.
given deploy "$newer" "$(runs completed success)"
run_case 'a later change that deployed ends the wait' 0 'observe=false' 3 \
  "$(state "$subject" 2)" "$(state "$subject" 2)" "$(state "$newer" 1)"
assert_stdout 'and names both commits' "main has moved on from ${subject:0:12} to ${newer:0:12}, which deployed"

given deploy "$newer" "$(runs completed success)"
run_case 'a later change that deployed ends the wait with an empty queue too' 0 'observe=false' 1 "$(state "$newer" 0)"

# A later change that did not deploy changed nothing in production: the wait goes on, and
# this run observes once nothing is queued. It hands the observation to no other run.
given deploy "$newer" "$(runs completed skipped)"
run_case 'a later change that did not deploy does not end the wait' 0 'observe=true' 4 \
  "$(state "$newer" 2)" "$(state "$newer" 2)" "$(state "$newer" 1)" "$(state "$newer" 0)"
assert_stdout 'and this run observes once nothing is queued' 'no change is waiting in the merge queue after 4 read(s)'
assert_equal 'its deploy is read once, not on every read' 1 "$(reads_of deploy "$newer")"
assert_equal 'and nothing asks how it reached the branch' 0 "$(reads_of queue-runs "$newer")"

for conclusion in failure cancelled timed_out skipped neutral; do
  given deploy "$newer" "$(runs completed "$conclusion")"
  run_case "a later deploy that ended ${conclusion} is not a deploy" 0 'observe=true' 2 "$(state "$newer" 1)" "$(state "$newer" 0)"
done
given deploy "$newer" "$(runs in_progress null)"
run_case 'a later deploy that is still running is not a deploy' 0 'observe=true' 2 "$(state "$newer" 1)" "$(state "$newer" 0)"
run_case 'a later push with no deploy at all is not a deploy' 0 'observe=true' 2 "$(state "$newer" 1)" "$(state "$newer" 0)"

# Each new tip is judged once: the first did not deploy, the second did.
given deploy "$newer" "$(runs completed skipped)"
given deploy "$newest" "$(runs completed success)"
run_case 'a second later change that deployed ends the wait' 0 'observe=false' 3 \
  "$(state "$newer" 2)" "$(state "$newer" 2)" "$(state "$newest" 1)"
assert_stdout 'and is the one named' "to ${newest:0:12}, which deployed"
assert_equal 'after one read of each' '1 1' "$(reads_of deploy "$newer") $(reads_of deploy "$newest")"

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
assert_stdout 'and the bound is what ended it' 'an hour passed with 2 change(s) still queued'
almost="$scratch/almost"
printf '#!/usr/bin/env bash\nSECONDS=3000\n. "%s"\n' "$script" >"$almost"
chmod +x "$almost"
CASE_RUNNER="$almost" run_case 'fifty minutes are not the bound' 0 'observe=true' 3 \
  "$(state "$subject" 2)" "$(state "$subject" 2)" "$(state "$subject" 0)"
assert_stdout 'so the queue is still waited out' 'no change is waiting in the merge queue after 3 read(s)'

printf 'await-idle-merge-queue.sh: a change that merged without deploying\n'

# It published nothing, so it does not wait: one read of the queue settles it.
merged_without_deploying
run_case 'a busy queue stands it down at once' 0 'observe=false' 1 "$(state "$subject" 3)"
assert_stdout 'and says why' "${subject:0:12} merged without deploying, so it left production as it was, and 3 change(s) are queued"
assert_equal 'without a pause' 0 "$(grep -c '^sleep ' "$scratch/log")"
merged_without_deploying
run_case 'an empty queue observes at once' 0 'observe=true' 1 "$(state "$subject" 0)"
assert_stdout 'and says why' '::notice title=Production observation::no change is waiting in the merge queue'
# The queue run counts, not the deploy check run: a queued change with no check run of the
# deploy job at all still came through the queue.
given queue-runs "$subject" '{"total_count":1}'
given deploy "$subject" '{"check_runs":[]}'
run_case 'a queued change with no deploy check run stands down the same way' 0 'observe=false' 1 "$(state "$subject" 3)"
merged_without_deploying
given deploy "$newer" "$(runs completed success)"
run_case 'what merged after it is not looked at' 0 'observe=false' 1 "$(state "$newer" 3)"
assert_equal 'so the later deploy is not read' 0 "$(reads_of deploy "$newer")"

printf 'await-idle-merge-queue.sh: a push that did not come through the merge queue\n'

pushed_past_the_queue
run_case 'it observes at once, whatever is queued' 0 'observe=true' 0 "$(state "$subject" 5)"
assert_stdout 'and says why' "::notice title=Production observation::${subject:0:12} did not come through the merge queue, so nothing deployed it"
given deploy "$subject" '{"check_runs":[]}'
run_case 'a commit with no check run of the deploy job observes at once too' 0 'observe=true' 0 "$(state "$subject" 5)"

printf 'await-idle-merge-queue.sh: what it cannot read\n'

# Every state the script cannot read falls back to observing, which is what happened
# before it existed. None of them may stand the observer down.
for response in FAILED EMPTY 'not json' '{"message":"Not Found"}' '{"check_runs":{}}' '{"check_runs":null}'; do
  given deploy "$subject" "$response"
  run_case "this push's deploy answering <${response}> observes" 0 'observe=true' 0 "$(state "$subject" 3)"
  assert_stdout "and says which read failed (${response})" "::warning title=Production observation::the deploy of ${subject:0:12} could not be read"
done
for response in FAILED EMPTY 'not json' '{"message":"Not Found"}' '{"total_count":"many"}' '{"total_count":null}' '{"total_count":-1}'; do
  given deploy "$subject" "$(runs completed skipped)"
  given queue-runs "$subject" "$response"
  run_case "this push's queue run answering <${response}> observes" 0 'observe=true' 0 "$(state "$subject" 3)"
  assert_stdout "and says which read failed (${response})" "::warning title=Production observation::the merge-queue run of ${subject:0:12} could not be read"
done
for response in FAILED EMPTY 'not json' '{"check_runs":{}}'; do
  given deploy "$newer" "$response"
  run_case "a later deploy answering <${response}> observes" 0 'observe=true' 1 "$(state "$newer" 2)"
  assert_stdout "and says which read failed (${response})" "::warning title=Production observation::the deploy of ${newer:0:12} could not be read"
done

CASE_QUEUE_RC=1 run_case 'a failed queue read observes' 0 'observe=true' - "$(state "$subject" 2)"
assert_stdout 'and marks the fallback as a warning' '::warning title=Production observation::the merge queue could not be read'
CASE_QUEUE_RC=124 run_case 'a queue read that ran out of time observes' 0 'observe=true' - "$(state "$subject" 2)"
merged_without_deploying
CASE_QUEUE_RC=1 run_case 'a failed queue read observes for a change that did not deploy too' 0 'observe=true' - "$(state "$subject" 2)"
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
merged_without_deploying
run_case 'a missing queue does not stand a change that did not deploy down' 0 'observe=true' 1 \
  "{\"data\":{\"repository\":{\"ref\":{\"target\":{\"oid\":\"$subject\"}},\"mergeQueue\":null}}}"

printf 'await-idle-merge-queue.sh: what it reads\n'

# The reads name this repository, its branch, the queue's workflow and its deploy job, and
# the script never writes.
given deploy "$newer" "$(runs completed skipped)"
run_case 'the reads of a change that deployed are scoped' 0 'observe=true' 1 "$(state "$newer" 0)"
# shellcheck disable=SC2016 # The GraphQL variables are matched literally.
assert_equal 'one query for the branch and its queue' 1 \
  "$(grep -cF -- 'gh api graphql -f query=query($owner:String!,$name:String!,$branch:String!,$ref:String!)' "$scratch/log")"
assert_equal 'with this repository and branch' 1 \
  "$(grep -cF -- '-f owner=devantler-tech -f name=platform -f branch=main -f ref=refs/heads/main' "$scratch/log")"
assert_equal 'one read of its own deploy' 1 \
  "$(grep -cF -- "gh api --method GET repos/devantler-tech/platform/commits/${subject}/check-runs -f check_name=${deploy_job} -f filter=latest" "$scratch/log")"
assert_equal 'one read of the later change'"'"'s deploy' 1 \
  "$(grep -cF -- "gh api --method GET repos/devantler-tech/platform/commits/${newer}/check-runs -f check_name=${deploy_job} -f filter=latest" "$scratch/log")"
assert_equal 'and nothing else' 3 "$(grep -c '^gh ' "$scratch/log")"
assert_equal 'each given 60 seconds' 3 "$(grep -c '^timeout -k 5 60$' "$scratch/log")"
assert_equal 'none of them a write' 0 "$(grep -cE 'mutation|--method (POST|PUT|PATCH|DELETE)|-X ' "$scratch/log")"

merged_without_deploying
run_case 'the reads of a change that did not deploy are scoped' 0 'observe=false' 1 "$(state "$subject" 3)"
assert_equal 'one read of how it reached the branch' 1 \
  "$(grep -cF -- "gh api --method GET repos/devantler-tech/platform/actions/workflows/${queue_workflow}/runs -f head_sha=${subject} -f event=merge_group -f per_page=1" "$scratch/log")"
assert_equal 'and nothing but its deploy and the queue besides' 3 "$(grep -c '^gh ' "$scratch/log")"
assert_equal 'each given 60 seconds' 3 "$(grep -c '^timeout -k 5 60$' "$scratch/log")"
assert_equal 'none of them a write' 0 "$(grep -cE 'mutation|--method (POST|PUT|PATCH|DELETE)|-X ' "$scratch/log")"

printf 'await-idle-merge-queue.sh: misconfiguration\n'

# Misconfiguration writes no answer and fails, so the observer does not run on a guess.
CASE_REPOSITORY='platform' run_case 'a repository without an owner is refused' 1 '' 0 "$(state "$subject" 0)"
CASE_REPOSITORY='devantler-tech/platform --jq .' run_case 'a repository with an option is refused' 1 '' 0 "$(state "$subject" 0)"
CASE_REPOSITORY='' run_case 'a missing repository is refused' 1 '' 0 "$(state "$subject" 0)"
CASE_BRANCH='main; rm -rf /' run_case 'a branch with shell syntax is refused' 1 '' 0 "$(state "$subject" 0)"
CASE_SHA='main' run_case 'a branch name in place of the commit is refused' 1 '' 0 "$(state "$subject" 0)"
CASE_SHA="${subject:0:12}" run_case 'an abbreviated commit is refused' 1 '' 0 "$(state "$subject" 0)"
assert_equal 'before anything is read' 0 "$(grep -c '^gh ' "$scratch/log")"
CASE_OUTPUT="$scratch/missing/output" run_case 'an unwritable answer fails' 1 '' 1 "$(state "$subject" 0)" 2>/dev/null

# The pauses and bounds are the script's own: it reads these variables and no others.
# shellcheck disable=SC2016 # The dollar sign is matched literally.
assert_equal 'nothing else in the environment changes the script' 'GITHUB_OUTPUT IDLE_BRANCH IDLE_REPOSITORY IDLE_SHA SECONDS' \
  "$(grep -v '^[[:space:]]*#' "$script" | grep -oE '\$\{?[A-Z_][A-Z0-9_]*' | tr -d '${' | sort -u | tr '\n' ' ' | sed 's/ $//')"

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
assert_workflow 'the wait can only read' "$workflow" "$waiter.permissions | to_entries | map(.key + \"=\" + .value) | sort | join(\" \")" \
  'actions=read checks=read contents=read'
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
# The script finds a queued change's run and its deploy by the names GitHub records.
assert_workflow 'the queue'"'"'s workflow runs on the merge queue' "$ci_workflow" '.on | has("merge_group")' 'true'
assert_workflow 'its deploy job has the name the script looks for' "$ci_workflow" '.jobs.deploy-prod.name' "$deploy_job"
assert_equal 'the script names that job' 1 "$(grep -cFx -- "deploy_job='${deploy_job}'" "$script")"
assert_equal 'the script names that workflow' 1 "$(grep -cFx -- "queue_workflow='${queue_workflow}'" "$script")"
assert_equal 'and it is the file this test read' "$queue_workflow" "$(basename "$ci_workflow")"

printf '%s assertions, %s failed\n' "$assertions" "$failures"
[ "$failures" = 0 ]
