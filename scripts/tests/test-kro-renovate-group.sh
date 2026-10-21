#!/usr/bin/env bash
# The web-app compiler check requires the KRO Go module to equal the KRO
# controller's chart tag. Renovate must therefore track the chart tag and move
# it in the same PR as the module, or every module bump fails on arrival and
# the controller is never updated at all (#5204).

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly renovate_config="${RENOVATE_CONFIG:-${root_dir}/.github/renovate.json}"
readonly oci_repository="${OCI_REPOSITORY:-${root_dir}/k8s/bases/infrastructure/controllers/kro/oci-repository.yaml}"
readonly ci_workflow="${root_dir}/.github/workflows/ci.yaml"
readonly oci_path='k8s/bases/infrastructure/controllers/kro/oci-repository.yaml'
readonly chart='registry.k8s.io/kro/charts/kro'
readonly module='github.com/kubernetes-sigs/kro'

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v jq >/dev/null 2>&1 || fail 'jq is required to inspect the Renovate policy'
command -v yq >/dev/null 2>&1 || fail 'yq is required to read the controller pin'

guards_job="$(awk '
  /^  validate-repository-guards:$/ { in_guards = 1 }
  in_guards && /^  [[:alnum:]_-]+:$/ && $0 != "  validate-repository-guards:" { exit }
  in_guards { print }
' "${ci_workflow}")"
grep -Fq 'bash scripts/tests/test-kro-renovate-group.sh' <<<"${guards_job}" ||
  fail 'the KRO Renovate group test must run unconditionally in the repository guards job'

jq -e . "${renovate_config}" >/dev/null 2>&1 || fail 'the Renovate configuration must be valid JSON'

# Exactly one custom manager reads the controller pin, through the docker datasource.
managers="$(jq -c --arg path "${oci_path}" '
  [.customManagers[] | select(any(.managerFilePatterns[]?;
    . as $pattern |
    ($pattern | startswith("/") and endswith("/")) and ($path | test($pattern[1:-1])) or $pattern == $path))]
' "${renovate_config}")"
[[ "$(jq -r 'length' <<<"${managers}")" == '1' ]] ||
  fail "exactly one custom manager must read ${oci_path}, found $(jq -r 'length' <<<"${managers}")"
[[ "$(jq -r '.[0].datasourceTemplate' <<<"${managers}")" == 'docker' ]] ||
  fail 'the KRO chart manager must resolve tags through the docker datasource'
[[ "$(jq -r '.[0].depNameTemplate' <<<"${managers}")" == "${chart}" ]] ||
  fail "the KRO chart manager must name the dependency ${chart}"

# The manager's pattern must capture the pin the compiler check reads, and nothing else.
[[ "$(jq -r '.[0].matchStrings | length' <<<"${managers}")" == '1' &&
  "$(jq -r '.[0].matchStrings[0]' <<<"${managers}")" == 'tag:\s*(?<currentValue>\S+)' ]] ||
  fail 'the KRO chart manager must use the single pattern tag:\s*(?<currentValue>\S+)'
pin="$(yq -er '.spec.ref.tag' "${oci_repository}")" || fail "cannot read .spec.ref.tag from ${oci_repository}"
matches="$(grep -Eo 'tag:[[:space:]]*[^[:space:]]+' "${oci_repository}" | sed -E 's/^tag:[[:space:]]*//')" ||
  fail "the KRO chart manager's pattern matches nothing in ${oci_repository}"
[[ "${matches}" == "${pin}" ]] ||
  fail "the KRO chart manager's pattern must capture exactly the pin ${pin}, captured '${matches//$'\n'/, }'"

# Both dependencies share one group rule that keeps automerge off and waives the release-age wait.
groups="$(jq -c --arg chart "${chart}" --arg module "${module}" '
  [.packageRules | to_entries[] |
    select(((.value.matchPackageNames // []) | index($chart)) and ((.value.matchPackageNames // []) | index($module)))]
' "${renovate_config}")"
[[ "$(jq -r 'length' <<<"${groups}")" == '1' ]] ||
  fail "exactly one package rule must name both ${chart} and ${module}, found $(jq -r 'length' <<<"${groups}")"
rule="$(jq -c '.[0].value' <<<"${groups}")"
at="$(jq -r '.[0].key' <<<"${groups}")"
[[ -n "$(jq -r '.groupName // ""' <<<"${rule}")" ]] || fail 'the KRO rule must set a groupName'
[[ "$(jq -r '.minimumGroupSize // 1' <<<"${rule}")" == '2' ]] ||
  fail 'the KRO rule must set minimumGroupSize to 2, so neither dependency is proposed without the other'
[[ "$(jq -r '.automerge' <<<"${rule}")" == 'false' ]] || fail 'the KRO rule must turn automerge off'
[[ "$(jq -r '.minimumReleaseAge // ""' <<<"${rule}")" == '0 days' ]] ||
  fail 'the KRO rule must set minimumReleaseAge to 0 days: the chart registry gives no release date'
for narrowing in matchManagers matchFileNames matchUpdateTypes matchCurrentVersion matchDepTypes enabled; do
  [[ "$(jq -r --arg key "${narrowing}" 'has($key)' <<<"${rule}")" == 'false' ]] ||
    fail "the KRO rule must not be narrowed by ${narrowing}"
done

# Renovate applies package rules in order and a later match wins. Only a later rule
# that names other packages literally, or by a plain `prefix/**` glob, is provably
# unrelated; any other pattern may match.
later="$(jq -r --argjson at "${at}" --arg chart "${chart}" --arg module "${module}" '
  [.packageRules[$at + 1:][] |
    select(has("automerge") or has("groupName") or has("minimumReleaseAge") or has("enabled")) |
    select((.matchManagers // []) != ["flux"]) |
    select(
      ((.matchPackageNames // []) | length) == 0 or
      any(.matchPackageNames[];
        if test("^[A-Za-z0-9@][A-Za-z0-9._/@-]*$") then . == $chart or . == $module
        elif test("^[A-Za-z0-9@][A-Za-z0-9._/@-]*/\\*\\*$") then
          .[:-2] as $prefix | ($chart | startswith($prefix)) or ($module | startswith($prefix))
        else true end)
    )] | length
' "${renovate_config}")" || fail 'cannot inspect the rules after the KRO group'
[[ "${later}" == '0' ]] ||
  fail 'a later rule that may match a KRO dependency overrides automerge, groupName, minimumReleaseAge or enabled'

printf 'PASS: Renovate moves the KRO chart (%s) and Go module together\n' "${pin}"
