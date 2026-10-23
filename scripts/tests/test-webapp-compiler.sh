#!/usr/bin/env bash
# Compile the repository's actual graph; never inherit a kubeconfig or test cluster.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
fixture="${root}/scripts/tests/webapp-compiler"
work="$(mktemp -d "${TMPDIR:-/tmp}/webapp-compiler.XXXXXX")"
trap 'rm -rf -- "${work}"' EXIT

export GOWORK=off
profile="$(go -C "${fixture}" list -mod=readonly -m -json github.com/kubernetes-sigs/kro)"
version="$(yq -er '.spec.ref.tag' "${root}/k8s/bases/infrastructure/controllers/kro/oci-repository.yaml")"
jq -e --arg version "v${version}" '
  .Path == "github.com/kubernetes-sigs/kro" and .Version == $version and .Replace == null
' <<<"${profile}" >/dev/null || { echo 'FAIL: compiler dependency differs from the controller pin' >&2; exit 1; }
# Module metadata exists before its source is downloaded on a clean runner.
# Resolve the verified source explicitly rather than relying on a warm module cache.
kro="$(go -C "${fixture}" mod download -json github.com/kubernetes-sigs/kro | jq -er '.Dir | select(type == "string" and length > 0)')"
gateway_profile="$(go -C "${fixture}" list -mod=readonly -m -json sigs.k8s.io/gateway-api)"
jq -e '.Replace == null' <<<"${gateway_profile}" >/dev/null || { echo 'FAIL: replaced HTTPRoute schema' >&2; exit 1; }
gateway="$(go -C "${fixture}" mod download -json sigs.k8s.io/gateway-api | jq -er '.Dir | select(type == "string" and length > 0)')"

# Real dependency schemas, pinned to the reviewed Flux 2.9.6 fixture and current
# External Secrets chart. A fixture pin is not an observation of production.
fetch_schema() {
  local url="$1" expected_digest="$2" output="$3" actual_digest
  curl --proto '=https' --proto-redir '=https' --location --tlsv1.2 \
    --fail --silent --show-error --connect-timeout 10 --max-time 90 \
    --max-filesize 10000000 "$url" --output "$output"
  actual_digest="$(shasum -a 256 "$output" | cut -d ' ' -f 1)"
  [[ "$actual_digest" == "$expected_digest" ]] || { echo 'FAIL: dependency schema checksum changed' >&2; exit 1; }
}
[[ "$(yq -er '.spec.chart.spec.version' "${root}/k8s/bases/infrastructure/controllers/external-secrets/helm-release.yaml")" == 2.12.0 ]] || {
  echo 'FAIL: review Tenant schema fixtures for the new External Secrets chart' >&2; exit 1;
}
fetch_schema 'https://raw.githubusercontent.com/fluxcd/source-controller/v1.9.6/config/crd/bases/source.toolkit.fluxcd.io_ocirepositories.yaml' \
  80c7c6e5c8de0764cc83acd4de5c74bea8e51186085304d22cecc5f2eb35500e "${work}/oci.yaml"
fetch_schema 'https://raw.githubusercontent.com/fluxcd/kustomize-controller/v1.9.6/config/crd/bases/kustomize.toolkit.fluxcd.io_kustomizations.yaml' \
  da23ca0e6b4c80d3d26a91583137491e147ef4ef1e2208aece0d0af4ef851974 "${work}/kustomization.yaml"
fetch_schema 'https://raw.githubusercontent.com/external-secrets/external-secrets/v2.12.0/deploy/crds/bundle.yaml' \
  d4da9492a6906a3834bb1f5b7367575a29b5c43411017babc7a30728f465d1ba "${work}/external-secrets.yaml"
yq -o=json -I=0 '.' "${work}/external-secrets.yaml" | jq -se '
  [.[] | select(.kind == "CustomResourceDefinition" and .metadata.name == "externalsecrets.external-secrets.io")]
  | if length == 1 then .[0] else error("expected exactly one actual ExternalSecret schema") end
' >"${work}/external-secret.json"

# Same reviewed envtest assets as the existing Kyverno admission fixture.
case "$(uname -s)/$(uname -m)" in
  Linux/x86_64)
    asset=envtest-v1.36.2-linux-amd64.tar.gz
    expected=5e99f4eef3d6f9d4dd063730299f708c98da8801f2f14d8fc762cb354f30c332 ;;
  Darwin/arm64)
    asset=envtest-v1.36.2-darwin-arm64.tar.gz
    expected=f344e7c70961b100471eeea4d2555006f282a6a27bece7f42fbede77b29b886e ;;
  *) echo 'FAIL: unreviewed API-server asset platform' >&2; exit 1 ;;
esac
curl --proto '=https' --proto-redir '=https' --location --tlsv1.2 \
  --fail --silent --show-error --connect-timeout 10 --max-time 180 \
  "https://github.com/kubernetes-sigs/controller-tools/releases/download/envtest-v1.36.2/${asset}" \
  --output "${work}/envtest.tar.gz"
actual="$(shasum -a 256 "${work}/envtest.tar.gz" | cut -d ' ' -f 1)"
[[ "${actual}" == "${expected}" ]] || { echo 'FAIL: API-server asset checksum changed' >&2; exit 1; }
tar -xzf "${work}/envtest.tar.gz" -C "${work}"
PLATFORM_TEST_ROOT="${root}" \
  KUBEBUILDER_ASSETS="${work}/controller-tools/envtest" \
  GATEWAY_HTTPROUTE_CRD="${gateway}/config/crd/standard/gateway.networking.k8s.io_httproutes.yaml" \
  KRO_GRAPH_CRD="${kro}/helm/crds/kro.run_resourcegraphdefinitions.yaml" \
  FLUX_OCI_CRD="${work}/oci.yaml" \
  FLUX_KUSTOMIZATION_CRD="${work}/kustomization.yaml" \
  EXTERNAL_SECRET_CRD="${work}/external-secret.json" \
  go -C "${fixture}" test -mod=readonly -race -count=1 -timeout=4m -v .
