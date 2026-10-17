#!/usr/bin/env bash
# Self-test for scripts/check-kyverno-1-20-cap.sh (#4994). The first case runs the
# check against the tree as committed; the rest run it against edited copies and
# require it to fail for the stated reason, never for another.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly check="${root_dir}/scripts/check-kyverno-1-20-cap.sh"
readonly renovate_config="${root_dir}/.github/renovate.json"
readonly ci_workflow="${root_dir}/.github/workflows/ci.yaml"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v jq >/dev/null 2>&1 || fail 'jq is required to edit the Renovate fixtures'

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

passed=0

expect_pass() {
  local name="$1" output
  shift
  output="$(env ${@+"$@"} bash "${check}" 2>&1)" || fail "${name}: expected a pass, got: ${output}"
  passed=$((passed + 1))
}

expect_fail() {
  local name="$1" reason="$2" output status=0
  shift 2
  output="$(env ${@+"$@"} bash "${check}" 2>&1)" || status=$?
  [[ "${status}" == '1' ]] || fail "${name}: expected exit 1, got ${status}: ${output}"
  [[ "${output}" == *"${reason}"* ]] || fail "${name}: failed for another reason: ${output}"
  passed=$((passed + 1))
}

# A Renovate config edited with a jq filter.
renovate_with() {
  local file="${work}/renovate-$1.json"
  jq "$2" "${renovate_config}" >"${file}"
  printf '%s\n' "${file}"
}

# The CI step must run unconditionally, outside the k8s deploy filter.
changes_job="$(awk '
  /^  changes:$/ { in_changes = 1 }
  in_changes && /^  [[:alnum:]_-]+:$/ && $0 != "  changes:" { exit }
  in_changes { print }
' "${ci_workflow}")"
grep -Fq 'bash scripts/tests/test-kyverno-1-20-cap.sh' <<<"${changes_job}" ||
  fail 'the Kyverno 1.20 cap test must run unconditionally in the changes job'

# 1. As committed: kyverno.io policies remain and the cap holds.
expect_pass 'committed tree'

# 2. The cap is removed while a ClusterPolicy remains.
no_caps="$(renovate_with no-caps 'del(.packageRules[] | select(has("allowedVersions") and ((.matchPackageNames // []) | (index("kyverno") or index("kyverno/kyverno")))))')"
expect_fail 'cap removed' 'exactly one allowedVersions rule, found 0' "RENOVATE_CONFIG=${no_caps}"

# 3. The chart cap is widened to the 1.20 chart line.
wide_chart="$(renovate_with wide-chart '(.packageRules[] | select(.matchPackageNames == ["kyverno"]) | .allowedVersions) = "<3.11.0"')"
expect_fail 'chart cap widened' 'admits the Kyverno 1.20 chart line' "RENOVATE_CONFIG=${wide_chart}"

# 4. The CLI cap is widened to 1.20.
wide_cli="$(renovate_with wide-cli '(.packageRules[] | select(.matchPackageNames == ["kyverno/kyverno"]) | .allowedVersions) = "<1.21.0"')"
expect_fail 'CLI cap widened' 'admits Kyverno 1.20' "RENOVATE_CONFIG=${wide_cli}"

# 5. A cap in an unauditable form.
regex_cli="$(renovate_with regex-cli '(.packageRules[] | select(.matchPackageNames == ["kyverno/kyverno"]) | .allowedVersions) = "/^1\\./"')"
expect_fail 'cap not a bound' "must have the form '<X.Y.Z'" "RENOVATE_CONFIG=${regex_cli}"

# 6. A later rule overrides the cap.
override="$(renovate_with override '.packageRules += [{"matchPackageNames": ["kyverno*"], "allowedVersions": "<9.0.0"}]')"
expect_fail 'later override' 'would override the cap' "RENOVATE_CONFIG=${override}"

