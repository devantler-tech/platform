#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
scratch="$(mktemp -d "${TMPDIR:-/tmp}/reloader-placement.XXXXXX")"
local_fixture="$(mktemp -d "${root_dir}/.reloader-local-opt-in.XXXXXX")"
trap 'rm -rf -- "${scratch}" "${local_fixture}"' EXIT

# Stop the render guard with the failed invariant.
fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

# Check the actual chart's placement, HA, security and resource bounds.
placement_is_independent() {
  jq -e -s --argjson replicas "$2" '
    length == 1 and (.[0] |
    .kind == "Deployment" and .metadata.name == "reloader-reloader" and
    .metadata.namespace == "reloader" and .spec.replicas == $replicas and
    any(.spec.template.spec.containers[];
      .name == "reloader-reloader" and any(.args[]; . == "--enable-ha=true") and
      .securityContext.allowPrivilegeEscalation == false and
      .securityContext.readOnlyRootFilesystem == true and
      .securityContext.runAsNonRoot == true and
      .securityContext.capabilities.drop == ["ALL"] and
      .resources == {requests:{cpu:"15m",memory:"128Mi"},limits:{cpu:"500m",memory:"512Mi"}}) and
    (
    .spec.template.metadata.labels as $labels |
    .spec.template.spec as $pod |
    ($pod.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution // []) as $terms |
    ($terms | length) == 1 and
    $terms[0].topologyKey == "kubernetes.io/hostname" and
    $terms[0].matchLabelKeys == ["pod-template-hash"] and
    $terms[0].namespaces == null and $terms[0].namespaceSelector == null and
    $terms[0].labelSelector == {matchLabels:{app:"reloader-reloader"}} and
    all($terms[0].labelSelector.matchLabels | to_entries[]; $labels[.key] == .value) and
    ($pod.topologySpreadConstraints | length) == 1 and
    $pod.topologySpreadConstraints[0] == {
      maxSkew:1, topologyKey:"kubernetes.io/hostname", whenUnsatisfiable:"DoNotSchedule",
      matchLabelKeys:["pod-template-hash"], labelSelector:{matchLabels:{app:"reloader-reloader"}}
    }))
  ' "$1" >/dev/null
}

release="${root_dir}/k8s/bases/infrastructure/controllers/reloader/helm-release.yaml"
helm_bin="${CONTROLLER_HELM:-helm}"
chart_version="$(yq -er '.spec.chart.spec.version' "${release}")"
chart_repo="$(yq -er '.spec.url' "${root_dir}/k8s/bases/infrastructure/controllers/reloader/helm-repository.yaml")"
kube_version="$(yq -er '.spec.cluster.kubernetesVersion | select(tag == "!!str" and . != "")' "${root_dir}/ksail.prod.yaml")"
"${helm_bin}" pull reloader --repo "${chart_repo}" --version "${chart_version}" --destination "${scratch}" >/dev/null

kubectl kustomize "${root_dir}/k8s/providers/hetzner/infrastructure/controllers" |
  yq ea 'select(.kind == "HelmRelease" and .metadata.name == "reloader")' - >"${scratch}/prod-release.yaml"
[[ "$(yq -er '.data.reloader_replicas' "${root_dir}/k8s/clusters/prod/bootstrap/config-map.yaml")" == 2 ]] ||
  fail 'revalidate independent placement if the production replica count changes'

# Reloader is opt-in, not part of the thin Docker overlay. Exercise its actual
# composition with that overlay in a one-replica fixture without enabling it
# in the shared local cluster configuration.
printf '%s\n' \
  'apiVersion: kustomize.config.k8s.io/v1beta1' \
  'kind: Kustomization' \
  'resources:' \
  '  - ../k8s/providers/docker/infrastructure/controllers' \
  '  - ../k8s/bases/infrastructure/controllers/reloader' \
  'patches:' \
  '  - target:' \
  '      kind: HelmRelease' \
  '      name: reloader' \
  '    patch: |-' \
  '      - op: replace' \
  '        path: /spec/values/reloader/deployment/replicas' \
  '        value: 1' >"${local_fixture}/kustomization.yaml"
kubectl kustomize "${local_fixture}" |
  yq ea 'select(.kind == "HelmRelease" and .metadata.name == "reloader")' - >"${scratch}/local-release.yaml"

for replicas in 2 1; do
  effective_release="${scratch}/local-release.yaml"
  [[ "${replicas}" != 2 ]] || effective_release="${scratch}/prod-release.yaml"
  yq '.spec.values' "${effective_release}" >"${scratch}/values.yaml"
  # Resolve the established Flux placeholder, rather than masking a literal
  # replica-count regression in the effective production release.
  expected_replicas=1
  [[ "${replicas}" != 2 ]] || expected_replicas="\${reloader_replicas:=2}"
  [[ "$(yq -er '.reloader.deployment.replicas' "${scratch}/values.yaml")" == "${expected_replicas}" ]] ||
    fail 'the effective reloader release must retain its configured replica count'
  RELOADER_REPLICAS="${replicas}" yq -i '.reloader.deployment.replicas = env(RELOADER_REPLICAS)' "${scratch}/values.yaml"
  "${helm_bin}" template reloader "${scratch}/reloader-${chart_version}.tgz" --namespace reloader \
    --kube-version "${kube_version}" \
    --values "${scratch}/values.yaml" >"${scratch}/rendered.yaml"
  yq ea -o=json -I=0 'select(.kind == "Deployment" and .metadata.name == "reloader-reloader")' \
    "${scratch}/rendered.yaml" >"${scratch}/deployment.json"
  placement_is_independent "${scratch}/deployment.json" "${replicas}" ||
    fail "the rendered ${replicas}-replica reloader must have rollout-scoped hard hostname placement"

  yq ea -o=json -I=0 'select(.kind == "PodDisruptionBudget" and .metadata.name == "reloader-reloader")' \
    "${scratch}/rendered.yaml" >"${scratch}/budget.json"
  jq -e -s --slurpfile deployment "${scratch}/deployment.json" 'length == 1 and (.[0] |
      .spec.maxUnavailable == 1 and .spec.minAvailable == null and
      .spec.selector.matchLabels == {app:"reloader-reloader",release:"reloader"} and
      all(.spec.selector.matchLabels | to_entries[];
        $deployment[0].spec.template.metadata.labels[.key] == .value))' \
    "${scratch}/budget.json" >/dev/null ||
    fail 'the actual chart must retain a selecting, drain-safe disruption budget'

  for mutation in \
    'del(.spec.template.spec.affinity)' \
    '.spec.template.spec.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution = []' \
    '.spec.template.spec.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[0].topologyKey = "topology.kubernetes.io/zone"' \
    '.spec.template.spec.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[0].labelSelector.matchLabels.app = "other"' \
    'del(.spec.template.spec.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[0].matchLabelKeys)' \
    '.spec.template.spec.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[0].namespaceSelector = {}' \
    '.spec.template.spec.topologySpreadConstraints[0].whenUnsatisfiable = "ScheduleAnyway"' \
    'del(.spec.template.spec.topologySpreadConstraints[0].matchLabelKeys)' \
    '.spec.template.spec.topologySpreadConstraints[0].labelSelector.matchLabels.app = "other"' \
    '.spec.template.spec.containers[0].args = []' \
    '.spec.template.spec.containers[0].securityContext.allowPrivilegeEscalation = true' \
    'del(.spec.template.spec.containers[0].resources)' \
    '.spec.template.spec.containers[0].resources.limits.memory = "1Gi"' \
    '.spec.replicas = 0'; do
    jq "${mutation}" "${scratch}/deployment.json" >"${scratch}/mutated.json"
    if placement_is_independent "${scratch}/mutated.json" "${replicas}"; then
      fail "the ${replicas}-replica placement guard accepted mutation: ${mutation}"
    fi
  done
done

grep -Fq "'scripts/tests/test-reloader-rendered-placement.sh'" "${root_dir}/.github/workflows/ci.yaml" ||
  fail 'the rendered placement test must trigger CI'
grep -Fq 'bash scripts/tests/test-reloader-rendered-placement.sh' "${root_dir}/.github/workflows/ci.yaml" ||
  fail 'CI must execute the rendered placement test'

printf 'PASS: the pinned reloader chart enforces independent rollout-scoped placement\n'
