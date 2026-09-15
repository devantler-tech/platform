#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly policy_dir="${root_dir}/k8s/bases/infrastructure/cluster-policies"
readonly kubescape_release="${root_dir}/k8s/bases/infrastructure/controllers/kubescape/helm-release.yaml"
readonly coroot="${root_dir}/k8s/bases/infrastructure/coroot/coroot.yaml"
readonly coroot_patch="${root_dir}/k8s/providers/hetzner/infrastructure/coroot/patches/enable-ha.yaml"
readonly coroot_db="${root_dir}/k8s/providers/hetzner/infrastructure/coroot/cluster.yaml"
readonly vault_snapshot="${root_dir}/k8s/bases/infrastructure/vault-backup/cron-job.yaml"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v yq >/dev/null 2>&1 || fail 'yq v4 is required'
command -v kubectl >/dev/null 2>&1 || fail 'kubectl is required'
command -v kyverno >/dev/null 2>&1 || fail 'Kyverno CLI is required'

policies="$(kubectl kustomize "${policy_dir}")" ||
  fail 'the cluster-policy bundle must render'
readonly policies

ssa_count="$(
  yq ea -N -r '
    [select(
      .kind == "ClusterPolicy" and
      .metadata.name == "add-default-deny" and
      .spec.useServerSideApply == true
    )] | length
  ' - <<<"${policies}"
)"
readonly ssa_count
[[ "${ssa_count}" == '1' ]] ||
  fail 'add-default-deny must use server-side apply for synchronized generated resources'

node_agent_tag="$(yq -er '.spec.values.nodeAgent.image.tag' "${kubescape_release}")" ||
  fail 'the Kubescape node-agent image tag is missing'
readonly node_agent_tag
[[ "${node_agent_tag}" == 'v0.3.219' ]] ||
  fail 'Kubescape node-agent must use the chart-supported v0.3.219 lifecycle fixes'

summary_workers="$(
  yq -er '.spec.values.storage.kindQueues.vulnerabilitymanifestsummaries.workerCount' \
    "${kubescape_release}"
)" || fail 'the vulnerability-manifest-summary queue worker count is missing'
readonly summary_workers
[[ "${summary_workers}" == '1' ]] ||
  fail 'vulnerability-manifest-summary writes must be serialized'

vex_capacity="$(
  yq -er '.spec.values.storage.kindQueues.openvulnerabilityexchangecontainers.maxObjectSize' \
    "${kubescape_release}"
)" || fail 'the VEX queue object-size limit is missing'
readonly vex_capacity
[[ "${vex_capacity}" == '1000000' ]] ||
  fail 'the VEX queue must admit valid records up to one megabyte'

coroot_node_agent_image="$(yq -er '.spec.nodeAgent.image.name' "${coroot}")" ||
  fail 'the Coroot node-agent image pin is missing'
readonly coroot_node_agent_image
[[ "${coroot_node_agent_image}" == 'ghcr.io/coroot/coroot-node-agent:1.35.8' ]] ||
  fail 'the Coroot node-agent must include the current upstream L7 and lifecycle fixes'

snapshot_success_history="$(yq -er '.spec.successfulJobsHistoryLimit' "${vault_snapshot}")" ||
  fail 'the OpenBao snapshot successful Job history limit is missing'
readonly snapshot_success_history
[[ "${snapshot_success_history}" == '1' ]] ||
  fail 'the OpenBao snapshot CronJob must retain only its latest successful Job object'

resolver_policy_count="$(
  yq ea -N -r '
    [select(
      .kind == "ClusterPolicy" and
      .metadata.name == "set-observability-dns-ndots" and
      (.spec.rules | length) == 2 and
      ([.spec.rules[] |
        select(
          (.match.any[0].resources.kinds | length) == 1 and
          .match.any[0].resources.kinds[0] == "Pod" and
          (.match.any[0].resources.namespaces | length) == 1 and
          .match.any[0].resources.namespaces[0] == "observability" and
          (.mutate.patchStrategicMerge.spec.dnsConfig.options | length) == 1 and
          .mutate.patchStrategicMerge.spec.dnsConfig.options[0].name == "ndots" and
          .mutate.patchStrategicMerge.spec.dnsConfig.options[0].value == "1"
        )] | length) == 2 and
      .spec.rules[0].match.any[0].resources.selector.matchLabels."app.kubernetes.io/part-of" == "coroot" and
      .spec.rules[0].match.any[0].resources.selector.matchLabels."app.kubernetes.io/managed-by" == "coroot-operator" and
      (.spec.rules[0].match.any[0].resources.selector.matchLabels | length) == 2 and
      .spec.rules[1].match.any[0].resources.selector.matchLabels."cnpg.io/cluster" == "coroot-db" and
      (.spec.rules[1].match.any[0].resources.selector.matchLabels | length) == 1
    )] | length
  ' - <<<"${policies}"
)"
readonly resolver_policy_count
[[ "${resolver_policy_count}" == '1' ]] ||
  fail 'the resolver policy must set ndots:1 only on Coroot and its CNPG database pods'

