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
  go -C "${fixture}" test -mod=readonly -race -count=1 -timeout=4m -v .
