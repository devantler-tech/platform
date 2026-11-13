#!/usr/bin/env bash
#
# Decides, without the production lock, when the post-merge production observer runs, so the
# observer never holds that lock in front of a waiting deploy (#6205).
#
# The observer reads whether production runs what main says, and redeploys main when it is
# behind. It takes the prod-deploy lock so that it reads only after a deploy or heal already
# in flight has finished. A job takes its lock before it gets a runner, and on every push the
# observer used to take it at once, ahead of the next queued change's deploy. On a busy day
# that deploy then waited for the observer's runner as well as its own: 9 of 14 minutes at
# the median on 2026-11-13.
#
# While a change is queued the observation can wait: that change's deploy brings production
# up to date, and if its deploy fails, its heal restores main. So this script waits here,
# outside the lock, until one of these is true:
#
#   observe     Nothing is queued and this push is still the tip of the branch.
#   stand down  The tip is a later change that deployed before it merged. Production is at
#               or past this push, so nothing this run set out to find is left.
#   stand down  Nothing is queued, the tip is a later push, and that push has a wait of its
#               own that is running or has succeeded. That run observes, so only one does.
#   observe     Nothing is queued and the later push has no such wait (its workflow was
#               skipped or is broken): nothing else would observe.
#   observe     An hour has passed.
#
# A later change that merged without deploying (documentation, say) changed nothing in
# production, so it does not end this wait. The hour is this run's own and no later merge
# restarts it: a queue kept busy by such changes cannot postpone the observation for longer.
#
# It answers `observe=true` whenever it cannot tell: an unreadable queue or check run, or a
# read that does not finish. That is the behaviour before this script existed: the observer
# takes the lock and the lock decides the order.
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

# The names GitHub records the queue's deploy job and this wait's own job under.
# scripts/tests/test-await-idle-merge-queue.sh fails when a workflow names either otherwise.
deploy_job='🚀 Deploy to Prod'
wait_job='🚦 Wait for an idle merge queue'

# Fixed here, not read from the environment: the workflow's job timeout is sized for them.
poll_seconds=30    # pause between two reads of the queue
read_seconds=60    # one read may take this long before it counts as failed
wait_seconds=3600  # this run observes after this long, whatever the queue holds
successor_reads=4  # reads a later push gets to list its own wait before it counts as absent

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

# Every read is bounded. One that hung would hold this job until its timeout, and the
# observer would then not run at all.
#
# "<tip commit> <queued changes>". A branch whose queue cannot be found prints no count,
# which is not a state: nothing is known about what is queued.
queue_state() {
  timeout "$read_seconds" gh api graphql -f query="$query" -f owner="$owner" -f name="$name" \
    -f branch="$branch" -f ref="refs/heads/${branch}" \
    --jq '.data.repository | "\(.ref.target.oid // "none") \(.mergeQueue.entries.totalCount // "none")"'
}

# <commit> <job name> <condition on one check run>: how many of the commit's check runs of
# that name meet the condition, counting only the latest attempt of each.
count_check_runs() {
  timeout "$read_seconds" gh api --method GET "repos/${repository}/commits/$1/check-runs" \
    -f check_name="$2" -f filter=latest \
    --jq ".check_runs | if type == \"array\" then [.[] | select($3)] | length else error(\"no list of check runs\") end"
}

polls=0
judged=""  # the later tip whose deploy has been read
absent=0   # reads, with nothing queued, that found no wait on that tip
while :; do
  polls=$((polls + 1))
  if ! state="$(queue_state)"; then
    answer true warning "the merge queue could not be read, so the observer takes the lock as before"
  fi
  read -r tip queued <<<"$state"
  if [[ ! "$tip" =~ ^[0-9a-f]{40}$ ]] || [[ ! "$queued" =~ ^[0-9]+$ ]]; then
    answer true warning "the merge queue answered '${state}', which is not a state; the observer takes the lock as before"
  fi

  if [ "$tip" != "$subject" ]; then
    moved="${branch} has moved on from ${subject:0:12} to ${tip:0:12}"
    if [ "$tip" != "$judged" ]; then
      judged="$tip"
      absent=0
      # A change's deploy finishes before the change merges, so one read per tip settles it.
      if ! deployed="$(count_check_runs "$tip" "$deploy_job" '.status == "completed" and .conclusion == "success"')" ||
        [[ ! "$deployed" =~ ^[0-9]+$ ]]; then
        answer true warning "the deploy of ${tip:0:12} could not be read, so the observer takes the lock as before"
      fi
      if [ "$deployed" -gt 0 ]; then
        answer false notice "${moved}, which deployed before it merged; production is at or past this push"
      fi
    fi
    if [ "$queued" -eq 0 ]; then
      if ! successor="$(count_check_runs "$tip" "$wait_job" '.status != "completed" or .conclusion == "success"')" ||
        [[ ! "$successor" =~ ^[0-9]+$ ]]; then
        answer true warning "the wait of the newer push ${tip:0:12} could not be read, so the observer takes the lock as before"
      fi
      if [ "$successor" -gt 0 ]; then
        answer false notice "no change is queued and ${moved}; that push observes production"
      fi
      absent=$((absent + 1))
      if [ "$absent" -ge "$successor_reads" ]; then
        answer true notice "no change is queued and the newer push ${tip:0:12} has no wait of its own after ${absent} reads; observing"
      fi
    fi
  elif [ "$queued" -eq 0 ]; then
    answer true notice "no change is waiting in the merge queue after ${polls} read(s)"
  fi

  if [ "$SECONDS" -ge "$wait_seconds" ]; then
    answer true warning "an hour passed with ${queued} change(s) still queued; observing anyway"
  fi
  sleep "$poll_seconds"
done
