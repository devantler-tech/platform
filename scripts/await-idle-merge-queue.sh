#!/usr/bin/env bash
#
# Decides, without the production lock, when the post-merge production observer runs, so the
# observer does not hold that lock in front of a waiting deploy (#6205).
#
# The observer reads whether production runs what main says, and redeploys main when it is
# behind. It takes the prod-deploy lock so that it reads only after a deploy or heal already
# in flight has finished. A job takes its lock before it gets a runner, and on every push the
# observer used to take it at once, ahead of the next queued change's deploy. On a busy day
# that deploy then waited for the observer's runner as well as its own: 9 of 14 minutes at
# the median on 2026-11-13.
#
# Production falls behind main in two ways, and each belongs to one push:
#
#   - A push that did not come through the merge queue was never deployed.
#   - A queued change deployed, and before it merged a recovery deploy put an older main
#     back. Production is behind from the moment that change merges.
#
# A queued change that merged without deploying (documentation, say) published nothing, so
# its push is neither. The answer therefore depends on which push this is:
#
#   not from the queue    Observe at once, as before this script existed.
#   merged, not deployed  Observe when nothing is queued, which costs no deploy anything.
#                         Otherwise stand down: this push left production as it was.
#   merged and deployed   Wait here, outside the lock, while a change is queued: that
#                         change's deploy brings production up to date, and if its deploy
#                         fails, its heal restores main. The wait ends when
#                           - the tip is a later change that deployed: stand down. That
#                             deploy carried this push to production, and that change's own
#                             run watches from there.
#                           - nothing is queued: observe.
#                           - an hour has passed: observe.
#
# The hour is this run's own and no later merge restarts it, so a queue kept busy by changes
# that do not deploy cannot postpone the observation for longer. No answer relies on another
# run finishing its work: a run stands down only for a deploy that has already succeeded, or
# because its own push published nothing.
#
# It answers `observe=true` whenever it cannot tell: an unreadable queue, run list or check
# run, or a read that does not finish. That is the behaviour before this script existed: the
# observer takes the lock and the lock decides the order.
#
# Writes `observe=true` or `observe=false` to $GITHUB_OUTPUT.
#
#   exit 0  the answer was written
#   exit 1  an input is missing or malformed, or the answer could not be written; the
#           observer then does not run, and this job fails visibly

set -uo pipefail

repository="${IDLE_REPOSITORY:?IDLE_REPOSITORY is required}"
branch="${IDLE_BRANCH:?IDLE_BRANCH is required}"
subject="${IDLE_SHA:?IDLE_SHA is required}"
output="${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}"

# The workflow a queued change runs, and the name GitHub records its deploy job under.
# scripts/tests/test-await-idle-merge-queue.sh fails when the workflow names either otherwise.
queue_workflow='ci.yaml'
deploy_job='🚀 Deploy to Prod'

# Fixed here, not read from the environment: the workflow's job timeout is sized for them.
poll_seconds=30   # pause between two reads of the queue
read_seconds=60   # one read may take this long before it counts as failed
wait_seconds=3600 # this run observes after this long, whatever the queue holds

fail() {
  printf '::error title=Merge-queue wait misconfigured::%s\n' "$1"
  exit 1
}

if [[ ! "$repository" =~ ^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$ ]]; then
  fail "'${repository}' is not an owner/name repository"
fi
owner="${repository%%/*}"
name="${repository#*/}"
[[ "$branch" =~ ^[A-Za-z0-9._/-]+$ ]] || fail "'${branch}' is not a branch name"
[[ "$subject" =~ ^[0-9a-f]{40}$ ]] || fail "'${subject}' is not a full commit id"

# shellcheck disable=SC2016 # GraphQL variables, not shell expansions.
query='query($owner:String!,$name:String!,$branch:String!,$ref:String!){repository(owner:$owner,name:$name){ref(qualifiedName:$ref){target{oid}} mergeQueue(branch:$branch){entries(first:1){totalCount}}}}'

