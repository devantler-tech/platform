#!/usr/bin/env bash

set -euo pipefail

usage() {
  printf 'Usage: %s --before-publish|--after-revision-ready|--after-deploy\n' "${0##*/}" >&2
  exit 2
}

[[ "$#" -eq 1 ]] || usage
readonly phase="$1"
case "${phase}" in
  --before-publish | --after-revision-ready | --after-deploy) ;;
  *) usage ;;
esac

root_dir="${PLATFORM_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
readonly root_dir
readonly controllers_dir="${root_dir}/k8s/providers/hetzner/infrastructure/controllers"
readonly production_bootstrap_dir="${root_dir}/k8s/clusters/prod/bootstrap"
readonly controllers_kustomization="${root_dir}/k8s/providers/hetzner/infrastructure/controllers/kustomization.yaml"
readonly component_kustomization="${root_dir}/k8s/providers/hetzner/infrastructure/controllers/cilium/components/homogeneous-devices/kustomization.yaml"
readonly kubectl_bin="${KUBECTL:-kubectl}"
readonly curl_bin="${CURL:-curl}"
readonly jq_bin="${JQ:-jq}"
readonly yq_bin="${YQ:-yq}"
readonly shasum_bin="${SHASUM:-shasum}"
readonly hcloud_api_url="${HCLOUD_API_URL:-https://api.hetzner.cloud/v1}"
readonly namespace='kube-system'
readonly deployment='cluster-autoscaler-hetzner-cluster-autoscaler'
readonly cilium_daemonset='cilium'
readonly cilium_selector='k8s-app=cilium'
readonly previous_replicas_annotation='platform.devantler.tech/cilium-device-rollout-previous-replicas'
readonly previous_replicas_jsonpath='{.metadata.annotations.platform\.devantler\.tech/cilium-device-rollout-previous-replicas}'
readonly approved_template_annotation='platform.devantler.tech/cilium-device-rollout-approved-template-sha'
readonly approved_template_jsonpath='{.metadata.annotations.platform\.devantler\.tech/cilium-device-rollout-approved-template-sha}'
readonly rollout_wait_seconds="${ROLLOUT_WAIT_SECONDS:-120}"
readonly rollout_poll_seconds="${ROLLOUT_POLL_SECONDS:-2}"
readonly provider_stability_seconds="${PROVIDER_STABILITY_SECONDS:-30}"
readonly provider_poll_seconds="${PROVIDER_POLL_SECONDS:-5}"
readonly revision_ready="${CILIUM_ROLLOUT_REVISION_READY:-false}"

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
[[ "${revision_ready}" == true || "${revision_ready}" == false ]] ||
  fail "CILIUM_ROLLOUT_REVISION_READY must be true or false: ${revision_ready}"

kubectl_prod() {
  "${kubectl_bin}" --context admin@prod "$@"
}

component_active=false
if grep -Eq \
  '^[[:space:]]*-[[:space:]]*cilium/components/homogeneous-devices/?[[:space:]]*(#.*)?$' \
  "${controllers_kustomization}"; then
  component_active=true
fi

rollout_gate_active=false
if [[ "${component_active}" == true ]] &&
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

get_approved_template_sha() {
  kubectl_prod -n "${namespace}" get deployment "${deployment}" \
    -o "jsonpath=${approved_template_jsonpath}"
}

candidate_template_sha() {
  local cilium_spec substitution_sources
  # shellcheck disable=SC2016
  cilium_spec="$(
    "${kubectl_bin}" kustomize "${controllers_dir}" |
      "${yq_bin}" -o=json -I=0 '.' - |
      "${jq_bin}" -csS --arg namespace "${namespace}" '
        [
          .[]
          | select(
              .apiVersion == "helm.toolkit.fluxcd.io/v2" and
              .kind == "HelmRelease" and
              .metadata.namespace == $namespace and
              .metadata.name == "cilium"
            )
          | .spec
          | del(.values.updateStrategy, .upgrade.disableWait)
        ]
        | if length == 1 then
          .[0]
        else
          error("expected exactly one rendered Cilium HelmRelease")
        end
      '
  )"
  substitution_sources="$(
    "${kubectl_bin}" kustomize "${production_bootstrap_dir}" |
      "${yq_bin}" -o=json -I=0 '
        select(
          .metadata.namespace == "flux-system" and
          (
            (
              .kind == "ConfigMap" and
              (
                .metadata.name == "variables-base" or
                .metadata.name == "variables-cluster"
              )
            ) or
            (
              .kind == "Secret" and
              .metadata.name == "variables-cluster"
            )
          )
        )
      ' - |
      "${jq_bin}" -csS '
        map({
          rank: (
            if .kind == "ConfigMap" and .metadata.name == "variables-base" then 0
            elif .kind == "ConfigMap" then 1
            else 2
            end
          ),
          values: (.data // .stringData // {})
        })
        | if length == 3 and ([.[].rank] | sort == [0, 1, 2]) then
            .
          else
            error("expected all three ordered Flux substitution sources")
          end
      '
  )"

  # Bind only substitutions referenced by Cilium, in Flux's documented
  # base ConfigMap -> cluster ConfigMap -> cluster Secret precedence. The raw
  # spec remains part of the hash, so placeholder defaults are covered too.
  # shellcheck disable=SC2016
  "${jq_bin}" -cnS \
    --argjson spec "${cilium_spec}" \
    --argjson sources "${substitution_sources}" '
      ([
        $spec
        | ..
        | strings
        | scan("\\$\\{([A-Za-z_][A-Za-z0-9_]*)(?::[-=][^}]*)?\\}")
        | .[0]
      ] | unique) as $keys
      | (
          $sources
          | sort_by(.rank)
          | reduce .[] as $source ({}; . * $source.values)
        ) as $resolved
      | {
          spec: $spec,
          substitutions: (
            $resolved
            | with_entries(
                select(.key as $key | $keys | index($key))
              )
          )
        }
    ' |
    "${shasum_bin}" -a 256 |
    awk '{print $1}'
}

