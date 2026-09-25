#!/usr/bin/env bash

# Exercise the pinned chart and Flux post-renderer, then inspect the workload
# whose pod template Flagger copies to the serving primary.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
release="${UMAMI_RELEASE_FILE:-${root_dir}/k8s/bases/apps/umami/helm-release.yaml}"
readonly release
repository="${root_dir}/k8s/bases/apps/umami/helm-repository.yaml"
readonly repository
scratch="$(mktemp -d)"
readonly scratch
trap 'rm -rf "${scratch}"' EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

for tool in helm jq kubectl yq; do
  command -v "${tool}" >/dev/null || fail "${tool} is required"
done

chart="$(yq -r '.spec.chart.spec.chart' "${release}")"
version="$(yq -r '.spec.chart.spec.version' "${release}")"
url="$(yq -r '.spec.url' "${repository}")"
helm pull "${chart}" --repo "${url}" --version "${version}" --destination "${scratch}" >/dev/null
yq -o=json '.spec.values' "${release}" >"${scratch}/values.json"
helm template umami "${scratch}/${chart}-${version}.tgz" --namespace umami \
  --values "${scratch}/values.json" >"${scratch}/resources.yaml"

renderer_count="$(yq '.spec.postRenderers | length' "${release}")"
for ((index = 0; index < renderer_count; index++)); do
  yq -o=json ".spec.postRenderers[${index}].kustomize" "${release}" >"${scratch}/renderer.json"
  jq -n --slurpfile renderer "${scratch}/renderer.json" \
    '{apiVersion: "kustomize.config.k8s.io/v1beta1", kind: "Kustomization",
      resources: ["resources.yaml"]} + $renderer[0]' >"${scratch}/kustomization.yaml"
  kubectl kustomize "${scratch}" >"${scratch}/next.yaml"
  mv "${scratch}/next.yaml" "${scratch}/resources.yaml"
done

yq ea -o=json '[.]' "${scratch}/resources.yaml" | jq -e '
  [.[] | select(.kind == "Deployment" and .metadata.name == "umami-umami") | (
    .spec.strategy.type == "RollingUpdate" and
    .spec.strategy.rollingUpdate.maxSurge == 0 and
    .spec.strategy.rollingUpdate.maxUnavailable == 1 and
    .spec.template.metadata.labels["app.kubernetes.io/name"] == "umami" and
    .spec.template.metadata.labels["app.kubernetes.io/instance"] == "umami" and
    ([.spec.template.spec.topologySpreadConstraints[]? | select(
      .topologyKey == "kubernetes.io/hostname" and
      .maxSkew == 1 and
      .minDomains == 2 and
      .nodeTaintsPolicy == "Honor" and
      .whenUnsatisfiable == "DoNotSchedule" and
      .labelSelector.matchLabels["app.kubernetes.io/name"] == "umami-primary" and
      (has("matchLabelKeys") | not)
    )] | length) == 1 and
    (.spec.template.spec.topologySpreadConstraints | length) == 1 and
    ([.spec.template.spec.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[]?] | length) == 0
  )] == [true]
' >/dev/null || fail 'rendered Umami source must spread every primary revision across the nodes it can use without blocking scale-out'

# The API server rejects a pod template whose spread constraints repeat a
# {topologyKey, whenUnsatisfiable} pair, so check every rendered pod template.
yq ea -o=json '[.]' "${scratch}/resources.yaml" | jq -e '
  [.[] | .spec.template.spec.topologySpreadConstraints? // empty
    | [.[] | [.topologyKey, .whenUnsatisfiable]] | (length == (unique | length))] | all
' >/dev/null || fail 'rendered pod templates must not repeat a topologyKey and whenUnsatisfiable pair'

policy="${root_dir}/k8s/bases/infrastructure/cluster-policies/best-practices/propagate-reloader-to-flagger-primary.yaml"
yq -o=json '.spec.rules' "${policy}" | jq -e '
  [.[] | select(
    .match.any == [{"resources": {"kinds": ["Deployment"], "names": ["umami-umami"], "namespaces": ["umami"]}}] and
    .mutate.mutateExistingOnPolicyUpdate == true and
    .mutate.targets == [{"apiVersion": "apps/v1", "kind": "Deployment", "name": "umami-umami-primary", "namespace": "umami"}] and
    .mutate.patchStrategicMerge.spec.strategy == {
      "type": "RollingUpdate", "rollingUpdate": {"maxSurge": 0, "maxUnavailable": 1}
    }
  )] | length == 1
' >/dev/null || fail 'Umami primary policy must retrofit no-surge strategy on the existing serving Deployment'

printf 'PASS: rendered Umami source and existing primary keep both spread constraints and no-surge rollout\n'
