#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly headlamp_release="${root_dir}/k8s/bases/apps/headlamp/helm-release.yaml"
readonly renovate_config="${root_dir}/.github/renovate.json"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v jq >/dev/null 2>&1 || fail 'jq is required to inspect the Renovate policy'
command -v yq >/dev/null 2>&1 || fail 'yq v4 is required to inspect the Headlamp HelmRelease'

headlamp_rules="$(jq -c '
  [.packageRules[] |
    select((.matchDatasources // []) | index("helm")) |
    select((.matchPackageNames // []) | index("headlamp"))]
' "${renovate_config}")" || fail 'the Renovate configuration must be valid JSON'

rule_count="$(jq -r 'length' <<<"${headlamp_rules}")"
[[ "${rule_count}" == '1' ]] ||
  fail "Renovate must define exactly one Helm package rule for Headlamp, found ${rule_count}"

allowed_versions="$(jq -r '.[0].allowedVersions // ""' <<<"${headlamp_rules}")"
automerge="$(jq -r 'if .[0] | has("automerge") then .[0].automerge else "unset" end' <<<"${headlamp_rules}")"
readonly expected_allowed_versions='/^v?0\.42\.0$/'

[[ "${allowed_versions}" == "${expected_allowed_versions}" ]] ||
  fail "Headlamp allowedVersions must pin exactly 0.42.0, got '${allowed_versions}'"

version_is_allowed() {
  local version="$1"
  local expression

  case "${allowed_versions}" in
    '!'/*'/')
      expression="${allowed_versions:2:${#allowed_versions}-3}"
      [[ ! "${version}" =~ ${expression} ]]
      ;;
    /*/)
      expression="${allowed_versions:1:${#allowed_versions}-2}"
      [[ "${version}" =~ ${expression} ]]
      ;;
    *)
      fail "Headlamp allowedVersions must be an auditable regex, got '${allowed_versions}'"
      ;;
  esac
}

version_is_allowed '0.42.0' || fail 'Renovate must retain the last Safari-verified Headlamp release'
version_is_allowed 'v0.42.0' || fail "the Headlamp gate must accept Renovate's optional v prefix"

for unsafe_version in 0.43.0 0.44.0 0.45.0; do
  if version_is_allowed "${unsafe_version}"; then
    fail "Renovate must not offer unverified Headlamp ${unsafe_version}"
  fi
done

[[ "${automerge}" == 'false' ]] ||
  fail 'Headlamp updates must require deliberate login verification before merge'

deployed_version="$(yq e -r '.spec.chart.spec.version // ""' "${headlamp_release}")"
[[ "${deployed_version}" == '0.42.0' ]] ||
  fail "Headlamp must stay on the last Safari-verified release 0.42.0, got ${deployed_version}"
version_is_allowed "${deployed_version}" ||
  fail "the deployed Headlamp version ${deployed_version} must satisfy the Renovate gate"

printf 'PASS: Headlamp stays on the Safari-verified release until a fixed release is verified\n'
