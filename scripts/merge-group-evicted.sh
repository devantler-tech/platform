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
# merge, so a PR that is still queued or already merged is on its way to main.
#
# It does not detect a queued PR whose merge group was REBUILT after an earlier entry left.
# That cannot happen while the queue builds one group at a time (`max_entries_to_build: 1`
# in the `Require merge queue` ruleset): a later group is only built once the earlier one
# has merged or left. The queue entry exposes only the PR head, not the generated merge-group
# commit, so comparing it with the deployed commit would report every normal deploy evicted.
#
# Writes `evicted=true` or `evicted=false` to $GITHUB_OUTPUT.
#
#   exit 0  the answer was written
#   exit 1  the ref, the PR number, or the PR's queue state could not be read; nothing is
#           written, so the heal does not run on a guess and this job fails visibly instead

set -uo pipefail

repository="${EVICTED_REPOSITORY:?EVICTED_REPOSITORY is required}"
head_ref="${EVICTED_HEAD_REF:?EVICTED_HEAD_REF is required}"
output="${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}"

fail() {
  printf '::error title=Merge-queue state unknown::%s\n' "$1"
  exit 1
}

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
query='query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){state isInMergeQueue}}}'
state="$(gh api graphql -f query="$query" -f owner="$owner" -f name="$name" -F number="$number" \
  --jq '.data.repository.pullRequest | "\(.state) \(.isInMergeQueue)"')" ||
  fail "could not read the queue state of #${number}"

case "$state" in
  "MERGED "*) evicted=false ;;
  "OPEN true") evicted=false ;;
  "OPEN false" | "CLOSED "*) evicted=true ;;
  *) fail "unexpected queue state '${state}' for #${number}" ;;
esac

printf 'evicted=%s\n' "$evicted" >>"$output" || fail "could not write ${output}"
if [ "$evicted" = true ]; then
  printf '::warning title=Deployed PR left the merge queue::#%s deployed successfully but is no longer queued (%s); restoring main\n' \
    "$number" "$state"
else
  printf '::notice title=Deployed PR still on its way to main::#%s is %s\n' "$number" "$state"
fi
