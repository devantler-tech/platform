#!/usr/bin/env bash
# The Kopia reader replaces github.com/kopia/kopia with the project-velero fork
# commit that the deployed Velero pins. Semver ranks that pseudo-version below
# every tag, so Renovate would offer an older tag as an automerged minor update.
# The rule stays only while the replacement is a pseudo-version.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly renovate_config="${RENOVATE_CONFIG:-${root_dir}/.github/renovate.json}"
readonly go_mod="${GO_MOD:-${root_dir}/go.mod}"
readonly ci_workflow="${CI_WORKFLOW:-${root_dir}/.github/workflows/ci.yaml}"
readonly module='github.com/project-velero/kopia'

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v jq >/dev/null 2>&1 || fail 'jq is required to inspect the Renovate policy'

guards_job="$(awk '
  /^  validate-repository-guards:$/ { in_guards = 1; print; next }
  in_guards && /^  [[:alnum:]_-]+:$/ { exit }
  in_guards { print }
' "${ci_workflow}")"
grep -Fq 'bash scripts/tests/test-velero-kopia-renovate-pin.sh' <<<"${guards_job}" ||
  fail 'the Kopia pin test must run unconditionally in the repository guards job'

replacement="$(awk -v module="${module}" '$1 == "replace" && $4 == module { print $5 }' "${go_mod}")"
[[ -n "${replacement}" ]] || fail "go.mod must replace github.com/kopia/kopia with ${module}"
[[ "${replacement}" =~ ^v0\.0\.0-[0-9]{14}-[0-9a-f]{12}$ ]] ||
  fail "the ${module} replacement ${replacement} is no longer a pseudo-version: remove this rule and test"

rules="$(jq -c --arg module "${module}" '
  [.packageRules[] | select((.matchPackageNames // []) | index($module))]
' "${renovate_config}")" || fail 'the Renovate configuration must be valid JSON'

[[ "$(jq -r 'length' <<<"${rules}")" == '1' ]] ||
  fail "Renovate must define exactly one package rule for ${module}, found $(jq -r 'length' <<<"${rules}")"
[[ "$(jq -c '.[0].matchManagers // []' <<<"${rules}")" == '["gomod"]' ]] ||
  fail "the ${module} rule must match the gomod manager only"
[[ "$(jq -r '.[0].enabled' <<<"${rules}")" == 'false' ]] ||
  fail "the ${module} rule must disable updates: every tag ranks above the pin"
for narrowing in matchFileNames matchUpdateTypes matchCurrentVersion matchDepTypes; do
  [[ "$(jq -r --arg key "${narrowing}" '.[0] | has($key)' <<<"${rules}")" == 'false' ]] ||
    fail "the ${module} rule must not be narrowed by ${narrowing}"
done

# Renovate applies package rules in order and a later match wins, so a later
# rule naming this module or enabling every gomod update would undo this one.
later="$(jq -r --arg module "${module}" '
  .packageRules as $rules
  | ($rules | to_entries | map(select((.value.matchPackageNames // []) | index($module))) | .[0].key) as $at
  | [$rules[($at + 1):][] | select(.enabled == true)] | length
' "${renovate_config}")"
[[ "${later}" == '0' ]] || fail "a later package rule re-enables updates and may undo the ${module} rule"

printf 'PASS: Renovate does not offer %s while go.mod pins %s\n' "${module}" "${replacement}"
