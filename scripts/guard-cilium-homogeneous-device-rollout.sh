#!/usr/bin/env bash

set -euo pipefail

usage() {
  printf 'Usage: %s --before-publish|--after-deploy\n' "${0##*/}" >&2
  exit 2
}

[[ "$#" -eq 1 ]] || usage
readonly phase="$1"
case "${phase}" in
  --before-publish | --after-deploy) ;;
  *) usage ;;
esac

root_dir="${PLATFORM_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
readonly root_dir
readonly controllers_kustomization="${root_dir}/k8s/providers/hetzner/infrastructure/controllers/kustomization.yaml"
readonly component_kustomization="${root_dir}/k8s/providers/hetzner/infrastructure/controllers/cilium/components/homogeneous-devices/kustomization.yaml"
readonly kubectl_bin="${KUBECTL:-kubectl}"
readonly curl_bin="${CURL:-curl}"
readonly jq_bin="${JQ:-jq}"
readonly hcloud_api_url="${HCLOUD_API_URL:-https://api.hetzner.cloud/v1}"
readonly namespace='kube-system'
readonly deployment='cluster-autoscaler-hetzner-cluster-autoscaler'
readonly previous_replicas_annotation='platform.devantler.tech/cilium-device-rollout-previous-replicas'
readonly previous_replicas_jsonpath='{.metadata.annotations.platform\.devantler\.tech/cilium-device-rollout-previous-replicas}'
readonly rollout_wait_seconds="${ROLLOUT_WAIT_SECONDS:-120}"
readonly rollout_poll_seconds="${ROLLOUT_POLL_SECONDS:-2}"
readonly provider_stability_seconds="${PROVIDER_STABILITY_SECONDS:-30}"
readonly provider_poll_seconds="${PROVIDER_POLL_SECONDS:-5}"

fail() {
  printf 'ERROR: %s\n' "$1" >&2
  exit 1
}

[[ -f "${controllers_kustomization}" ]] ||
  fail "missing controllers kustomization: ${controllers_kustomization}"
[[ -f "${component_kustomization}" ]] ||
  fail "missing homogeneous-device component: ${component_kustomization}"
[[ "${rollout_wait_seconds}" =~ ^[0-9]+$ ]] ||
  fail "ROLLOUT_WAIT_SECONDS is not a non-negative integer: ${rollout_wait_seconds}"
[[ "${rollout_poll_seconds}" =~ ^[0-9]+([.][0-9]+)?$ ]] ||
  fail "ROLLOUT_POLL_SECONDS is not a non-negative number: ${rollout_poll_seconds}"
[[ "${provider_stability_seconds}" =~ ^[0-9]+$ ]] ||
  fail "PROVIDER_STABILITY_SECONDS is not a non-negative integer: ${provider_stability_seconds}"
[[ "${provider_poll_seconds}" =~ ^[0-9]+([.][0-9]+)?$ ]] ||
  fail "PROVIDER_POLL_SECONDS is not a non-negative number: ${provider_poll_seconds}"

kubectl_prod() {
  "${kubectl_bin}" --context admin@prod "$@"
}

rollout_gate_active=false
if grep -Eq \
  '^[[:space:]]*-[[:space:]]*cilium/components/homogeneous-devices/?[[:space:]]*(#.*)?$' \
  "${controllers_kustomization}" &&
  grep -Eq '^[[:space:]]*type:[[:space:]]*OnDelete[[:space:]]*(#.*)?$' \
    "${component_kustomization}"; then
  rollout_gate_active=true
fi

if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  printf 'active=%s\n' "${rollout_gate_active}" >>"${GITHUB_OUTPUT}"
fi

get_previous_replicas() {
  kubectl_prod -n "${namespace}" get deployment "${deployment}" \
    -o "jsonpath=${previous_replicas_jsonpath}"
}

get_current_replicas() {
  kubectl_prod -n "${namespace}" get deployment "${deployment}" \
    -o 'jsonpath={.spec.replicas}'
}

get_status_replicas() {
  local replicas
  replicas="$(
    kubectl_prod -n "${namespace}" get deployment "${deployment}" \
      -o 'jsonpath={.status.replicas}'
  )"
  printf '%s\n' "${replicas:-0}"
}

get_hcloud_autoscaler_ids() {
  [[ -n "${HCLOUD_TOKEN:-}" ]] ||
    fail 'HCLOUD_TOKEN is required to fence in-flight autoscaler additions'
  "${curl_bin}" --fail --silent --show-error --get \
    --header "Authorization: Bearer ${HCLOUD_TOKEN}" \
    --data-urlencode 'label_selector=cluster.autoscaler.nodeGroupLabel' \
    "${hcloud_api_url}/servers" |
    "${jq_bin}" -er '[.servers[].id | tostring] | sort | join(",")'
}

