#!/usr/bin/env bash
# The offline Helm renderer and the OpenBao transition test build against the
# exact Helm SDK the deployed helm-controller embeds. Renovate must not offer
# another version for them: such an update can never pass the build guard, and
# closing it only delays the next offer (#4738).

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly renovate_config="${RENOVATE_CONFIG:-${root_dir}/.github/renovate.json}"
readonly build_script="${BUILD_SCRIPT:-${root_dir}/scripts/build-controller-helm.sh}"
readonly ci_workflow="${root_dir}/.github/workflows/ci.yaml"
readonly module='helm.sh/helm/v4'

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v jq >/dev/null 2>&1 || fail 'jq is required to inspect the Renovate policy'

changes_job="$(awk '
  /^  changes:$/ { in_changes = 1 }
  in_changes && /^  [[:alnum:]_-]+:$/ && $0 != "  changes:" { exit }
  in_changes { print }
' "${ci_workflow}")"
grep -Fq 'bash scripts/tests/test-controller-helm-sdk-renovate-pin.sh' <<<"${changes_job}" ||
  fail 'the Helm SDK pin test must run unconditionally in the changes job'

# The version the build guard requires is the single source of truth.
required="$(sed -n 's/.*\.Version == "\(v[0-9][0-9.]*\)".*/\1/p' "${build_script}")"
[[ "${required}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
  fail "cannot read exactly one required Helm SDK version from ${build_script}, got '${required}'"

rules="$(jq -c --arg module "${module}" '
  [.packageRules[] | select((.matchPackageNames // []) | index($module))]
' "${renovate_config}")" || fail 'the Renovate configuration must be valid JSON'

[[ "$(jq -r 'length' <<<"${rules}")" == '1' ]] ||
  fail "Renovate must define exactly one package rule for ${module}, found $(jq -r 'length' <<<"${rules}")"

[[ "$(jq -c '.[0].matchManagers // []' <<<"${rules}")" == '["gomod"]' ]] ||
  fail "the ${module} rule must match the gomod manager only"

for narrowing in matchFileNames matchUpdateTypes matchCurrentVersion matchDepTypes; do
  [[ "$(jq -r --arg key "${narrowing}" '.[0] | has($key)' <<<"${rules}")" == 'false' ]] ||
    fail "the ${module} rule must not be narrowed by ${narrowing}: every Go module here follows the controller"
done

[[ "$(jq -r 'if .[0] | has("enabled") then .[0].enabled else true end' <<<"${rules}")" == 'true' ]] ||
  fail "the ${module} rule must pin with allowedVersions, not disable the package"

allowed="$(jq -r '.[0].allowedVersions // ""' <<<"${rules}")"
expected="/^v?${required#v}\$/"
expected="${expected//./\\.}"
[[ "${allowed}" == "${expected}" ]] ||
  fail "${module} allowedVersions must be '${expected}' to match ${build_script}, got '${allowed}'"

# Renovate applies package rules in order and a later match wins.
later="$(jq -r --arg module "${module}" '
  (.packageRules | map((.matchPackageNames // []) | index($module) != null) | index(true)) as $at |
  [.packageRules[$at + 1:][] |
    select(has("allowedVersions") or has("enabled")) |
    select(((.matchManagers // ["gomod"]) | index("gomod")) != null) |
    select(
      ((.matchPackageNames // []) | length) == 0 or
      any(.matchPackageNames[];
        . == $module or (test("^[A-Za-z0-9@][A-Za-z0-9._/@-]*$") | not))
    )] | length
' "${renovate_config}")" || fail 'cannot inspect the rules after the Helm SDK pin'
# matchPackageNames accepts globs, regexes and negations. Only a later rule that
# names other packages literally is provably unrelated; anything else may match.
[[ "${later}" == '0' ]] ||
  fail "a later rule that may match ${module} sets allowedVersions or enabled and would override the pin"

# Every Go module that requires the SDK must already be on the required version.
# grep exits 1 on no match and 2 on a read error; neither may read as a pass.
go_mods="$(grep -rl --include=go.mod -F "${module} " "${MODULE_ROOT:-${root_dir}/scripts}")" ||
  fail "found no go.mod that requires ${module}, so nothing was compared with the controller's ${required}"
checked=0
while IFS= read -r go_mod; do
  version="$(awk -v module="${module}" '$1 == module { print $2 } $1 == "require" && $2 == module { print $3 }' "${go_mod}")"
  [[ "${version}" == "${required}" ]] ||
    fail "${go_mod#"${root_dir}"/} requires ${module} '${version}', not the controller's ${required}"
  checked=$((checked + 1))
done <<<"${go_mods}"
[[ "${checked}" -ge 1 ]] || fail "compared no go.mod with the controller's ${required}"

printf 'PASS: Renovate holds %s at %s, the version %s requires\n' "${module}" "${required}" "${build_script#"${root_dir}"/}"
