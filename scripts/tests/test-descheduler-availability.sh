#!/usr/bin/env bash

# Render the real chart: a replica count alone cannot prove safe failover.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
release="${DESCHEDULER_RELEASE_FILE:-${root_dir}/k8s/providers/hetzner/infrastructure/controllers/descheduler/helm-release.yaml}"
repository="${root_dir}/k8s/providers/hetzner/infrastructure/controllers/descheduler/helm-repository.yaml"
scratch="$(mktemp -d)"
readonly root_dir release repository scratch
trap 'rm -rf "${scratch}"' EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
for tool in helm jq kubectl kyverno yq; do
  command -v "${tool}" >/dev/null || fail "${tool} is required"
done

chart="$(yq -r '.spec.chart.spec.chart' "${release}")"
version="$(yq -r '.spec.chart.spec.version' "${release}")"
url="$(yq -r '.spec.url' "${repository}")"
yq -o=json '.spec.values' "${release}" >"${scratch}/values.json"
helm template descheduler "${chart}" --repo "${url}" --version "${version}" \
  --namespace kube-system --values "${scratch}/values.json" >"${scratch}/resources.yaml"

renderer_count="$(yq '.spec.postRenderers | length' "${release}")"
for ((index = 0; index < renderer_count; index++)); do
  yq -o=json ".spec.postRenderers[${index}].kustomize" "${release}" >"${scratch}/renderer.json"
  jq -n --slurpfile renderer "${scratch}/renderer.json" \
    '{apiVersion: "kustomize.config.k8s.io/v1beta1", kind: "Kustomization",
      resources: ["resources.yaml"]} + $renderer[0]' >"${scratch}/kustomization.yaml"
  kubectl kustomize "${scratch}" >"${scratch}/next.yaml"
  mv "${scratch}/next.yaml" "${scratch}/resources.yaml"
done

yq ea -o=json '[.]' "${scratch}/resources.yaml" >"${scratch}/resources.json"
jq -e '[.[] | select(.kind == "Deployment" and .metadata.name == "descheduler") |
  .spec.replicas >= 2] == [true]' "${scratch}/resources.json" >/dev/null ||
  fail 'descheduler needs a standby replica for maintenance availability'

jq -e '[.[] | select(.kind == "Deployment" and .metadata.name == "descheduler") |
  .spec.template.spec.containers[] | select(.name == "descheduler") | .args | (
    contains(["--leader-elect=true", "--leader-elect-resource-lock=leases",
      "--leader-elect-resource-name=descheduler",
      "--leader-elect-resource-namespace=kube-system"]) and
    (any(.[]; startswith("--dry-run")) | not)
  )] == [true]' "${scratch}/resources.json" >/dev/null ||
  fail 'descheduler replicas must use one shared leader-election lease'

jq -e '[.[] | select(.kind == "Deployment" and .metadata.name == "descheduler") |
  .spec.template.spec.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[]? |
  select(.topologyKey == "kubernetes.io/hostname" and
    .labelSelector.matchLabels == {"app.kubernetes.io/name": "descheduler",
      "app.kubernetes.io/instance": "descheduler"})] | length == 1' \
  "${scratch}/resources.json" >/dev/null || fail 'the standby must not share a node with the active replica'

# Bind the lease permissions to the account the Deployment actually runs as.
jq -e '
  . as $resources |
  [.[] | select(.kind == "Deployment" and .metadata.name == "descheduler") |
    .spec.template.spec.serviceAccountName] as $accounts |
  [.[] | select(.kind == "ClusterRoleBinding") |
    select(any(.subjects[]?; .kind == "ServiceAccount" and
      .namespace == "kube-system" and .name == $accounts[0])) |
    .roleRef | select(.kind == "ClusterRole") | .name] as $roles |
  ["get", "create", "update"] | all(. as $verb |
    any($resources[]; .kind == "ClusterRole" and
      (.metadata.name as $name | $roles | index($name)) != null and
      any(.rules[]?;
        (.apiGroups | index("coordination.k8s.io")) != null and
        (.resources | index("leases")) != null and
        (.verbs | index($verb)) != null and
        (if $verb == "create" then (.resourceNames // [] | length) == 0
         else (.resourceNames // ["descheduler"] | index("descheduler")) != null end))))
' "${scratch}/resources.json" >/dev/null || fail 'the running account needs lease get/create/update permissions'

# A skip is not a fix: require this non-exempt Deployment to pass the real rule.
yq 'select(.kind == "Deployment" and .metadata.name == "descheduler")' \
  "${scratch}/resources.yaml" >"${scratch}/deployment.yaml"
kyverno apply "${root_dir}/k8s/bases/infrastructure/cluster-policies/best-practices/validate-replica-floor.yaml" \
  --resource "${scratch}/deployment.yaml" --warn-no-pass --warn-exit-code 1 ||
  fail 'the rendered descheduler must pass the replica-floor policy, not skip it'

printf 'PASS: descheduler renders redundant replicas with shared lease election and passes the replica floor\n'
