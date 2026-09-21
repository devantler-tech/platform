#!/usr/bin/env bash
# Dispatch the approved-revision regeneration as soon as devantler-tech/actions publishes a release
# (#3960).
#
# The approved set carries the latest actions release as each app tenant's release candidate
# (#3983), but the regeneration that records it runs once a day, while a tenant's publish-app pin
# can move within about an hour of a release. Until the next daily run the platform would reject the
# first release that tenant signs with the new revision. This check closes that gap by polling rather
# than being notified: it only READS the actions release, and dispatches this repository's own
# regeneration when the committed candidate is behind. A push from the actions repository would need
# a token there that can write here; a poll adds no such cross-repository write path.
#
# Decision, in order:
#   1. Resolve the latest actions release to one commit that carries publish-app.yaml. Any failed
#      or implausible read exits 2: the answer is unknown, so nothing is dispatched.
#   2. If the committed set has no publish-app row, there is no candidate to keep current: exit 0.
#   3. If every publish-app row already names that commit, the set is current: exit 0.
#   4. If the open regeneration branch already names it for every publish-app row, a pull request
#      carrying it is pending review; dispatching again would only rerun the same regeneration.
#   5. If a regeneration already started after the release was published, it has had its chance;
#      a failed one is not retried every poll (the daily schedule retries it, visibly).
#   6. Otherwise dispatch the regeneration on main (only with WATCH_DISPATCH=1; a dry run prints the
#      decision and changes nothing).
#
# Usage: watch-actions-release-candidate.sh [approved-set.tsv]
# Env:   GITHUB_REPOSITORY (default devantler-tech/platform), WATCH_DISPATCH=1 to dispatch.
# Exit:  0 decided (current, pending or dispatched) · 2 unknown (a read failed or was implausible).
set -euo pipefail

set_file="${1:-scripts/publish-workflow-approved-revisions.tsv}"
repo="${GITHUB_REPOSITORY:-devantler-tech/platform}"
readonly regen_workflow='regenerate-publish-workflow-approved-revisions.yaml'
readonly regen_branch='regenerate-publish-workflow-approved-revisions'
readonly set_path='scripts/publish-workflow-approved-revisions.tsv'

unknown() { printf 'watch-actions-release: UNKNOWN: %s\n' "$1" >&2; exit 2; }
is_sha() { [[ "$1" =~ ^[0-9a-f]{40}$ ]]; }

# The distinct release candidates on publish-app rows, one per line. Fails when the header is not
# the approved-set header, so a reshaped file is never read as "no candidates".
candidates() {
  awk -F '\t' '
    NR == 1 { if ($7 != "release_candidate_sha" || $2 != "workflow") bad = 1; next }
    $2 == "publish-app" { print $7 }
    END { exit bad }' "$1" | LC_ALL=C sort -u
}

[[ -r "$set_file" ]] || unknown "cannot read $set_file"

release="$(gh api repos/devantler-tech/actions/releases/latest --jq '[.tag_name, .published_at] | @tsv')" ||
  unknown 'the latest devantler-tech/actions release could not be read'
tag="${release%%$'\t'*}"
published="${release#*$'\t'}"
[[ "$published" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:]{8}Z$ ]] || unknown "implausible release time '$published'"
[[ "$tag" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || unknown "implausible release tag '$tag'"
sha="$(gh api "repos/devantler-tech/actions/commits/${tag}" --jq .sha)" ||
  unknown "release $tag could not be resolved to a commit"
is_sha "$sha" || unknown "release $tag resolved to '$sha', not one commit"
blob="$(gh api "repos/devantler-tech/actions/contents/.github/workflows/publish-app.yaml?ref=${sha}" --jq .sha)" ||
  unknown "release $tag ($sha) does not carry publish-app.yaml, so the generator would refuse it"
is_sha "$blob" || unknown "release $tag ($sha) publish-app.yaml read returned '$blob'"

committed="$(candidates "$set_file")" || unknown "$set_file does not carry the approved-set header"
if [[ -z "$committed" ]]; then
  echo "watch-actions-release: no publish-app consumer in $set_file; nothing to keep current"
  exit 0
fi
if [[ "$committed" == "$sha" ]]; then
  echo "watch-actions-release: CURRENT: the committed candidate is release $tag ($sha)"
  exit 0
fi

pending_refs="$(gh api "repos/${repo}/git/matching-refs/heads/${regen_branch}" \
  --jq "map(select(.ref == \"refs/heads/${regen_branch}\")) | length")" ||
  unknown "the regeneration branch could not be looked up"
[[ "$pending_refs" =~ ^[01]$ ]] || unknown "regeneration branch lookup returned '$pending_refs'"
if [[ "$pending_refs" == 1 ]]; then
  pending_set="$(mktemp)"
  trap 'rm -f "$pending_set"' EXIT
  gh api "repos/${repo}/contents/${set_path}?ref=${regen_branch}" \
    -H 'Accept: application/vnd.github.raw' >"$pending_set" ||
    unknown 'the pending regeneration set could not be read'
  pending="$(candidates "$pending_set")" || unknown 'the pending regeneration set is malformed'
  if [[ "$pending" == "$sha" ]]; then
    echo "watch-actions-release: PENDING: the open regeneration pull request already carries release $tag ($sha)"
    exit 0
  fi
fi

# A regeneration that already started after the release was published has had its chance: if it
# refused or failed, dispatching it again every poll would only repeat that. The daily schedule and
# its red run are how such a failure is seen and retried.
last_run="$(gh api "repos/${repo}/actions/workflows/${regen_workflow}/runs?per_page=1" \
  --jq '.workflow_runs[0].created_at // "none"')" || unknown 'the latest regeneration run could not be read'
[[ "$last_run" == none || "$last_run" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:]{8}Z$ ]] ||
  unknown "implausible regeneration run time '$last_run'"
if [[ "$last_run" != none && ! "$last_run" < "$published" ]]; then
  echo "watch-actions-release: ALREADY-RAN: a regeneration started at $last_run, after release $tag was published at $published; leaving it to the daily schedule"
  exit 0
fi

echo "watch-actions-release: BEHIND: committed candidate(s) $(printf '%s\n' "$committed" | tr '\n' ' ')differ from release $tag ($sha)"
if [[ "${WATCH_DISPATCH:-}" == 1 ]]; then
  gh workflow run "$regen_workflow" --repo "$repo" --ref main
  echo "watch-actions-release: DISPATCHED $regen_workflow on main"
else
  echo "watch-actions-release: dry run; set WATCH_DISPATCH=1 to dispatch $regen_workflow"
fi
