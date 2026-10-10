#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
scratch="$(mktemp -d "${TMPDIR:-/tmp}/openbao-strategy-api.XXXXXX")"
trap 'rm -rf -- "${scratch}"' EXIT
: "${OPENBAO_TRANSITION_MANIFEST:?actual post-rendered StatefulSet required}"
: "${OPENBAO_TRANSITION_RELEASE:?effective HelmRelease required}"

# A local API server and etcd exercise server-side apply without Docker, a
# controller, production access or a user kubeconfig. Check every downloaded
# executable against the reviewed official release asset digest before running.
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
[[ "${actual_sha}" == "${expected_sha}" ]] || {
  echo 'FAIL: local API archive checksum changed' >&2
  exit 1
}
tar -xzf "${scratch}/envtest.tar.gz" -C "${scratch}"
cd "${root_dir}/scripts/tests/openbao-strategy-transition"
KUBEBUILDER_ASSETS="${scratch}/controller-tools/envtest" go test -count=1 -timeout=3m .