record_approved_template_sha() {
  local approved_sha
  approved_sha="$(candidate_template_sha)"
  [[ "${approved_sha}" =~ ^[0-9a-f]{64}$ ]] ||
    fail "candidate Cilium template hash is invalid: ${approved_sha:-<empty>}"
  kubectl_prod -n "${namespace}" annotate deployment "${deployment}" \
    "${approved_template_annotation}=${approved_sha}" --overwrite
}

require_candidate_matches_approved_template() {
  local approved_sha candidate_sha
  approved_sha="$(get_approved_template_sha)"
  [[ "${approved_sha}" =~ ^[0-9a-f]{64}$ ]] ||
    fail 'the active Cilium rollout has no approved template hash'
  candidate_sha="$(candidate_template_sha)"
  [[ "${candidate_sha}" =~ ^[0-9a-f]{64}$ ]] ||
    fail "candidate Cilium template hash is invalid: ${candidate_sha:-<empty>}"
  [[ "${candidate_sha}" == "${approved_sha}" ]] ||
    fail "gate removal changes the approved Cilium template: approved=${approved_sha} candidate=${candidate_sha}"
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
    --data-urlencode 'label_selector=hcloud/node-group' \
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

  # A remembered count of ZERO cannot be "restored". Honouring it leaves the
  # Deployment at zero — which `ksail cluster update` waits on and KSail treats
  # as never-ready — and the restore below would then DELETE the ownership
  # annotation, destroying the state a retry needs.
  #
  # The refusal lives HERE, inside the release itself, rather than in one phase:
  # the normal deploy's always() post-deploy reassert and the DR workflow both
  # release exclusively through this function, so a refusal guarding only
  # --after-revision-ready would be undone moments later by --after-deploy.
  #
  # The remediation must change the REMEMBERED value, not just the running
  # replica count: get_previous_replicas reads the annotation, so scaling the
  # Deployment alone leaves the same 0 recorded and the next attempt fails here
  # again. Both commands pin admin@prod — every mutation this guard performs
  # does, and an operator pastes these from whatever context they happen to be
  # on.
  if [[ "${previous_replicas}" == "0" ]]; then
    fail "the rollout gate owns a remembered autoscaler count of 0, so releasing it cannot satisfy the readiness check that ksail cluster update performs (KSail treats a zero-replica Deployment as never-ready). Record the intended count and scale to match, then retry: kubectl --context admin@prod -n ${namespace} annotate deployment ${deployment} ${previous_replicas_annotation}=<count> --overwrite && kubectl --context admin@prod -n ${namespace} scale deployment ${deployment} --replicas=<count>. Scaling alone is not enough — the remembered value is what this check reads."
  fi

  require_replica_count "${previous_replicas}" 'remembered autoscaler replica count'
  kubectl_prod -n "${namespace}" scale deployment "${deployment}" \
    --replicas="${previous_replicas}"
  wait_for_replicas "${previous_replicas}"
  kubectl_prod -n "${namespace}" annotate deployment "${deployment}" \
    "${previous_replicas_annotation}-"
  kubectl_prod -n "${namespace}" annotate deployment "${deployment}" \
    "${approved_template_annotation}-"
  printf 'Cilium homogeneous-device rollout gate released: Cluster Autoscaler restored to %s replicas.\n' \
    "${previous_replicas}"
}

