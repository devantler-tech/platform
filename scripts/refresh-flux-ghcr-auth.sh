#!/usr/bin/env bash
# Refresh the KSail-managed root Flux pull Secret from the Git/SOPS source.
#
# Flux cannot fetch the artifact containing a rotated credential while its
# bootstrap Secret is stale. Keep this bridge outside Flux so a deployment can
# repair that bootstrap edge before asking Flux to reconcile.

set -euo pipefail
set +x

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
# shellcheck source=scripts/ghcr-auth-lib.sh
source "${SCRIPT_DIR}/ghcr-auth-lib.sh"
# shellcheck source=scripts/refresh-flux-ghcr-auth-safety.sh
source "${SCRIPT_DIR}/refresh-flux-ghcr-auth-safety.sh"
require_flux_ghcr_yaml_tool

check_only=false
allow_incomplete_fanout=false
if (($# > 1)); then
  echo "Usage: $0 [--check-only|--allow-incomplete-fanout]" >&2
  exit 64
fi
if (($# == 1)); then
  case "$1" in
    --check-only) check_only=true ;;
    --allow-incomplete-fanout) allow_incomplete_fanout=true ;;
    *)
      echo "Usage: $0 [--check-only|--allow-incomplete-fanout]" >&2
      exit 64
      ;;
  esac
fi

readonly SECRET_FILE="${FLUX_GHCR_SECRET_FILE:-k8s/bases/bootstrap/secret.enc.yaml}"
readonly KUBE_CONTEXT="${KUBE_CONTEXT:-admin@prod}"
readonly SYNC_ATTEMPTS="${FLUX_GHCR_SYNC_ATTEMPTS:-60}"
readonly SYNC_INTERVAL="${FLUX_GHCR_SYNC_INTERVAL:-2}"
readonly TALOS_CONVERGENCE_ATTEMPTS="${FLUX_GHCR_TALOS_CONVERGENCE_ATTEMPTS:-${SYNC_ATTEMPTS}}"
readonly DRAIN_TIMEOUT="${FLUX_GHCR_DRAIN_TIMEOUT:-45m}"
readonly RUNTIME_PROBE_CREATE_ATTEMPTS=3
readonly SYNC_LEASE_NAME="ghcr-auth-refresh"
readonly SYNC_LEASE_DURATION_SECONDS=120
readonly SYNC_LEASE_HEARTBEAT_SECONDS=30
readonly CORDON_OWNER_ANNOTATION="platform.devantler.tech/ghcr-auth-drain-owner"
readonly CORDON_OWNER_JSON_PATH="/metadata/annotations/platform.devantler.tech~1ghcr-auth-drain-owner"
readonly CORDON_RECOVERY_ANNOTATION="platform.devantler.tech/ghcr-auth-drain-recovery"
readonly CORDON_RECOVERY_JSON_PATH="/metadata/annotations/platform.devantler.tech~1ghcr-auth-drain-recovery"
KSAIL_OPERATOR_VERSION="$(yq -er '.spec.chart.spec.version' \
  k8s/bases/infrastructure/controllers/ksail-operator/helm-release.yaml)"
readonly KSAIL_OPERATOR_VERSION
readonly KSAIL_OPERATOR_IMAGE="ghcr.io/devantler-tech/ksail:v${KSAIL_OPERATOR_VERSION}"
# Both tenant release workflows create/update latest alongside every semver
# artifact and image tag. Flux still selects the signed semver artifact; latest
# is the stable read-permission/existence probe for the same private packages.
readonly -a REQUIRED_PULL_TARGETS=(
  "devantler-tech/platform/manifests:latest"
  "devantler-tech/wedding-app/manifests:latest"
  "devantler-tech/ascoachingogvaner/manifests:latest"
  "devantler-tech/wedding-app:latest"
  "devantler-tech/ascoachingogvaner:latest"
  "devantler-tech/ksail:v${KSAIL_OPERATOR_VERSION}"
  "devantler-tech/provider-upjet-unifi:v0.1.0"
)
# These packages are intentionally private and have independent ACLs. A public
# image (including KSail itself) can prove registry reachability but cannot
# prove that containerd loaded a working credential.
readonly -a RUNTIME_CREDENTIAL_PROBE_IMAGES=(
  "ghcr.io/devantler-tech/wedding-app:latest"
  "ghcr.io/devantler-tech/ascoachingogvaner:latest"
)
readonly -a FANOUT_NAMESPACES=(
  "wedding-app"
  "ascoachingogvaner"
  "kyverno"
)

if ! [[ "${SYNC_ATTEMPTS}" =~ ^[1-9][0-9]*$ ]] \
  || ! [[ "${TALOS_CONVERGENCE_ATTEMPTS}" =~ ^[3-9]$|^[1-9][0-9]+$ ]] \
  || ! [[ "${SYNC_INTERVAL}" =~ ^[0-9]+([.][0-9]+)?$ ]] \
  || ! [[ "${DRAIN_TIMEOUT}" =~ ^[1-9][0-9]*(s|m|h)$ ]]; then
  echo "::error::FLUX_GHCR_SYNC_ATTEMPTS must be positive, FLUX_GHCR_TALOS_CONVERGENCE_ATTEMPTS must be at least 3, FLUX_GHCR_SYNC_INTERVAL must be non-negative, and FLUX_GHCR_DRAIN_TIMEOUT must be a positive whole number of seconds, minutes, or hours."
  exit 64
fi

work_dir="$(mktemp -d)"
chmod 700 "${work_dir}"
umask 077
active_runtime_probe=""
bootstrap_cordon_dir="${work_dir}/bootstrap-cordons"
bootstrap_retain_dir="${work_dir}/bootstrap-retain"
bootstrap_ordered_targets="${work_dir}/bootstrap-ordered-targets.tsv"
bootstrap_overlap_result="${work_dir}/bootstrap-overlap-result.txt"
bootstrap_seed_uid=""
mkdir -p "${bootstrap_cordon_dir}" "${bootstrap_retain_dir}"

cleanup_refresh_work() {
  local original_status=$?
  local cleanup_status=0

  trap - EXIT

  if [[ -n "${active_runtime_probe}" ]]; then
    kubectl \
      --context "${KUBE_CONTEXT}" \
      --namespace ksail-operator \
      delete pod "${active_runtime_probe}" \
      --ignore-not-found \
      --wait=false \
      >/dev/null 2>&1 || true
  fi
  if ! cleanup_bootstrap_quarantine; then
    cleanup_status=1
    echo "::error::Bootstrap quarantine cleanup was incomplete; durable recovery annotations remain on the affected nodes."
  fi
  if declare -F release_sync_lease >/dev/null \
    && [[ "${sync_lease_acquired:-false}" == "true" ]] \
    && ! release_sync_lease; then
    cleanup_status=1
    echo "::error::Could not safely release the GHCR synchronization lease."
  fi
  rm -rf "${work_dir}"
  if ((original_status == 0 && cleanup_status != 0)); then
    exit 1
  fi
  exit "${original_status}"
}
trap cleanup_refresh_work EXIT

docker_config="${work_dir}/config.json"
credentials_file="${work_dir}/credentials.json"
basic_curl_config="${work_dir}/curl-basic.config"
bearer_curl_config="${work_dir}/curl-bearer.config"
token_response="${work_dir}/token.json"
current_root_secret_file="${work_dir}/current-root-secret.json"
current_root_docker_config="${work_dir}/current-root-config.json"
current_root_credentials_file="${work_dir}/current-root-credentials.json"
current_root_basic_curl_config="${work_dir}/current-root-curl-basic.config"
current_root_token_response="${work_dir}/current-root-token.json"
current_root_bearer_curl_config="${work_dir}/current-root-curl-bearer.config"
patch_file="${work_dir}/patch.json"
variables_patch_file="${work_dir}/variables-patch.json"
expected_normalized="${work_dir}/expected-normalized.json"
fanout_api_resources="${work_dir}/fanout-api-resources.txt"
talos_auth_patch_file="${work_dir}/talos-registry-auth.json"
talos_revision_patch_file="${work_dir}/talos-registry-revision.json"
talos_result_file="${work_dir}/talos-result.txt"
drain_result_file="${work_dir}/drain-result.txt"
reboot_result_file="${work_dir}/reboot-result.txt"
cordon_state_file="${work_dir}/cordon-state.json"
cordon_claim_patch_file="${work_dir}/cordon-claim-patch.json"
cordon_release_patch_file="${work_dir}/cordon-release-patch.json"
cordon_recovery_patch_file="${work_dir}/cordon-recovery-patch.json"
talos_nodes_file="${work_dir}/talos-nodes.json"
talos_node_targets="${work_dir}/talos-node-targets.tsv"
talos_pending_targets="${work_dir}/talos-pending-targets.tsv"
talos_processed_targets="${work_dir}/talos-processed-targets.tsv"
talos_stage_result_file="${work_dir}/talos-stage-result.txt"
runtime_probe_nodes_file="${work_dir}/runtime-probe-nodes.json"
runtime_probe_targets_file="${work_dir}/runtime-probe-targets.tsv"
runtime_proved_targets_file="${work_dir}/runtime-proved-targets.txt"
runtime_probe_manifest_file="${work_dir}/runtime-probe-pod.json"
runtime_probe_state_file="${work_dir}/runtime-probe-state.json"
runtime_probe_result_file="${work_dir}/runtime-probe-result.txt"
recovery_nodes_file="${work_dir}/recovery-nodes.json"
recovery_node_file="${work_dir}/recovery-node.json"
recovery_targets_file="${work_dir}/recovery-targets.jsonl"
recovery_record_file="${work_dir}/recovery-record.json"
recovery_blocked_owners_file="${work_dir}/recovery-blocked-owners.txt"
sync_lease_file="${work_dir}/sync-lease.json"
sync_lease_manifest_file="${work_dir}/sync-lease-manifest.json"
sync_lease_patch_file="${work_dir}/sync-lease-patch.json"
sync_lease_result_file="${work_dir}/sync-lease-result.txt"
sync_lease_lost_file="${work_dir}/sync-lease-lost"
root_secret_state_file="${work_dir}/root-secret-state.json"
root_secret_cas_patch_file="${work_dir}/root-secret-cas-patch.json"
variables_secret_state_file="${work_dir}/variables-secret-state.json"
variables_secret_cas_patch_file="${work_dir}/variables-secret-cas-patch.json"
sync_lease_holder=""
sync_lease_acquired=false
sync_lease_heartbeat_pid=""
runtime_probe_sequence=0
runtime_probe_bootstrap_needed=0

# Force an ESO resource to reconcile and observe a post-annotation Ready edge.
force_sync_resource() {
  local kind="$1"
  local namespace="$2"
  local name="$3"
  local before_file="${work_dir}/${kind}-${namespace}-${name}-before.json"
  local annotated_file="${work_dir}/${kind}-${namespace}-${name}-annotated.json"
  local current_file="${work_dir}/${kind}-${namespace}-${name}-current.json"
  local before_refresh
  local annotated_resource_version
  local attempt
  local stamp

  kubectl \
    --context "${KUBE_CONTEXT}" \
    --namespace "${namespace}" \
    get "${kind}" "${name}" \
    -o json \
    > "${before_file}"
  before_refresh="$(jq -r '.status.refreshTime // ""' "${before_file}")"
  stamp="$(date -u +%Y%m%dT%H%M%SZ)-$$-${RANDOM}"

  kubectl \
    --context "${KUBE_CONTEXT}" \
    --namespace "${namespace}" \
    annotate "${kind}" "${name}" \
    "force-sync=${stamp}" \
    --overwrite \
    -o json \
    > "${annotated_file}"
  annotated_resource_version="$(jq -er '.metadata.resourceVersion' \
    "${annotated_file}")"

  for ((attempt = 1; attempt <= SYNC_ATTEMPTS; attempt++)); do
    kubectl \
      --context "${KUBE_CONTEXT}" \
      --namespace "${namespace}" \
      get "${kind}" "${name}" \
      -o json \
      > "${current_file}"
    if jq -e \
      --arg before "${before_refresh}" \
      --arg annotated_resource_version "${annotated_resource_version}" '
      (.status.refreshTime // "") as $refresh
      | (($refresh != "" and $refresh != $before)
          or ((.metadata.resourceVersion // "") != ""
            and .metadata.resourceVersion != $annotated_resource_version))
        and any(.status.conditions[]?;
          .type == "Ready" and .status == "True")
    ' "${current_file}" >/dev/null; then
      return 0
    fi
    sleep "${SYNC_INTERVAL}"
  done

  echo "::error::Timed out waiting for ${kind}/${namespace}/${name} to complete the forced GHCR credential sync."
  return 1
}

# Verify that a namespace's materialized GHCR Secret matches the SOPS source.
verify_consumer_secret() {
  local namespace="$1"
  local secret_file="${work_dir}/consumer-${namespace}.json"
  local decoded_file="${work_dir}/consumer-${namespace}-decoded.json"
  local normalized_file="${work_dir}/consumer-${namespace}-normalized.json"

  kubectl \
    --context "${KUBE_CONTEXT}" \
    --namespace "${namespace}" \
    get secret ghcr-auth \
    -o json \
    > "${secret_file}"
  if ! jq -er '.data[".dockerconfigjson"] | @base64d' \
    "${secret_file}" \
    > "${decoded_file}" 2>/dev/null \
    || ! jq -S -c . "${decoded_file}" > "${normalized_file}" 2>/dev/null \
    || ! cmp -s "${expected_normalized}" "${normalized_file}"; then
    echo "::error::ExternalSecret ${namespace}/ghcr-auth did not materialise the Git/SOPS GHCR credential."
    return 1
  fi
}

# Emit bounded, printable output only from operations that cannot contain the
# registry credential. Prefix each line so it cannot become a workflow command.
emit_safe_operation_output() {
  local label="$1" result_file="$2"
  [[ -s "${result_file}" ]] || return 0

  LC_ALL=C tr -cd '\11\12\40-\176' < "${result_file}" \
    | tail -n 50 \
    | sed -e "s/^/${label}: /" >&2 \
    || true
}

# Prove a Docker credential with real manifest reads for every package this
# deployment can pull. Callers provide mode-0600 curl config/temp paths so the
# credential never appears in argv or output. This serves both the incoming
# SOPS credential and the still-live root credential whose overlap keeps peers
# safe while the first stale node drains.
verify_ghcr_pull_credential() {
  local basic_config="$1"
  local token_file="$2"
  local bearer_config="$3"
  local credential_label="$4"
  local target repository reference http_status

  for target in "${REQUIRED_PULL_TARGETS[@]}"; do
    repository="${target%:*}"
    reference="${target##*:}"
    if ! http_status="$(curl --disable \
      --config "${basic_config}" \
      --connect-timeout 10 \
      --max-time 60 \
      --silent \
      --show-error \
      --output "${token_file}" \
      --write-out '%{http_code}' \
      --get \
      --data-urlencode 'service=ghcr.io' \
      --data-urlencode "scope=repository:${repository}:pull" \
      'https://ghcr.io/token')"; then
      echo "::error::Could not request a GHCR pull token for ${repository} with the ${credential_label}; root Flux auth was not changed."
      return 1
    fi
    if [[ "${http_status}" != "200" ]] || ! jq -e '
      (.token // .access_token // "")
      | type == "string" and length > 0
    ' "${token_file}" >/dev/null; then
      echo "::error::The ${credential_label} could not obtain a pull token for ${repository} (GHCR HTTP ${http_status}); root Flux auth was not changed."
      return 1
    fi

    jq -r '
      (.token // .access_token) as $token
      | "header = " + (("Authorization: Bearer " + $token) | @json)
    ' "${token_file}" > "${bearer_config}"
    chmod 600 "${bearer_config}"

    if ! http_status="$(curl --disable \
      --config "${bearer_config}" \
      --connect-timeout 10 \
      --max-time 60 \
      --silent \
      --show-error \
      --output /dev/null \
      --write-out '%{http_code}' \
      --header 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json' \
      "https://ghcr.io/v2/${repository}/manifests/${reference}")"; then
      echo "::error::Could not read the GHCR manifest for ${target} with the ${credential_label}; root Flux auth was not changed."
      return 1
    fi
    if [[ "${http_status}" != "200" ]]; then
      echo "::error::The ${credential_label} cannot read ${target} (GHCR HTTP ${http_status}); root Flux auth was not changed."
      return 1
    fi
  done
}

# Before the first credential-stale node is drained, prove that the credential
# still stored in the live root Secret remains accepted by every GHCR package.
# Peers have not rebooted onto the incoming credential yet, so a revoked old
# credential would make them unsafe eviction destinations. Root auth stays old
# until the complete Talos convergence succeeds.
verify_current_root_credential_overlap() {
  if ! kubectl \
    --context "${KUBE_CONTEXT}" \
    --namespace flux-system \
    get secret ksail-registry-credentials \
    -o json \
    > "${current_root_secret_file}"; then
    echo "::error::Could not read the current root GHCR credential; refusing to drain onto peers whose runtime credential cannot be proved."
    return 1
  fi
  if ! jq -er '.data[".dockerconfigjson"] | @base64d' \
    "${current_root_secret_file}" \
    > "${current_root_docker_config}" 2>/dev/null \
    || ! jq -e . "${current_root_docker_config}" >/dev/null 2>&1; then
    echo "::error::The current root GHCR credential is malformed; refusing to drain onto unproved peers."
    return 1
  fi
  if ! write_flux_ghcr_credentials \
    "${current_root_docker_config}" \
    "${current_root_credentials_file}"; then
    echo "::error::The current root GHCR credential cannot be parsed; refusing to drain onto unproved peers."
    return 1
  fi
  jq -r '
    "user = " + ((.username + ":" + .password) | @json)
  ' "${current_root_credentials_file}" \
    > "${current_root_basic_curl_config}"
  chmod 600 \
    "${current_root_docker_config}" \
    "${current_root_credentials_file}" \
    "${current_root_basic_curl_config}"

  verify_ghcr_pull_credential \
    "${current_root_basic_curl_config}" \
    "${current_root_token_response}" \
    "${current_root_bearer_curl_config}" \
    "current root GHCR credential"
}

delete_runtime_pull_probe() {
  local probe_name="$1"

  if ! kubectl \
    --context "${KUBE_CONTEXT}" \
    --namespace ksail-operator \
    delete pod "${probe_name}" \
    --ignore-not-found \
    --wait=false \
    > "${runtime_probe_result_file}" 2>&1; then
    echo "::error::Could not remove runtime pull probe ${probe_name}; root Flux auth remains unchanged."
    emit_safe_operation_output "runtime-probe-delete" \
      "${runtime_probe_result_file}"
    return 1
  fi
  if [[ "${active_runtime_probe}" == "${probe_name}" ]]; then
    active_runtime_probe=""
  fi
}

# Exercise each possible eviction destination through kubelet/containerd with
# no imagePullSecret. A valid live root Secret is not sufficient evidence: in
# the legacy outage state, machine config already held the new token while the
# running runtime still presented a revoked predecessor. imagePullPolicy Always
# forces a registry resolution even when the exact private image is cached.
probe_node_runtime_pull() {
  local node_name="$1"
  local probe_image="$2"
  local probe_name
  local attempt create_attempt image_id waiting_reason auth_rejected
  local probe_created=0

  assert_sync_lease_held || return 1
  runtime_probe_sequence=$((runtime_probe_sequence + 1))
  probe_name="ghcr-runtime-probe-$$-${RANDOM}-${runtime_probe_sequence}"
  jq -n \
    --arg name "${probe_name}" \
    --arg node "${node_name}" \
    --arg image "${probe_image}" '
    {
      apiVersion: "v1",
      kind: "Pod",
      metadata: {
        name: $name,
        namespace: "ksail-operator",
        labels: {
          "app.kubernetes.io/name": "ghcr-runtime-probe",
          "app.kubernetes.io/component": "credential-verification",
          "app.kubernetes.io/managed-by": "refresh-flux-ghcr-auth"
        }
      },
      spec: {
        nodeName: $node,
        automountServiceAccountToken: false,
        enableServiceLinks: false,
        restartPolicy: "Never",
        terminationGracePeriodSeconds: 0,
        securityContext: {
          runAsNonRoot: true,
          runAsUser: 65532,
          runAsGroup: 65532,
          seccompProfile: {type: "RuntimeDefault"}
        },
        containers: [{
          name: "pull-probe",
          image: $image,
          imagePullPolicy: "Always",
          args: ["--version"],
          resources: {
            requests: {cpu: "10m", memory: "16Mi"},
            limits: {cpu: "100m", memory: "64Mi"}
          },
          securityContext: {
            allowPrivilegeEscalation: false,
            readOnlyRootFilesystem: true,
            capabilities: {drop: ["ALL"]}
          }
        }]
      }
    }
  ' > "${runtime_probe_manifest_file}"

  active_runtime_probe="${probe_name}"
  for ((create_attempt = 1; create_attempt <= RUNTIME_PROBE_CREATE_ATTEMPTS; create_attempt++)); do
    assert_sync_lease_held || return 1
    if kubectl \
      --context "${KUBE_CONTEXT}" \
      --namespace ksail-operator \
      create --filename "${runtime_probe_manifest_file}" \
      -o name \
      > "${runtime_probe_result_file}" 2>&1; then
      probe_created=1
      break
    fi

    # A timed-out admission response is ambiguous: the API server may have
    # persisted the Pod after the client stopped waiting. Reuse that exact
    # named probe when it exists; otherwise retry the same immutable manifest.
    if kubectl \
      --context "${KUBE_CONTEXT}" \
      --namespace ksail-operator \
      get pod "${probe_name}" \
      -o name \
      >/dev/null 2>&1; then
      probe_created=1
      break
    fi

    if ((create_attempt < RUNTIME_PROBE_CREATE_ATTEMPTS)); then
      echo "::warning::Runtime pull probe admission failed on ${node_name} (attempt ${create_attempt}/${RUNTIME_PROBE_CREATE_ATTEMPTS}); retrying the same target."
      sleep "${SYNC_INTERVAL}"
    fi
  done

  if ((probe_created == 0)); then
    echo "::error::Could not create a kubelet/containerd GHCR pull probe on ${node_name}; refusing to drain onto an unproved runtime."
    emit_safe_operation_output "runtime-probe-create" \
      "${runtime_probe_result_file}"
    return 1
  fi

  for ((attempt = 1; attempt <= SYNC_ATTEMPTS; attempt++)); do
    if ! kubectl \
      --context "${KUBE_CONTEXT}" \
      --namespace ksail-operator \
      get pod "${probe_name}" \
      -o json \
      > "${runtime_probe_state_file}" 2> "${runtime_probe_result_file}"; then
      echo "::error::Could not read the kubelet/containerd GHCR pull probe on ${node_name}; refusing to drain onto an unproved runtime."
      emit_safe_operation_output "runtime-probe-read" \
        "${runtime_probe_result_file}"
      delete_runtime_pull_probe "${probe_name}" || true
      return 1
    fi
    if ! jq -e \
      '(.spec.imagePullSecrets // [] | length) == 0' \
      "${runtime_probe_state_file}" >/dev/null; then
      delete_runtime_pull_probe "${probe_name}" || true
      echo "::error::Runtime probe on ${node_name} received an imagePullSecret, so it did not prove the running containerd credential; refusing the drain."
      return 1
    fi
    image_id="$(jq -r '
      first(.status.containerStatuses[]?
        | select(.name == "pull-probe")
        | .imageID) // ""
    ' \
      "${runtime_probe_state_file}")"
    if [[ -n "${image_id}" ]]; then
      delete_runtime_pull_probe "${probe_name}" || return 1
      return 0
    fi
    waiting_reason="$(jq -r '
      first(.status.containerStatuses[]?
        | select(.name == "pull-probe")
        | .state.waiting.reason) // ""
    ' \
      "${runtime_probe_state_file}")"
    case "${waiting_reason}" in
      ErrImagePull|ImagePullBackOff)
        auth_rejected="$(jq -r '
          first(.status.containerStatuses[]?
            | select(.name == "pull-probe")
            | .state.waiting.message) // ""
          | test(
              "(^|.*: )(unexpected status from GET request to https://ghcr\\.io/token(?:\\?[^[:space:]]*)?: (401 Unauthorized|403 Forbidden)|unauthorized: authentication required|insufficient_scope: authorization failed)$";
              "i"
            )
        ' "${runtime_probe_state_file}")"
        if [[ "${auth_rejected}" == "true" ]]; then
          runtime_probe_bootstrap_needed=1
        fi
        delete_runtime_pull_probe "${probe_name}" || true
        echo "::error::The running containerd on ${node_name} could not pull ${probe_image} (${waiting_reason}); refusing to drain workloads onto peers with unproved runtime auth."
        return 1
        ;;
      InvalidImageName)
        delete_runtime_pull_probe "${probe_name}" || true
        echo "::error::The runtime probe image ${probe_image} was invalid on ${node_name}; refusing to treat that as stale credential evidence."
        return 1
        ;;
    esac
    sleep "${SYNC_INTERVAL}"
  done

  delete_runtime_pull_probe "${probe_name}" || true
  echo "::error::Timed out proving the running containerd GHCR credential on ${node_name}; refusing to drain workloads onto an unproved runtime."
  return 1
}

verify_peer_runtime_pull_overlap() {
  local draining_node="$1"
  local peer_name peer_uid
  local probe_image

  if ! kubectl \
    --context "${KUBE_CONTEXT}" \
    get nodes \
    -o json \
    > "${runtime_probe_nodes_file}"; then
    echo "::error::Could not list eviction destinations for runtime GHCR proof; refusing to drain ${draining_node}."
    return 1
  fi
  if ! validate_talos_node_inventory "${runtime_probe_nodes_file}"; then
    echo "::error::Eviction-destination inventory was ambiguous during runtime GHCR proof; refusing to drain ${draining_node}."
    return 1
  fi
  if ! jq -r \
    --arg draining "${draining_node}" '
    .items[]
    | select(.metadata.name != $draining)
    | select(.metadata.deletionTimestamp == null)
    | select((.spec.unschedulable // false) == false)
    | select(any(.spec.taints[]?;
        .effect == "NoSchedule" or .effect == "NoExecute") | not)
    | select(any(.status.conditions[]?;
        .type == "Ready" and .status == "True"))
    | [.metadata.name, .metadata.uid]
    | @tsv
  ' "${runtime_probe_nodes_file}" > "${runtime_probe_targets_file}"; then
    echo "::error::Could not select eviction destinations for runtime GHCR proof; refusing to drain ${draining_node}."
    return 1
  fi
  if [[ ! -s "${runtime_probe_targets_file}" ]]; then
    runtime_probe_bootstrap_needed=1
    echo "::error::No Ready schedulable peer can receive workloads while ${draining_node} reboots; refusing the drain."
    return 1
  fi

  while IFS=$'\t' read -r peer_name peer_uid; do
    [[ -n "${peer_name}" && -n "${peer_uid}" ]] || {
      echo "::error::Eviction-destination identity was empty during runtime GHCR proof; refusing to drain ${draining_node}."
      return 1
    }
    if grep -Fqx -- "${peer_uid}" "${runtime_proved_targets_file}"; then
      continue
    fi
    for probe_image in "${RUNTIME_CREDENTIAL_PROBE_IMAGES[@]}"; do
      probe_node_runtime_pull "${peer_name}" "${probe_image}" || return 1
    done
    printf '%s\n' "${peer_uid}" >> "${runtime_proved_targets_file}"
  done < "${runtime_probe_targets_file}"
}

verify_bootstrap_quarantine_covers_unproved_destinations() {
  local pending_targets_file="$1"
  local peer_name peer_uid

  if ! jq -r '
    .items[]
    | select(.metadata.deletionTimestamp == null)
    | select((.spec.unschedulable // false) == false)
    | select(any(.spec.taints[]?;
        .effect == "NoSchedule" or .effect == "NoExecute") | not)
    | select(any(.status.conditions[]?;
        .type == "Ready" and .status == "True"))
    | [.metadata.name, .metadata.uid]
    | @tsv
  ' "${runtime_probe_nodes_file}" > "${runtime_probe_targets_file}"; then
    echo "::error::Could not enumerate workload destinations for bootstrap quarantine; refusing the roll."
    return 1
  fi

  while IFS=$'\t' read -r peer_name peer_uid; do
    [[ -n "${peer_name}" && -n "${peer_uid}" ]] || {
      echo "::error::Workload-destination identity was empty during bootstrap quarantine; refusing the roll."
      return 1
    }
    if grep -Fqx -- "${peer_uid}" "${runtime_proved_targets_file}"; then
      continue
    fi
    if ! awk -F '\t' -v uid="${peer_uid}" '
      $4 == "reboot" && $5 == uid { found = 1 }
      END { exit !found }
    ' "${pending_targets_file}"; then
      echo "::error::Runtime-unproved workload destination ${peer_name} is not a pending credential-reboot target; refusing bootstrap quarantine."
      return 1
    fi
  done < "${runtime_probe_targets_file}"
}

# Atomically claim the right to reverse the cordon and make the node
# unschedulable. Combining both mutations closes the gap where another actor
# could cordon after our ownership annotation but before kubectl drain. A bare
# cordon after this patch is an idempotent no-op; an actor taking over an
# already-cordoned node must replace the annotation to express new ownership.
claim_node_cordon_ownership() {
  local node_name="$1" owner_token="$2" state_file="$3" result_file="$4"
  local recovery_record="${5:-}"
  local resource_version node_uid
  resource_version="$(jq -er '.metadata.resourceVersion' "${state_file}")"
  node_uid="$(jq -er '.metadata.uid' "${state_file}")"

  if jq -e '.metadata.annotations | type == "object"' \
    "${state_file}" >/dev/null; then
    jq -n \
      --arg owner_path "${CORDON_OWNER_JSON_PATH}" \
      --arg owner "${owner_token}" \
      --arg recovery_path "${CORDON_RECOVERY_JSON_PATH}" \
      --arg recovery "${recovery_record}" \
      --arg uid "${node_uid}" \
      --arg resource_version "${resource_version}" '
      [
        {
          op: "test",
          path: "/metadata/resourceVersion",
          value: $resource_version
        },
        {op: "test", path: "/metadata/uid", value: $uid},
        {op: "add", path: $owner_path, value: $owner}
      ]
      + (if $recovery == "" then [] else
          [{op: "add", path: $recovery_path, value: $recovery}]
        end)
      + [
        {op: "add", path: "/spec/unschedulable", value: true}
      ]
    ' > "${cordon_claim_patch_file}"
  else
    jq -n \
      --arg owner_annotation "${CORDON_OWNER_ANNOTATION}" \
      --arg owner "${owner_token}" \
      --arg recovery_annotation "${CORDON_RECOVERY_ANNOTATION}" \
      --arg recovery "${recovery_record}" \
      --arg uid "${node_uid}" \
      --arg resource_version "${resource_version}" '
      [
        {
          op: "test",
          path: "/metadata/resourceVersion",
          value: $resource_version
        },
        {op: "test", path: "/metadata/uid", value: $uid},
        {
          op: "add",
          path: "/metadata/annotations",
          value: ({($owner_annotation): $owner}
            + (if $recovery == "" then {} else
                {($recovery_annotation): $recovery}
              end))
        },
        {op: "add", path: "/spec/unschedulable", value: true}
      ]
    ' > "${cordon_claim_patch_file}"
  fi

  if ! kubectl \
    --context "${KUBE_CONTEXT}" \
    patch node "${node_name}" \
    --type=json \
    --patch-file="${cordon_claim_patch_file}" \
    >"${result_file}" 2>&1; then
    echo "::error::Could not atomically claim and cordon Talos node ${node_name}; refusing to drain it."
    emit_safe_operation_output "cordon-claim" "${result_file}"
    return 1
  fi
}

# The atomic claim cordons the node before kubectl drain. Restore schedulability
# only when this bridge owns that cordon; a pre-existing operator cordon must
# remain untouched.
restore_node_schedulability_if_needed() {
  local node_name="$1" was_cordoned="$2" owner_token="$3"
  local initial_node_uid="$4" initial_node_taints="$5" result_file="$6"
  local expected_recovery="${7:-}"
  local current_resource_version current_recovery

  if [[ -z "${owner_token}" ]]; then
    echo "::error::Refusing to release Talos node ${node_name} without a bridge ownership token."
    return 1
  fi

  if ! kubectl \
    --context "${KUBE_CONTEXT}" \
    get node "${node_name}" \
    --output json \
    > "${cordon_state_file}" 2> "${result_file}"; then
    echo "::error::Could not re-read Talos node ${node_name}; refusing to uncordon it."
    emit_safe_operation_output "uncordon-read" "${result_file}"
    return 1
  fi
  if ! node_scheduling_state_is_safe_to_reboot \
    "${cordon_state_file}" \
    "${was_cordoned}" \
    "${owner_token}" \
    "${initial_node_uid}" \
    "${initial_node_taints}"; then
    echo "::error::Cordon ownership changed or scheduling safety state changed for Talos node ${node_name}; refusing to uncordon it."
    return 1
  fi
  current_resource_version="$(jq -er \
    '.metadata.resourceVersion' "${cordon_state_file}")"
  current_recovery="$(jq -r \
    --arg recovery_annotation "${CORDON_RECOVERY_ANNOTATION}" \
    '.metadata.annotations[$recovery_annotation] // ""' \
    "${cordon_state_file}")"
  if [[ "${current_recovery}" != "${expected_recovery}" ]]; then
    echo "::error::Recovery journal changed for Talos node ${node_name}; refusing to release its cordon ownership."
    return 1
  fi
  if [[ -n "${current_recovery}" ]] \
    && ! jq -ne \
      --arg recovery "${current_recovery}" \
      --arg owner "${owner_token}" \
      --arg uid "${initial_node_uid}" '
      ($recovery | fromjson?) as $record
      | $record != null
      and $record.v == 1
      and $record.owner == $owner
      and $record.uid == $uid
      and ($record.phase == "rollback-safe"
        or $record.phase == "active"
        or $record.phase == "retain"
        or $record.phase == "release-ready")
    ' >/dev/null; then
    echo "::error::Recovery journal changed or was malformed for Talos node ${node_name}; refusing to release its cordon ownership."
    return 1
  fi

  jq -n \
    --arg path "${CORDON_OWNER_JSON_PATH}" \
    --arg owner "${owner_token}" \
    --arg recovery_path "${CORDON_RECOVERY_JSON_PATH}" \
    --arg recovery "${current_recovery}" \
    --arg uid "${initial_node_uid}" \
    --arg resource_version "${current_resource_version}" \
    --argjson was_cordoned "${was_cordoned}" '
    [
      {op: "test", path: $path, value: $owner},
      {op: "test", path: "/metadata/uid", value: $uid},
      {
        op: "test",
        path: "/metadata/resourceVersion",
        value: $resource_version
      }
    ]
    + (if $was_cordoned == 0 then
        [{op: "add", path: "/spec/unschedulable", value: false}]
      else [] end)
    + (if $recovery == "" then [] else
        [
          {op: "test", path: $recovery_path, value: $recovery},
          {op: "remove", path: $recovery_path}
        ]
      end)
    + [{op: "remove", path: $path}]
  ' > "${cordon_release_patch_file}"

  if ! kubectl \
    --context "${KUBE_CONTEXT}" \
    patch node "${node_name}" \
    --type=json \
    --patch-file="${cordon_release_patch_file}" \
    >"${result_file}" 2>&1; then
    echo "::error::Cordon ownership changed or could not be released for Talos node ${node_name}; refusing to uncordon it."
    emit_safe_operation_output "uncordon" "${result_file}"
    return 1
  fi
  if [[ "${was_cordoned}" == "0" ]]; then
    echo "Restored schedulability on ${node_name}."
  else
    echo "Released bridge ownership while preserving the pre-existing cordon on ${node_name}."
  fi
}

update_bootstrap_recovery_phase() {
  local node_name="$1" owner_token="$2" initial_node_uid="$3"
  local desired_revision="$4" expected_phase="$5" next_phase="$6"
  local result_file="$7"
  local current_recovery updated_recovery current_resource_version
  local was_cordoned initial_taints

  if ! kubectl \
    --context "${KUBE_CONTEXT}" \
    get node "${node_name}" \
    --output json \
    > "${cordon_state_file}" 2> "${result_file}"; then
    echo "::error::Could not re-read bootstrap recovery journal for ${node_name}; refusing to cross the reboot/release edge."
    emit_safe_operation_output "recovery-read" "${result_file}"
    return 1
  fi
  current_recovery="$(jq -r \
    --arg recovery_annotation "${CORDON_RECOVERY_ANNOTATION}" \
    '.metadata.annotations[$recovery_annotation] // ""' \
    "${cordon_state_file}")"
  if ! jq -ne \
    --arg recovery "${current_recovery}" \
    --arg owner "${owner_token}" \
    --arg uid "${initial_node_uid}" \
    --arg revision "${desired_revision}" \
    --arg phase "${expected_phase}" '
    ($recovery | fromjson?) as $record
    | $record != null
    and ($record | keys | sort) == ([
      "desiredRevision", "initialTaints", "owner", "phase",
      "uid", "v", "wasCordoned"
    ] | sort)
    and $record.v == 1
    and $record.owner == $owner
    and $record.uid == $uid
    and $record.desiredRevision == $revision
    and ($record.wasCordoned == 0 or $record.wasCordoned == 1)
    and ($record.initialTaints | type == "array")
    and $record.phase == $phase
  '; then
    echo "::error::Bootstrap recovery journal for ${node_name} was missing, malformed, or changed; refusing to cross the reboot/release edge."
    return 1
  fi
  was_cordoned="$(jq -nr \
    --arg recovery "${current_recovery}" \
    '$recovery | fromjson | .wasCordoned')"
  initial_taints="$(jq -nc \
    --arg recovery "${current_recovery}" \
    '$recovery | fromjson | .initialTaints')"
  if ! node_scheduling_state_is_safe_to_reboot \
    "${cordon_state_file}" "${was_cordoned}" "${owner_token}" \
    "${initial_node_uid}" "${initial_taints}"; then
    echo "::error::Bootstrap scheduling state changed on ${node_name}; refusing to cross the reboot/release edge."
    return 1
  fi
  current_resource_version="$(jq -er \
    '.metadata.resourceVersion' "${cordon_state_file}")"
  updated_recovery="$(jq -cn \
    --arg recovery "${current_recovery}" \
    --arg phase "${next_phase}" '
    ($recovery | fromjson) + {phase: $phase}
  ')"
  jq -n \
    --arg owner_path "${CORDON_OWNER_JSON_PATH}" \
    --arg recovery_path "${CORDON_RECOVERY_JSON_PATH}" \
    --arg owner "${owner_token}" \
    --arg recovery "${current_recovery}" \
    --arg updated_recovery "${updated_recovery}" \
    --arg uid "${initial_node_uid}" \
    --arg resource_version "${current_resource_version}" '
    [
      {op: "test", path: $owner_path, value: $owner},
      {op: "test", path: $recovery_path, value: $recovery},
      {op: "test", path: "/metadata/uid", value: $uid},
      {
        op: "test",
        path: "/metadata/resourceVersion",
        value: $resource_version
      },
      {op: "replace", path: $recovery_path, value: $updated_recovery}
    ]
  ' > "${cordon_recovery_patch_file}"
  if ! kubectl \
    --context "${KUBE_CONTEXT}" \
    patch node "${node_name}" \
    --type=json \
    --patch-file="${cordon_recovery_patch_file}" \
    >"${result_file}" 2>&1; then
    echo "::error::Bootstrap recovery phase changed or could not be updated for ${node_name}; refusing to cross the reboot/release edge."
    emit_safe_operation_output "recovery-phase" "${result_file}"
    return 1
  fi
}

# Rollback only bootstrap cordons still owned by this invocation. A node that
# reached the reboot edge stays cordoned on uncertainty, matching the normal
# fail-closed path. Missing ownership means the node was already restored or a
# newer actor took over; neither case is ours to reverse.
cleanup_bootstrap_quarantine() {
  local state_file node_name was_cordoned owner_token initial_uid
  local initial_taints current_owner current_recovery expected_recovery
  local expected_phase desired_revision
  local cleanup_failed=0

  [[ -d "${bootstrap_cordon_dir:-}" ]] || return 0
  for state_file in "${bootstrap_cordon_dir}"/*.json; do
    [[ -e "${state_file}" ]] || continue
    if ! node_name="$(jq -er '.nodeName' "${state_file}")" \
      || ! was_cordoned="$(jq -er '.wasCordoned' "${state_file}")"; then
      echo "::error::Could not read bootstrap recovery state from ${state_file}; the durable node journal was left intact."
      cleanup_failed=1
      continue
    fi
    if [[ -e "${bootstrap_retain_dir}/${node_name}" ]]; then
      continue
    fi
    if ! owner_token="$(jq -er '.ownerToken' "${state_file}")" \
      || ! initial_uid="$(jq -er '.initialUID' "${state_file}")" \
      || ! initial_taints="$(jq -c '.initialTaints' "${state_file}")" \
      || ! expected_recovery="$(jq -er '.recoveryRecord' "${state_file}")"; then
      echo "::error::Bootstrap recovery state for ${node_name} was malformed; the durable node journal was left intact."
      cleanup_failed=1
      continue
    fi
    if ! kubectl \
      --context "${KUBE_CONTEXT}" \
      get node "${node_name}" \
      --output json \
      > "${cordon_state_file}" 2>/dev/null; then
      echo "::error::Could not re-read bootstrap-owned node ${node_name} during rollback; its durable recovery journal was left intact."
      cleanup_failed=1
      continue
    fi
    current_owner="$(jq -r \
      --arg owner_annotation "${CORDON_OWNER_ANNOTATION}" \
      '.metadata.annotations[$owner_annotation] // ""' \
      "${cordon_state_file}")"
    current_recovery="$(jq -r \
      --arg recovery_annotation "${CORDON_RECOVERY_ANNOTATION}" \
      '.metadata.annotations[$recovery_annotation] // ""' \
      "${cordon_state_file}")"
    if [[ "${current_owner}" != "${owner_token}" ]]; then
      if [[ -z "${current_owner}" && -z "${current_recovery}" ]]; then
        rm -f "${state_file}"
      elif [[ -z "${current_owner}" ]]; then
        echo "::error::Bootstrap recovery journal on ${node_name} remained after its owner disappeared; refusing to discard the local recovery state."
        cleanup_failed=1
      else
        echo "::error::Bootstrap owner changed on ${node_name} during rollback; refusing to release the cordon."
        cleanup_failed=1
      fi
      continue
    fi
    expected_phase="$(jq -nr \
      --arg recovery "${expected_recovery}" \
      '$recovery | fromjson? | .phase // ""')"
    if [[ "${current_recovery}" == "${expected_recovery}" \
      && "${expected_phase}" == "active" ]]; then
      desired_revision="$(jq -nr \
        --arg recovery "${expected_recovery}" \
        '$recovery | fromjson? | .desiredRevision // ""')"
      if [[ ! "${desired_revision}" =~ ^[0-9a-f]{64}$ ]] \
        || ! update_bootstrap_recovery_phase \
          "${node_name}" "${owner_token}" "${initial_uid}" \
          "${desired_revision}" "active" "rollback-safe" \
          "${drain_result_file}"; then
        echo "::error::Could not mark bootstrap recovery on ${node_name} rollback-safe during cleanup; leaving it cordoned."
        cleanup_failed=1
        continue
      fi
      expected_recovery="$(jq -cn \
        --arg recovery "${expected_recovery}" '
        ($recovery | fromjson) + {phase: "rollback-safe"}
      ')"
    fi
    if restore_node_schedulability_if_needed \
      "${node_name}" "${was_cordoned}" "${owner_token}" \
      "${initial_uid}" "${initial_taints}" \
      "${drain_result_file}" "${expected_recovery}"; then
      rm -f "${state_file}"
    else
      cleanup_failed=1
    fi
  done
  return "${cleanup_failed}"
}

reconcile_bootstrap_recovery_journals() {
  local desired_revision="$1"
  local node_json node_name owner_token initial_uid initial_taints
  local was_cordoned phase recorded_revision recovery_record
  local reconcile_failed=0

  assert_sync_lease_held || return 1
  if ! kubectl \
    --context "${KUBE_CONTEXT}" \
    get nodes \
    -o json > "${recovery_nodes_file}"; then
    echo "::error::Could not inspect durable GHCR bootstrap recovery journals; refusing a new rollout."
    return 1
  fi
  if ! validate_talos_node_inventory "${recovery_nodes_file}"; then
    echo "::error::Node inventory was malformed while reconciling durable GHCR bootstrap recovery journals."
    return 1
  fi
  if ! jq -c \
    --arg recovery_annotation "${CORDON_RECOVERY_ANNOTATION}" '
    .items[]
    | select((.metadata.annotations[$recovery_annotation] // "") != "")
  ' "${recovery_nodes_file}" > "${recovery_targets_file}"; then
    echo "::error::Could not select durable GHCR bootstrap recovery journals."
    return 1
  fi
  if ! jq -e \
    --arg recovery_annotation "${CORDON_RECOVERY_ANNOTATION}" \
    --arg owner_annotation "${CORDON_OWNER_ANNOTATION}" '
    [
      .items[]
      | select((.metadata.annotations[$recovery_annotation] // "") != "")
      | . as $node
      | ($node.metadata.annotations[$recovery_annotation] | fromjson?) as $record
      | {node: $node, record: $record}
    ] as $journals
    | all($journals[];
        .record != null
        and (.record | keys | sort) == ([
          "desiredRevision", "initialTaints", "owner", "phase",
          "uid", "v", "wasCordoned"
        ] | sort)
        and .record.v == 1
        and (.record.owner | type == "string" and length > 0)
        and (.record.uid | type == "string" and length > 0)
        and (.record.desiredRevision
          | type == "string" and test("^[0-9a-f]{64}$"))
        and (.record.wasCordoned == 0 or .record.wasCordoned == 1)
        and (.record.initialTaints | type == "array")
        and (.record.phase == "rollback-safe"
          or .record.phase == "active"
          or .record.phase == "retain"
          or .record.phase == "release-ready")
        and .node.metadata.uid == .record.uid
        and .node.metadata.deletionTimestamp == null
        and .node.metadata.annotations[$owner_annotation] == .record.owner)
  ' "${recovery_nodes_file}" >/dev/null; then
    echo "::error::At least one durable GHCR bootstrap recovery journal is malformed or does not match its owner/UID; refusing every recovery mutation."
    return 1
  fi
  if ! jq -r \
    --arg recovery_annotation "${CORDON_RECOVERY_ANNOTATION}" '
    [
      .items[]
      | select((.metadata.annotations[$recovery_annotation] // "") != "")
      | (.metadata.annotations[$recovery_annotation] | fromjson)
    ]
    | sort_by(.owner)
    | group_by(.owner)[]
    | select(
        (map(.phase) | unique | length) > 1
        or .[0].phase == "active"
        or .[0].phase == "retain"
      )
    | .[0].owner
  ' "${recovery_nodes_file}" > "${recovery_blocked_owners_file}"; then
    echo "::error::Could not group durable GHCR bootstrap recovery journals by owner."
    return 1
  fi

  while IFS= read -r node_json; do
    [[ -n "${node_json}" ]] || continue
    printf '%s\n' "${node_json}" > "${recovery_node_file}"
    node_name="$(jq -r '.metadata.name // ""' "${recovery_node_file}")"
    recovery_record="$(jq -r \
      --arg recovery_annotation "${CORDON_RECOVERY_ANNOTATION}" \
      '.metadata.annotations[$recovery_annotation] // ""' \
      "${recovery_node_file}")"
    if ! jq -e \
      --arg recovery "${recovery_record}" \
      --arg owner_annotation "${CORDON_OWNER_ANNOTATION}" \
      --arg node_name "${node_name}" '
      ($recovery | fromjson?) as $record
      | $record != null
      and ($record | keys | sort) == ([
        "desiredRevision", "initialTaints", "owner", "phase",
        "uid", "v", "wasCordoned"
      ] | sort)
      and $record.v == 1
      and ($record.owner | type == "string" and length > 0)
      and ($record.uid | type == "string" and length > 0)
      and ($record.desiredRevision
        | type == "string" and test("^[0-9a-f]{64}$"))
      and ($record.wasCordoned == 0 or $record.wasCordoned == 1)
      and ($record.initialTaints | type == "array")
      and ($record.phase == "rollback-safe"
        or $record.phase == "active"
        or $record.phase == "retain"
        or $record.phase == "release-ready")
      and .metadata.name == $node_name
      and .metadata.uid == $record.uid
      and .metadata.deletionTimestamp == null
      and .metadata.annotations[$owner_annotation] == $record.owner
    ' "${recovery_node_file}" >/dev/null; then
      echo "::error::Durable GHCR bootstrap recovery journal on ${node_name:-unknown node} is malformed or does not match its owner/UID; refusing to execute it."
      reconcile_failed=1
      continue
    fi
    printf '%s\n' "${recovery_record}" > "${recovery_record_file}"
    owner_token="$(jq -er '.owner' "${recovery_record_file}")"
    initial_uid="$(jq -er '.uid' "${recovery_record_file}")"
    initial_taints="$(jq -c '.initialTaints' "${recovery_record_file}")"
    was_cordoned="$(jq -er '.wasCordoned' "${recovery_record_file}")"
    phase="$(jq -er '.phase' "${recovery_record_file}")"
    recorded_revision="$(jq -er '.desiredRevision' "${recovery_record_file}")"

    if grep -Fqx -- "${owner_token}" "${recovery_blocked_owners_file}"; then
      echo "::error::Bootstrap recovery owner ${owner_token} still has an active, retained, or mixed-phase quarantine; refusing to release any node in that batch."
      reconcile_failed=1
      continue
    fi

    case "${phase}" in
      rollback-safe)
        ;;
      release-ready)
        if [[ "${recorded_revision}" != "${desired_revision}" ]]; then
          echo "::error::Release-ready bootstrap journal on ${node_name} belongs to a different credential revision; leaving it cordoned."
          reconcile_failed=1
          continue
        fi
        ;;
      active)
        echo "::error::Bootstrap node ${node_name} has an active or interrupted pre-reboot mutation; leaving it cordoned for explicit recovery."
        reconcile_failed=1
        continue
        ;;
      retain)
        echo "::error::Bootstrap node ${node_name} crossed the reboot edge without a release-ready proof; leaving it cordoned for explicit recovery."
        reconcile_failed=1
        continue
        ;;
    esac

    if ! assert_sync_lease_held; then
      reconcile_failed=1
      continue
    fi
    if ! restore_node_schedulability_if_needed \
      "${node_name}" "${was_cordoned}" "${owner_token}" \
      "${initial_uid}" "${initial_taints}" "${drain_result_file}" \
      "${recovery_record}"; then
      echo "::error::Could not reconcile durable GHCR bootstrap recovery journal on ${node_name}; leaving it cordoned."
      reconcile_failed=1
    fi
  done < "${recovery_targets_file}"

  return "${reconcile_failed}"
}

node_has_no_evictable_workloads() {
  local node_name="$1"
  local pods_file="${work_dir}/bootstrap-pods-${node_name}.json"

  if ! kubectl \
    --context "${KUBE_CONTEXT}" \
    get pods \
    --all-namespaces \
    --field-selector "spec.nodeName=${node_name}" \
    -o json > "${pods_file}"; then
    echo "::error::Could not inspect workloads on bootstrap candidate ${node_name}; refusing to infer that it is empty."
    return 2
  fi
  jq -e '
    (.items | type == "array")
    and all(.items[];
      (.status.phase == "Succeeded" or .status.phase == "Failed")
      or ((.metadata.annotations["kubernetes.io/config.mirror"] // "") != "")
      or any(.metadata.ownerReferences[]?; .kind == "DaemonSet"))
  ' "${pods_file}" >/dev/null
}

node_is_ready_workload_destination() {
  local state_file="$1"
  local expected_uid="$2"

  jq -e \
    --arg uid "${expected_uid}" '
    .metadata.uid == $uid
    and .metadata.deletionTimestamp == null
    and ((.spec.unschedulable // false) == false)
    and any(.status.conditions[]?;
      .type == "Ready" and .status == "True")
    and all(.spec.taints[]?;
      .effect != "NoSchedule" and .effect != "NoExecute")
  ' "${state_file}" >/dev/null
}

wait_for_bootstrap_seed_release() {
  local node_name="$1" node_uid="$2" node_ip="$3" node_role="$4"
  local attempt

  for ((attempt = 1; attempt <= SYNC_ATTEMPTS; attempt++)); do
    if ! kubectl \
      --context "${KUBE_CONTEXT}" \
      get node "${node_name}" \
      --output json > "${cordon_state_file}"; then
      echo "::error::Could not re-read proven bootstrap seed ${node_name} while waiting for scheduling release."
      return 1
    fi
    if ! selected_node_identity_is_current \
      "${cordon_state_file}" "${node_name}" "${node_uid}" \
      "${node_ip}" "${node_role}"; then
      echo "::error::Proven bootstrap seed ${node_name} changed identity before it could become an eviction destination."
      return 1
    fi
    # The release patch is already committed at this point. Wait only for
    # Kubernetes' controller-owned Ready/taint projection; an owner, renewed
    # spec cordon, or unrelated hard taint is newer scheduling intent.
    if ! jq -e \
      --arg owner_annotation "${CORDON_OWNER_ANNOTATION}" '
      ((.metadata.annotations[$owner_annotation] // "") == "")
      and ((.spec.unschedulable // false) == false)
      and all(.spec.taints[]?;
        (.effect != "NoSchedule" and .effect != "NoExecute")
        or .key == "node.kubernetes.io/unschedulable"
        or .key == "node.kubernetes.io/not-ready"
        or .key == "node.kubernetes.io/unreachable")
    ' "${cordon_state_file}" >/dev/null; then
      echo "::error::Scheduling intent changed on proven bootstrap seed ${node_name} after its owned cordon was released."
      return 1
    fi
    if node_is_ready_workload_destination \
      "${cordon_state_file}" "${node_uid}"; then
      return 0
    fi
    if ((attempt < SYNC_ATTEMPTS)); then
      sleep "${SYNC_INTERVAL}"
    fi
  done

  echo "::error::Timed out waiting for proven bootstrap seed ${node_name} to become a workload-schedulable eviction destination; root Flux auth remains unchanged."
  return 1
}

# When the previous host credential is already revoked, no stale runtime can
# receive an eviction. Use the platform's empty warm worker as a seed: atomically
# cordon every stale target first, reboot the empty workload-schedulable seed,
# then release nodes one by one only after their runtime pull proof succeeds.
# If the warm-spare contract is not currently satisfied, make no destructive
# progress and leave the pre-existing scheduling state intact.
prepare_runtime_bootstrap_roll() {
  local desired_revision="$1"
  local pending_targets_file="$2"
  local node_role node_name node_ip node_mode node_uid
  local seed_line="" state_file was_cordoned owner_token existing_owner
  local initial_taints bootstrap_owner existing_recovery recovery_record
  local workload_rc

  bootstrap_seed_uid=""
  : > "${bootstrap_ordered_targets}"
  assert_sync_lease_held || return 1

  while IFS=$'\t' read -r \
    node_role node_name node_ip node_mode node_uid; do
    [[ "${node_mode}" == "reboot" ]] || continue
    if ! kubectl \
      --context "${KUBE_CONTEXT}" \
      get node "${node_name}" \
      --output json > "${cordon_state_file}"; then
      echo "::error::Could not inspect bootstrap candidate ${node_name}; refusing the all-stale rollout."
      return 1
    fi
    if ! selected_node_identity_is_current \
      "${cordon_state_file}" "${node_name}" "${node_uid}" \
      "${node_ip}" "${node_role}"; then
      echo "::error::Bootstrap candidate ${node_name} changed identity; refusing the all-stale rollout."
      return 1
    fi
    if ! node_is_ready_workload_destination \
      "${cordon_state_file}" "${node_uid}"; then
      continue
    fi
    if node_has_no_evictable_workloads "${node_name}"; then
      bootstrap_seed_uid="${node_uid}"
      seed_line="${node_role}"$'\t'"${node_name}"$'\t'"${node_ip}"$'\t'"${node_mode}"$'\t'"${node_uid}"
      break
    else
      workload_rc=$?
      ((workload_rc == 1)) || return "${workload_rc}"
    fi
  done < "${pending_targets_file}"

  if [[ -z "${bootstrap_seed_uid}" ]]; then
    echo "::error::All eligible runtimes use the stale GHCR credential and no empty workload-schedulable node is available to seed the refresh; refusing to drain any workload."
    return 1
  fi

  bootstrap_owner="bootstrap-${desired_revision:0:12}-$$-${RANDOM}"
  while IFS=$'\t' read -r \
    node_role node_name node_ip node_mode node_uid; do
    [[ "${node_mode}" == "reboot" ]] || continue
    state_file="${bootstrap_cordon_dir}/${node_name}.json"
    if ! kubectl \
      --context "${KUBE_CONTEXT}" \
      get node "${node_name}" \
      --output json > "${cordon_state_file}"; then
      echo "::error::Could not capture scheduling state for stale node ${node_name}; refusing the bootstrap roll."
      return 1
    fi
    if ! selected_node_identity_is_current \
      "${cordon_state_file}" "${node_name}" "${node_uid}" \
      "${node_ip}" "${node_role}"; then
      echo "::error::Stale node ${node_name} changed identity before bootstrap quarantine."
      return 1
    fi
    if ! jq -e \
      --arg owner_annotation "${CORDON_OWNER_ANNOTATION}" '
      (.metadata.resourceVersion | type == "string" and length > 0)
      and ((.spec.unschedulable // false) | type == "boolean")
      and ((.metadata.annotations[$owner_annotation] // "")
        | type == "string")
      and ((.spec.taints // []) | type == "array")
    ' "${cordon_state_file}" >/dev/null; then
      echo "::error::Scheduling state for stale node ${node_name} was malformed; refusing bootstrap quarantine."
      return 1
    fi
    if ! existing_owner="$(jq -er \
      --arg owner_annotation "${CORDON_OWNER_ANNOTATION}" \
      '.metadata.annotations[$owner_annotation] // ""' \
      "${cordon_state_file}")"; then
      echo "::error::Could not read GHCR bridge ownership for stale node ${node_name}."
      return 1
    fi
    if [[ -n "${existing_owner}" ]]; then
      echo "::error::Stale node ${node_name} already has a GHCR bridge owner; refusing concurrent bootstrap quarantine."
      return 1
    fi
    existing_recovery="$(jq -r \
      --arg recovery_annotation "${CORDON_RECOVERY_ANNOTATION}" \
      '.metadata.annotations[$recovery_annotation] // ""' \
      "${cordon_state_file}")"
    if [[ -n "${existing_recovery}" ]]; then
      echo "::error::Stale node ${node_name} has a GHCR bridge recovery journal without an owner; refusing bootstrap quarantine."
      return 1
    fi
    if ! initial_taints="$(jq -ecS '
      (.spec.taints // [])
      | map(select((
          .key == "node.kubernetes.io/unschedulable"
          and .effect == "NoSchedule"
          and (.value // "") == ""
        ) | not))
      | sort_by([.key, .effect, (.value // ""), (.timeAdded // "")])
    ' "${cordon_state_file}")"; then
      echo "::error::Could not normalize scheduling taints for stale node ${node_name}."
      return 1
    fi
    if jq -e '.spec.unschedulable == true' \
      "${cordon_state_file}" >/dev/null; then
      was_cordoned=1
    else
      was_cordoned=0
    fi
    owner_token="${bootstrap_owner}"
    recovery_record="$(jq -cn \
      --arg owner "${owner_token}" \
      --arg uid "${node_uid}" \
      --arg desired_revision "${desired_revision}" \
      --argjson was_cordoned "${was_cordoned}" \
      --argjson initial_taints "${initial_taints}" '
      {
        v: 1,
        owner: $owner,
        uid: $uid,
        desiredRevision: $desired_revision,
        wasCordoned: $was_cordoned,
        initialTaints: $initial_taints,
        phase: "active"
      }
    ')"
    if ! jq -n \
      --arg node_name "${node_name}" \
      --arg owner_token "${owner_token}" \
      --arg recovery_record "${recovery_record}" \
      --arg initial_uid "${node_uid}" \
      --argjson was_cordoned "${was_cordoned}" \
      --argjson initial_taints "${initial_taints}" '
      {
        nodeName: $node_name,
        ownerToken: $owner_token,
        recoveryRecord: $recovery_record,
        initialUID: $initial_uid,
        wasCordoned: $was_cordoned,
        initialTaints: $initial_taints
      }
    ' > "${state_file}"; then
      echo "::error::Could not persist bootstrap ownership state for stale node ${node_name}."
      return 1
    fi
    assert_sync_lease_held || return 1
    if ! claim_node_cordon_ownership \
        "${node_name}" "${owner_token}" \
        "${cordon_state_file}" "${drain_result_file}" \
        "${recovery_record}"; then
      return 1
    fi
    assert_sync_lease_held || return 1
  done < "${pending_targets_file}"

  printf '%s\n' "${seed_line}" > "${bootstrap_ordered_targets}"
  awk -F '\t' -v seed_uid="${bootstrap_seed_uid}" \
    '$5 != seed_uid' "${pending_targets_file}" \
    >> "${bootstrap_ordered_targets}"
}

# Close every post-cordon scheduling race. A drain, reboot, readiness wait, or
# image proof can outlive an operator/autoscaler change, so re-read before each
# destructive Talos edge and fail closed when the captured guard no longer holds.
revalidate_node_scheduling_guard() {
  local node_name="$1" was_cordoned="$2" owner_token="$3"
  local initial_node_uid="$4" initial_node_taints="$5" result_file="$6"
  local selected_node_ip="$7" selected_node_role="$8"
  local operation="$9"

  assert_sync_lease_held || return 1
  if ! kubectl \
    --context "${KUBE_CONTEXT}" \
    get node "${node_name}" \
    --output json \
    > "${cordon_state_file}" 2> "${result_file}"; then
    echo "::error::Could not re-read Talos node ${node_name} immediately before ${operation}; refusing the mutation."
    emit_safe_operation_output "scheduling-guard" "${result_file}"
    return 1
  fi
  if ! selected_node_identity_is_current \
    "${cordon_state_file}" \
    "${node_name}" \
    "${initial_node_uid}" \
    "${selected_node_ip}" \
    "${selected_node_role}" \
    || ! node_scheduling_state_is_safe_to_reboot \
    "${cordon_state_file}" \
    "${was_cordoned}" \
    "${owner_token}" \
    "${initial_node_uid}" \
    "${initial_node_taints}"; then
    echo "::error::Talos node ${node_name} identity changed, cordon ownership changed, or scheduling safety state changed before ${operation}; refusing the mutation."
    return 1
  fi
}

wait_for_node_lifecycle_taints_to_clear() {
  local node_name="$1" was_cordoned="$2" owner_token="$3"
  local initial_node_uid="$4" initial_node_taints="$5" result_file="$6"
  local selected_node_ip="$7" selected_node_role="$8"
  local attempt

  for ((attempt = 1; attempt <= SYNC_ATTEMPTS; attempt++)); do
    if ! kubectl \
      --context "${KUBE_CONTEXT}" \
      get node "${node_name}" \
      --output json \
      > "${cordon_state_file}" 2> "${result_file}"; then
      echo "::error::Could not re-read Talos node ${node_name} while waiting for its post-reboot lifecycle taints to clear; refusing image verification."
      emit_safe_operation_output "lifecycle-taint-read" "${result_file}"
      return 1
    fi
    if ! selected_node_identity_is_current \
      "${cordon_state_file}" \
      "${node_name}" \
      "${initial_node_uid}" \
      "${selected_node_ip}" \
      "${selected_node_role}" \
      || ! node_scheduling_state_is_safe_while_lifecycle_taints_clear \
        "${cordon_state_file}" \
        "${was_cordoned}" \
        "${owner_token}" \
        "${initial_node_uid}" \
        "${initial_node_taints}"; then
      echo "::error::Talos node ${node_name} identity changed, cordon ownership changed, or non-lifecycle scheduling safety state changed while waiting for its post-reboot lifecycle taints to clear; refusing image verification."
      return 1
    fi
    if ! node_has_lifecycle_taints "${cordon_state_file}" \
      && jq -e '
        any(.status.conditions[]?;
          .type == "Ready" and .status == "True")
      ' "${cordon_state_file}" >/dev/null; then
      return 0
    fi
    if ((attempt < SYNC_ATTEMPTS)); then
      sleep "${SYNC_INTERVAL}"
    fi
  done

  echo "::error::Timed out waiting for Talos node ${node_name} to remain Ready and for post-reboot lifecycle taints to clear; it remains cordoned and image verification was not attempted."
  return 1
}

# Talos returns gRPC NotFound with the exact image reference when that image is
# already absent from the selected runtime namespace. Match both so transport,
# authorization, and unrelated removal failures remain fatal.
talos_image_remove_reports_absent() {
  local result_file="$1"
  local operator_image="$2"

  LC_ALL=C grep -Fq -- \
    "rpc error: code = NotFound desc = image ${operator_image} not found" \
    "${result_file}"
}

revalidate_selected_node_identity_before_mutation() {
  local node_name="$1" node_uid="$2" node_ip="$3" node_role="$4"

  if ! kubectl \
    --context "${KUBE_CONTEXT}" \
    get node "${node_name}" \
    --output json \
    > "${cordon_state_file}" 2> "${talos_result_file}"; then
    echo "::error::Could not re-read Talos node ${node_name} before mutation; refusing to target a stale address."
    emit_safe_operation_output "node-identity" "${talos_result_file}"
    return 1
  fi
  if ! selected_node_identity_is_current \
    "${cordon_state_file}" \
    "${node_name}" \
    "${node_uid}" \
    "${node_ip}" \
    "${node_role}"; then
    echo "::error::Talos node ${node_name} identity changed after inventory selection; refusing to patch, drain, or reboot it."
    return 1
  fi
}

# Apply Git/SOPS auth to stale Talos nodes, reboot them so containerd actually
# adopts the credential, prove an uncached pull of the declared incoming image,
# and only then record its non-secret revision+image proof markers so either
# credential or target changes trigger verification.
process_talos_node_target() {
  local desired_revision="$1"
  local operator_image="$2"
  local node_role="$3"
  local node_name="$4"
  local node_ip="$5"
  local node_mode="$6"
  local node_uid="$7"
  local was_cordoned=0 existing_cordon_owner="" existing_cordon_recovery=""
  local cordon_owner_token=""
  local initial_node_uid="" initial_node_taints="[]"
  local bootstrap_state_file="${bootstrap_cordon_dir}/${node_name}.json"
  local probe_image recovery_record

  assert_sync_lease_held || return 1

  if [[ "${node_mode}" != "reboot" && "${node_mode}" != "image-only" ]]; then
    echo "::error::Unknown Talos GHCR synchronization mode '${node_mode}' for ${node_name}."
    return 1
  fi
  revalidate_selected_node_identity_before_mutation \
    "${node_name}" "${node_uid}" "${node_ip}" "${node_role}" || return 1

  if [[ "${node_mode}" == "reboot" ]]; then
    # Writing the credential is NOT enough to make a RUNNING node use it, and
    # this is the step whose absence caused the 2026-07-14 outage.
    #
    # containerd reads registry auth from its STATIC config
    # (plugins.'io.containerd.cri.v1.images'.registry.configs.'ghcr.io'.auth),
    # which it loads ONCE at process start. Talos re-renders that file
    # (/etc/cri/conf.d/01-registries.part) immediately on a config change, but
    # it does not restart containerd — and it refuses to let us either:
    #
    #   $ talosctl service cri restart
    #   error: service "cri" doesn't support restart operation via API
    #
    # So after a --mode=no-reboot patch the new credential sits on disk,
    # correct and INERT, while the running containerd keeps presenting the old
    # one. A REBOOT is the only supported way to make it adopt the new auth.
    #
    # Do not be tempted to drop this and trust the `image pull` check below:
    # that check goes through the TALOS image API, which builds its auth from
    # the machine config we just wrote, NOT from containerd's CRI plugin. It
    # therefore passes on a node whose kubelet pulls are still failing 403 —
    # which is exactly what happened: every node had the legacy unversioned
    # ghcr-pull-verified-revision marker while every ksail-operator pod sat in
    # ImagePullBackOff, and prod stayed four releases behind for over a day.
    # The pull check proves the CREDENTIAL is good; only the reboot proves
    # CONTAINERD is using it.
    #
    # Credential-revision drift always takes this reboot path; a desired-machine
    # marker is not evidence that the running containerd loaded the credential.
    # A node whose v2 credential proof is already current but whose declared
    # image changed takes the image-only path below and is never rebooted.
    #
    # etcd tolerates exactly one control plane down in a 3-member cluster. This
    # loop is serial and control planes sort last, but a peer can be
    # Kubernetes-Ready while its etcd member is unhealthy. Re-read the peer
    # inventory, then prove every other peer is Ready, answers `etcd status`,
    # and has no etcd alarm immediately before each control-plane reboot.
    if [[ "${node_role}" == "1" ]] \
      && ! other_control_planes_safe_to_reboot \
        "${node_name}" "${KUBE_CONTEXT}" "${work_dir}"; then
      echo "::error::Refusing to reboot control plane ${node_name} for the GHCR auth refresh: another control plane is not Ready with healthy, alarm-free etcd, so rebooting this one risks quorum."
      return 1
    fi
  fi

    # Remember scheduling intent before any cordon. Both reboot and image-only
    # verification exclude new placements while the exact target is removed;
    # only the reboot path drains existing workloads.
    if ! kubectl \
      --context "${KUBE_CONTEXT}" \
      get node "${node_name}" \
      --output json \
      > "${cordon_state_file}"; then
      echo "::error::Refusing to synchronize ${node_name}: its scheduling state could not be read."
      return 1
    fi
    if ! selected_node_identity_is_current \
      "${cordon_state_file}" \
      "${node_name}" \
      "${node_uid}" \
      "${node_ip}" \
      "${node_role}" \
      || ! jq -e \
      --arg owner_annotation "${CORDON_OWNER_ANNOTATION}" '
      (.metadata.uid | type == "string" and length > 0)
      and (.metadata.resourceVersion | type == "string" and length > 0)
      and ((.spec.unschedulable // false) | type == "boolean")
      and ((.metadata.annotations[$owner_annotation] // "")
        | type == "string")
    ' "${cordon_state_file}" >/dev/null; then
      echo "::error::Refusing to synchronize ${node_name}: its identity changed or scheduling state was malformed."
      return 1
    fi
    if [[ -f "${bootstrap_state_file}" ]]; then
      if ! jq -e \
        --arg node_name "${node_name}" \
        --arg node_uid "${node_uid}" '
        .nodeName == $node_name
        and .initialUID == $node_uid
        and (.ownerToken | type == "string")
        and (.recoveryRecord | type == "string" and length > 0)
        and (.wasCordoned == 0 or .wasCordoned == 1)
        and (.initialTaints | type == "array")
      ' "${bootstrap_state_file}" >/dev/null; then
        echo "::error::Bootstrap ownership state for ${node_name} was malformed; refusing the mutation."
        return 1
      fi
      initial_node_uid="$(jq -er '.initialUID' "${bootstrap_state_file}")"
      initial_node_taints="$(jq -c '.initialTaints' "${bootstrap_state_file}")"
      was_cordoned="$(jq -er '.wasCordoned' "${bootstrap_state_file}")"
      cordon_owner_token="$(jq -er '.ownerToken' "${bootstrap_state_file}")"
      recovery_record="$(jq -er '.recoveryRecord' "${bootstrap_state_file}")"
      if ! jq -e \
        --arg recovery_annotation "${CORDON_RECOVERY_ANNOTATION}" \
        --arg recovery "${recovery_record}" \
        '.metadata.annotations[$recovery_annotation] == $recovery' \
        "${cordon_state_file}" >/dev/null; then
        echo "::error::Bootstrap recovery journal changed for ${node_name}; refusing the mutation."
        return 1
      fi
      if ! node_scheduling_state_is_safe_to_reboot \
        "${cordon_state_file}" \
        "${was_cordoned}" \
        "${cordon_owner_token}" \
        "${initial_node_uid}" \
        "${initial_node_taints}"; then
        echo "::error::Bootstrap quarantine ownership or scheduling state changed for ${node_name}; refusing the mutation."
        return 1
      fi
    else
      initial_node_uid="$(jq -r '.metadata.uid' "${cordon_state_file}")"
      initial_node_taints="$(jq -cS '
        (.spec.taints // [])
        | map(select((
            .key == "node.kubernetes.io/unschedulable"
            and .effect == "NoSchedule"
            and (.value // "") == ""
          ) | not))
        | sort_by([.key, .effect, (.value // ""), (.timeAdded // "")])
      ' "${cordon_state_file}")"
      existing_cordon_owner="$(jq -r \
        --arg owner_annotation "${CORDON_OWNER_ANNOTATION}" \
        '.metadata.annotations[$owner_annotation] // ""' \
        "${cordon_state_file}")"
      if [[ -n "${existing_cordon_owner}" ]]; then
        echo "::error::Refusing to synchronize ${node_name}: it already has a GHCR bridge cordon owner, so a previous or concurrent roll must be resolved first."
        return 1
      fi
      existing_cordon_recovery="$(jq -r \
        --arg recovery_annotation "${CORDON_RECOVERY_ANNOTATION}" \
        '.metadata.annotations[$recovery_annotation] // ""' \
        "${cordon_state_file}")"
      if [[ -n "${existing_cordon_recovery}" ]]; then
        echo "::error::Refusing to synchronize ${node_name}: it has a GHCR bridge recovery journal without an owner."
        return 1
      fi
      if jq -e '.spec.unschedulable == true' \
        "${cordon_state_file}" >/dev/null; then
        was_cordoned=1
      else
        was_cordoned=0
      fi
      cordon_owner_token="${desired_revision:0:16}-$$-${RANDOM}"
      assert_sync_lease_held || return 1
      claim_node_cordon_ownership \
        "${node_name}" "${cordon_owner_token}" \
        "${cordon_state_file}" "${drain_result_file}" || return 1
    fi

  # A node resourceVersion fences only the claim itself. Renew after the claim
  # and re-read the owned scheduling guard so a process that lost its cluster
  # transaction cannot carry stale credentials into Talos.
  if ! revalidate_node_scheduling_guard \
    "${node_name}" "${was_cordoned}" "${cordon_owner_token}" \
    "${initial_node_uid}" "${initial_node_taints}" \
    "${talos_result_file}" "${node_ip}" "${node_role}" \
    "credential patch"; then
    restore_node_schedulability_if_needed \
      "${node_name}" "${was_cordoned}" "${cordon_owner_token}" \
      "${initial_node_uid}" "${initial_node_taints}" \
      "${drain_result_file}" "${recovery_record}" || true
    return 1
  fi

  if [[ "${node_mode}" == "reboot" ]]; then
    if ! talosctl \
      --nodes "${node_ip}" \
      patch machineconfig \
      --mode=no-reboot \
      --patch-file="${talos_auth_patch_file}" \
      >"${talos_result_file}" 2>&1; then
      echo "::error::Talos node ${node_name} did not accept the Git/SOPS GHCR registry auth."
      restore_node_schedulability_if_needed \
        "${node_name}" "${was_cordoned}" "${cordon_owner_token}" \
        "${initial_node_uid}" "${initial_node_taints}" \
        "${drain_result_file}" "${recovery_record}" || return 1
      return 1
    fi

    # The Talos API call above is a concurrency window. Rebind both the selected
    # machine identity and the owned scheduling state before asking Kubernetes
    # to evict anything from that node.
    revalidate_node_scheduling_guard \
      "${node_name}" "${was_cordoned}" "${cordon_owner_token}" \
      "${initial_node_uid}" "${initial_node_taints}" \
      "${drain_result_file}" "${node_ip}" "${node_role}" "drain" || return 1

    # Drain through the Kubernetes context already proven by this deployment.
    # Talos v1.13's integrated --drain path fetches a separate admin kubeconfig;
    # this cluster's generated config targets an unreachable API endpoint.
    # kubectl also retries PDB-protected evictions, giving CloudNativePG time to
    # switch primaries and Longhorn time to enforce its data-safety policy.
    if ! kubectl \
      --context "${KUBE_CONTEXT}" \
      drain "${node_name}" \
      --ignore-daemonsets \
      --delete-emptydir-data \
      --timeout="${DRAIN_TIMEOUT}" \
      >"${drain_result_file}" 2>&1; then
      echo "::error::Talos node ${node_name} could not be safely drained before its GHCR auth reboot."
      emit_safe_operation_output "drain" "${drain_result_file}"
      restore_node_schedulability_if_needed \
        "${node_name}" "${was_cordoned}" "${cordon_owner_token}" \
        "${initial_node_uid}" "${initial_node_taints}" \
        "${drain_result_file}" "${recovery_record}" || return 1
      return 1
    fi

    # A PDB-respecting drain can legitimately take most of DRAIN_TIMEOUT. An
    # etcd peer that was healthy before it began may fail while workloads move,
    # so refresh the quorum proof at the last safe point before the reboot.
    if [[ "${node_role}" == "1" ]] \
      && ! other_control_planes_safe_to_reboot \
        "${node_name}" "${KUBE_CONTEXT}" "${work_dir}"; then
      echo "::error::Refusing to reboot control plane ${node_name} after its drain: another control plane is no longer Ready with healthy, alarm-free etcd, so rebooting this one risks quorum."
      restore_node_schedulability_if_needed \
        "${node_name}" "${was_cordoned}" "${cordon_owner_token}" \
        "${initial_node_uid}" "${initial_node_taints}" \
        "${drain_result_file}" "${recovery_record}" || return 1
      return 1
    fi

    if ! revalidate_node_scheduling_guard \
      "${node_name}" "${was_cordoned}" "${cordon_owner_token}" \
      "${initial_node_uid}" "${initial_node_taints}" \
      "${drain_result_file}" "${node_ip}" "${node_role}" "reboot"; then
      # Scheduling intent changed after the PDB-respecting drain. Never reboot
      # or undo the newer actor's decision; leave the node in its observed state
      # for an operator or the next run to reconcile explicitly.
      return 1
    fi

    # The node is now cordoned and fully drained under PDB control, so a plain
    # Talos reboot cannot terminate a workload behind Kubernetes' back. Keep
    # --wait explicit so Kubernetes readiness is checked only after a new boot.
    if [[ -f "${bootstrap_state_file}" ]]; then
      update_bootstrap_recovery_phase \
        "${node_name}" "${cordon_owner_token}" \
        "${initial_node_uid}" "${desired_revision}" \
        "active" "retain" "${drain_result_file}" || return 1
      : > "${bootstrap_retain_dir}/${node_name}"
    fi
    assert_sync_lease_held || return 1
    if ! talosctl \
      --nodes "${node_ip}" \
      reboot \
      --wait \
      >"${reboot_result_file}" 2>&1; then
      echo "::error::Talos node ${node_name} did not reboot to load the refreshed GHCR registry auth; it remains cordoned because its reboot state is uncertain."
      emit_safe_operation_output "reboot" "${reboot_result_file}"
      return 1
    fi
    if ! kubectl \
      --context "${KUBE_CONTEXT}" \
      wait \
      --for=condition=Ready \
      "node/${node_name}" \
      --timeout=10m \
      >"${reboot_result_file}" 2>&1; then
      echo "::error::Talos node ${node_name} did not return Ready after its GHCR auth reboot; it remains cordoned and the next node will not be rolled."
      emit_safe_operation_output "ready" "${reboot_result_file}"
      return 1
    fi
    wait_for_node_lifecycle_taints_to_clear \
      "${node_name}" "${was_cordoned}" "${cordon_owner_token}" \
      "${initial_node_uid}" "${initial_node_taints}" \
      "${reboot_result_file}" "${node_ip}" "${node_role}" || return 1
  fi

    # A reboot/readiness wait or even a short image-only cordon can outlive a
    # replacement, uncordon, taint, or owner change. Rebind identity and the
    # scheduling guard at the final Talos edge before touching the image cache.
    revalidate_node_scheduling_guard \
      "${node_name}" "${was_cordoned}" "${cordon_owner_token}" \
      "${initial_node_uid}" "${initial_node_taints}" \
      "${talos_result_file}" "${node_ip}" "${node_role}" \
      "image verification" || return 1

    # A cached image can make a pull look healthy without proving that the
    # node's runtime can authenticate to GHCR. Remove the incoming exact target
    # first so the following pull must complete a registry round-trip.
    if ! talosctl \
      --nodes "${node_ip}" \
      image remove "${operator_image}" \
      --namespace cri \
      >"${talos_result_file}" 2>&1; then
      if ! talos_image_remove_reports_absent \
        "${talos_result_file}" "${operator_image}"; then
        echo "::error::Talos node ${node_name} could not remove the cached incoming KSail image before GHCR verification; it remains cordoned because registry access is unproved."
        return 1
      fi
    fi

    revalidate_node_scheduling_guard \
      "${node_name}" "${was_cordoned}" "${cordon_owner_token}" \
      "${initial_node_uid}" "${initial_node_taints}" \
      "${talos_result_file}" "${node_ip}" "${node_role}" \
      "image pull" || return 1

    # Credential validity against GHCR (see the caveat above: this is not, on
    # its own, proof that containerd is using it — the reboot is).
    if ! talosctl \
      --nodes "${node_ip}" \
      image pull "${operator_image}" \
      --namespace cri \
      >"${talos_result_file}" 2>&1; then
      echo "::error::Talos node ${node_name} could not pull the exact incoming KSail image after its auth refresh; it remains cordoned because registry access is unproved."
      return 1
    fi

    revalidate_node_scheduling_guard \
      "${node_name}" "${was_cordoned}" "${cordon_owner_token}" \
      "${initial_node_uid}" "${initial_node_taints}" \
      "${talos_result_file}" "${node_ip}" "${node_role}" \
      "runtime pull proof" || return 1

    # Talos' image API authenticates from machine config, not through the
    # kubelet's running CRI client. Before this freshly rebooted node can
    # receive workloads, prove both private images through kubelet/containerd
    # while the bridge-owned cordon is still in place.
    if [[ "${node_mode}" == "reboot" ]]; then
      for probe_image in "${RUNTIME_CREDENTIAL_PROBE_IMAGES[@]}"; do
        probe_node_runtime_pull "${node_name}" "${probe_image}" || return 1
      done
      if ! grep -Fqx -- "${node_uid}" "${runtime_proved_targets_file}"; then
        printf '%s\n' "${node_uid}" >> "${runtime_proved_targets_file}"
      fi
    fi

    revalidate_node_scheduling_guard \
      "${node_name}" "${was_cordoned}" "${cordon_owner_token}" \
      "${initial_node_uid}" "${initial_node_taints}" \
      "${talos_result_file}" "${node_ip}" "${node_role}" \
      "revision marker" || return 1

    # Record the proof only after the real runtime checks, while the selected
    # machine remains protected by the owned cordon. Releasing ownership first
    # would let a concurrent credential revision race this marker write.
    if ! talosctl \
      --nodes "${node_ip}" \
      patch machineconfig \
      --mode=no-reboot \
      --patch-file="${talos_revision_patch_file}" \
      >"${talos_result_file}" 2>&1; then
      echo "::error::Talos node ${node_name} proved GHCR access but could not record the synchronized credential revision."
      return 1
    fi

    if [[ -f "${bootstrap_state_file}" ]]; then
      update_bootstrap_recovery_phase \
        "${node_name}" "${cordon_owner_token}" \
        "${initial_node_uid}" "${desired_revision}" \
        "retain" "release-ready" "${drain_result_file}" || return 1
      recovery_record="$(jq -cn \
        --arg recovery "${recovery_record}" '
        ($recovery | fromjson) + {phase: "release-ready"}
      ')"
    fi

    # Restore original scheduling intent only after the proof marker is durable.
    # Residual ownership makes the next selector fail closed rather than letting
    # a release failure masquerade as a clean node.
    rm -f "${bootstrap_retain_dir}/${node_name}"
    assert_sync_lease_held || return 1
    restore_node_schedulability_if_needed \
      "${node_name}" "${was_cordoned}" "${cordon_owner_token}" \
      "${initial_node_uid}" "${initial_node_taints}" \
      "${drain_result_file}" "${recovery_record}" || return 1

    # The release is the final replacement boundary before this UID is marked
    # processed in the convergence loop. Rebind it once more so a replacement
    # cannot inherit the old machine's proof within this pass.
    revalidate_selected_node_identity_before_mutation \
      "${node_name}" "${node_uid}" "${node_ip}" "${node_role}" || return 1
}

validate_talos_node_inventory() {
  local nodes_file="$1"

  # talosctl proxies node targets through the public control-plane endpoints,
  # so use the stable, unique InternalIP. UID is part of convergence identity:
  # an autoscaler replacement may reuse a name or address and still needs proof.
  jq -e '
    (.items | length) > 0
    and all(.items[];
      (.metadata.name | type == "string" and test("^[^\\t\\r\\n]+$"))
      and (.metadata.uid | type == "string" and test("^[^\\t\\r\\n]+$"))
      and ([.status.addresses[]?
        | select(.type == "InternalIP") | .address] | length) == 1
      and (([.status.addresses[]?
        | select(.type == "InternalIP") | .address][0])
        | type == "string" and test("^[^\\t\\r\\n]+$")))
    and (([.items[].metadata.uid] | unique | length) == (.items | length))
    and (([.items[]
      | [.status.addresses[]?
        | select(.type == "InternalIP") | .address][0]]
      | unique | length) == (.items | length))
  ' "${nodes_file}" >/dev/null
}

# Converge the live node set, rather than trusting one inventory captured before
# a potentially long roll. Completed node UIDs are not rolled twice while their
# Kubernetes annotations propagate; newly autoscaled/replaced nodes are picked
# up in the next pass. Two consecutive clean inventories close the common
# cutover race, and the bounded loop fails before root auth changes if the set
# never stabilizes.
sync_talos_registry_auth() {
  local desired_revision="$1"
  local operator_image="$2"
  local sync_result_file="$3"
  local convergence_attempt=0
  local consecutive_clean_inventories=0
  local processed_any_node=0
  local node_role node_name node_ip node_mode node_uid
  local batch_targets_file first_reboot_name bootstrap_mode

  : > "${talos_result_file}"
  : > "${drain_result_file}"
  : > "${reboot_result_file}"
  : > "${talos_processed_targets}"
  : > "${sync_result_file}"
  : > "${runtime_proved_targets_file}"
  chmod 600 \
    "${talos_result_file}" \
    "${drain_result_file}" \
    "${reboot_result_file}" \
    "${talos_processed_targets}" \
    "${sync_result_file}" \
    "${runtime_proved_targets_file}"

  reconcile_bootstrap_recovery_journals "${desired_revision}" || return 1

  while ((convergence_attempt < TALOS_CONVERGENCE_ATTEMPTS)); do
    convergence_attempt=$((convergence_attempt + 1))
    if ! kubectl \
      --context "${KUBE_CONTEXT}" \
      get nodes \
      -o json \
      > "${talos_nodes_file}"; then
      echo "::error::Could not list Talos nodes; refusing to mutate any Kubernetes credential consumers."
      return 1
    fi
    if ! validate_talos_node_inventory "${talos_nodes_file}"; then
      echo "::error::Every Talos node must expose a non-empty unique UID and exactly one non-empty unique InternalIP before GHCR auth can be synchronized."
      return 1
    fi
    if ! select_talos_node_targets \
      "${talos_nodes_file}" \
      "${desired_revision}" \
      "${operator_image}" \
      "${talos_node_targets}"; then
      echo "::error::Could not select Talos nodes requiring GHCR synchronization."
      return 1
    fi

    : > "${talos_pending_targets}"
    while IFS=$'\t' read -r \
      node_role node_name node_ip node_mode node_uid; do
      [[ -n "${node_name}" ]] || continue
      if grep -Fqx -- "${node_uid}" "${talos_processed_targets}"; then
        continue
      fi
      printf '%s\t%s\t%s\t%s\t%s\n' \
        "${node_role}" "${node_name}" "${node_ip}" \
        "${node_mode}" "${node_uid}" \
        >> "${talos_pending_targets}"
    done < "${talos_node_targets}"

    if [[ ! -s "${talos_pending_targets}" ]]; then
      if [[ ! -s "${talos_node_targets}" ]]; then
        consecutive_clean_inventories=$((consecutive_clean_inventories + 1))
        if ((consecutive_clean_inventories >= 2)); then
          if ((processed_any_node == 1)); then
            printf '%s\n' processed > "${sync_result_file}"
          else
            printf '%s\n' clean > "${sync_result_file}"
          fi
          return 0
        fi
      else
        # A completed node can remain in the selector briefly while Talos node
        # annotations propagate back to Kubernetes. Wait; never re-roll it.
        consecutive_clean_inventories=0
      fi
    else
      consecutive_clean_inventories=0
      batch_targets_file="${talos_pending_targets}"
      bootstrap_mode=0
      # Prefer direct peer-runtime overlap: it avoids batch-wide quarantine
      # while every possible destination can still pull. A revoked root Secret
      # does not outweigh that stronger live proof. Only a stale/no-peer result
      # enters the owned warm-spare bootstrap; admission and probe-integrity
      # errors remain immediate fail-closed outcomes.
      if awk -F '\t' '$4 == "reboot" { found = 1 } END { exit !found }' \
        "${talos_pending_targets}"; then
        first_reboot_name="$(awk -F '\t' '$4 == "reboot" { print $2; exit }' \
          "${talos_pending_targets}")"
        : > "${bootstrap_overlap_result}"
        runtime_probe_bootstrap_needed=0
        verify_current_root_credential_overlap \
          >> "${bootstrap_overlap_result}" 2>&1 || true
        if ! verify_peer_runtime_pull_overlap "${first_reboot_name}" \
          >> "${bootstrap_overlap_result}" 2>&1; then
          if ((runtime_probe_bootstrap_needed == 0)); then
            emit_safe_operation_output \
              "runtime-overlap" "${bootstrap_overlap_result}"
            return 1
          fi
          if ! verify_bootstrap_quarantine_covers_unproved_destinations \
            "${talos_pending_targets}"; then
            emit_safe_operation_output \
              "runtime-overlap" "${bootstrap_overlap_result}"
            return 1
          fi
          if ! prepare_runtime_bootstrap_roll \
            "${desired_revision}" "${talos_pending_targets}"; then
            emit_safe_operation_output \
              "runtime-overlap" "${bootstrap_overlap_result}"
            return 1
          fi
          bootstrap_mode=1
          batch_targets_file="${bootstrap_ordered_targets}"
        fi
      fi

      # Targets are sorted workers-first and processed strictly sequentially,
      # so only one node is down and control planes go last.
      while IFS=$'\t' read -r \
        node_role node_name node_ip node_mode node_uid; do
        if [[ "${node_mode}" == "reboot" ]]; then
          if ((bootstrap_mode == 1)) \
            && [[ "${node_uid}" == "${bootstrap_seed_uid}" ]]; then
            if ! node_has_no_evictable_workloads "${node_name}"; then
              echo "::error::Bootstrap seed ${node_name} gained an evictable workload before its reboot; refusing the roll."
              return 1
            fi
          else
            verify_peer_runtime_pull_overlap \
              "${node_name}" || return 1
          fi
        fi
        process_talos_node_target \
          "${desired_revision}" \
          "${operator_image}" \
          "${node_role}" \
          "${node_name}" \
          "${node_ip}" \
          "${node_mode}" \
          "${node_uid}" || return 1
        if ((bootstrap_mode == 1)) \
          && [[ "${node_uid}" == "${bootstrap_seed_uid}" ]]; then
          wait_for_bootstrap_seed_release \
            "${node_name}" "${node_uid}" \
            "${node_ip}" "${node_role}" || return 1
        fi
        rm -f \
          "${bootstrap_cordon_dir}/${node_name}.json" \
          "${bootstrap_retain_dir}/${node_name}"
        processed_any_node=1
        printf '%s\n' "${node_uid}" >> "${talos_processed_targets}"
      done < "${batch_targets_file}"
    fi

    if ((convergence_attempt < TALOS_CONVERGENCE_ATTEMPTS)); then
      sleep "${SYNC_INTERVAL}"
    fi
  done

  echo "::error::Talos node inventory did not converge after ${TALOS_CONVERGENCE_ATTEMPTS} checks; root Flux auth remains unchanged."
  return 1
}

# KSail embeds SOPS, so the deploy uses the same pinned toolchain as workload
# reconciliation. Decrypt only the Docker config scalar and never emit it to
# stdout or place its plaintext/base64 representation in an argument.
decrypt_flux_ghcr_docker_config "${docker_config}" "${SECRET_FILE}"
write_flux_ghcr_credentials "${docker_config}" "${credentials_file}"
jq -S -c . "${docker_config}" > "${expected_normalized}"

# Build curl's Basic-auth config without putting the credential in argv or
# stdout. Support both Docker config representations used in this repository:
# explicit username/password and base64(username:password) in auth.
jq -r '
  "user = " + ((.username + ":" + .password) | @json)
' "${credentials_file}" > "${basic_curl_config}"
chmod 600 "${basic_curl_config}"

# GHCR permissions are package-granular, so a token response alone is not proof
# of access. Exchange and read every required manifest with the incoming SOPS
# credential before touching any cluster consumer.
verify_ghcr_pull_credential \
  "${basic_curl_config}" \
  "${token_response}" \
  "${bearer_curl_config}" \
  "SOPS GHCR credential" || exit 1

if [[ "${check_only}" == "true" ]]; then
  echo "✅ Validated every required GHCR package pull from Git/SOPS."
  exit 0
fi

# Talos image verification resolves cosign artifacts with host registry auth;
# pod imagePullSecrets cannot satisfy that request. Prepare the supported v1.13
# RegistryAuthConfig and post-reboot proof patch without placing credentials in
# argv. Existing-cluster nodes are synchronized only after the complete tenant
# fan-out has been staged and verified below.
jq '
  {
    apiVersion: "v1alpha1",
    kind: "RegistryAuthConfig",
    name: "ghcr.io",
    username: .username,
    password: .password
  }
' "${credentials_file}" > "${talos_auth_patch_file}"
pull_revision="$(flux_ghcr_revision "${SECRET_FILE}")"
readonly pull_revision
jq -n \
  --arg revision "${pull_revision}" \
  --arg image "${KSAIL_OPERATOR_IMAGE}" \
  --arg revision_annotation "${GHCR_PULL_VERIFIED_REVISION_ANNOTATION}" \
  --arg image_annotation "${GHCR_PULL_VERIFIED_IMAGE_ANNOTATION}" '
  {
    machine: {
      nodeAnnotations: {
        ($revision_annotation): $revision,
        ($image_annotation): $image
      }
    }
  }
' > "${talos_revision_patch_file}"
chmod 600 "${talos_auth_patch_file}" "${talos_revision_patch_file}"

# Merge only Secret data fields so ownership metadata survives. The sensitive
# payload stays in pipes/temp files and never appears in argv or logs.
base64 < "${docker_config}" \
  | tr -d '\r\n' \
  | jq -Rs '{data: {".dockerconfigjson": .}}' \
  > "${patch_file}"

sync_lease_is_available() {
  # Talos machine-config writes do not expose a downstream fencing token. An
  # expired shell process could resume after an automatic timeout takeover and
  # write stale credentials even if every Kubernetes write uses CAS. Therefore
  # expiry is diagnostic only: a non-empty holder always requires explicit
  # recovery after the old process has been proven dead.
  jq -e '(.spec.holderIdentity // "") == ""' \
    "${sync_lease_file}" >/dev/null
}

acquire_sync_lease() {
  local desired_revision="$1"
  local attempt now resource_version current_holder transitions

  sync_lease_holder="${desired_revision:0:16}-$$-${RANDOM}"
  export FLUX_GHCR_SYNC_LEASE_HOLDER="${sync_lease_holder}"
  for attempt in 1 2 3; do
    : > "${sync_lease_file}"
    if ! kubectl \
      --context "${KUBE_CONTEXT}" \
      --namespace flux-system \
      get lease "${SYNC_LEASE_NAME}" \
      --ignore-not-found \
      -o json > "${sync_lease_file}"; then
      echo "::error::Could not inspect the GHCR synchronization lease."
      return 1
    fi
    now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    if [[ ! -s "${sync_lease_file}" ]]; then
      jq -n \
        --arg name "${SYNC_LEASE_NAME}" \
        --arg holder "${sync_lease_holder}" \
        --arg now "${now}" \
        --argjson duration "${SYNC_LEASE_DURATION_SECONDS}" '
        {
          apiVersion: "coordination.k8s.io/v1",
          kind: "Lease",
          metadata: {name: $name, namespace: "flux-system"},
          spec: {
            holderIdentity: $holder,
            leaseDurationSeconds: $duration,
            acquireTime: $now,
            renewTime: $now,
            leaseTransitions: 0
          }
        }
      ' > "${sync_lease_manifest_file}"
      if kubectl \
        --context "${KUBE_CONTEXT}" \
        --namespace flux-system \
        create --filename "${sync_lease_manifest_file}" \
        > "${sync_lease_result_file}" 2>&1; then
        sync_lease_acquired=true
        sync_lease_heartbeat_loop &
        sync_lease_heartbeat_pid=$!
        return 0
      fi
      continue
    fi
    if ! jq -e '
      (.metadata.resourceVersion | type == "string" and length > 0)
      and ((.spec.holderIdentity // "") | type == "string")
      and (.spec.leaseDurationSeconds | type == "number" and . > 0)
      and ((.spec.renewTime // .spec.acquireTime // "")
        | type == "string" and length > 0)
      and ((.spec.leaseTransitions // 0) | type == "number")
    ' "${sync_lease_file}" >/dev/null; then
      echo "::error::The GHCR synchronization lease is malformed; refusing cluster mutation."
      return 1
    fi
    if ! sync_lease_is_available; then
      echo "::error::Another GHCR synchronization transaction holds the synchronization lease; automatic expiry takeover is disabled because Talos writes cannot be fenced. Prove the prior process is dead before explicitly recovering the Lease."
      return 1
    fi
    resource_version="$(jq -er '.metadata.resourceVersion' "${sync_lease_file}")"
    current_holder="$(jq -r '.spec.holderIdentity // ""' "${sync_lease_file}")"
    transitions="$(jq -er '(.spec.leaseTransitions // 0) + 1' "${sync_lease_file}")"
    jq -n \
      --arg resource_version "${resource_version}" \
      --arg current_holder "${current_holder}" \
      --arg holder "${sync_lease_holder}" \
      --arg now "${now}" \
      --argjson duration "${SYNC_LEASE_DURATION_SECONDS}" \
      --argjson transitions "${transitions}" '
      [
        {op: "test", path: "/metadata/resourceVersion", value: $resource_version},
        {op: "test", path: "/spec/holderIdentity", value: $current_holder},
        {op: "replace", path: "/spec/holderIdentity", value: $holder},
        {op: "replace", path: "/spec/leaseDurationSeconds", value: $duration},
        {op: "replace", path: "/spec/acquireTime", value: $now},
        {op: "replace", path: "/spec/renewTime", value: $now},
        {op: "replace", path: "/spec/leaseTransitions", value: $transitions}
      ]
    ' > "${sync_lease_patch_file}"
    if kubectl \
      --context "${KUBE_CONTEXT}" \
      --namespace flux-system \
      patch lease "${SYNC_LEASE_NAME}" \
      --type=json \
      --patch-file="${sync_lease_patch_file}" \
      > "${sync_lease_result_file}" 2>&1; then
      sync_lease_acquired=true
      sync_lease_heartbeat_loop &
      sync_lease_heartbeat_pid=$!
      return 0
    fi
  done

  echo "::error::Could not atomically acquire the GHCR synchronization lease after concurrent updates."
  return 1
}

renew_sync_lease() {
  local invocation_id="$$-${RANDOM}"
  local lease_file="${work_dir}/sync-lease-renew-${invocation_id}.json"
  local patch_file_local="${work_dir}/sync-lease-renew-patch-${invocation_id}.json"
  local result_file="${work_dir}/sync-lease-renew-result-${invocation_id}.txt"
  local resource_version now

  [[ "${sync_lease_acquired}" == "true" && -n "${sync_lease_holder}" ]] || return 1
  if ! kubectl \
    --context "${KUBE_CONTEXT}" \
    --namespace flux-system \
    get lease "${SYNC_LEASE_NAME}" \
    -o json > "${lease_file}"; then
    return 1
  fi
  resource_version="$(jq -er \
    --arg holder "${sync_lease_holder}" '
    select(.spec.holderIdentity == $holder)
    | .metadata.resourceVersion
  ' "${lease_file}")" || return 1
  now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  jq -n \
    --arg resource_version "${resource_version}" \
    --arg holder "${sync_lease_holder}" \
    --arg now "${now}" \
    --argjson duration "${SYNC_LEASE_DURATION_SECONDS}" '
    [
      {op: "test", path: "/metadata/resourceVersion", value: $resource_version},
      {op: "test", path: "/spec/holderIdentity", value: $holder},
      {op: "replace", path: "/spec/renewTime", value: $now},
      {op: "replace", path: "/spec/leaseDurationSeconds", value: $duration}
    ]
  ' > "${patch_file_local}"
  if kubectl \
    --context "${KUBE_CONTEXT}" \
    --namespace flux-system \
    patch lease "${SYNC_LEASE_NAME}" \
    --type=json \
    --patch-file="${patch_file_local}" \
    > "${result_file}" 2>&1; then
    return 0
  fi

  # Foreground guards and the heartbeat can legitimately race each other. A
  # resourceVersion conflict is harmless when the winning renewal still belongs
  # to this transaction and remains live; re-read before declaring lease loss.
  if ! kubectl \
    --context "${KUBE_CONTEXT}" \
    --namespace flux-system \
    get lease "${SYNC_LEASE_NAME}" \
    -o json > "${lease_file}"; then
    return 1
  fi
  jq -e \
    --arg holder "${sync_lease_holder}" \
    --argjson now_epoch "$(date -u +%s)" '
    .spec.holderIdentity == $holder
    and (((.spec.renewTime // .spec.acquireTime)
      | sub("\\.[0-9]+Z$"; "Z")
      | fromdateiso8601) + .spec.leaseDurationSeconds > $now_epoch)
  ' "${lease_file}" >/dev/null
}

sync_lease_heartbeat_loop() {
  local elapsed
  while true; do
    for ((elapsed = 0; elapsed < SYNC_LEASE_HEARTBEAT_SECONDS; elapsed++)); do
      sleep 1
    done
    if ! renew_sync_lease; then
      : > "${sync_lease_lost_file}"
      return 1
    fi
  done
}

assert_sync_lease_held() {
  if [[ -e "${sync_lease_lost_file}" ]] || ! renew_sync_lease; then
    echo "::error::The GHCR synchronization lease was lost; refusing further cluster mutation."
    return 1
  fi
}

release_sync_lease() {
  local lease_file="${work_dir}/sync-lease-release.json"
  local patch_file_local="${work_dir}/sync-lease-release-patch.json"
  local result_file="${work_dir}/sync-lease-release-result.txt"
  local resource_version now

  if [[ -n "${sync_lease_heartbeat_pid}" ]]; then
    kill "${sync_lease_heartbeat_pid}" 2>/dev/null || true
    wait "${sync_lease_heartbeat_pid}" 2>/dev/null || true
    sync_lease_heartbeat_pid=""
  fi
  [[ "${sync_lease_acquired}" == "true" && -n "${sync_lease_holder}" ]] || return 0
  if ! kubectl \
    --context "${KUBE_CONTEXT}" \
    --namespace flux-system \
    get lease "${SYNC_LEASE_NAME}" \
    -o json > "${lease_file}"; then
    return 1
  fi
  resource_version="$(jq -er \
    --arg holder "${sync_lease_holder}" '
    select(.spec.holderIdentity == $holder)
    | .metadata.resourceVersion
  ' "${lease_file}")" || return 1
  now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  jq -n \
    --arg resource_version "${resource_version}" \
    --arg holder "${sync_lease_holder}" \
    --arg now "${now}" '
    [
      {op: "test", path: "/metadata/resourceVersion", value: $resource_version},
      {op: "test", path: "/spec/holderIdentity", value: $holder},
      {op: "replace", path: "/spec/holderIdentity", value: ""},
      {op: "replace", path: "/spec/leaseDurationSeconds", value: 1},
      {op: "replace", path: "/spec/renewTime", value: $now}
    ]
  ' > "${patch_file_local}"
  if ! kubectl \
    --context "${KUBE_CONTEXT}" \
    --namespace flux-system \
    patch lease "${SYNC_LEASE_NAME}" \
    --type=json \
    --patch-file="${patch_file_local}" \
    > "${result_file}" 2>&1; then
    return 1
  fi
  sync_lease_holder=""
  sync_lease_acquired=false
}

# Every Secret write is fenced by the resourceVersion observed after a
# foreground lease renewal. A delayed request from an expired lease holder can
# therefore never overwrite a newer transaction that has already updated the
# same Secret. Keep the credential payload in files so it never enters argv.
patch_secret_data_with_cas() {
  local namespace="$1"
  local name="$2"
  local data_key="$3"
  local payload_file="$4"
  local state_file="$5"
  local cas_patch_file="$6"
  local resource_version attempt patch_status=1

  for attempt in 1 2 3; do
    assert_sync_lease_held || return 1
    if ! kubectl \
      --context "${KUBE_CONTEXT}" \
      --namespace "${namespace}" \
      get secret "${name}" \
      -o json > "${state_file}"; then
      echo "::error::Could not inspect Secret ${namespace}/${name} for an atomic credential update."
      return 1
    fi
    resource_version="$(jq -er '
      .metadata.resourceVersion
      | select(type == "string" and length > 0)
    ' "${state_file}")" || {
      echo "::error::Secret ${namespace}/${name} has no valid resourceVersion; refusing a non-atomic credential update."
      return 1
    }
    jq -n \
      --arg resource_version "${resource_version}" \
      --arg data_path "/data/${data_key}" \
      --arg data_key "${data_key}" \
      --slurpfile payload "${payload_file}" '
      ($payload[0].data[$data_key] // null) as $value
      | if ($value | type) != "string" or ($value | length) == 0 then
          error("credential payload is missing its data key")
        else
          [
            {op: "test", path: "/metadata/resourceVersion", value: $resource_version},
            {op: "add", path: $data_path, value: $value}
          ]
        end
    ' > "${cas_patch_file}"

    # Renew again after the read/build window. If a stale request lands after
    # this point, the captured Secret resourceVersion rejects it; if it lands
    # first, the current holder retries and deterministically wins.
    assert_sync_lease_held || return 1
    if kubectl \
      --context "${KUBE_CONTEXT}" \
      --namespace "${namespace}" \
      patch secret "${name}" \
      --type=json \
      --patch-file="${cas_patch_file}"; then
      return 0
    else
      patch_status=$?
    fi
    assert_sync_lease_held || return 1
  done

  echo "::error::Could not atomically update Secret ${namespace}/${name} after concurrent writes."
  return "${patch_status}"
}

# Patch only the root Flux Secret payload, preserving KSail ownership metadata.
patch_root_secret() {
  patch_secret_data_with_cas \
    flux-system \
    ksail-registry-credentials \
    .dockerconfigjson \
    "${patch_file}" \
    "${root_secret_state_file}" \
    "${root_secret_cas_patch_file}"
}

patch_variables_base() {
  patch_secret_data_with_cas \
    flux-system \
    variables-base \
    ghcr_dockerconfigjson \
    "${variables_patch_file}" \
    "${variables_secret_state_file}" \
    "${variables_secret_cas_patch_file}"
}

acquire_sync_lease "${pull_revision}"

# A fresh DR cluster does not have variables-base or the ESO fan-out resources
# until its first Flux reconcile. In that case the current artifact creates the
# chain from the same SOPS value, so only the root bootstrap patch is needed.
if ! variables_base_name="$(kubectl \
  --context "${KUBE_CONTEXT}" \
  --namespace flux-system \
  get secret variables-base \
  --ignore-not-found \
  -o name)"; then
  echo "::error::Could not determine whether the GHCR fan-out exists; refusing to reconcile with an unverified tenant credential path."
  exit 1
fi
if [[ -z "${variables_base_name}" ]]; then
  if [[ "${allow_incomplete_fanout}" != "true" ]]; then
    echo "::error::The GHCR fan-out is not initialized; root Flux auth was not changed. Use --allow-incomplete-fanout only during the DR bootstrap, then run the full verifier after reconciliation."
    exit 1
  fi
  patch_root_secret
  echo "✅ Refreshed root Flux GHCR auth; the first reconcile will create the downstream fan-out."
  exit 0
fi

# Prepare the variables-base payload locally, but do not mutate its live Secret
# until normal mode has proved the complete fan-out exists. Otherwise a failed
# normal deploy could leave PushSecret free to propagate an unmerged credential
# even though root Flux auth stayed unchanged.
jq '{data: {ghcr_dockerconfigjson: .data[".dockerconfigjson"]}}' \
  "${patch_file}" \
  > "${variables_patch_file}"

# A partially-bootstrapped DR cluster can already have variables-base while ESO
# CRDs or individual fan-out objects do not exist yet. That state still needs
# root auth so Flux can fetch the artifact that completes the chain. Distinguish
# an absent API/resource from a failed lookup, and never force-sync a partial set.
if ! kubectl \
  --context "${KUBE_CONTEXT}" \
  --namespace flux-system \
  api-resources \
  --api-group=external-secrets.io \
  -o name \
  > "${fanout_api_resources}"; then
  echo "::error::Could not inspect the External Secrets API; refusing to change root Flux auth."
  exit 1
fi

fanout_complete=true
if ! grep -qx 'pushsecrets.external-secrets.io' "${fanout_api_resources}" \
  || ! grep -qx 'externalsecrets.external-secrets.io' "${fanout_api_resources}"; then
  fanout_complete=false
else
  if ! pushsecret_name="$(kubectl \
    --context "${KUBE_CONTEXT}" \
    --namespace flux-system \
    get pushsecret seed-ghcr \
    --ignore-not-found \
    -o name)"; then
    echo "::error::Could not determine whether PushSecret flux-system/seed-ghcr exists; refusing to change root Flux auth."
    exit 1
  fi
  if [[ -z "${pushsecret_name}" ]]; then
    fanout_complete=false
  fi

  for namespace in "${FANOUT_NAMESPACES[@]}"; do
    if ! externalsecret_name="$(kubectl \
      --context "${KUBE_CONTEXT}" \
      --namespace "${namespace}" \
      get externalsecret ghcr-auth \
      --ignore-not-found \
      -o name)"; then
      echo "::error::Could not determine whether ExternalSecret ${namespace}/ghcr-auth exists; refusing to change root Flux auth."
      exit 1
    fi
    if [[ -z "${externalsecret_name}" ]]; then
      fanout_complete=false
    fi
  done
fi

if [[ "${fanout_complete}" != "true" ]]; then
  if [[ "${allow_incomplete_fanout}" != "true" ]]; then
    echo "::error::The GHCR fan-out is incomplete; root Flux auth was not changed. Use --allow-incomplete-fanout only during the DR bootstrap, then run the full verifier after reconciliation."
    exit 1
  fi
  patch_root_secret
  patch_variables_base
  patch_root_secret
  echo "✅ Staged the Git/SOPS credential and refreshed root Flux auth; the first reconcile will complete the missing downstream fan-out."
  exit 0
fi

# Existing clusters update and verify the whole SOPS -> variables-base ->
# PushSecret -> OpenBao -> ExternalSecret chain before the first Talos drain.
# Root Flux auth remains last so any failed node proof leaves it unchanged.
stage_fanout_before_talos \
  "${pull_revision}" \
  "${KSAIL_OPERATOR_IMAGE}" \
  "${talos_stage_result_file}" \
  "${FANOUT_NAMESPACES[@]}"

echo "✅ Synchronised every existing consumer and refreshed root Flux GHCR auth from Git/SOPS."
