#!/usr/bin/env bash
#
# Waits, without the production lock, until no change is waiting in the merge queue, so the
# post-merge production observer never holds that lock in front of a waiting deploy (#6205).
#
# The observer reads whether production runs what main says, and redeploys main when it is
# behind. It takes the prod-deploy lock so that it reads only after a deploy or heal already
# in flight has finished. A job takes its lock before it gets a runner, and on every push the
# observer used to take it at once, ahead of the next queued change's deploy. On a busy day
# that deploy then waited for the observer's runner as well as its own: 9 of 14 minutes at
# the median on 2026-11-13.
#
# While a change is queued the observation can wait. That change's own deploy brings
# production up to date, and if its deploy fails, its heal restores main. A queued change may
# also leave without deploying (a documentation change, or one rejected before its deploy),
# so this script does not skip the observation: it waits here, outside the lock, until the
# queue is empty, and only then lets the observer take the lock.
#
# Every merge is a push with a run of its own. Once main has moved on from this run's commit,
# the newer run's observer takes over and this one stands down, so one waiter is alive at a
# time, not one per merge.
#
# It answers `observe=true` whenever it cannot tell: an unreadable queue, or a queue that
# never drains within the polling bound. That is the behaviour before this script existed:
# the observer takes the lock and the lock decides the order.
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
poll_seconds="${IDLE_POLL_SECONDS:-30}"
max_polls="${IDLE_MAX_POLLS:-120}"

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
[[ "$poll_seconds" =~ ^[0-9]+$ ]] || fail "IDLE_POLL_SECONDS '${poll_seconds}' is not a number"
[[ "$max_polls" =~ ^[1-9][0-9]*$ ]] || fail "IDLE_MAX_POLLS '${max_polls}' is not a positive number"

# shellcheck disable=SC2016 # GraphQL variables, not shell expansions.
query='query($owner:String!,$name:String!,$branch:String!,$ref:String!){repository(owner:$owner,name:$name){ref(qualifiedName:$ref){target{oid}} mergeQueue(branch:$branch){entries(first:1){totalCount}}}}'

# <observe> <notice|warning> <reason>. A warning marks an answer given without knowing the
# queue: the observer runs as it did before this script, so the wait bought nothing.
answer() {
  printf 'observe=%s\n' "$1" >>"$output" || fail "could not write ${output}"
  printf '::%s title=Production observation::%s\n' "$2" "$3"
  exit 0
}

polls=0
while :; do
  polls=$((polls + 1))
  # A branch with no merge queue has no entries to wait for, so a missing queue reads as 0.
  if ! state="$(gh api graphql -f query="$query" -f owner="$owner" -f name="$name" -f branch="$branch" \
    -f ref="refs/heads/${branch}" \
    --jq '.data.repository | "\(.ref.target.oid // "none") \(if .mergeQueue == null then 0 else (.mergeQueue.entries.totalCount // "none") end)"')"; then
    answer true warning "the merge queue could not be read, so the observer takes the lock as before"
  fi
  read -r tip queued <<<"$state"
  if [[ ! "$tip" =~ ^[0-9a-f]{40}$ ]] || [[ ! "$queued" =~ ^[0-9]+$ ]]; then
    answer true warning "the merge queue answered '${state}', which is not a state; the observer takes the lock as before"
  fi

  if [ "$tip" != "$subject" ]; then
    answer false notice "${branch} has moved on from ${subject:0:12} to ${tip:0:12}; the newer push observes production"
  fi
  if [ "$queued" -eq 0 ]; then
    answer true notice "no change is waiting in the merge queue after ${polls} read(s)"
  fi
  if [ "$polls" -ge "$max_polls" ]; then
    answer true warning "${queued} change(s) were still queued after ${max_polls} reads and ${branch} had not moved; observing anyway"
  fi
  sleep "$poll_seconds"
done
