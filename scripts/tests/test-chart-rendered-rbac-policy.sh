#!/usr/bin/env bash
# The privileged-RBAC policy excludes roles by the exact names their charts render.
# Those charts render in-cluster through Flux, so nothing else evaluates their RBAC
# before admission: a chart bump that renames an excluded role, or adds a privileged
# one, would pass its own PR and then fail the whole infrastructure-controllers
# Kustomization. This test renders each chart at its pinned version and committed
# values and applies the Enforce policy to the rendered Roles and ClusterRoles.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
policy="${repo_root}/k8s/bases/infrastructure/cluster-policies/best-practices/audit-privileged-rbac.yaml"

# The HelmReleases whose chart-rendered roles the policy names as exclusions.
releases=(
  k8s/bases/infrastructure/controllers/kro
  k8s/bases/infrastructure/controllers/ksail-operator
  k8s/bases/infrastructure/controllers/velero
  k8s/providers/hetzner/infrastructure/controllers/crossplane
  k8s/providers/hetzner/infrastructure/controllers/longhorn
)

# Every exclusion these charts are responsible for. Each must appear in the render,
# so an exclusion can never silently cover a role the chart no longer creates.
expected_chart_exclusions='[
  "ClusterRole||crossplane",
  "ClusterRole||crossplane-rbac-manager",
  "ClusterRole||kro:controller",
  "ClusterRole||ksail-operator",
  "ClusterRole||longhorn-role",
  "Role|longhorn-system|longhorn",
  "Role|velero|velero-server"
]'

for tool in helm jq kyverno yq; do
  command -v "$tool" >/dev/null || {
    printf 'FAIL: %s is required\n' "$tool" >&2
    exit 1
  }
done

scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

# Flux substitutes ${name:=default} before Helm sees the values. Resolve those to
# their defaults, and give a bare ${name} a placeholder string.
substitute() {
  sed -E \
    -e 's/\$\{[A-Za-z0-9_]+:=([^}]*)\}/\1/g' \
    -e 's/\$\{[A-Za-z0-9_]+\}/placeholder/g' \
    "$1"
}

