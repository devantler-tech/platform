#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
scratch="$(mktemp -d "${TMPDIR:-/tmp}/openbao-canary.XXXXXX")"
readonly scratch
trap 'rm -rf -- "${scratch}"' EXIT

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

kubectl kustomize "${root_dir}/k8s/providers/hetzner/infrastructure/controllers" |
  yq ea 'select(.kind == "HelmRelease" and .metadata.name == "openbao")' - >"${scratch}/release.yaml"
readonly release="${scratch}/release.yaml"
chart_version="$(yq -er '.spec.chart.spec.version' "${release}")"
readonly chart_version
[[ "${chart_version}" == '0.29.6' ]] ||
  fail 'the OpenBao chart changed; revalidate the staged rollout before updating this guard'

helm pull openbao --repo https://openbao.github.io/openbao-helm \
  --version "${chart_version}" --destination "${scratch}" >/dev/null
readonly archive="${scratch}/openbao-${chart_version}.tgz"
readonly expected_sha='8079e985bdf608f965ada59c70051693d14dd2454ac16311229f367d0c48c4b9'
if command -v sha256sum >/dev/null 2>&1; then
  actual_sha="$(sha256sum "${archive}" | cut -d ' ' -f 1)"
else
  actual_sha="$(shasum -a 256 "${archive}" | cut -d ' ' -f 1)"
fi
[[ "${actual_sha}" == "${expected_sha}" ]] || fail 'the pinned OpenBao chart checksum changed'

replicas="$(yq -er '.data.openbao_replicas' "${root_dir}/k8s/clusters/prod/bootstrap/config-map.yaml")"
readonly replicas
[[ "${replicas}" == '3' ]] || fail 'the OpenBao canary requires revalidation when replica count changes'
yq '.spec.values' "${release}" >"${scratch}/values.yaml"
helm template openbao "${archive}" --namespace openbao --values "${scratch}/values.yaml" \
  --set "server.replicas=${replicas}" --set "server.ha.replicas=${replicas}" >"${scratch}/rendered.yaml"

# Apply the actual Flux post-renderer patches, not just their declared values.
yq -o=json '.spec.postRenderers' "${release}" | jq -e '
  length == 1 and
  (.[0] | keys) == ["kustomize"] and
  (.[0].kustomize | keys) == ["patches"] and
  (.[0].kustomize.patches | length) == 1 and
  (.[0].kustomize.patches[0] | keys) == ["patch", "target"] and
  .[0].kustomize.patches[0].target == {
    "group": "apps", "version": "v1", "kind": "StatefulSet",
    "name": "openbao", "namespace": "openbao"
  }
' >/dev/null || fail 'production OpenBao must permit only the highest-ordinal canary replacement'
yq '.spec.postRenderers[0].kustomize.patches // [] |
  {"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization",
   "resources": ["rendered.yaml"], "patches": .}' "${release}" >"${scratch}/kustomization.yaml"
kubectl kustomize "${scratch}" |
  yq ea 'select(.kind == "StatefulSet" and .metadata.name == "openbao")' - >"${scratch}/statefulset.yaml"
readonly statefulset="${scratch}/statefulset.yaml"

yq -e '.spec.updateStrategy.type == "RollingUpdate" and
  .spec.updateStrategy.rollingUpdate.partition == 2 and
  .spec.replicas == 3' "${statefulset}" >/dev/null ||
  fail 'production OpenBao must permit only the highest-ordinal canary replacement'
yq -e '.spec.template.spec.containers[] | select(.name == "openbao") |
  .image == "quay.io/openbao/openbao:2.6.3"' "${statefulset}" >/dev/null ||
  fail 'the canary must use the released standby OIDC repair'

before="$(yq ea -o=json -I=0 'select(.kind == "StatefulSet" and .metadata.name == "openbao") |
  .spec | del(.updateStrategy)' "${scratch}/rendered.yaml" | jq -cS .)"
after="$(yq -o=json -I=0 '.spec | del(.updateStrategy)' "${statefulset}" | jq -cS .)"
[[ "${before}" == "${after}" ]] ||
  fail 'the canary post-renderer must not alter storage, probes, unseal, identity or placement'

# Local/default installations keep the chart's deliberate OnDelete behavior.
yq '.spec.values' "${root_dir}/k8s/bases/infrastructure/controllers/openbao/helm-release.yaml" >"${scratch}/base-values.yaml"
# Resolve the base's ${openbao_replicas:=1} Flux default for Helm's integer schema.
helm template openbao "${archive}" --namespace openbao --values "${scratch}/base-values.yaml" \
  --set server.replicas=1 --set server.ha.replicas=1 |
  yq ea -e 'select(.kind == "StatefulSet" and .metadata.name == "openbao") |
    .spec.updateStrategy.type == "OnDelete"' - >/dev/null ||
  fail 'the staged production rollout must not enable local/default automatic replacement'

grep -Fq "'scripts/tests/test-openbao-standby-canary.sh'" "${root_dir}/.github/workflows/ci.yaml" ||
  fail 'the OpenBao canary regression must trigger CI'
grep -Fq 'bash scripts/tests/test-openbao-standby-canary.sh' "${root_dir}/.github/workflows/ci.yaml" ||
  fail 'CI must execute the OpenBao canary regression'

printf 'PASS: OpenBao 2.6.3 rollout is confined to the production canary\n'
