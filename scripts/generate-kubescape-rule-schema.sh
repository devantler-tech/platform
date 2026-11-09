#!/usr/bin/env bash
# Reproduce the narrow Rules schema correction from two immutable upstream inputs.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mode="${1:---check}"
[[ "$#" -le 1 && ("$mode" == --check || "$mode" == --write) ]] || {
  printf 'usage: %s [--check|--write]\n' "$0" >&2
  exit 2
}
scratch="$(mktemp -d "${TMPDIR:-/tmp}/kubescape-rule-schema.XXXXXX")"
trap 'rm -rf -- "$scratch"' EXIT

catalog_revision=6ec4c804fcb9b0a90fec9dd468aa57b984be150a
catalog_sha256=cb9df542316730cf11399ec9fdbc91993bc07215343a0511926447637b6bfcb6
chart_version=1.40.5
chart_sha256=80c6d177b534936f66df7d1a10544bd5e13d48b581a562c480bd16314221aa92
[[ "$(yq -er '.spec.chart.spec.version' "$root/k8s/bases/infrastructure/controllers/kubescape/helm-release.yaml")" == "$chart_version" ]] || {
  printf 'FAIL: audit and regenerate the Rules schema for the changed chart pin\n' >&2
  exit 1
}
curl --fail --silent --show-error --location --retry 3 \
  "https://raw.githubusercontent.com/datreeio/CRDs-catalog/$catalog_revision/kubescape.io/rules_v1.json" \
  --output "$scratch/catalog.json"
curl --fail --silent --show-error --location --retry 3 \
  "https://github.com/kubescape/helm-charts/releases/download/kubescape-operator-$chart_version/kubescape-operator-$chart_version.tgz" \
  --output "$scratch/chart.tgz"
printf '%s  %s\n' "$catalog_sha256" "$scratch/catalog.json" "$chart_sha256" "$scratch/chart.tgz" |
  shasum -a 256 --check --status
tar -xOf "$scratch/chart.tgz" kubescape-operator/crds/rules.crd.yaml |
  yq -o=json '.spec.versions[] | select(.name == "v1") |
    .schema.openAPIV3Schema.properties.spec.properties.rules.items.properties.profileDataRequired' - \
    >"$scratch/property.json"
jq -e '.type == "object" and .["x-kubernetes-preserve-unknown-fields"] == true' "$scratch/property.json" >/dev/null
jq -e '.properties.spec.properties.rules.items.properties | has("profileDataRequired") | not' "$scratch/catalog.json" >/dev/null
jq -S --slurpfile property "$scratch/property.json" \
  '.properties.spec.properties.rules.items.properties.profileDataRequired = $property[0]' \
  "$scratch/catalog.json" >"$scratch/generated.json"
# All pre-existing constraints must survive byte-for-byte after canonicalization.
jq -S 'del(.properties.spec.properties.rules.items.properties.profileDataRequired)' "$scratch/generated.json" >"$scratch/rest.json"
jq -S . "$scratch/catalog.json" >"$scratch/original.json"
cmp "$scratch/original.json" "$scratch/rest.json"
destination="$root/schemas/kubescape.io/rules_v1.json"
if [[ "$mode" == --write ]]; then
  mkdir -p "$(dirname "$destination")"
  cp "$scratch/generated.json" "$destination"
else
  cmp "$scratch/generated.json" "$destination" || {
    printf 'FAIL: regenerate the Rules schema with %s --write\n' "$0" >&2
    exit 1
  }
fi
printf 'PASS: Rules schema preserves the pinned catalog and adds only the chart-owned projection field\n'