# 7. The cap is narrowed so it no longer holds every update.
narrowed="$(renovate_with narrowed '(.packageRules[] | select(.matchPackageNames == ["kyverno"])) += {"matchUpdateTypes": ["major"]}')"
expect_fail 'cap narrowed' 'must not be narrowed by matchUpdateTypes' "RENOVATE_CONFIG=${narrowed}"

# 7b. Any filter counts, including one no list of known filters names.
dep_narrowed="$(renovate_with dep-narrowed '(.packageRules[] | select(.matchPackageNames == ["kyverno/kyverno"])) += {"matchDepNames": ["other"]}')"
expect_fail 'cap narrowed by another filter' 'must not be narrowed by matchDepNames' "RENOVATE_CONFIG=${dep_narrowed}"
disabled="$(renovate_with disabled '(.packageRules[] | select(.matchPackageNames == ["kyverno"])) += {"enabled": false}')"
expect_fail 'cap disables instead' 'must not be narrowed by enabled' "RENOVATE_CONFIG=${disabled}"
shared="$(renovate_with shared '(.packageRules[] | select(.matchPackageNames == ["kyverno"]) | .matchPackageNames) = ["kyverno", "other"]')"
expect_fail 'cap shared with another package' 'must name only kyverno in matchPackageNames' "RENOVATE_CONFIG=${shared}"

# 8. The CLI pin is bumped to 1.20 by hand while the cap is in place.
sed 's/^KYVERNO_VERSION=.*/KYVERNO_VERSION="1.20.0"/' "${root_dir}/.github/scripts/kyverno-version.sh" >"${work}/kyverno-version.sh"
expect_fail 'CLI pinned at 1.20' 'outside its cap' "KYVERNO_CLI_PIN_FILE=${work}/kyverno-version.sh"

# 9. The chart pin is bumped to the 1.20 chart line by hand.
sed 's/^      version: .*/      version: 3.10.0/' \
  "${root_dir}/k8s/bases/infrastructure/controllers/kyverno/helm-release.yaml" >"${work}/helm-release.yaml"
expect_fail 'chart pinned at 3.10' 'outside its cap' "KYVERNO_HELM_RELEASE=${work}/helm-release.yaml"

# Manifest fixtures. Only the kyverno.io group counts; policies.kyverno.io is the new API.
mkdir -p "${work}/migrated" "${work}/legacy" "${work}/empty"
cat >"${work}/migrated/validating-policy.yaml" <<'YAML'
apiVersion: policies.kyverno.io/v1
kind: ValidatingPolicy
metadata:
  name: migrated
YAML
cat >"${work}/legacy/policy-exception.yaml" <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: first-document
data:
  note: |
    apiVersion: kyverno.io/v1
---
kind: PolicyException
metadata:
  name: legacy
apiVersion: "kyverno.io/v2"
YAML

# 10. Migration finished but the cap is still in place: the check says to remove it.
expect_fail 'cap outlives migration' 'remove the Kyverno 1.20 cap rules' "K8S_ROOT=${work}/migrated"

# 11. Migration finished and the cap removed.
expect_pass 'cap removed after migration' "K8S_ROOT=${work}/migrated" "RENOVATE_CONFIG=${no_caps}"

# 12. A legacy resource in a later document, kind before apiVersion, quoted: still counted.
expect_fail 'legacy in a later document' 'exactly one allowedVersions rule, found 0' \
  "K8S_ROOT=${work}/legacy" "RENOVATE_CONFIG=${no_caps}"
expect_pass 'legacy in a later document, capped' "K8S_ROOT=${work}/legacy"

# 13. A manifest root with no YAML is an unexamined tree, never a finished migration.
expect_fail 'nothing examined' 'nothing was examined' "K8S_ROOT=${work}/empty" "RENOVATE_CONFIG=${no_caps}"

printf 'PASS: the Kyverno 1.20 cap check passed %s cases\n' "${passed}"
