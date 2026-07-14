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
KSAIL_OPERATOR_VERSION="$(yq -er '.spec.chart.spec.version' \
  k8s/bases/infrastructure/controllers/ksail-operator/helm-release.yaml)"
readonly KSAIL_OPERATOR_VERSION
readonly -a REQUIRED_PULL_TARGETS=(
  "devantler-tech/platform/manifests:latest"
  "devantler-tech/wedding-app/manifests:latest"
  "devantler-tech/ascoachingogvaner/manifests:latest"
  "devantler-tech/wedding-app:latest"
  "devantler-tech/ascoachingogvaner:latest"
  "devantler-tech/ksail:v${KSAIL_OPERATOR_VERSION}"
  "devantler-tech/provider-upjet-unifi:v0.1.0"
)
readonly -a FANOUT_NAMESPACES=(
  "wedding-app"
  "ascoachingogvaner"
  "kyverno"
)

if ! [[ "${SYNC_ATTEMPTS}" =~ ^[1-9][0-9]*$ ]] \
  || ! [[ "${SYNC_INTERVAL}" =~ ^[0-9]+([.][0-9]+)?$ ]]; then
  echo "::error::FLUX_GHCR_SYNC_ATTEMPTS and FLUX_GHCR_SYNC_INTERVAL must be non-negative numbers, with at least one attempt."
  exit 64
fi

work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT
chmod 700 "${work_dir}"
umask 077

docker_config="${work_dir}/config.json"
credentials_file="${work_dir}/credentials.json"
basic_curl_config="${work_dir}/curl-basic.config"
bearer_curl_config="${work_dir}/curl-bearer.config"
token_response="${work_dir}/token.json"
patch_file="${work_dir}/patch.json"
variables_patch_file="${work_dir}/variables-patch.json"
expected_normalized="${work_dir}/expected-normalized.json"
fanout_api_resources="${work_dir}/fanout-api-resources.txt"
talos_auth_patch_file="${work_dir}/talos-registry-auth.json"
talos_revision_patch_file="${work_dir}/talos-registry-revision.json"
talos_result_file="${work_dir}/talos-result.txt"
talos_nodes_file="${work_dir}/talos-nodes.json"
talos_node_targets="${work_dir}/talos-node-targets.tsv"
ksail_operator_deployment="${work_dir}/ksail-operator-deployment.json"

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

