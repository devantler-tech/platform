#!/usr/bin/env bash
#
# Reports whether a DIVERGED production can only be a stranded artifact, so main must be
# redeployed (#4186).
#
# DIVERGED means `:latest` names a commit that main does not contain. Two things produce it:
#
#   - a queued merge group that has deployed but not merged yet; redeploying main would roll
#     that group back, so this must never redeploy
#   - an artifact that will never reach main: a heal that was overtaken, a queue that stayed
#     busy past the eviction poll, a cancelled heal, or a group amended to drop its manifest
#     change and re-enqueued. Nothing else replaces it until an unrelated deploy happens
#
# Only the first needs a merge group. So once the merge queue for main is empty AND no
# merge-group run of ci.yaml is unfinished, the divergence is stranded. The caller holds the
# prod-deploy lock and runs this BEFORE it reads main and `:latest`: with the lock held no
# deploy can promote `:latest`, and a group can only merge from the queue this read found
# empty, so the report read afterwards cannot see a group this gate missed.
#
# Writes `stranded=true` or `stranded=false` to $GITHUB_OUTPUT.
#
#   exit 0  the answer was written
#   exit 1  the queue or the run list could not be read; nothing is written, so nothing is
#           redeployed on a guess and the job fails visibly instead

set -uo pipefail

repository="${STRANDED_REPOSITORY:?STRANDED_REPOSITORY is required}"
base="${STRANDED_BASE:-main}"
output="${GITHUB_OUTPUT:-/dev/null}"

fail() {
  printf '::error title=Stranded-production gate unknown::%s\n' "$1"
  exit 1
}

[[ "$repository" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || fail "STRANDED_REPOSITORY '${repository}' is not <owner>/<name>"
owner="${repository%%/*}"
name="${repository#*/}"

# shellcheck disable=SC2016  # $owner, $name, $base are GraphQL variables; the shell must not expand them.
query='query($owner:String!,$name:String!,$base:String!){repository(owner:$owner,name:$name){mergeQueue(branch:$base){entries(first:1){totalCount}}}}'

queued="$(gh api graphql -f query="$query" -f owner="$owner" -f name="$name" -f base="$base" \
  --jq '.data.repository.mergeQueue.entries.totalCount // "none"')" ||
  fail "could not read the merge queue for ${base}"
[[ "$queued" =~ ^[0-9]+$ ]] || fail "unexpected merge queue size '${queued}' for ${base}"

in_flight="$(gh run list --repo "$repository" --workflow ci.yaml --event merge_group --limit 100 \
  --json status --jq '[.[] | select(.status != "completed")] | length')" ||
  fail "could not list merge-group runs of ci.yaml"
[[ "$in_flight" =~ ^[0-9]+$ ]] || fail "unexpected merge-group run count '${in_flight}'"

if [ "$queued" -eq 0 ] && [ "$in_flight" -eq 0 ]; then
  printf 'stranded=true\n' >>"$output" || fail "could not write ${output}"
  printf '::notice title=Merge queue idle::no queued group and no unfinished merge-group run; a DIVERGED prod is stranded\n'
else
  printf 'stranded=false\n' >>"$output" || fail "could not write ${output}"
  printf '::notice title=Merge queue busy::%s queued, %s merge-group run(s) unfinished; a DIVERGED prod may still be a group that has not merged\n' \
    "$queued" "$in_flight"
fi
