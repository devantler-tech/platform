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
readonly CORDON_OWNER_ANNOTATION="platform.devantler.tech/ghcr-auth-drain-owner"
readonly CORDON_OWNER_JSON_PATH="/metadata/annotations/platform.devantler.tech~1ghcr-auth-drain-owner"
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
  if [[ -n "${active_runtime_probe}" ]]; then
    kubectl \
      --context "${KUBE_CONTEXT}" \
      --namespace ksail-operator \
      delete pod "${active_runtime_probe}" \
      --ignore-not-found \
      --wait=false \
      >/dev/null 2>&1 || true
  fi
  cleanup_bootstrap_quarantine || true
  rm -rf "${work_dir}"
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
  local attempt create_attempt image_id waiting_reason
  local probe_created=0

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
    image_id="$(jq -r \
      '.status.containerStatuses[0].imageID // ""' \
      "${runtime_probe_state_file}")"
    if [[ -n "${image_id}" ]]; then
      delete_runtime_pull_probe "${probe_name}" || return 1
      return 0
    fi
    waiting_reason="$(jq -r \
      '.status.containerStatuses[0].state.waiting.reason // ""' \
      "${runtime_probe_state_file}")"
    case "${waiting_reason}" in
      ErrImagePull|ImagePullBackOff)
        runtime_probe_bootstrap_needed=1
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

# Atomically claim the right to reverse the cordon and make the node
# unschedulable. Combining both mutations closes the gap where another actor
# could cordon after our ownership annotation but before kubectl drain. A bare
# cordon after this patch is an idempotent no-op; an actor taking over an
# already-cordoned node must replace the annotation to express new ownership.
claim_node_cordon_ownership() {
  local node_name="$1" owner_token="$2" state_file="$3" result_file="$4"
  local resource_version node_uid
  resource_version="$(jq -er '.metadata.resourceVersion' "${state_file}")"
  node_uid="$(jq -er '.metadata.uid' "${state_file}")"

  if jq -e '.metadata.annotations | type == "object"' \
    "${state_file}" >/dev/null; then
    jq -n \
      --arg owner_path "${CORDON_OWNER_JSON_PATH}" \
      --arg owner "${owner_token}" \
      --arg uid "${node_uid}" \
      --arg resource_version "${resource_version}" '
      [
        {
          op: "test",
          path: "/metadata/resourceVersion",
          value: $resource_version
        },
        {op: "test", path: "/metadata/uid", value: $uid},
        {op: "add", path: $owner_path, value: $owner},
        {op: "add", path: "/spec/unschedulable", value: true}
      ]
    ' > "${cordon_claim_patch_file}"
  else
    jq -n \
      --arg owner_annotation "${CORDON_OWNER_ANNOTATION}" \
      --arg owner "${owner_token}" \
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
          value: {($owner_annotation): $owner}
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
  local current_resource_version
  [[ "${was_cordoned}" == "0" ]] || return 0

  if [[ -z "${owner_token}" ]]; then
    echo "::error::Refusing to uncordon Talos node ${node_name} without a bridge ownership token."
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

  jq -n \
    --arg path "${CORDON_OWNER_JSON_PATH}" \
    --arg owner "${owner_token}" \
    --arg uid "${initial_node_uid}" \
    --arg resource_version "${current_resource_version}" '
    [
      {op: "test", path: $path, value: $owner},
      {op: "test", path: "/metadata/uid", value: $uid},
      {
        op: "test",
        path: "/metadata/resourceVersion",
        value: $resource_version
      },
      {op: "add", path: "/spec/unschedulable", value: false},
      {op: "remove", path: $path}
    ]
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
  echo "Restored schedulability on ${node_name}."
}