# Apply Git/SOPS auth to stale Talos nodes, prove an exact image pull, and only
# then record the non-secret revision marker that makes the operation retryable.
sync_talos_registry_auth() {
  local desired_revision="$1"
  local operator_image="ghcr.io/devantler-tech/ksail:v${KSAIL_OPERATOR_VERSION}"
  local image_reference
  local node_name
  local node_ip
  local _node_role

  if ! kubectl \
    --context "${KUBE_CONTEXT}" \
    get nodes \
    -o json \
    > "${talos_nodes_file}"; then
    echo "::error::Could not list Talos nodes; refusing to mutate any Kubernetes credential consumers."
    return 1
  fi
  if ! jq -e '
    (.items | length) > 0
    and all(.items[];
      ([.status.addresses[]? | select(.type == "InternalIP") | .address]
        | length) == 1
      and (([.status.addresses[]?
        | select(.type == "InternalIP") | .address][0])
        | type == "string" and length > 0))
    and (([.items[]
      | [.status.addresses[]?
        | select(.type == "InternalIP") | .address][0]]
      | unique | length) == (.items | length))
  ' "${talos_nodes_file}" >/dev/null; then
    echo "::error::Every Talos node must expose exactly one non-empty, unique InternalIP before GHCR auth can be synchronized."
    return 1
  fi

  if ! jq -r --arg revision "${desired_revision}" '
    .items[]
    | select(
        (.metadata.annotations[
          "platform.devantler.tech/ghcr-pull-verified-revision"
        ] // "")
          != $revision
      )
    | (.metadata.labels // {}) as $labels
    | [
        (if (($labels | has("node-role.kubernetes.io/control-plane"))
          or ($labels | has("node-role.kubernetes.io/master")))
          then "1" else "0" end),
        .metadata.name,
        ([.status.addresses[]
          | select(.type == "InternalIP") | .address][0])
      ]
    | @tsv
  ' "${talos_nodes_file}" \
    | LC_ALL=C sort -k1,1 -k2,2 \
    > "${talos_node_targets}"; then
    echo "::error::Could not select Talos nodes requiring the GHCR auth revision."
    return 1
  fi

  # Normal deploys should not regain an all-node Talos API dependency once the
  # current ciphertext revision has been proved on every node.
  if [[ ! -s "${talos_node_targets}" ]]; then
    return 0
  fi

  if ! kubectl \
    --context "${KUBE_CONTEXT}" \
    --namespace ksail-operator \
    get deployment ksail-operator \
    --ignore-not-found \
    -o json \
    > "${ksail_operator_deployment}"; then
    echo "::error::Could not determine the exact live KSail operator image."
    return 1
  fi
  if [[ -s "${ksail_operator_deployment}" ]]; then
    if ! operator_image="$(jq -er '
      [.spec.template.spec.containers[]? | select(.name == "operator") | .image]
      | if length == 1 then .[0] else error("expected one operator image") end
    ' "${ksail_operator_deployment}")"; then
      echo "::error::The live ksail-operator Deployment does not expose one expected first-party image."
      return 1
    fi
  elif [[ "${allow_incomplete_fanout}" != "true" ]]; then
    echo "::error::The live ksail-operator Deployment is absent outside explicit DR bootstrap mode."
    return 1
  fi

  if [[ "${operator_image}" == ghcr.io/devantler-tech/ksail:* ]]; then
    image_reference="${operator_image##*:}"
    if [[ -z "${image_reference}" || "${image_reference}" == "latest" ]]; then
      echo "::error::The KSail operator verification image must use an exact non-latest tag or digest."
      return 1
    fi
  elif ! [[ "${operator_image}" =~ ^ghcr\.io/devantler-tech/ksail@sha256:[0-9a-f]{64}$ ]]; then
    echo "::error::The KSail operator verification image must be the expected first-party package."
    return 1
  fi

  : > "${talos_result_file}"
  chmod 600 "${talos_result_file}"
  while IFS=$'\t' read -r _node_role node_name node_ip; do
    if ! talosctl \
      --nodes "${node_ip}" \
      patch machineconfig \
      --mode=no-reboot \
      --patch-file="${talos_auth_patch_file}" \
      >"${talos_result_file}" 2>&1; then
      echo "::error::Talos node ${node_name} did not accept the Git/SOPS GHCR registry auth."
      return 1
    fi
    if ! talosctl \
      --nodes "${node_ip}" \
      image pull "${operator_image}" \
      --namespace cri \
      >"${talos_result_file}" 2>&1; then
      echo "::error::Talos node ${node_name} could not pull the exact live KSail image after its auth refresh."
      return 1
    fi
    if ! talosctl \
      --nodes "${node_ip}" \
      patch machineconfig \
      --mode=no-reboot \
      --patch-file="${talos_revision_patch_file}" \
      >"${talos_result_file}" 2>&1; then
      echo "::error::Talos node ${node_name} proved GHCR access but could not record the synchronized credential revision."
      return 1
    fi
  done < "${talos_node_targets}"
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

# The same pull credential fans out to Flux OCI sources and private tenant
# workloads. GHCR permissions are package-granular, and the token endpoint can
# return only the intersection of requested and granted scopes. Therefore a
# token HTTP 200 is not proof of pull access: exchange it for a bearer token,
# then perform a real registry manifest GET for every package. Both credentials
# stay in mode-0600 files. --disable must remain curl's first argument so an
# ambient ~/.curlrc cannot enable tracing, add URLs, or otherwise expose auth.
for target in "${REQUIRED_PULL_TARGETS[@]}"; do
  repository="${target%:*}"
  reference="${target##*:}"
  if ! http_status="$(curl --disable \
    --config "${basic_curl_config}" \
    --silent \
    --show-error \
    --output "${token_response}" \
    --write-out '%{http_code}' \
    --get \
    --data-urlencode 'service=ghcr.io' \
    --data-urlencode "scope=repository:${repository}:pull" \
    'https://ghcr.io/token')"; then
    echo "::error::Could not request a GHCR pull token for ${repository}; the root Flux Secret was not changed."
    exit 1
  fi
  if [[ "${http_status}" != "200" ]] || ! jq -e '
    (.token // .access_token // "")
    | type == "string" and length > 0
  ' "${token_response}" >/dev/null; then
    echo "::error::The SOPS GHCR credential could not obtain a pull token for ${repository} (GHCR HTTP ${http_status}); the root Flux Secret was not changed."
    exit 1
  fi

  jq -r '
    (.token // .access_token) as $token
    | "header = " + (("Authorization: Bearer " + $token) | @json)
  ' "${token_response}" > "${bearer_curl_config}"
  chmod 600 "${bearer_curl_config}"

  if ! http_status="$(curl --disable \
    --config "${bearer_curl_config}" \
    --silent \
    --show-error \
    --output /dev/null \
    --write-out '%{http_code}' \
    --header 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json' \
    "https://ghcr.io/v2/${repository}/manifests/${reference}")"; then
    echo "::error::Could not read the GHCR manifest for ${target}; the root Flux Secret was not changed."
    exit 1
  fi
  if [[ "${http_status}" != "200" ]]; then
    echo "::error::The SOPS GHCR pull credential cannot read ${target} (GHCR HTTP ${http_status}); the root Flux Secret was not changed."
    exit 1
  fi
done

if [[ "${check_only}" == "true" ]]; then
  echo "✅ Validated every required GHCR package pull from Git/SOPS."
  exit 0
fi

# Talos image verification resolves cosign artifacts with host registry auth;
# pod imagePullSecrets cannot satisfy that request. Use the supported v1.13
# RegistryAuthConfig document and a file-backed patch so credentials never
# enter argv. Synchronize and verify nodes before touching Kubernetes Secrets.
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
jq -n --arg revision "${pull_revision}" '
  {
    machine: {
      nodeAnnotations: {
        "platform.devantler.tech/ghcr-pull-verified-revision": $revision
      }
    }
  }
' > "${talos_revision_patch_file}"
chmod 600 "${talos_auth_patch_file}" "${talos_revision_patch_file}"
sync_talos_registry_auth "${pull_revision}"

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

# Existing clusters must update and verify the whole SOPS -> variables-base ->
# PushSecret -> OpenBao -> ExternalSecret chain before switching root Flux auth.
# Otherwise a failed fan-out can still let normal root-source polling apply the
# newly-published artifact while tenant OCI/image pulls keep stale credentials.
jq '{data: {ghcr_dockerconfigjson: .data[".dockerconfigjson"]}}' \
  "${patch_file}" \
  > "${variables_patch_file}"
kubectl \
  --context "${KUBE_CONTEXT}" \
  --namespace flux-system \
  patch secret variables-base \
  --type=merge \
  --patch-file="${variables_patch_file}"

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
  echo "✅ Staged the Git/SOPS credential and refreshed root Flux auth; the first reconcile will complete the missing downstream fan-out."
  exit 0
fi

force_sync_resource pushsecret flux-system seed-ghcr
for namespace in "${FANOUT_NAMESPACES[@]}"; do
  force_sync_resource externalsecret "${namespace}" ghcr-auth
  verify_consumer_secret "${namespace}"
done

patch_root_secret

echo "✅ Synchronised every existing consumer and refreshed root Flux GHCR auth from Git/SOPS."
