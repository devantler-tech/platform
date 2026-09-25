#!/usr/bin/env bash
#
# Reports whether a merge group's pull request left the merge queue without merging (#3091).
#
# A merge-group deploy runs on the speculative merge ref, before the PR lands on main.
# When the deploy FAILS, the heal job restores main. A deploy can also SUCCEED after the
# queue has already given up on the PR (the queue timeout, or a manual dequeue), and then
# prod keeps running an artifact that never reaches main. This script tells the heal job
# which of the two happened, so it can restore main in the second case too.
#
# It runs right after a successful deploy, before `CI - Required Checks` lets the queue
# merge, so a PR that is already merged, or still queued with the deployed commit, is on its
# way to main. A PR still queued under a REBUILT merge group is not: its deployed commit is
# obsolete, so that counts as evicted too.
#
# Writes `evicted=true` or `evicted=false` to $GITHUB_OUTPUT.
#
#   exit 0  the answer was written
#   exit 1  the ref, the PR number, or the PR's queue state could not be read; nothing is
#           written, so the heal does not run on a guess and this job fails visibly instead

set -uo pipefail

repository="${EVICTED_REPOSITORY:?EVICTED_REPOSITORY is required}"
head_ref="${EVICTED_HEAD_REF:?EVICTED_HEAD_REF is required}"
deployed_sha="${EVICTED_DEPLOYED_SHA:?EVICTED_DEPLOYED_SHA is required}"
output="${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}"

fail() {
  printf '::error title=Merge-queue state unknown::%s\n' "$1"
  exit 1
}

if [[ ! "$deployed_sha" =~ ^[0-9a-f]{40}$ ]]; then
  fail "'${deployed_sha}' is not the deployed merge-group commit"
fi

# refs/heads/gh-readonly-queue/<base>/pr-<number>-<head sha>
if [[ ! "$head_ref" =~ ^refs/heads/gh-readonly-queue/.+/pr-([1-9][0-9]*)-[0-9a-f]{40}$ ]]; then
  fail "'${head_ref}' is not a merge-queue ref"
fi
number="${BASH_REMATCH[1]}"

owner="${repository%%/*}"
name="${repository#*/}"
if [[ ! "$repository" =~ ^[^/]+/[^/]+$ ]]; then
  fail "'${repository}' is not an owner/name repository"
fi

# shellcheck disable=SC2016 # GraphQL variables, not shell expansions.
query='query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){state isInMergeQueue mergeQueueEntry{headCommit{oid}}}}}'
state="$(gh api graphql -f query="$query" -f owner="$owner" -f name="$name" -F number="$number" \
  --jq '.data.repository.pullRequest | "\(.state) \(.isInMergeQueue) \(.mergeQueueEntry.headCommit.oid // "none")"')" ||
  fail "could not read the queue state of #${number}"

# A queued PR counts only while its entry still builds the commit that was deployed. When an
# earlier entry leaves the queue, GitHub rebuilds the later groups without it, so a deploy of
# the old group ships an artifact that no longer matches anything headed for main.
read -r pr_state queued entry_sha <<<"$state"
if [ "$pr_state" = MERGED ]; then
  evicted=false
elif [ "$pr_state" = OPEN ] && [ "$queued" = true ] && [ "$entry_sha" = "$deployed_sha" ]; then
  evicted=false
elif [ "$pr_state" = OPEN ] && [ "$queued" = true ] && [[ "$entry_sha" =~ ^[0-9a-f]{40}$ ]]; then
  evicted=true
elif { [ "$pr_state" = OPEN ] && [ "$queued" = false ]; } || [ "$pr_state" = CLOSED ]; then
  evicted=true
else
  fail "unexpected queue state '${state}' for #${number}"
fi

printf 'evicted=%s\n' "$evicted" >>"$output" || fail "could not write ${output}"
if [ "$evicted" = true ]; then
  printf '::warning title=Deployed PR left the merge queue::#%s deployed successfully but that deploy is no longer headed for main (%s); restoring main\n' \
    "$number" "$state"
else
  printf '::notice title=Deployed PR still on its way to main::#%s is %s\n' "$number" "$state"
fi
