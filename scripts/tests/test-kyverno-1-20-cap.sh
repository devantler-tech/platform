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
command -v yq >/dev/null 2>&1 || fail 'yq is required to edit the overlay fixtures'

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
guards_job="$(awk '
  /^  validate-repository-guards:$/ { in_guards = 1 }
  in_guards && /^  [[:alnum:]_-]+:$/ && $0 != "  validate-repository-guards:" { exit }
  in_guards { print }
' "${ci_workflow}")"
grep -Fq 'bash scripts/tests/test-kyverno-1-20-cap.sh' <<<"${guards_job}" ||
  fail 'the Kyverno 1.20 cap test must run unconditionally in the repository guards job'

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

# 6b. Renovate matches package names without regard to case, so a later rule naming the
# package in another case overrides the cap too.
override_case="$(renovate_with override-case '.packageRules += [{"matchPackageNames": ["Kyverno"], "allowedVersions": "<9.0.0"}]')"
expect_fail 'later override in another case' 'would override the cap' "RENOVATE_CONFIG=${override_case}"
override_cli_case="$(renovate_with override-cli-case '.packageRules += [{"matchPackageNames": ["KYVERNO/Kyverno"], "allowedVersions": "<9.0.0"}]')"
expect_fail 'later CLI override in another case' 'would override the cap' "RENOVATE_CONFIG=${override_cli_case}"

# 6c. A cap that names the package in another case is not the literal cap, so none is found.
cap_case="$(renovate_with cap-case '(.packageRules[] | select(.matchPackageNames == ["kyverno"]) | .matchPackageNames) = ["Kyverno"]')"
expect_fail 'cap named in another case' 'exactly one allowedVersions rule, found 0' "RENOVATE_CONFIG=${cap_case}"

# 6d. A root renovate.json comes before .github/renovate.json, so the cap would not apply.
mkdir -p "${work}/root-config"
printf '{}\n' >"${work}/root-config/renovate.json"
expect_fail 'root renovate.json takes precedence' 'takes precedence over .github/renovate.json' "RENOVATE_ROOT_DIR=${work}/root-config"

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

# 9. Every chart version Flux would deploy is judged, in the base or set by an overlay.
# Each case renders an edited copy of the manifest tree; the resource scan reads the real one.
tree_copy() {
  local copy="${work}/tree-$1"
  cp -R "${root_dir}/k8s" "${copy}"
  printf '%s\n' "${copy}"
}

base_bump="$(tree_copy base-bump)"
sed 's/^      version: .*/      version: 3.10.0/' \
  "${root_dir}/k8s/bases/infrastructure/controllers/kyverno/helm-release.yaml" \
  >"${base_bump}/bases/infrastructure/controllers/kyverno/helm-release.yaml"
expect_fail 'base chart pinned at 3.10' \
  'deploys the Kyverno chart in kyverno/kyverno at 3.10.0, outside its cap' "RENDER_K8S_ROOT=${base_bump}"

# 9b. A provider overlay patch pins the 1.20 chart line while the base stays on 1.19.
overlay_pin="$(tree_copy overlay-pin)"
cat >"${overlay_pin}/providers/hetzner/infrastructure/controllers/kyverno/patches/pin-chart.yaml" <<'YAML'
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: kyverno
  namespace: kyverno
spec:
  chart:
    spec:
      version: 3.10.0
YAML
yq -i '.patches += [{"path": "kyverno/patches/pin-chart.yaml"}]' \
  "${overlay_pin}/providers/hetzner/infrastructure/controllers/kustomization.yaml"
expect_fail 'provider overlay pins the 1.20 chart' \
  'providers/hetzner/infrastructure/controllers deploys the Kyverno chart in kyverno/kyverno at 3.10.0, outside its cap' \
  "RENDER_K8S_ROOT=${overlay_pin}"

# 9c. An inline patch replaces the pin with a range, which no cap can hold.
inline_range="$(tree_copy inline-range)"
yq -i '.patches += [{"target": {"kind": "HelmRelease", "name": "kyverno"}, "patch": "- op: replace\n  path: /spec/chart/spec/version\n  value: \">=3.9.0\"\n"}]' \
  "${inline_range}/providers/docker/infrastructure/controllers/kustomization.yaml"
expect_fail 'inline patch sets a range' \
  "providers/docker/infrastructure/controllers deploys the Kyverno chart in kyverno/kyverno at '>=3.9.0', not one exact version" \
  "RENDER_K8S_ROOT=${inline_range}"

# A tree whose one cluster overlay names one Flux root, which renders a ConfigMap.
mini_tree() {
  local tree="${work}/mini-$1"
  mkdir -p "${tree}/clusters/test" "${tree}/root"
  printf 'resources:\n  - flux-kustomization.yaml\n' >"${tree}/clusters/test/kustomization.yaml"
  cat >"${tree}/clusters/test/flux-kustomization.yaml" <<'YAML'
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: root
  namespace: flux-system
spec:
  path: ./root
YAML
  printf 'resources:\n  - config-map.yaml\n' >"${tree}/root/kustomization.yaml"
  printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: other\n' >"${tree}/root/config-map.yaml"
  printf '%s\n' "${tree}"
}

# 9d. A walk that finds fewer Kyverno releases than Flux deploys examined too little.
no_release="$(mini_tree no-release)"
expect_fail 'no Kyverno release rendered' \
  'hold 0 Kyverno chart HelmReleases, expected at least 2' "RENDER_K8S_ROOT=${no_release}"

# 9e. Flux applies a Flux Kustomization's own patches after the build, out of the render's sight.
flux_patches="$(mini_tree flux-patches)"
printf '  patches:\n    - patch: "{}"\n' >>"${flux_patches}/clusters/test/flux-kustomization.yaml"
expect_fail 'Flux Kustomization carries patches' \
  'Flux Kustomization root in clusters/test carries spec.patches' "RENDER_K8S_ROOT=${flux_patches}"

# 9e2. Flux adds a Flux Kustomization's own components to the build, out of the render's sight.
flux_components="$(mini_tree flux-components)"
printf '  components:\n    - ../component\n' >>"${flux_components}/clusters/test/flux-kustomization.yaml"
expect_fail 'Flux Kustomization carries components' \
  'Flux Kustomization root in clusters/test carries spec.components' "RENDER_K8S_ROOT=${flux_components}"

# 9f. A root that does not render is unknown, never clean.
unrenderable="$(mini_tree unrenderable)"
rm "${unrenderable}/root/kustomization.yaml"
expect_fail 'root does not render' 'cannot render root,' "RENDER_K8S_ROOT=${unrenderable}"

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
expect_pass 'legacy in a later document, capped' "K8S_ROOT=${work}/legacy" "RENDER_K8S_ROOT=${root_dir}/k8s"

# 13. A manifest root with no YAML is an unexamined tree, never a finished migration.
expect_fail 'nothing examined' 'nothing was examined' "K8S_ROOT=${work}/empty" "RENOVATE_CONFIG=${no_caps}"

printf 'PASS: the Kyverno 1.20 cap check passed %s cases\n' "${passed}"
