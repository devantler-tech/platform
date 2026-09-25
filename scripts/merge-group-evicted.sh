#!/usr/bin/env bash
#
# Reports whether a merge group's pull request left the merge queue without merging, and
# production must be restored to main because nothing else will replace the deployed
# artifact (#3091).
#
# A merge-group deploy runs on the speculative merge ref, before the PR lands on main.
# When the deploy FAILS, the heal job restores main. A deploy can also SUCCEED after the
# queue has already given up on the PR (the queue timeout, or a manual dequeue), and then
# prod keeps running an artifact that never reaches main. This script tells the heal job
# which of the two happened, so it can restore main in the second case too.
#
# It WAITS for a terminal answer rather than reading once. It runs right after the deploy,
# while `CI - Required Checks` is still settling, and the queue timeout or a manual dequeue
# can still remove the PR after a single early read. So while this group's own queue entry
# is still queued, it polls until the PR merges or leaves. `CI - Required Checks` does not
# depend on this job, so waiting here never holds up the merge it is waiting for.
#
# "This group's own entry" is told apart from a REPLACEMENT entry by time: the original
# entry was enqueued before this merge group's commit was created, while an entry for the
# same PR enqueued after it is a re-enqueue, and this group has left the queue.
#
# When the PR has left, it defers to any merge group still queued. The queue builds a later
# group only once this one has left, and that group's deploy is often already waiting on
# the prod-deploy lock ahead of the heal. Restoring main then would publish main without
# that group's change over its deployment, and the change would merge undeployed. A queued
# group instead replaces this artifact itself: it merges (prod then matches main), or its
# own failed or evicted deploy is healed. The residual case is a later group that leaves
# the queue before it deploys; prod then keeps this artifact until the next deploy, which
# is where it stood before this check existed.
#
# Writes `evicted=true` or `evicted=false` to $GITHUB_OUTPUT.
#
#   exit 0  the answer was written
#   exit 1  an input, the PR's queue state, or a terminal answer within the polling bound
#           could not be obtained; nothing is written, so the heal does not run on a guess
#           and this job fails visibly instead

set -uo pipefail

repository="${EVICTED_REPOSITORY:?EVICTED_REPOSITORY is required}"
head_ref="${EVICTED_HEAD_REF:?EVICTED_HEAD_REF is required}"
group_created="${EVICTED_GROUP_CREATED_AT:?EVICTED_GROUP_CREATED_AT is required}"
output="${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}"
poll_seconds="${EVICTED_POLL_SECONDS:-30}"
max_polls="${EVICTED_MAX_POLLS:-40}"

fail() {
  printf '::error title=Merge-queue state unknown::%s\n' "$1"
  exit 1
}

# refs/heads/gh-readonly-queue/<base>/pr-<number>-<head sha>
if [[ ! "$head_ref" =~ ^refs/heads/gh-readonly-queue/(.+)/pr-([1-9][0-9]*)-[0-9a-f]{40}$ ]]; then
  fail "'${head_ref}' is not a merge-queue ref"
fi
base="${BASH_REMATCH[1]}"
number="${BASH_REMATCH[2]}"

owner="${repository%%/*}"
name="${repository#*/}"
if [[ ! "$repository" =~ ^[^/]+/[^/]+$ ]]; then
  fail "'${repository}' is not an owner/name repository"
fi

# GraphQL returns UTC timestamps ending in Z. The merge-group commit timestamp may carry a
# +00:00 offset instead; any other offset is refused rather than compared wrongly.
case "$group_created" in
  *+00:00) group_created="${group_created%+00:00}Z" ;;
esac
if [[ ! "$group_created" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]]; then
  fail "merge group creation time '${EVICTED_GROUP_CREATED_AT}' is not a UTC timestamp"
fi
[[ "$poll_seconds" =~ ^[0-9]+$ ]] || fail "EVICTED_POLL_SECONDS '${poll_seconds}' is not a number"
[[ "$max_polls" =~ ^[1-9][0-9]*$ ]] || fail "EVICTED_MAX_POLLS '${max_polls}' is not a positive number"

# shellcheck disable=SC2016 # GraphQL variables, not shell expansions.
query='query($owner:String!,$name:String!,$number:Int!,$base:String!){repository(owner:$owner,name:$name){pullRequest(number:$number){state isInMergeQueue mergeQueueEntry{enqueuedAt}} mergeQueue(branch:$base){entries(first:1){totalCount}}}}'

write_answer() {
  printf 'evicted=%s\n' "$1" >>"$output" || fail "could not write ${output}"
}

polls=0
while :; do
  polls=$((polls + 1))
  state="$(gh api graphql -f query="$query" -f owner="$owner" -f name="$name" -F number="$number" -f base="$base" \
    --jq '.data.repository | "\(.pullRequest.state) \(.pullRequest.isInMergeQueue) \(.pullRequest.mergeQueueEntry.enqueuedAt // "none") \(.mergeQueue.entries.totalCount // "none")"')" ||
    fail "could not read the queue state of #${number}"

  read -r pr_state in_queue enqueued_at queued <<<"$state"
  [[ "$queued" =~ ^[0-9]+$ ]] || fail "unexpected queue state '${state}' for #${number}"

  left=""
  case "$pr_state $in_queue" in
    "MERGED "*)
      write_answer false
      printf '::notice title=Deployed PR merged::#%s merged, so production matches main\n' "$number"
      exit 0
      ;;
    "OPEN true")
      [[ "$enqueued_at" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] ||
        fail "unexpected queue state '${state}' for #${number}"
      # Same-format UTC timestamps compare correctly as strings.
      if [[ "$enqueued_at" > "$group_created" ]]; then
        left="re-enqueued at ${enqueued_at}, after this merge group was created at ${group_created}"
      fi
      ;;
    "OPEN false" | "CLOSED "*) left="${pr_state}, no longer queued" ;;
    *) fail "unexpected queue state '${state}' for #${number}" ;;
  esac

  if [ -n "$left" ]; then
    if [ "$queued" -gt 0 ]; then
      write_answer false
      printf '::notice title=Deployed PR left the merge queue::#%s left the queue (%s), but %s merge group(s) are queued and will replace this deploy; not restoring main\n' \
        "$number" "$left" "$queued"
    else
      write_answer true
      printf '::warning title=Deployed PR left the merge queue::#%s deployed successfully but left the queue (%s) and nothing is queued to replace it; restoring main\n' \
        "$number" "$left"
    fi
    exit 0
  fi

  [ "$polls" -lt "$max_polls" ] ||
    fail "#${number} was still queued after ${max_polls} reads; its outcome is unknown"
  sleep "$poll_seconds"
done
