#!/usr/bin/env bash
# Hold Kyverno below 1.20 while any kyverno.io policy resource remains under k8s/.
#
# Kyverno 1.20 removes the kyverno.io ClusterPolicy and Policy APIs. While the platform
# still has resources of that API group, the Kyverno chart and the CI policy CLI must
# stay on the 1.19 line: Renovate must not propose 1.20, and nothing committed may
# deploy or run it (#4994). Once the last such resource is migrated, this check fails
# until the cap rules and the check itself are removed, so the cap cannot outlive its
# reason.
#
# The chart version is judged in what Flux applies, not in the base file: every Flux
# root is rendered with kubectl kustomize, so a provider overlay or an inline patch that
# sets the version is judged like the base pin.
#
# Exit 0: the cap and the pins agree with the tree. Exit 1: they do not, or the inputs
# could not be read; a read failure is never a pass.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly root_dir
readonly renovate_config="${RENOVATE_CONFIG:-${root_dir}/.github/renovate.json}"
readonly k8s_root="${K8S_ROOT:-${root_dir}/k8s}"
# The tree whose Flux roots are rendered. Separate from K8S_ROOT only so the test's
# resource-scan fixtures need no cluster overlays.
readonly render_root="${RENDER_K8S_ROOT:-${k8s_root}}"
readonly cli_pin_file="${KYVERNO_CLI_PIN_FILE:-${root_dir}/.github/scripts/kyverno-version.sh}"

# The first releases on the Kyverno 1.20 line. The chart's minor tracks Kyverno's
# minor: chart 3.0 carries 1.10 and every later chart minor carries the next Kyverno
# minor, through chart 3.9 carrying 1.19 (read from https://kyverno.github.io/kyverno/index.yaml).
readonly chart_package='kyverno'
readonly chart_bound='3.10.0'
readonly cli_package='kyverno/kyverno'
readonly cli_bound='1.20.0'

# Kyverno chart HelmReleases the rendered Flux roots must contain: one in each provider's
# controllers layer on 2026-10-17. A walk that finds fewer checked less than it should,
# never that the tree is clean. Lower it only after checking by hand that the walk still
# finds every release Flux deploys.
readonly expected_min_chart_releases=2

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v jq >/dev/null 2>&1 || fail 'jq is required to inspect the Renovate policy'
# Renovate reads the first config file it finds, and a root renovate.json or renovate.json5
# comes before .github/renovate.json, so the cap would not be the policy Renovate runs.
for preferred in renovate.json renovate.json5; do
  [[ ! -e "${RENOVATE_ROOT_DIR:-${root_dir}}/${preferred}" ]] ||
    fail "${preferred} at the repository root takes precedence over .github/renovate.json, so the Kyverno cap there is not the policy Renovate runs"
