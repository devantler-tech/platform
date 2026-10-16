#!/usr/bin/env bash
set -euo pipefail
test_repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/k8s/source"
cat >"$work/k8s/source/oci-repository.yaml" <<'EOF'
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: product-manifests
  namespace: product
spec:
  url: oci://ghcr.io/devantler-tech/data-product-controller/manifests
  ref:
    digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
EOF
jq -n '{
 schemaVersion:1,publications:[{
 kind:"published-bootstrap",consumer:"data-product-controller",workflow:"publish-app",
 sourcePath:"k8s/source/oci-repository.yaml",namespace:"product",name:"product-manifests",
 artifactURL:"oci://ghcr.io/devantler-tech/data-product-controller/manifests",
 gitTag:"v5.2.0",sourceSHA:"1111111111111111111111111111111111111111",
 digest:"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
 signerSHA:"2222222222222222222222222222222222222222",
 mainPinSHA:"3333333333333333333333333333333333333333",
 candidateSHA:"4444444444444444444444444444444444444444",verifiedOn:"2026-10-15"
 }]}' >"$work/good.json"

if ! (
  # shellcheck source=scripts/tenant-publication-binding.lib.sh
  source "$test_repo_root/scripts/tenant-publication-binding.lib.sh"
  read_bound_publications "$work" "$work/good.json"
) >"$work/result.json" 2>"$work/error"; then
  echo 'verified publication binding was not accepted' >&2
  cat "$work/error" >&2
  exit 1
fi
jq -e '.publications[0].kind == "published-bootstrap"' "$work/result.json" >/dev/null

for mutation in \
  '.schemaVersion=2' '.publications[0].kind="applied"' \
  '.publications[0].digest="sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"' \
  '.publications[0].namespace="another-product"' '.publications[0].name="another-source"' \
  '.publications[0].artifactURL="oci://ghcr.io/devantler-tech/other/manifests"' \
  '.publications[0].consumer="other"' '.publications[0].workflow="floating"' \
  '.publications[0].sourcePath="../outside.yaml"' \
  '.publications[0].signerSHA="main"' 'del(.publications[0].sourceSHA)' \
  '.publications[0].extra="unreviewed"' '.publications += .publications' \
  '.publications[0].name="product-manifests\nother"' \
  '.publications[0].gitTag="main"' '.publications[0].verifiedOn="unknown"'; do
  jq "$mutation" "$work/good.json" >"$work/bad.json"
  if (
    source "$test_repo_root/scripts/tenant-publication-binding.lib.sh"
    read_bound_publications "$work" "$work/bad.json"
  ) >"$work/unexpected" 2>/dev/null; then
    echo "accepted an incomplete or mismatched publication: $mutation" >&2
    exit 1
  fi
  test ! -s "$work/unexpected"
done
cat "$work/good.json" "$work/good.json" >"$work/bad.json"
if (
  source "$test_repo_root/scripts/tenant-publication-binding.lib.sh"
  read_bound_publications "$work" "$work/bad.json"
) >"$work/unexpected" 2>/dev/null; then
  echo 'accepted trailing JSON documents' >&2
  exit 1
fi
test ! -s "$work/unexpected"
echo 'publication bindings accept only complete, exact source identities'
