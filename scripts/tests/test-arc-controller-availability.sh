#!/usr/bin/env bash
# Keep the scoped ARC controller available during a worker drain without
# activating runner capacity or widening its watched namespace.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
base="${repo_root}/k8s/bases/infrastructure/controllers/actions-runner-controller"

yq -e '.spec.values.replicaCount == 2' "${base}/helm-release.yaml" >/dev/null || {
  printf 'FAIL: ARC controller must meet the two-replica availability floor\n' >&2
  exit 1
}

yq -e '(.spec.values.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution | length) == 1 and
  .spec.values.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[0].topologyKey == "kubernetes.io/hostname" and
  .spec.values.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[0].labelSelector.matchLabels."app.kubernetes.io/component" == "controller-manager" and
  .spec.values.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[0].labelSelector.matchLabels."app.kubernetes.io/part-of" == "gha-rs-controller"' \
  "${base}/helm-release.yaml" >/dev/null || {
  printf 'FAIL: ARC controller replicas must occupy different workers\n' >&2
  exit 1
}

yq -e '.spec.values.flags.watchSingleNamespace == "arc-runners" and
  .spec.values.resources.requests.cpu == "100m" and
  .spec.values.resources.requests.memory == "256Mi" and
  .spec.suspend == false' "${base}/helm-release.yaml" >/dev/null || {
  printf 'FAIL: controller availability must preserve staged scope and resource bounds\n' >&2
  exit 1
}

yq -e '.kind == "PodDisruptionBudget" and .metadata.namespace == "arc-systems" and
  .spec.minAvailable == 1 and
  .spec.selector.matchLabels."app.kubernetes.io/component" == "controller-manager" and
  .spec.selector.matchLabels."app.kubernetes.io/part-of" == "gha-rs-controller"' \
  "${base}/pod-disruption-budget.yaml" >/dev/null || {
  printf 'FAIL: ARC controller must retain one replica during voluntary disruption\n' >&2
  exit 1
}

yq -e '.resources | any_c(. == "pod-disruption-budget.yaml")' "${base}/kustomization.yaml" >/dev/null || {
  printf 'FAIL: controller disruption budget must be part of the deployed base\n' >&2
  exit 1
}

printf 'PASS: scoped ARC controller has two independently placed replicas and a disruption budget\n'
