#!/usr/bin/env bash
# Read only two fixed policy manifests after binding a trusted same-repo PR head.
# Candidate scripts, workflows and checkout code are never executed.
set -euo pipefail
umask 077
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
[[ "${CANDIDATE_PR:-}" =~ ^[1-9][0-9]*$ && "${CANDIDATE_HEAD:-}" =~ ^[0-9a-f]{40}$ ]] || fail 'candidate PR and exact SHA required'
[[ "${RUNNER_TEMP:-}" == /* && -d "$RUNNER_TEMP" ]] || fail 'absolute runner scratch directory required'
readonly candidate="${RUNNER_TEMP}/cilium-deny-candidate"
[[ ! -e "$candidate" ]] || fail 'candidate directory already exists'
pr="$(gh api "repos/devantler-tech/platform/pulls/${CANDIDATE_PR}")" || fail 'cannot verify candidate PR'
verify_pr() {
  # Exact login match; reviewer identities and branch names cannot extend trust.
  jq -e --arg head "$CANDIDATE_HEAD" '.state == "open" and .head.sha == $head and .head.repo.full_name == "devantler-tech/platform" and (.user.login as $author | ["devantler","ksail-bot","dependabot[bot]","github-actions[bot]","renovate[bot]"] | index($author) != null)' <<<"$1" >/dev/null
}
verify_pr "$pr" || fail 'candidate is not an open, exact-head, trusted same-repo PR'
mkdir "$candidate"
fetch_file() {
  local path="$1" output="$2" response
  response="$(gh api "repos/devantler-tech/platform/contents/${path}?ref=${CANDIDATE_HEAD}")" || fail 'fixed candidate manifest read failed'
  jq -e '.type == "file" and .encoding == "base64" and (.content | type == "string")' <<<"$response" >/dev/null || fail 'candidate path is not a normal file'
  jq -r '.content' <<<"$response" | base64 --decode >"${candidate}/${output}"
  [[ -s "${candidate}/${output}" && "$(wc -c <"${candidate}/${output}")" -le 65536 ]] || fail 'empty or oversized candidate manifest'
}
fetch_file k8s/bases/infrastructure/cluster-policies/best-practices/add-default-deny.yaml generator.yaml
fetch_file k8s/bases/infrastructure/controllers/oauth2-proxy/cilium-network-policy-default-deny.yaml flux-copy.yaml
rebound="$(gh api "repos/devantler-tech/platform/pulls/${CANDIDATE_PR}")" || fail 'candidate reread failed'
verify_pr "$rebound" || fail 'candidate head moved during reads'
generator_digest="$(sha256sum "${candidate}/generator.yaml" | cut -d' ' -f1)"
flux_digest="$(sha256sum "${candidate}/flux-copy.yaml" | cut -d' ' -f1)"
jq -n --argjson pr "$CANDIDATE_PR" --arg head "$CANDIDATE_HEAD" --arg author "$(jq -r '.user.login' <<<"$pr")" \
  --arg generator "$generator_digest" --arg flux "$flux_digest" \
  '{schema:1,repository:"devantler-tech/platform",pr:$pr,head:$head,author:$author,generator_sha256:$generator,flux_copy_sha256:$flux}' >"${candidate}/candidate.json"
printf 'Verified fixed candidate policy data at %s\n' "$CANDIDATE_HEAD"
