#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
policy="${COROOT_DNS_POLICY_FILE:-${root_dir}/k8s/bases/infrastructure/cluster-policies/best-practices/set-observability-dns-ndots.yaml}"
scratch="$(mktemp -d)"
readonly root_dir policy scratch
trap 'rm -rf "${scratch}"' EXIT

for tool in docker go kyverno yq; do
  command -v "${tool}" >/dev/null || { printf '%s is required\n' "${tool}" >&2; exit 1; }
done

# Exercise admission, then feed its effective option to both real resolvers.
kyverno apply "${policy}" \
  --resource "${root_dir}/scripts/tests/fixtures/coroot-dns-probe/pod.yaml" \
  --output "${scratch}/mutated.yaml" --remove-color
ndots="$(yq -er '.spec.dnsConfig.options[] | select(.name == "ndots") | .value' "${scratch}/mutated.yaml")"
CGO_ENABLED=0 GOOS=linux go build -o "${scratch}/probe" \
  "${root_dir}/scripts/tests/fixtures/coroot-dns-probe/main.go"
docker run --rm --network none --read-only --cap-drop ALL \
  --security-opt no-new-privileges \
  --dns 127.0.0.1 --dns-search observability.svc.cluster.local \
  --dns-search svc.cluster.local --dns-search cluster.local \
  --dns-opt "ndots:${ndots}" --dns-opt timeout:1 --dns-opt attempts:1 \
  --mount "type=bind,src=${scratch}/probe,dst=/probe,readonly" \
  --entrypoint /probe golang:1.26.6
