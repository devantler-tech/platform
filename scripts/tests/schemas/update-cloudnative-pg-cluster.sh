#!/usr/bin/env bash
# Generate the full v1 schema without editing/reducing the upstream OpenAPI schema.
# The immutable controller CRD must match the CRD shipped in the pinned Helm chart.
# Update these reviewed source pins together when the controller chart changes.
set -Eeuo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
schema_path="$repo_root/scripts/tests/schemas/cloudnative-pg-cluster-v1.json"
controller_dir="$repo_root/k8s/bases/infrastructure/controllers/cloudnative-pg"
chart_version=0.29.1
controller_version=1.30.1
source_commit=2a35abb4628f209d149825ef3c38011e0701ff2f
source_sha256=d255176c368727b8ee84893bb2f37b5c5578a497e6ee7461a81fa6f3f80e552b
chart_sha256=b53d3991fe84bcf38767e7702cae78666265427a127a26fff168ab4207d2b1df
schema_sha256=ee1b3e6e7acd44f98c82b1eca294123773a1b80fa31e607f6a8dedaa3ace4ab8
source_url="https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/$source_commit/config/crd/bases/postgresql.cnpg.io_clusters.yaml"
chart_url="https://github.com/cloudnative-pg/charts/releases/download/cloudnative-pg-v$chart_version/cloudnative-pg-$chart_version.tgz"
mode="${1:---check-committed}"

fail() {
  echo "cnpg-schema: $*" >&2
  exit 1
}

digest() {
  sha256sum "$1" | awk '{print $1}'
}

test "$#" -le 1 || fail 'usage: update-cloudnative-pg-cluster.sh [--check-committed|--check-source|--write]'
case "$mode" in
  --check-committed | --check-source | --write) ;;
  *) fail "unknown mode: $mode" ;;
esac
for tool in yq jq sha256sum; do
  command -v "$tool" >/dev/null || fail "$tool is required"
done
yq -o=json '.spec.chart.spec' "$controller_dir/helm-release.yaml" |
  jq -e '.chart == "cloudnative-pg" and .sourceRef.kind == "HelmRepository" and .sourceRef.name == "cloudnative-pg" and (.sourceRef.namespace // "cnpg-system") == "cnpg-system"' >/dev/null ||
  fail 'controller chart identity changed; review schema provenance'
test "$(yq -r '.spec.chart.spec.version' "$controller_dir/helm-release.yaml")" = "$chart_version" ||
  fail 'controller chart changed; review and regenerate its full schema'
test "$(yq -r '.spec.url' "$controller_dir/helm-repository.yaml")" = 'https://cloudnative-pg.github.io/charts' ||
  fail 'controller chart source changed; review schema provenance'

if test "$mode" = --check-committed; then
  test "$(digest "$schema_path")" = "$schema_sha256" || fail 'committed schema differs from the pinned generated artifact'
  echo "PASS: committed CNPG Cluster v1 schema matches controller $controller_version/chart $chart_version and its pinned digest"
  exit 0
fi

for tool in curl tar; do
  command -v "$tool" >/dev/null || fail "$tool is required"
done
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/cnpg-schema-source.XXXXXX")"
trap 'rm -rf -- "$work_dir"' EXIT
curl --fail --silent --show-error --location --retry 3 "$source_url" --output "$work_dir/cluster.yaml"
test "$(digest "$work_dir/cluster.yaml")" = "$source_sha256" || fail 'upstream CRD source digest mismatch'
curl --fail --silent --show-error --location --retry 3 "$chart_url" --output "$work_dir/chart.tgz"
test "$(digest "$work_dir/chart.tgz")" = "$chart_sha256" || fail 'upstream chart digest mismatch'
tar -xOzf "$work_dir/chart.tgz" cloudnative-pg/Chart.yaml >"$work_dir/Chart.yaml"
test "$(yq -r '.version' "$work_dir/Chart.yaml")" = "$chart_version" || fail 'chart version mismatch'
test "$(yq -r '.appVersion' "$work_dir/Chart.yaml")" = "$controller_version" || fail 'chart/controller version mismatch'
tar -xOzf "$work_dir/chart.tgz" cloudnative-pg/templates/crds/crds.yaml >"$work_dir/crds-template.yaml"
test "$(head -n 1 "$work_dir/crds-template.yaml")" = '{{- if .Values.crds.create }}' || fail 'chart CRD wrapper changed'
test "$(tail -n 1 "$work_dir/crds-template.yaml")" = '{{- end }}' || fail 'chart CRD wrapper changed'
sed '1d;$d' "$work_dir/crds-template.yaml" >"$work_dir/crds.yaml"

# JSON key ordering is the only transformation. Keep every type, constraint,
# property, description, default and Kubernetes extension from the original CRD.
query='select(.kind == "CustomResourceDefinition" and .metadata.name == "clusters.postgresql.cnpg.io") | .spec.versions[] | select(.name == "v1" and .served == true and .storage == true) | .schema.openAPIV3Schema'
yq -o=json "$query" "$work_dir/cluster.yaml" | jq -S . >"$work_dir/source.json"
yq -o=json "$query" "$work_dir/crds.yaml" | jq -S . >"$work_dir/chart.json"
cmp -s "$work_dir/source.json" "$work_dir/chart.json" || fail 'immutable controller CRD differs from the pinned chart CRD'
test "$(digest "$work_dir/source.json")" = "$schema_sha256" || fail 'generated full schema digest mismatch'

if test "$mode" = --write; then
  cp "$work_dir/source.json" "$schema_path"
else
  cmp -s "$work_dir/source.json" "$schema_path" || fail 'regeneration differs from the committed schema'
fi
echo "PASS: full CNPG Cluster v1 schema reproduces from commit $source_commit and chart $chart_version"