get_kubernetes_autoscaler_ids() {
  kubectl_prod get nodes -o json |
    "${jq_bin}" -er '
      [.items[]
        | select(.metadata.name | startswith("autoscale-"))
        | .spec.providerID
        | sub("^hcloud://"; "")]
      | sort
      | join(",")
    '
}

require_replica_count() {
  local value="$1"
  local description="$2"
  [[ "${value}" =~ ^[0-9]+$ ]] ||
    fail "${description} is not a non-negative integer: ${value:-<empty>}"
}

wait_for_replicas() {
  local expected="$1"
  kubectl_prod -n "${namespace}" rollout status \
    "deployment/${deployment}" --timeout=2m

  local requested actual deadline
  deadline=$((SECONDS + rollout_wait_seconds))
  while true; do
    requested="$(get_current_replicas)"
    actual="$(get_status_replicas)"
    if [[ "${requested}" == "${expected}" && "${actual}" == "${expected}" ]]; then
      return
    fi
    ((SECONDS < deadline)) ||
      fail "${deployment} replicas did not converge: requested=${requested:-<empty>} actual=${actual:-<empty>} expected=${expected}"
    sleep "${rollout_poll_seconds}"
  done
}

fence_provider_additions() {
  local expected_provider_ids="$1"
  local expected_node_ids="$2"
  local provider_ids node_ids deadline

  deadline=$((SECONDS + provider_stability_seconds))
  while true; do
    provider_ids="$(get_hcloud_autoscaler_ids)"
    node_ids="$(get_kubernetes_autoscaler_ids)"
    [[ "${provider_ids}" == "${expected_provider_ids}" ]] ||
      fail "Hetzner autoscaler servers changed during suspension: before=${expected_provider_ids:-<none>} after=${provider_ids:-<none>}"
    [[ "${node_ids}" == "${expected_node_ids}" ]] ||
      fail "Kubernetes autoscaler nodes changed during suspension: before=${expected_node_ids:-<none>} after=${node_ids:-<none>}"
    [[ "${provider_ids}" == "${node_ids}" ]] ||
      fail "an autoscaler server is still joining or leaving: provider=${provider_ids:-<none>} nodes=${node_ids:-<none>}"
    ((SECONDS >= deadline)) && return
    sleep "${provider_poll_seconds}"
  done
}

suspend_autoscaler() {
  local previous_replicas provider_ids node_ids
  previous_replicas="$(get_previous_replicas)"

  if [[ "${phase}" == '--before-publish' ]]; then
    provider_ids="$(get_hcloud_autoscaler_ids)"
    node_ids="$(get_kubernetes_autoscaler_ids)"
    [[ "${provider_ids}" == "${node_ids}" ]] ||
      fail "an autoscaler server is already joining or leaving: provider=${provider_ids:-<none>} nodes=${node_ids:-<none>}"
  fi

  if [[ -z "${previous_replicas}" ]]; then
    previous_replicas="$(get_current_replicas)"
    require_replica_count "${previous_replicas}" 'current autoscaler replica count'
    kubectl_prod -n "${namespace}" annotate deployment "${deployment}" \
      "${previous_replicas_annotation}=${previous_replicas}" --overwrite
  else
    require_replica_count "${previous_replicas}" 'remembered autoscaler replica count'
  fi

  kubectl_prod -n "${namespace}" scale deployment "${deployment}" --replicas=0
  wait_for_replicas 0
  if [[ "${phase}" == '--before-publish' ]]; then
    fence_provider_additions "${provider_ids}" "${node_ids}"
  fi
  printf 'Cilium homogeneous-device rollout gate active: Cluster Autoscaler is suspended.\n'
}

restore_autoscaler_if_owned() {
  local previous_replicas
  previous_replicas="$(get_previous_replicas)"
  if [[ -z "${previous_replicas}" ]]; then
    printf 'Cilium homogeneous-device rollout gate inactive: no owned autoscaler suspension to restore.\n'
    return
  fi

  require_replica_count "${previous_replicas}" 'remembered autoscaler replica count'
  kubectl_prod -n "${namespace}" scale deployment "${deployment}" \
    --replicas="${previous_replicas}"
  wait_for_replicas "${previous_replicas}"
  kubectl_prod -n "${namespace}" annotate deployment "${deployment}" \
    "${previous_replicas_annotation}-"
  printf 'Cilium homogeneous-device rollout gate released: Cluster Autoscaler restored to %s replicas.\n' \
    "${previous_replicas}"
}

if [[ "${rollout_gate_active}" == true ]]; then
  suspend_autoscaler
elif [[ "${phase}" == '--after-deploy' ]]; then
  restore_autoscaler_if_owned
else
  printf 'Cilium homogeneous-device rollout gate inactive: leaving Cluster Autoscaler unchanged before publish.\n'
fi
