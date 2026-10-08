#!/usr/bin/env bash
# A recovery may publish only its validated checkout while that revision is main.
set -euo pipefail

unknown() { echo 'PROD_RECOVERY_SOURCE=UNKNOWN: cannot prove the validated recovery is current' >&2; exit 2; }
stale() { echo 'PROD_RECOVERY_SOURCE=STALE: restart recovery from current main and revalidate' >&2; exit 1; }
expected="${1:-}"
[[ "$expected" =~ ^[0-9a-f]{40}$ ]] || unknown
[[ "${GITHUB_REPOSITORY:-}" == devantler-tech/platform ]] || unknown
[[ -n "${GITHUB_WORKSPACE:-}" && -n "${GH_TOKEN:-}" ]] || unknown
command -v timeout >/dev/null 2>&1 || unknown
command -v gh >/dev/null 2>&1 || unknown
command -v jq >/dev/null 2>&1 || unknown

checkout_git() {
  env -u GIT_DIR -u GIT_WORK_TREE -u GIT_COMMON_DIR \
    git --no-replace-objects -C "$GITHUB_WORKSPACE" "$@"
}
check_diff() {
  local status=0
  checkout_git diff --quiet --no-ext-diff "$@" || status=$?
  case "$status" in
    0) ;;
    1) stale ;;
    *) unknown ;;
  esac
}
check_checkout() {
  local actual
  actual="$(checkout_git rev-parse --verify 'HEAD^{commit}' 2>/dev/null)" || unknown
  [[ "$actual" =~ ^[0-9a-f]{40}$ ]] || unknown
  [[ "$actual" == "$expected" ]] || stale
  check_diff
  check_diff --cached
}

check_checkout
# Inspect the complete successful response. Plausible output from a failed
# request is never a freshness receipt; neither are an absent ref or a tag.
response="$(timeout 30s gh api --hostname github.com --method GET \
  repos/devantler-tech/platform/git/ref/heads/main 2>/dev/null)" || unknown
remote="$(jq -er '
  select(type == "object" and .ref == "refs/heads/main" and .object.type == "commit")
  | .object.sha | select(type == "string" and test("^[0-9a-f]{40}$"))
' <<< "$response" 2>/dev/null)" || unknown
[[ "$remote" =~ ^[0-9a-f]{40}$ ]] || unknown
[[ "$remote" == "$expected" ]] || stale
# Refuse a checkout changed while the remote read was in progress as well.
check_checkout
echo 'PROD_RECOVERY_SOURCE=PASS'