require_cilium_fleet_current() {
  local daemonset_json pods_json generation observed desired scheduled updated ready pod_count stale_nodes
  daemonset_json="$(
    kubectl_prod -n "${namespace}" get daemonset "${cilium_daemonset}" -o json
  )"
  pods_json="$(
    kubectl_prod -n "${namespace}" get pods -l "${cilium_selector}" -o json
  )"
  generation="$(
    "${jq_bin}" -er '.metadata.generation | tostring' <<<"${daemonset_json}"
  )" || fail 'the Cilium DaemonSet has no observable template generation'
  observed="$(
    "${jq_bin}" -er '.status.observedGeneration | tostring' <<<"${daemonset_json}"
  )" || fail 'the Cilium DaemonSet controller has not observed a generation'
  desired="$(
    "${jq_bin}" -er '.status.desiredNumberScheduled | tostring' <<<"${daemonset_json}"
  )" || fail 'the Cilium DaemonSet has no desired-agent count'
  scheduled="$(
    "${jq_bin}" -er '.status.currentNumberScheduled | tostring' <<<"${daemonset_json}"
  )" || fail 'the Cilium DaemonSet has no scheduled-agent count'
  updated="$(
    "${jq_bin}" -er '.status.updatedNumberScheduled | tostring' <<<"${daemonset_json}"
  )" || fail 'the Cilium DaemonSet has no updated-agent count'
  ready="$(
    "${jq_bin}" -er '.status.numberReady | tostring' <<<"${daemonset_json}"
  )" || fail 'the Cilium DaemonSet has no ready-agent count'
  pod_count="$(
    "${jq_bin}" -er '.items | length' <<<"${pods_json}"
  )" || fail 'the Cilium pod inventory is unreadable'
  stale_nodes="$(
    # shellcheck disable=SC2016
    "${jq_bin}" -er --arg generation "${generation}" '
      [.items[]
        | select(
            .metadata.labels["pod-template-generation"] != $generation or
            ([.status.containerStatuses[]?
              | select(.name == "cilium-agent" and .ready == true)]
              | length) != 1
          )
        | .spec.nodeName]
      | sort
      | join(",")
    ' <<<"${pods_json}"
  )" || fail 'the Cilium pod template generations are unreadable'

  require_replica_count "${generation}" 'Cilium DaemonSet generation'
  require_replica_count "${observed}" 'observed Cilium DaemonSet generation'
  require_replica_count "${desired}" 'desired Cilium agent count'
  require_replica_count "${scheduled}" 'scheduled Cilium agent count'
  require_replica_count "${updated}" 'updated Cilium agent count'
  require_replica_count "${ready}" 'ready Cilium agent count'
  require_replica_count "${pod_count}" 'observed Cilium pod count'
  ((desired > 0)) ||
    fail 'Cilium rollout cannot complete with zero desired agents'
  [[ "${observed}" == "${generation}" && "${scheduled}" == "${desired}" &&
    "${updated}" == "${desired}" &&
    "${pod_count}" == "${desired}" &&
    "${ready}" == "${desired}" ]] ||
    fail "Cilium rollout is incomplete for DaemonSet generation ${generation}: observed=${observed} desired=${desired} scheduled=${scheduled} updated=${updated} pods=${pod_count} ready=${ready} stale_nodes=${stale_nodes:-<none>}"
}

if [[ "${rollout_gate_active}" == true ]]; then
  # An active gate keeps autoscaling suspended in every phase, including the
  # post-revision one: the whole point is that no node may join while agents are
  # being stepped.
  suspend_autoscaler
  if [[ "${phase}" == '--after-deploy' && "${revision_ready}" == true ]]; then
    record_approved_template_sha
  fi
elif [[ "${phase}" == '--after-revision-ready' ]]; then
  # The release artifact has reconciled and Flux reports this exact revision
  # Ready, so the suspension the retired gate owned has served its purpose.
  # Restore it HERE, before `ksail cluster update`, because that step waits for
  # the cluster-autoscaler Deployment to become ready and KSail treats
  # `Status.Replicas == 0` as never-ready — it polls to its deadline and fails
  # the very deploy that releases the gate. Restoring in --after-deploy is too
  # late for that wait, and restoring in --before-publish would do it before the
  # safe artifact is deployed, which the gate deliberately forbids.
  #
  # A remembered count of ZERO is refused inside restore_autoscaler_if_owned,
  # so it covers this phase and every other release path alike.
  restore_autoscaler_if_owned
elif [[ "${phase}" == '--after-deploy' ]]; then
  restore_autoscaler_if_owned
elif [[ -n "$(get_previous_replicas)" ]]; then
  if [[ "${component_active}" == true ]]; then
    require_candidate_matches_approved_template
    require_cilium_fleet_current
    printf 'Cilium homogeneous-device rollout is complete at the current DaemonSet generation; gate removal may publish.\n'
  else
    printf 'Cilium homogeneous-device rollback may publish while Cluster Autoscaler remains suspended.\n'
  fi
else
  printf 'Cilium homogeneous-device rollout gate inactive: leaving Cluster Autoscaler unchanged before publish.\n'
fi
