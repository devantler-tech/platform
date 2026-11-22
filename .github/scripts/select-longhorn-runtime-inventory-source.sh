#!/usr/bin/env bash
# A queued read-only inventory may follow an unrelated main update after its tests.
set -euo pipefail

refuse() {
  printf 'LONGHORN_RUNTIME_SOURCE=HOLD: %s\n' "$1" >&2
  exit 1
}

[[ "${GITHUB_ACTIONS:-}" == true && "${GITHUB_EVENT_NAME:-}" == workflow_dispatch &&
   "${GITHUB_REF:-}" == refs/heads/main && "${GITHUB_REPOSITORY:-}" == devantler-tech/platform &&
   "${GITHUB_RUN_ATTEMPT:-}" == 1 && "${LONGHORN_V2_OPERATION:-}" == inventory-v1-runtime &&
   "${LONGHORN_V2_CONFIRM:-}" == inventory-v1-runtime && -n "${GH_TOKEN:-}" &&
   -n "${GITHUB_WORKSPACE:-}" && "${GITHUB_SHA:-}" =~ ^[0-9a-f]{40}$ ]] ||
  refuse 'the confirmed first-attempt inventory is required'

checkout_git() {
  env -u GIT_DIR -u GIT_WORK_TREE -u GIT_COMMON_DIR \
    git --no-replace-objects -C "$GITHUB_WORKSPACE" "$@"
}

original="$(checkout_git rev-parse --verify 'HEAD^{commit}')" || refuse 'cannot identify the tested checkout'
checkout_status="$(checkout_git status --porcelain=v1 --untracked-files=all)" || refuse 'cannot inspect the tested checkout'
[[ "$original" == "$GITHUB_SHA" && -z "$checkout_status" ]] || refuse 'tested checkout changed'
origin="$(checkout_git config --local --get remote.origin.url)" || refuse 'cannot identify the workflow origin'
[[ "$origin" == https://github.com/devantler-tech/platform || "$origin" == https://github.com/devantler-tech/platform.git ]] ||
  refuse 'unexpected workflow origin'

response="$(timeout 30s gh api --hostname github.com --method GET repos/devantler-tech/platform/git/ref/heads/main)" ||
  refuse 'cannot read current main'
current="$(jq -ser '
  select(length == 1) | .[0] | select(.ref == "refs/heads/main" and .object.type == "commit")
  | .object.sha | select(type == "string" and test("^[0-9a-f]{40}$"))
' <<<"$response")" || refuse 'main ref is unverified'
if [[ "$original" == "$current" ]]; then
  printf '%s\n' "$current"
  exit 0
fi

response="$(timeout 30s gh api --hostname github.com --method GET "repos/devantler-tech/platform/commits/$current")" ||
  refuse 'cannot read the current commit verification'
jq -se --arg sha "$current" '
  length == 1 and .[0].sha == $sha and .[0].commit.verification.verified == true
' <<<"$response" >/dev/null || refuse 'current commit signature is unverified'

timeout 60s env -u GIT_DIR -u GIT_WORK_TREE -u GIT_COMMON_DIR \
  git --no-replace-objects -C "$GITHUB_WORKSPACE" fetch --quiet --depth=64 --no-tags --no-write-fetch-head \
  origin +refs/heads/main:refs/remotes/origin/main || refuse 'cannot fetch current main'
[[ "$(checkout_git rev-parse --verify 'refs/remotes/origin/main^{commit}')" == "$current" ]] ||
  refuse 'main moved during fetch'
checkout_git merge-base --is-ancestor "$original" "$current" ||
  refuse 'current main does not extend the tested revision'

# Execute only the same complete reader inputs that passed the dispatch tests.
checkout_git diff --quiet --no-ext-diff "$original" "$current" -- \
  .github/workflows/prepare-longhorn-v2-node.yaml \
  .github/scripts/select-longhorn-runtime-inventory-source.sh .github/scripts/setup-talosctl.sh .github/scripts/release-asset-download.sh \
  scripts/verify-prod-recovery-source.sh scripts/use-prod-stable-api-endpoint.sh \
  scripts/migrate-longhorn-v2 internal vendor go.mod go.sum go.work go.work.sum ||
  refuse 'the current reader or its protected workflow differs from the tested revision'
checkout_git switch --quiet --detach "$current" || refuse 'cannot bind the runner checkout to current main'
printf '%s\n' "$current"