render() {
  local dir="$1" work="$2"
  local release_file="${repo_root}/${dir}/helm-release.yaml"
  local repository_file="${repo_root}/${dir}/helm-repository.yaml"
  mkdir -p "$work"

  substitute "$release_file" | yq -o=json 'select(.kind == "HelmRelease")' >"${work}/release.json"
  local chart version release namespace url
  chart="$(jq -r '.spec.chart.spec.chart' "${work}/release.json")"
  version="$(jq -r '.spec.chart.spec.version' "${work}/release.json")"
  release="$(jq -r '.spec.releaseName // .metadata.name' "${work}/release.json")"
  namespace="$(jq -r '.spec.targetNamespace // .metadata.namespace' "${work}/release.json")"
  url="$(yq -r 'select(.kind == "HelmRepository") | .spec.url' "$repository_file")"
  # jq and yq print "null" for a missing field, so an empty check alone passes it.
  local field
  for field in "$chart" "$version" "$release" "$namespace" "$url"; do
    if [ -z "$field" ] || [ "$field" = "null" ]; then
      fail "${dir}: could not read chart, version, release, namespace and repository"
    fi
  done

  # The render skips Flux post-renderers, which is exact for RBAC only while no
  # post-renderer touches a Role or ClusterRole.
  jq -e '(.spec.postRenderers // []) | tostring | test("\\b(Cluster)?Role\\b") | not' \
    "${work}/release.json" >/dev/null ||
    fail "${dir}: a post-renderer references Role or ClusterRole; render it through the post-renderer"

  jq '.spec.values // {}' "${work}/release.json" >"${work}/values.json"
  case "$url" in
    oci://*) helm pull "${url}/${chart}" --version "$version" --destination "$work" >/dev/null ;;
    *) helm pull "$chart" --repo "$url" --version "$version" --destination "$work" >/dev/null ;;
  esac
  helm template "$release" "${work}/${chart}-${version}.tgz" \
    --namespace "$namespace" --values "${work}/values.json" >"${work}/rendered.yaml"

  # Helm installs a namespaced object into the release namespace when its template
  # leaves the namespace unset, so the render must carry it for exclusions to match.
  yq "select(.kind == \"Role\" or .kind == \"ClusterRole\")
      | (select(.kind == \"Role\" and (.metadata.namespace // \"\") == \"\") | .metadata.namespace) = \"${namespace}\"" \
    "${work}/rendered.yaml" >"${work}/rbac.yaml"

  local count
  count="$(yq ea '[select(.kind == "Role" or .kind == "ClusterRole")] | length' "${work}/rbac.yaml")"
  [ "$count" -gt 0 ] || fail "${dir}: ${chart} ${version} rendered no Role or ClusterRole"
  printf '%s %s rendered %s roles\n' "$chart" "$version" "$count"
}

# Writes "pass fail warn error skip" from one kyverno apply run to the census file
# and returns kyverno's exit status.
census() {
  local resources="$1" output="$2" numbers="$3" status=0
  kyverno apply "$policy" --resource "$resources" >"$output" 2>&1 || status=$?
  local line
  line="$(grep -Eo 'pass: [0-9]+, fail: [0-9]+, warn: [0-9]+, error: [0-9]+, skip: [0-9]+' "$output" || true)"
  [ "$(printf '%s\n' "$line" | grep -c .)" -eq 1 ] || {
    cat "$output" >&2
    fail "kyverno apply did not report exactly one result census"
  }
  # read needs the trailing newline that tr strips, or it fails under set -e.
  printf '%s\n' "$(printf '%s' "$line" | tr -dc '0-9 ' | tr -s ' ')" >"$numbers"
  return "$status"
}

: >"${scratch}/rbac.yaml"
for dir in "${releases[@]}"; do
  work="${scratch}/$(basename "$dir")"
  render "$dir" "$work"
  {
    printf -- '---\n'
    cat "${work}/rbac.yaml"
  } >>"${scratch}/rbac.yaml"
done

yq ea -o=json '[select(.kind == "Role" or .kind == "ClusterRole")]' "${scratch}/rbac.yaml" |
  jq '[.[] | "\(.kind)|\(.metadata.namespace // "")|\(.metadata.name)"]' >"${scratch}/identities.json"
missing="$(jq -r --argjson expected "$expected_chart_exclusions" \
  '. as $rendered | $expected - $rendered | .[]' "${scratch}/identities.json")"
[ -z "$missing" ] ||
  fail "excluded roles no longer rendered by their charts, review the policy exclusions: ${missing//$'\n'/, }"

status=0
census "${scratch}/rbac.yaml" "${scratch}/result.txt" "${scratch}/result.census" || status=$?
read -r pass failed _ errored _ <"${scratch}/result.census"
if [ "$status" -ne 0 ] || [ "$failed" -ne 0 ] || [ "$errored" -ne 0 ]; then
  cat "${scratch}/result.txt" >&2
  fail "chart-rendered RBAC violates the privileged-RBAC policy (pass ${pass}, fail ${failed}, error ${errored})"
fi
[ "$pass" -gt 0 ] || fail "no rendered role was evaluated; the census is vacuous"

# Negative control: the same render plus one privileged role the policy does not
# exclude, as a chart bump would add. It must fail, or the check above proves nothing.
{
  cat "${scratch}/rbac.yaml"
  cat <<'EOF'
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: longhorn-role-renamed
rules:
  - apiGroups: ["rbac.authorization.k8s.io"]
    resources: ["clusterroles"]
    verbs: ["bind"]
EOF
} >"${scratch}/negative.yaml"
negative_status=0
census "${scratch}/negative.yaml" "${scratch}/negative.txt" "${scratch}/negative.census" || negative_status=$?
read -r _ negative_failed _ _ _ <"${scratch}/negative.census"
if [ "$negative_status" -ne 1 ] || [ "$negative_failed" -ne 1 ] ||
  ! grep -Fq 'longhorn-role-renamed' "${scratch}/negative.txt"; then
  cat "${scratch}/negative.txt" >&2
  fail "an unexcluded privileged role in the render was not refused"
fi

printf 'PASS: %s chart-rendered roles pass the privileged-RBAC policy (%s excluded roles present); an unexcluded privileged role fails\n' \
  "$pass" "$(jq length <<<"$expected_chart_exclusions")"
