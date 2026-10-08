#!/usr/bin/env bash
# Resolve the chart the RBAC renderer must pull from the built Flux overlay.
# Support the existing HelmRepository path and exact-tag OCI chartRef sources;
# refuse definitions whose chart artifact this renderer cannot model.
set -euo pipefail

release_file="${1:?HelmRelease JSON is required}"
overlay_file="${2:?built overlay YAML is required}"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

definition="$(jq -r '
  if .spec.chart != null and .spec.chartRef == null then "repository"
  elif .spec.chart == null and .spec.chartRef != null then "artifact"
  else "unsupported" end' "$release_file")"
[ "$definition" != unsupported ] || fail 'expected one chart definition'

if [ "$definition" = artifact ]; then
  kind="$(jq -r '.spec.chartRef.kind' "$release_file")"
  [ "$kind" = OCIRepository ] || fail 'chartRef must reference an OCIRepository'
  reference='.spec.chartRef'
else
  kind="$(jq -r '.spec.chart.spec.sourceRef.kind' "$release_file")"
  [ "$kind" = HelmRepository ] || fail 'chart source must reference a HelmRepository'
  reference='.spec.chart.spec.sourceRef'
fi
name="$(jq -r "${reference}.name" "$release_file")"
namespace="$(jq -r "${reference}.namespace // .metadata.namespace" "$release_file")"
for field in "$name" "$namespace"; do
  [ "$field" != null ] && [[ "$field" =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ ]] || fail 'invalid chart source identity'
done

# Match both identity fields and refuse missing or duplicate resources.
source="$(yq -o=json "select(.kind == \"${kind}\" and .metadata.name == \"${name}\"
  and .metadata.namespace == \"${namespace}\")" "$overlay_file" | jq -s '.')"
[ "$(jq length <<<"$source")" -eq 1 ] || fail 'expected exactly one chart source in the production overlay'
source="$(jq '.[0]' <<<"$source")"
url="$(jq -r '.spec.url' <<<"$source")"

if [ "$definition" = artifact ]; then
  [[ "$url" =~ ^oci://[^/[:space:]]+/.+/[a-z0-9][a-z0-9.-]*$ ]] || fail 'invalid OCI chart URL'
  jq -e '
    (.spec.ref | keys) == ["tag"] and
    (.spec.ref.tag | type == "string" and test("^[0-9]+\\.[0-9]+\\.[0-9]+(-[0-9A-Za-z.-]+)?$"))
  ' <<<"$source" >/dev/null || fail 'OCI chart source needs one exact version tag'
  jq -e '.spec.layerSelector == {
    "mediaType": "application/vnd.cncf.helm.chart.content.v1.tar+gzip", "operation": "copy"
  }' <<<"$source" >/dev/null || fail 'OCI chart source must copy the Helm chart layer'
  chart="${url##*/}"
  version="$(jq -r '.spec.ref.tag' <<<"$source")"
else
  chart="$(jq -r '.spec.chart.spec.chart' "$release_file")"
  version="$(jq -r '.spec.chart.spec.version' "$release_file")"
  for field in "$chart" "$version" "$url"; do
    [ -n "$field" ] && [ "$field" != null ] || fail 'missing chart, version or repository URL'
  done
  case "$url" in oci://*) url="${url}/${chart}" ;; esac
fi

jq -n --arg chart "$chart" --arg version "$version" --arg url "$url" \
  '{chart: $chart, version: $version, url: $url}'
