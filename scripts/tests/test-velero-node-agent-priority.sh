#!/usr/bin/env bash
# The real chart must admit backup agents above movable workloads, not databases.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
release="${root_dir}/k8s/bases/infrastructure/controllers/velero/helm-release.yaml"
scratch="$(mktemp -d "${TMPDIR:-/tmp}/velero-priority.XXXXXX")"
trap 'rm -r -- "${scratch}"' EXIT
fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

helm_bin="${CONTROLLER_HELM:-helm}"
chart_version="$(yq -er '.spec.chart.spec.version' "${release}")"
chart_repo="$(yq -er '.spec.url' "${root_dir}/k8s/bases/infrastructure/controllers/velero/helm-repository.yaml")"
kube_version="$(yq -er '.spec.cluster.kubernetesVersion' "${root_dir}/ksail.prod.yaml")"
"${helm_bin}" pull velero --repo "${chart_repo}" --version "${chart_version}" --destination "${scratch}" >/dev/null
kubectl kustomize "${root_dir}/k8s/providers/hetzner/infrastructure/controllers" |
  yq ea 'select(.kind == "HelmRelease" and .metadata.name == "velero") | .spec.values' - >"${scratch}/values.yaml"
yq -o=json -I=0 '.' "${root_dir}/k8s/bases/bootstrap/priority-classes/hcloud-volume-workload.yaml" >"${scratch}/class.json"

priority_is_safe() {
  jq -e -s --slurpfile tier "${scratch}/class.json" '
    length == 2 and ($tier | length) == 1 and
    $tier[0].kind == "PriorityClass" and
    $tier[0].metadata.name == "hcloud-volume-workload" and
    $tier[0].value == 1000 and $tier[0].globalDefault == false and
    $tier[0].preemptionPolicy == "PreemptLowerPriority" and
    ([.[] | select(.kind == "DaemonSet" and .metadata.name == "node-agent")] | length) == 1 and
    all(.[] | select(.kind == "DaemonSet");
      .metadata.namespace == "velero" and
      .spec.template.spec.priorityClassName == "hcloud-volume-workload" and
      (.spec.template.spec.preemptionPolicy // "PreemptLowerPriority") == "PreemptLowerPriority" and
      .spec.updateStrategy.type == "RollingUpdate" and .spec.updateStrategy.rollingUpdate.maxUnavailable == 1) and
    ([.[] | select(.kind == "Deployment" and .metadata.name == "velero")] | length) == 1 and
    all(.[] | select(.kind == "Deployment");
      (.spec.template.spec.priorityClassName // "") == "" and
      .spec.replicas == 1 and .spec.strategy.type == "Recreate")
  ' "$1" >/dev/null
}

for mode in install upgrade; do
  flags=()
  [[ "${mode}" != upgrade ]] || flags+=(--is-upgrade)
  "${helm_bin}" template velero "${scratch}/velero-${chart_version}.tgz" --namespace velero \
    --kube-version "${kube_version}" --values "${scratch}/values.yaml" "${flags[@]}" >"${scratch}/rendered.yaml"
  yq ea -o=json -I=0 'select(.kind == "Deployment" or .kind == "DaemonSet")' \
    "${scratch}/rendered.yaml" >"${scratch}/workloads.json"
  priority_is_safe "${scratch}/workloads.json" ||
    fail "${mode}: the actual backup node-agent lacks its modest storage tier or changes the server/rollout boundary"

  for mutation in \
    'if .kind == "DaemonSet" then del(.spec.template.spec.priorityClassName) else . end' \
    'if .kind == "DaemonSet" then .spec.template.spec.priorityClassName = "platform-critical" else . end' \
    'if .kind == "DaemonSet" then .spec.template.spec.preemptionPolicy = "Never" else . end' \
    'if .kind == "Deployment" then .spec.template.spec.priorityClassName = "hcloud-volume-workload" else . end' \
    'if .kind == "DaemonSet" then .spec.updateStrategy.rollingUpdate.maxUnavailable = 2 else . end'; do
    jq "${mutation}" "${scratch}/workloads.json" >"${scratch}/mutated.json"
    if priority_is_safe "${scratch}/mutated.json"; then
      fail "${mode}: the priority guard accepted mutation: ${mutation}"
    fi
  done
done

# Other consumers lack production's equal-priority database admission policy.
# The provider patch must not leak into their chart values.
yq '.spec.values' "${release}" >"${scratch}/base-values.yaml"
"${helm_bin}" template velero "${scratch}/velero-${chart_version}.tgz" --namespace velero \
  --kube-version "${kube_version}" --values "${scratch}/base-values.yaml" >"${scratch}/base-rendered.yaml"
yq ea -o=json -I=0 'select(.kind == "DaemonSet" and .metadata.name == "node-agent")' \
  "${scratch}/base-rendered.yaml" |
  jq -e -s 'length == 1 and (.[0].spec.template.spec.priorityClassName // "") == ""' >/dev/null ||
  fail 'production priority leaked into the shared chart consumer'

printf 'PASS: real install/upgrade charts prioritize only the backup agent at the database-safe tier\n'
