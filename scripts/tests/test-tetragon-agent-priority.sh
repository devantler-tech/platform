#!/usr/bin/env bash
# A node-pinned agent cannot move to an autoscaled worker when ordinary
# reservations fill its own worker. Keep its existing platform add-on priority
# above ordinary workloads, below core infrastructure, and confined to the agent.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
release="${repo_root}/k8s/bases/infrastructure/controllers/tetragon/helm-release.yaml"
class="${repo_root}/k8s/bases/bootstrap/priority-classes/platform-critical.yaml"

yq -e '.spec.values.priorityClassName == "platform-critical"' "$release" >/dev/null || {
  printf 'FAIL: Tetragon agent must use the existing platform add-on priority\n' >&2
  exit 1
}

yq -e '.kind == "PriorityClass" and .metadata.name == "platform-critical" and
  .value > 0 and .value < 2000000000 and .globalDefault == false and
  (.preemptionPolicy // "PreemptLowerPriority") == "PreemptLowerPriority"' "$class" >/dev/null || {
  printf 'FAIL: agent priority must reclaim ordinary reservations but remain below core services\n' >&2
  exit 1
}

yq -e '(.spec.values.tetragonOperator.priorityClassName // "") == "" and
  .spec.values.tetragon.resources.requests.cpu == "100m" and
  .spec.values.tetragon.resources.requests.memory == "128Mi" and
  .spec.values.tetragon.resources.limits.cpu == "2" and
  .spec.values.tetragon.resources.limits.memory == null and
  .spec.values.export.resources.requests.cpu == "15m" and
  .spec.values.export.resources.limits.cpu == "2"' "$release" >/dev/null || {
  printf 'FAIL: agent scheduling repair must not elevate the operator or lower resource reservations\n' >&2
  exit 1
}

printf 'PASS: agent-only add-on priority preserves core-service ordering and resource requests\n'
