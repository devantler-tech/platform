#!/usr/bin/env bash
# Keeps every GitHub token broker trust policy in this repository confined to
# callers from the devantler-tech organization (devantler-tech/monorepo ADR
# 0020). The broker (octo-sts) reads a repository's policies from
# .github/chainguard/<identity>.sts.yaml on its default branch. Any GitHub
# repository in the world can get an Actions OIDC token for the broker's
# audience, so the issuer alone does not say who is calling. Each policy must
# therefore also pin the caller's organization by its numeric ID, which
# survives renames and cannot be re-registered by someone else.
#
# Every policy must:
# - accept exactly the GitHub Actions issuer (`issuer`, never `issuer_pattern`);
# - pin `claim_pattern.repository_owner_id` to the string "178524219", the
#   devantler-tech organization's ID. octo-sts anchors the pattern, so these
#   digits match that ID and nothing else.
# - be a single YAML document.
#
# Fails (exit 1) on any policy that breaks a rule. Exits 2 (UNKNOWN) when yq is
# missing or the repository root cannot be read, so a check that examined
# nothing never passes.
#
# Usage: guard-github-broker-policies.sh [repository root]
set -euo pipefail

readonly actions_issuer=https://token.actions.githubusercontent.com
readonly org_id=178524219

unknown() {
  printf 'UNKNOWN: %s\n' "$1" >&2
  exit 2
}

root="${1:-.}"
[[ -d "${root}" ]] || unknown "cannot read the repository root ${root}"
cd "${root}"
command -v yq >/dev/null || unknown 'yq is not installed'

shopt -s nullglob
policies=(.github/chainguard/*.sts.yaml)
shopt -u nullglob

failures=0
fail() {
  printf 'FAIL: %s: %s\n' "$1" "$2" >&2
  failures=$((failures + 1))
}

for policy in "${policies[@]}"; do
  if ! documents="$(yq ea '[.] | length' "${policy}" 2>/dev/null)"; then
    fail "${policy}" 'is not valid YAML'
    continue
  fi
  if [[ "${documents}" != 1 ]]; then
    fail "${policy}" "holds ${documents} YAML documents, not one"
    continue
  fi
  if [[ "$(yq 'has("issuer_pattern")' "${policy}")" != false ]]; then
    fail "${policy}" 'uses issuer_pattern; name the issuer exactly'
  fi
  if [[ "$(yq '.issuer // ""' "${policy}")" != "${actions_issuer}" ]]; then
    fail "${policy}" "must accept exactly the GitHub Actions issuer ${actions_issuer}"
  fi
  owner="$(yq '.claim_pattern.repository_owner_id // ""' "${policy}")"
  owner_tag="$(yq '.claim_pattern.repository_owner_id | tag' "${policy}")"
  if [[ "${owner}" != "${org_id}" || "${owner_tag}" != '!!str' ]]; then
    fail "${policy}" "must pin claim_pattern.repository_owner_id to the string \"${org_id}\" (devantler-tech)"
  fi
done

if ((failures > 0)); then
  printf 'FAIL: %d problem(s) in %d broker trust policies\n' "${failures}" "${#policies[@]}" >&2
  exit 1
fi
printf 'PASS: %d broker trust policies accept only devantler-tech callers\n' "${#policies[@]}"
