#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
scratch="$(mktemp -d "${TMPDIR:-/tmp}/kyverno-auth-reviews.XXXXXX")"
trap 'rm -rf -- "${scratch}"' EXIT
release="${root_dir}/k8s/bases/infrastructure/controllers/kyverno/helm-release.yaml"
repository="${root_dir}/k8s/bases/infrastructure/controllers/kyverno/helm-repository.yaml"
chart="$(yq -r '.spec.chart.spec.chart' "${release}")"
version="$(yq -r '.spec.chart.spec.version' "${release}")"
chart_url="$(yq -r '.spec.url' "${repository}")"
yq -o yaml '.spec.values' "${release}" >"${scratch}/values.yaml"
yq 'del(.config.matchConditions)' "${scratch}/values.yaml" >"${scratch}/before.yaml"

for variant in values before; do
  helm template kyverno "${chart}" --repo "${chart_url}" --version "${version}" \
    --namespace kyverno --values "${scratch}/${variant}.yaml" >"${scratch}/${variant}-rendered.yaml"
  yq ea -N -r 'select(.kind == "ConfigMap" and .metadata.name == "kyverno") | .data | to_json' \
    "${scratch}/${variant}-rendered.yaml" | jq -S . >"${scratch}/${variant}-config.json"
done

# The chart must deliver the exact authored condition to Kyverno's ConfigMap.
jq -r '.matchConditions // "[]"' "${scratch}/values-config.json" | jq . >"${scratch}/conditions.json"
yq -o json -I=0 '.spec.values.config.matchConditions // []' "${release}" | jq -S . >"${scratch}/authored.json"
jq -S . "${scratch}/conditions.json" >"${scratch}/consumed.json"
diff -u "${scratch}/authored.json" "${scratch}/consumed.json"
jq -S 'del(.matchConditions)' "${scratch}/values-config.json" >"${scratch}/unchanged-config.json"
diff -u "${scratch}/before-config.json" "${scratch}/unchanged-config.json"
for kind in TokenReview SubjectAccessReview SelfSubjectAccessReview; do
  jq -e --arg filter "[${kind},*,*]" '.resourceFilters | contains($filter)' \
    "${scratch}/values-config.json" >/dev/null
done

# Reviewed official local API binaries; never use ambient Kubernetes credentials.
case "$(uname -s)/$(uname -m)" in
  Linux/x86_64)
    asset=envtest-v1.36.2-linux-amd64.tar.gz
    expected_sha=5e99f4eef3d6f9d4dd063730299f708c98da8801f2f14d8fc762cb354f30c332
    ;;
  Darwin/arm64)
    asset=envtest-v1.36.2-darwin-arm64.tar.gz
    expected_sha=f344e7c70961b100471eeea4d2555006f282a6a27bece7f42fbede77b29b886e
    ;;
  *) echo 'FAIL: unreviewed local API asset platform' >&2; exit 1 ;;
esac
curl --proto '=https' --proto-redir '=https' --location --tlsv1.2 \
  --fail --silent --show-error --connect-timeout 10 --max-time 180 \
  "https://github.com/kubernetes-sigs/controller-tools/releases/download/envtest-v1.36.2/${asset}" \
  --output "${scratch}/envtest.tar.gz"
if command -v sha256sum >/dev/null 2>&1; then
  actual_sha="$(sha256sum "${scratch}/envtest.tar.gz" | cut -d ' ' -f 1)"
else
  actual_sha="$(shasum -a 256 "${scratch}/envtest.tar.gz" | cut -d ' ' -f 1)"
fi
[[ "${actual_sha}" == "${expected_sha}" ]] || { echo 'FAIL: local API checksum changed' >&2; exit 1; }
tar -xzf "${scratch}/envtest.tar.gz" -C "${scratch}"
cd "${root_dir}/scripts/tests/kyverno-auth-reviews/api"
KYVERNO_MATCH_CONDITIONS="${scratch}/conditions.json" \
  KUBEBUILDER_ASSETS="${scratch}/controller-tools/envtest" go test -count=1 -timeout=3m -v .
