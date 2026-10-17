#!/usr/bin/env bash
# Hold Kyverno below 1.20 while any kyverno.io policy resource remains under k8s/.
#
# Kyverno 1.20 removes the kyverno.io ClusterPolicy and Policy APIs. While the platform
# still has resources of that API group, the Kyverno chart and the CI policy CLI must
# stay on the 1.19 line: Renovate must not propose 1.20, and the committed pins must
# not be on it (#4994). Once the last such resource is migrated, this check fails until
# the cap rules and the check itself are removed, so the cap cannot outlive its reason.
#
# Exit 0: the cap and the pins agree with the tree. Exit 1: they do not, or the inputs
# could not be read; a read failure is never a pass.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly root_dir
readonly renovate_config="${RENOVATE_CONFIG:-${root_dir}/.github/renovate.json}"
readonly k8s_root="${K8S_ROOT:-${root_dir}/k8s}"
readonly cli_pin_file="${KYVERNO_CLI_PIN_FILE:-${root_dir}/.github/scripts/kyverno-version.sh}"
readonly chart_pin_file="${KYVERNO_HELM_RELEASE:-${root_dir}/k8s/bases/infrastructure/controllers/kyverno/helm-release.yaml}"

# The first releases on the Kyverno 1.20 line. The chart's minor tracks Kyverno's
# minor: chart 3.0 carries 1.10 and every later chart minor carries the next Kyverno
# minor, through chart 3.9 carrying 1.19 (read from https://kyverno.github.io/kyverno/index.yaml).
readonly chart_package='kyverno'
readonly chart_bound='3.10.0'
readonly cli_package='kyverno/kyverno'
readonly cli_bound='1.20.0'

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v jq >/dev/null 2>&1 || fail 'jq is required to inspect the Renovate policy'
[[ -d "${k8s_root}" ]] || fail "manifest root ${k8s_root} does not exist"