# Rollback only bootstrap cordons still owned by this invocation. A node that
# reached the reboot edge stays cordoned on uncertainty, matching the normal
# fail-closed path. Missing ownership means the node was already restored or a
# newer actor took over; neither case is ours to reverse.
cleanup_bootstrap_quarantine() {
  local state_file node_name was_cordoned owner_token initial_uid
  local initial_taints current_owner

  [[ -d "${bootstrap_cordon_dir:-}" ]] || return 0
  for state_file in "${bootstrap_cordon_dir}"/*.json; do
    [[ -e "${state_file}" ]] || continue
    node_name="$(jq -er '.nodeName' "${state_file}")" || continue
    was_cordoned="$(jq -er '.wasCordoned' "${state_file}")" || continue
    if [[ "${was_cordoned}" == "1" ]]; then
      rm -f "${state_file}"
      continue
    fi
    if [[ -e "${bootstrap_retain_dir}/${node_name}" ]]; then
      continue
    fi
    owner_token="$(jq -er '.ownerToken' "${state_file}")" || continue
    initial_uid="$(jq -er '.initialUID' "${state_file}")" || continue
    initial_taints="$(jq -c '.initialTaints' "${state_file}")" || continue
    if ! kubectl \
      --context "${KUBE_CONTEXT}" \
      get node "${node_name}" \
      --output json \
      > "${cordon_state_file}" 2>/dev/null; then
      continue
    fi
    current_owner="$(jq -r \
      --arg owner_annotation "${CORDON_OWNER_ANNOTATION}" \
      '.metadata.annotations[$owner_annotation] // ""' \
      "${cordon_state_file}")"
    if [[ "${current_owner}" != "${owner_token}" ]]; then
      [[ -z "${current_owner}" ]] && rm -f "${state_file}"
      continue
    fi
    if restore_node_schedulability_if_needed \
      "${node_name}" 0 "${owner_token}" \
      "${initial_uid}" "${initial_taints}" \
      "${drain_result_file}"; then
      rm -f "${state_file}"
    fi
  done
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
    all(.items[]?;
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
  local initial_taints bootstrap_owner
  local workload_rc

  bootstrap_seed_uid=""
  : > "${bootstrap_ordered_targets}"

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
      owner_token=""
    else
      was_cordoned=0
      owner_token="${bootstrap_owner}"
    fi
    if ! jq -n \
      --arg node_name "${node_name}" \
      --arg owner_token "${owner_token}" \
      --arg initial_uid "${node_uid}" \
      --argjson was_cordoned "${was_cordoned}" \
      --argjson initial_taints "${initial_taints}" '
      {
        nodeName: $node_name,
        ownerToken: $owner_token,
        initialUID: $initial_uid,
        wasCordoned: $was_cordoned,
        initialTaints: $initial_taints
      }
    ' > "${state_file}"; then
      echo "::error::Could not persist bootstrap ownership state for stale node ${node_name}."
      return 1
    fi
    if [[ "${was_cordoned}" == "0" ]] \
      && ! claim_node_cordon_ownership \
        "${node_name}" "${owner_token}" \
        "${cordon_state_file}" "${drain_result_file}"; then
      return 1
    fi
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
  local was_cordoned=0 existing_cordon_owner="" cordon_owner_token=""
  local initial_node_uid="" initial_node_taints="[]"
  local bootstrap_state_file="${bootstrap_cordon_dir}/${node_name}.json"

  if [[ "${node_mode}" != "reboot" && "${node_mode}" != "image-only" ]]; then
    echo "::error::Unknown Talos GHCR synchronization mode '${node_mode}' for ${node_name}."
    return 1
  fi
  revalidate_selected_node_identity_before_mutation \
    "${node_name}" "${node_uid}" "${node_ip}" "${node_role}" || return 1

  if [[ "${node_mode}" == "reboot" ]]; then
    if ! talosctl \
      --nodes "${node_ip}" \
      patch machineconfig \
      --mode=no-reboot \
      --patch-file="${talos_auth_patch_file}" \
      >"${talos_result_file}" 2>&1; then
      echo "::error::Talos node ${node_name} did not accept the Git/SOPS GHCR registry auth."
      return 1
    fi

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
      if jq -e '.spec.unschedulable == true' \
        "${cordon_state_file}" >/dev/null; then
        was_cordoned=1
      else
        cordon_owner_token="${desired_revision:0:16}-$$-${RANDOM}"
        claim_node_cordon_ownership \
          "${node_name}" "${cordon_owner_token}" \
          "${cordon_state_file}" "${drain_result_file}" || return 1
      fi
    fi

  if [[ "${node_mode}" == "reboot" ]]; then
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
        "${drain_result_file}" || return 1
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
        "${drain_result_file}" || return 1
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
      : > "${bootstrap_retain_dir}/${node_name}"
    fi
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

    # Restore original scheduling intent only after the uncached pull succeeds.
    # On failure, a cordon claimed by this bridge remains as a fail-closed signal
    # that the node must not receive new pods until registry access is repaired.
    rm -f "${bootstrap_retain_dir}/${node_name}"
    restore_node_schedulability_if_needed \
      "${node_name}" "${was_cordoned}" "${cordon_owner_token}" \
      "${initial_node_uid}" "${initial_node_taints}" \
      "${drain_result_file}" || return 1

    # The scheduling release is a Kubernetes mutation and therefore another
    # autoscaler replacement boundary. Rebind the selected UID and InternalIP
    # at the last possible point before the final Talos proof-marker write.
    revalidate_selected_node_identity_before_mutation \
      "${node_name}" "${node_uid}" "${node_ip}" "${node_role}" || return 1

    # Recorded LAST, and only now: the marker means "this node's containerd has
    # provably loaded this credential revision", so it must not be written
    # before the reboot that makes that true.
    if ! talosctl \
      --nodes "${node_ip}" \
      patch machineconfig \
      --mode=no-reboot \
      --patch-file="${talos_revision_patch_file}" \
      >"${talos_result_file}" 2>&1; then
      echo "::error::Talos node ${node_name} proved GHCR access but could not record the synchronized credential revision."
      return 1
    fi
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

# Patch only the root Flux Secret payload, preserving KSail ownership metadata.
patch_root_secret() {
  kubectl \
    --context "${KUBE_CONTEXT}" \
    --namespace flux-system \
    patch secret ksail-registry-credentials \
    --type=merge \
    --patch-file="${patch_file}"
}

patch_variables_base() {
  kubectl \
    --context "${KUBE_CONTEXT}" \
    --namespace flux-system \
    patch secret variables-base \
    --type=merge \
    --patch-file="${variables_patch_file}"
}

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
