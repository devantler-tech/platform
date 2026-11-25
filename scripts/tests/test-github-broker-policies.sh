#!/usr/bin/env bash
# Regression controls for scripts/guard-github-broker-policies.sh
# (devantler-tech/monorepo ADR 0020).
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
guard="${root_dir}/scripts/guard-github-broker-policies.sh"
readonly guard
work="$(mktemp -d)"
readonly work
trap 'rm -rf "${work}"' EXIT

# The tree as committed passes, and the guard examined its policies.
bash "${guard}" "${root_dir}" >"${work}/committed"
committed="$(find "${root_dir}/.github/chainguard" -name '*.sts.yaml' -type f | wc -l | tr -d ' ')"
((committed > 0)) || {
  echo 'FAIL: expected at least one committed trust policy' >&2
  exit 1
}
grep -qx "PASS: ${committed} broker trust policies accept only devantler-tech callers" "${work}/committed"

# A policy built from an issuer line and an owner line, so each case changes
# exactly one of them. An empty line leaves that field out.
policy() {
  printf '%s\n' '---' "$1" \
    'subject: repo:devantler-tech/platform:ref:refs/heads/main' \
    'claim_pattern:' "$2" \
    '  job_workflow_ref: devantler-tech/platform/\.github/workflows/smoke\.yaml@refs/heads/main' \
    'permissions:' '  contents: read'
}
readonly issuer='issuer: https://token.actions.githubusercontent.com'
readonly owner='  repository_owner_id: "178524219"'

expect() {
  local want="$1" message="$2" status=0
  rm -rf "${work}/repo"
  mkdir -p "${work}/repo/.github/chainguard"
  policy "${issuer}" "${owner}" >"${work}/repo/.github/chainguard/valid.sts.yaml"
  printf '%s\n' "$3" >"${work}/repo/.github/chainguard/case.sts.yaml"
  bash "${guard}" "${work}/repo" >"${work}/result" 2>&1 || status=$?
  if [[ "${status}" != "${want}" ]]; then
    printf 'FAIL: %s: expected exit %s, got %s\n' "${message}" "${want}" "${status}" >&2
    cat "${work}/result" >&2
    exit 1
  fi
}

expect 0 'a policy pinned to devantler-tech' "$(policy "${issuer}" "${owner}")"
grep -qx 'PASS: 2 broker trust policies accept only devantler-tech callers' "${work}/result"

# Every way a policy could admit a caller from outside devantler-tech fails.
expect 1 'no owner pin' "$(policy "${issuer}" '')"
grep -q 'must pin claim_pattern.repository_owner_id' "${work}/result"
expect 1 'another organization' "$(policy "${issuer}" '  repository_owner_id: "1"')"
expect 1 'an owner pattern wider than the ID' "$(policy "${issuer}" '  repository_owner_id: "178524219|.*"')"
expect 1 'any owner' "$(policy "${issuer}" '  repository_owner_id: ".*"')"
expect 1 'an owner ID that is not a string' "$(policy "${issuer}" '  repository_owner_id: 178524219')"
expect 1 'an issuer pattern' "$(policy 'issuer_pattern: https://.*' "${owner}")"
grep -q 'uses issuer_pattern' "${work}/result"
expect 1 'an issuer pattern beside the issuer' "$(
  policy "${issuer}" "${owner}"
  echo 'issuer_pattern: https://.*'
)"
expect 1 'another issuer' "$(policy 'issuer: https://issuer.example.com' "${owner}")"
grep -q 'must accept exactly the GitHub Actions issuer' "${work}/result"
expect 1 'no issuer' "$(policy '' "${owner}")"
expect 1 'a second document' "$(
  policy "${issuer}" "${owner}"
  policy 'issuer: https://issuer.example.com' ''
)"
grep -q 'holds 2 YAML documents' "${work}/result"
expect 1 'invalid YAML' 'issuer: [unclosed'
grep -q 'is not valid YAML' "${work}/result"

# A missing root is UNKNOWN, never a pass.
status=0
bash "${guard}" "${work}/missing" >/dev/null 2>&1 || status=$?
[[ "${status}" == 2 ]] || {
  echo "FAIL: a missing root: expected exit 2, got ${status}" >&2
  exit 1
}

echo 'PASS: test-github-broker-policies'