# version_lt A B: true when A sorts strictly before B.
version_lt() {
  [[ "$1" != "$2" ]] && [[ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | head -n 1)" == "$1" ]]
}

# Every YAML document under k8s/ whose top-level apiVersion is in the kyverno.io group.
# The newer policies.kyverno.io group is a different group and is not matched.
manifests="$(find "${k8s_root}" -type f \( -name '*.yaml' -o -name '*.yml' \) -print)" ||
  fail "cannot list manifests under ${k8s_root}"
[[ -n "${manifests}" ]] || fail "found no YAML under ${k8s_root}, so nothing was examined"
manifest_files=()
while IFS= read -r manifest; do manifest_files+=("${manifest}"); done <<<"${manifests}"
manifest_count="${#manifest_files[@]}"

legacy="$(awk '
  function flush() {
    if (api ~ /^kyverno\.io\//) print file ": " kind " (" api ")"
    api = ""; kind = ""
  }
  FNR == 1 { flush(); file = FILENAME }
  /^---/ { flush(); next }
  /^apiVersion:[[:space:]]/ { api = $2; gsub(/["\047]/, "", api) }
  /^kind:[[:space:]]/ { kind = $2; gsub(/["\047]/, "", kind) }
  END { flush() }
' "${manifest_files[@]}")" || fail "cannot read the manifests under ${k8s_root}"
legacy_count=0
[[ -z "${legacy}" ]] || legacy_count="$(wc -l <<<"${legacy}" | tr -d ' ')"

# The rules that name a package literally and set allowedVersions, in file order.
cap_rules() {
  jq -c --arg package "$1" '
    [.packageRules // [] | to_entries[] |
      select((.value.matchPackageNames // []) | index($package)) |
      select(.value | has("allowedVersions"))]
  ' "${renovate_config}"
}

# Read one package's cap and check it cannot be widened or bypassed.
# Prints the bound the cap sets.
read_cap() {
  local package="$1" rules count index allowed later extra

  rules="$(cap_rules "${package}")" || fail "${renovate_config} must be valid JSON"
  count="$(jq -r 'length' <<<"${rules}")"
  [[ "${count}" == '1' ]] ||
    fail "Renovate must hold ${package} with exactly one allowedVersions rule, found ${count}"

  # The cap names its one package and nothing else, so no filter can narrow which updates
  # it holds. Listing the allowed keys, not the narrowing ones, keeps a filter Renovate adds
  # later from slipping past this check.
  [[ "$(jq -c '.[0].value.matchPackageNames' <<<"${rules}")" == "$(jq -cn --arg p "${package}" '[$p]')" ]] ||
    fail "the ${package} cap must name only ${package} in matchPackageNames"
  extra="$(jq -r '.[0].value | keys - ["description", "matchPackageNames", "allowedVersions"] | join(", ")' <<<"${rules}")"
  [[ -z "${extra}" ]] ||
    fail "the ${package} cap must not be narrowed by ${extra}, so it holds every update"

  allowed="$(jq -r '.[0].value.allowedVersions' <<<"${rules}")"
  [[ "${allowed}" =~ ^\<([0-9]+\.[0-9]+\.[0-9]+)$ ]] ||
    fail "the ${package} cap must have the form '<X.Y.Z', got '${allowed}'"

  # Renovate applies package rules in order and a later match wins. Only a later rule
  # that names other packages literally is provably unrelated; anything else may match.
  index="$(jq -r '.[0].key' <<<"${rules}")"
  later="$(jq -r --arg package "${package}" --argjson at "${index}" '
    [.packageRules[$at + 1:][] |
      select(has("allowedVersions")) |
      select(
        ((.matchPackageNames // []) | length) == 0 or
        any(.matchPackageNames[];
          . == $package or (test("^[A-Za-z0-9@][A-Za-z0-9._/@-]*$") | not))
      )] | length
  ' "${renovate_config}")" || fail "cannot inspect the rules after the ${package} cap"
  [[ "${later}" == '0' ]] ||
    fail "a later rule that may match ${package} sets allowedVersions and would override the cap"

  printf '%s\n' "${BASH_REMATCH[1]}"
}

has_cap() {
  local rules
  rules="$(cap_rules "$1")" || fail "${renovate_config} must be valid JSON"
  [[ "$(jq -r 'length' <<<"${rules}")" != '0' ]]
}

if [[ "${legacy_count}" == '0' ]]; then
  if has_cap "${chart_package}" || has_cap "${cli_package}"; then
    fail "no kyverno.io resource remains under ${k8s_root#"${root_dir}"/} (${manifest_count} files read): remove the Kyverno 1.20 cap rules from .github/renovate.json and this check with its CI step (#4994)"
  fi
  printf 'PASS: no kyverno.io resource remains in %s files and no Kyverno 1.20 cap is left\n' "${manifest_count}"
  exit 0
fi

chart_cap="$(read_cap "${chart_package}")"
cli_cap="$(read_cap "${cli_package}")"

version_lt "${chart_bound}" "${chart_cap}" &&
  fail "the ${chart_package} cap <${chart_cap} admits the Kyverno 1.20 chart line; it must be <${chart_bound} or lower"
version_lt "${cli_bound}" "${cli_cap}" &&
  fail "the ${cli_package} cap <${cli_cap} admits Kyverno 1.20; it must be <${cli_bound} or lower"

# The committed pins must be inside their caps, so a hand-edited bump cannot bypass them.
chart_pin="$(awk '
  $1 == "chart:" && $2 == "kyverno" { in_chart = 1; next }
  in_chart && $1 == "version:" { print $2; exit }
' "${chart_pin_file}")" || fail "cannot read ${chart_pin_file}"
[[ "${chart_pin}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
  fail "cannot read the Kyverno chart version from ${chart_pin_file}, got '${chart_pin}'"
cli_pin="$(sed -n 's/^KYVERNO_VERSION="\([^"]*\)"$/\1/p' "${cli_pin_file}")" ||
  fail "cannot read ${cli_pin_file}"
[[ "${cli_pin}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
  fail "cannot read exactly one KYVERNO_VERSION from ${cli_pin_file}, got '${cli_pin}'"

version_lt "${chart_pin}" "${chart_cap}" ||
  fail "the Kyverno chart is pinned at ${chart_pin}, outside its cap <${chart_cap}"
version_lt "${cli_pin}" "${cli_cap}" ||
  fail "the Kyverno CLI is pinned at ${cli_pin}, outside its cap <${cli_cap}"

printf 'PASS: %s kyverno.io resources remain in %s files; Kyverno is held below 1.20 (chart %s <%s, CLI %s <%s)\n' \
  "${legacy_count}" "${manifest_count}" "${chart_pin}" "${chart_cap}" "${cli_pin}" "${cli_cap}"