# <observe> <notice|warning> <reason>. A warning marks an answer given without knowing: the
# observer runs as it did before this script, so the wait bought nothing.
answer() {
  printf 'observe=%s\n' "$1" >>"$output" || fail "could not write ${output}"
  printf '::%s title=Production observation::%s\n' "$2" "$3"
  exit 0
}

# <what could not be read>
unknown() {
  answer true warning "$1 could not be read, so the observer takes the lock as before"
}

# Every read is bounded. One that hung would hold this job until its timeout, and the
# observer would then not run at all.
bounded() {
  timeout -k 5 "$read_seconds" "$@"
}

# Sets `tip` and `queued`: the branch's tip commit and how many changes are queued. A branch
# whose queue cannot be found has no count, which is not a state: nothing is known about what
# is queued.
read_queue() {
  local state
  state="$(bounded gh api graphql -f query="$query" -f owner="$owner" -f name="$name" \
    -f branch="$branch" -f ref="refs/heads/${branch}" \
    --jq '.data.repository | "\(.ref.target.oid // "none") \(.mergeQueue.entries.totalCount // "none")"')" ||
    unknown "the merge queue"
  read -r tip queued <<<"$state"
  if [[ ! "$tip" =~ ^[0-9a-f]{40}$ ]] || [[ ! "$queued" =~ ^[0-9]+$ ]]; then
    answer true warning "the merge queue answered '${state}', which is not a state; the observer takes the lock as before"
  fi
}

# <commit>: sets `deploys` to how many successful deploys the commit's queue run made (0 or
# 1). Only the latest attempt of the job counts. A change deploys before it merges, so one
# read of a commit on the branch is final.
read_deploys() {
  deploys="$(bounded gh api --method GET "repos/${repository}/commits/$1/check-runs" \
    -f check_name="$deploy_job" -f filter=latest \
    --jq '.check_runs | if type == "array" then [.[] | select(.status == "completed" and .conclusion == "success")] | length else error("no list of check runs") end')" ||
    unknown "the deploy of ${1:0:12}"
  [[ "$deploys" =~ ^[0-9]+$ ]] || unknown "the deploy of ${1:0:12}"
}

# <commit>: sets `queue_runs` to how many merge-queue runs the commit has. A commit that was
# pushed past the queue has none, whatever its pull request's own runs left on it.
read_queue_runs() {
  queue_runs="$(bounded gh api --method GET "repos/${repository}/actions/workflows/${queue_workflow}/runs" \
    -f head_sha="$1" -f event=merge_group -f per_page=1 \
    --jq 'if (.total_count | type) == "number" then .total_count else error("no run count") end')" ||
    unknown "the merge-queue run of ${1:0:12}"
  [[ "$queue_runs" =~ ^[0-9]+$ ]] || unknown "the merge-queue run of ${1:0:12}"
}

read_deploys "$subject"
if [ "$deploys" -eq 0 ]; then
  read_queue_runs "$subject"
  if [ "$queue_runs" -eq 0 ]; then
    answer true notice "${subject:0:12} did not come through the merge queue, so nothing deployed it"
  fi
  read_queue
  if [ "$queued" -eq 0 ]; then
    answer true notice "no change is waiting in the merge queue"
  fi
  answer false notice "${subject:0:12} merged without deploying, so it left production as it was, and ${queued} change(s) are queued"
fi

polls=0
judged="" # the later tip whose deploy has been read
while :; do
  polls=$((polls + 1))
  read_queue
  if [ "$tip" != "$subject" ] && [ "$tip" != "$judged" ]; then
    judged="$tip"
    read_deploys "$tip"
    if [ "$deploys" -gt 0 ]; then
      answer false notice "${branch} has moved on from ${subject:0:12} to ${tip:0:12}, which deployed before it merged; its own run watches from there"
    fi
  fi
  if [ "$queued" -eq 0 ]; then
    answer true notice "no change is waiting in the merge queue after ${polls} read(s)"
  fi
  if [ "$SECONDS" -ge "$wait_seconds" ]; then
    answer true warning "an hour passed with ${queued} change(s) still queued; observing anyway"
  fi
  sleep "$poll_seconds"
done