apply_resolver_policy() {
  local labels="$1"
  printf '%s\n' \
    'apiVersion: v1' \
    'kind: Pod' \
    'metadata:' \
    '  name: resolver-contract' \
    '  namespace: observability' \
    '  labels:' \
    "${labels}" \
    'spec:' \
    '  containers:' \
    '    - name: test' \
    '      image: registry.k8s.io/pause:3.10' |
    kyverno apply \
      "${policy_dir}/best-practices/set-observability-dns-ndots.yaml" \
      --resource - \
      --remove-color 2>/dev/null
}

coroot_mutation="$(apply_resolver_policy $'    app.kubernetes.io/part-of: coroot\n    app.kubernetes.io/managed-by: coroot-operator')" ||
  fail 'the Coroot resolver mutation could not be evaluated'
readonly coroot_mutation
[[ "${coroot_mutation}" == *'dnsConfig:'* && "${coroot_mutation}" == *'value: "1"'* ]] ||
  fail 'the Coroot selector must produce an effective ndots:1 Pod mutation'

database_mutation="$(apply_resolver_policy '    cnpg.io/cluster: coroot-db')" ||
  fail 'the Coroot database resolver mutation could not be evaluated'
readonly database_mutation
[[ "${database_mutation}" == *'dnsConfig:'* && "${database_mutation}" == *'value: "1"'* ]] ||
  fail 'the Coroot database selector must produce an effective ndots:1 Pod mutation'

unrelated_mutation="$(apply_resolver_policy '    app.kubernetes.io/part-of: coroot')" ||
  fail 'the unrelated Pod resolver case could not be evaluated'
readonly unrelated_mutation
[[ "${unrelated_mutation}" != *'dnsConfig:'* && "${unrelated_mutation}" == *'pass: 0'* ]] ||
  fail 'the resolver mutation must not change unrelated observability Pods'

authored_options_mutation="$(
  printf '%s\n' \
    'apiVersion: v1' \
    'kind: Pod' \
    'metadata:' \
    '  name: resolver-authored-options' \
    '  namespace: observability' \
    '  labels:' \
    '    app.kubernetes.io/part-of: coroot' \
    '    app.kubernetes.io/managed-by: coroot-operator' \
    'spec:' \
    '  dnsConfig:' \
    '    options:' \
    '      - name: single-request-reopen' \
    '  containers:' \
    '    - name: test' \
    '      image: registry.k8s.io/pause:3.10' |
    kyverno apply \
      "${policy_dir}/best-practices/set-observability-dns-ndots.yaml" \
      --resource - \
      --remove-color 2>/dev/null
)" || fail 'the authored resolver-options case could not be evaluated'
readonly authored_options_mutation
[[ "${authored_options_mutation}" == *'single-request-reopen'* &&
  "${authored_options_mutation}" != *'value: "1"'* &&
  "${authored_options_mutation}" == *'pass: 0'* ]] ||
  fail 'the resolver mutation must preserve an explicitly authored DNS options list'

postgres_host="$(yq -er '.spec.postgres.host' "${coroot_patch}")" ||
  fail 'the Coroot PostgreSQL host is missing'
readonly postgres_host
[[ "${postgres_host}" == 'coroot-db-rw' ]] ||
  fail 'Coroot must use the same-namespace PostgreSQL service name without search-path amplification'

rollout_annotation='platform.devantler.tech/dns-policy-rollout'
readonly rollout_annotation
rollout_token="$(yq -er ".spec.podAnnotations.\"${rollout_annotation}\"" "${coroot}")" ||
  fail 'the Coroot server DNS-policy rollout token is missing'
readonly rollout_token
[[ "${rollout_token}" == '2026-09-15-ndots-1' ]] ||
  fail 'the Coroot server must retain the first DNS-policy rollout token'

for component_path in \
  nodeAgent \
  prometheus \
  clickhouse \
  clickhouse.keeper; do
  component_token="$(
    yq -er ".spec.${component_path}.podAnnotations.\"${rollout_annotation}\"" "${coroot}"
  )" || fail "the ${component_path} DNS-policy rollout token is missing"
  [[ "${component_token}" == "${rollout_token}" ]] ||
    fail "the ${component_path} DNS-policy rollout token must match the server token"
done

cluster_agent_rollout_token="$(
  yq -er ".spec.clusterAgent.podAnnotations.\"${rollout_annotation}\"" "${coroot}"
)" || fail 'the clusterAgent DNS-policy rollout token is missing'
readonly cluster_agent_rollout_token
[[ "${cluster_agent_rollout_token}" == '2026-09-15-ndots-2' ]] ||
  fail 'the clusterAgent must use the post-policy rollout token'

database_rollout_token="$(
  yq -er ".spec.inheritedMetadata.annotations.\"${rollout_annotation}\"" "${coroot_db}"
)" || fail 'the Coroot database DNS-policy rollout token is missing'
readonly database_rollout_token
[[ "${database_rollout_token}" == "${rollout_token}" ]] ||
  fail 'the Coroot database DNS-policy rollout token must match every Coroot component'

database_restart_request="$(
  yq -er '.metadata.annotations."kubectl.kubernetes.io/restartedAt"' "${coroot_db}"
)" || fail 'the Coroot database declarative restart request is missing'
readonly database_restart_request
[[ "${database_restart_request}" == '2026-09-14T22:48:00Z' ]] ||
  fail 'the Coroot database must request the reviewed post-policy rolling restart'

printf 'Coroot alert remediation contract is valid.\n'