done
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
  # Renovate compares package names without regard to case, so this does too.
  index="$(jq -r '.[0].key' <<<"${rules}")"
  later="$(jq -r --arg package "${package}" --argjson at "${index}" '
    [.packageRules[$at + 1:][] |
      select(has("allowedVersions")) |
      select(
        ((.matchPackageNames // []) | length) == 0 or
        any(.matchPackageNames[];
          ascii_downcase == ($package | ascii_downcase) or (test("^[A-Za-z0-9@][A-Za-z0-9._/@-]*$") | not))
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
cli_pin="$(sed -n 's/^KYVERNO_VERSION="\([^"]*\)"$/\1/p' "${cli_pin_file}")" ||
  fail "cannot read ${cli_pin_file}"
[[ "${cli_pin}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
  fail "cannot read exactly one KYVERNO_VERSION from ${cli_pin_file}, got '${cli_pin}'"
version_lt "${cli_pin}" "${cli_cap}" ||
  fail "the Kyverno CLI is pinned at ${cli_pin}, outside its cap <${cli_cap}"

# Every Kyverno chart version Flux would deploy, in the base or set by any overlay. The
# roots are the spec.path of every Flux Kustomization each cluster overlay renders, read
# from the render so a newly wired layer is judged too (the discovery
# scripts/guard-oci-repository-verify.sh uses). Flux applies a Flux Kustomization's own
# spec.patches after the build rendered here, so one that carries any is refused.
command -v kubectl >/dev/null 2>&1 || fail 'kubectl is required to render the Flux roots'
command -v yq >/dev/null 2>&1 || fail 'yq is required to read the rendered Flux roots'
[[ -d "${render_root}/clusters" ]] ||
  fail "no cluster overlays under ${render_root}, so no Flux root could be rendered"

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

render() {
  kubectl kustomize "$1" >"${work}/render.yaml" 2>"${work}/render.err" ||
    fail "cannot render ${1#"${render_root}"/}, so the Kyverno chart version it deploys is unknown: $(tr '\n' ' ' <"${work}/render.err")"
}

roots=()
for overlay in "${render_root}"/clusters/*/; do
  overlay="${overlay%/}"
  # clusters/base is the wiring template the cluster overlays consume, not a cluster.
  [[ "${overlay}" == */base ]] && continue
  render "${overlay}"
  # Evaluated per document: yq's eval-all pairs every match with every other in a
  # collected array, so one row per match needs the per-document form.
  flux_rows="$(yq -N -r '
    .. | select(type == "!!map" and .kind == "Kustomization" and
      ((.apiVersion // "") | test("^kustomize\\.toolkit\\.fluxcd\\.io/"))) |
    [(.metadata.name // "-"), (((.spec.patches // []) | length) | tostring), (((.spec.components // []) | length) | tostring), (.spec.path // "")] |
    join("	")
  ' "${work}/render.yaml")" || fail "cannot read the Flux Kustomizations in ${overlay#"${render_root}"/}"
  while IFS=$'\t' read -r name patch_count component_count path; do
    [[ -n "${name}" ]] || continue
    [[ "${patch_count}" == '0' ]] ||
      fail "Flux Kustomization ${name} in ${overlay#"${render_root}"/} carries spec.patches, which Flux applies after the build this check renders, so the Kyverno chart version it deploys is unknown"
    [[ "${component_count}" == '0' ]] ||
      fail "Flux Kustomization ${name} in ${overlay#"${render_root}"/} carries spec.components, which Flux adds to the build this check renders, so the Kyverno chart version it deploys is unknown"
    [[ -n "${path}" ]] || fail "Flux Kustomization ${name} in ${overlay#"${render_root}"/} names no spec.path"
    roots+=("${render_root}/${path#./}")
  done <<<"${flux_rows}"
done
[[ "${#roots[@]}" -gt 0 ]] || fail "the cluster overlays under ${render_root} name no Flux root"

chart_releases=0
chart_pins=''
for root in "${roots[@]}"; do
  render "${root}"
  release_rows="$(yq -N -r '
    .. | select(type == "!!map" and .kind == "HelmRelease" and .spec.chart.spec.chart == "kyverno") |
    [((.metadata.namespace // "-") + "/" + (.metadata.name // "-")), ((.spec.chart.spec.version // "") | tostring)] |
    join("	")
  ' "${work}/render.yaml")" || fail "cannot read the HelmReleases in ${root#"${render_root}"/}"
  while IFS=$'\t' read -r release chart_pin; do
    [[ -n "${release}" ]] || continue
    chart_releases=$((chart_releases + 1))
    [[ "${chart_pin}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
      fail "${root#"${render_root}"/} deploys the Kyverno chart in ${release} at '${chart_pin}', not one exact version, so it cannot be held below the cap"
    version_lt "${chart_pin}" "${chart_cap}" ||
      fail "${root#"${render_root}"/} deploys the Kyverno chart in ${release} at ${chart_pin}, outside its cap <${chart_cap}"
    [[ " ${chart_pins} " == *" ${chart_pin} "* ]] || chart_pins="${chart_pins:+${chart_pins} }${chart_pin}"
  done <<<"${release_rows}"
done
[[ "${chart_releases}" -ge "${expected_min_chart_releases}" ]] ||
  fail "the ${#roots[@]} rendered Flux roots hold ${chart_releases} Kyverno chart HelmReleases, expected at least ${expected_min_chart_releases}, so some were not examined"

printf 'PASS: %s kyverno.io resources remain in %s files; Kyverno is held below 1.20 (chart %s <%s in %s HelmReleases across %s Flux roots, CLI %s <%s)\n' \
  "${legacy_count}" "${manifest_count}" "${chart_pins}" "${chart_cap}" "${chart_releases}" "${#roots[@]}" "${cli_pin}" "${cli_cap}"
