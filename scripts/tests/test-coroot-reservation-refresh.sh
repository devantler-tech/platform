#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
scratch="$(mktemp -d)"
readonly scratch
trap 'rm -rf "${scratch}"' EXIT

for tool in kubectl yq; do
  command -v "${tool}" >/dev/null || { printf '%s is required\n' "${tool}" >&2; exit 1; }
done

# Inspect the delivered custom resource, not just the overlay's patch. The
# operator copies these annotations to its server StatefulSet's Pod template.
kubectl kustomize "${root_dir}/k8s/providers/hetzner/infrastructure" >"${scratch}/infrastructure.yaml"
yq -e '
  select(.kind == "Coroot" and .metadata.name == "coroot") |
  .spec.podAnnotations."platform.devantler.tech/vpa-recommendation-rollout" == "2026-10-11-coroot-reservation-refresh" and
  .spec.podAnnotations."secret.reloader.stakater.com/reload" == "coroot-db-app" and
  .spec.replicas == 2 and
  .spec.resources.requests.memory == "1Gi" and
  .spec.resources.limits.memory == "6Gi" and
  (.spec.clickhouse.podAnnotations | has("platform.devantler.tech/vpa-recommendation-rollout") | not) and
  (.spec.prometheus.podAnnotations | has("platform.devantler.tech/vpa-recommendation-rollout") | not)
' "${scratch}/infrastructure.yaml" >/dev/null || {
  printf 'FAIL: refresh only the two Coroot servers without changing authored sizing or credential reloads\n' >&2
  exit 1
}

yq -e '
  select(.kind == "PodDisruptionBudget" and .metadata.name == "coroot-coroot") |
  .spec.maxUnavailable == 1 and (.spec | has("minAvailable") | not)
' "${scratch}/infrastructure.yaml" >/dev/null
yq -e '
  .spec.rules[] | select(.name == "generate-vpa-for-statefulset") |
  .generate.data.spec.updatePolicy.updateMode == "Initial"
' "${root_dir}/k8s/bases/infrastructure/cluster-policies/best-practices/auto-vpa.yaml" >/dev/null

printf 'PASS: Coroot receives a server-only, HA-preserving refresh of its admission-time reservation\n'
