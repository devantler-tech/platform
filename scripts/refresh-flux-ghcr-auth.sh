#!/usr/bin/env bash
# Refresh the KSail-managed root Flux pull Secret from the Git/SOPS source.
#
# Flux cannot fetch the artifact containing a rotated credential while its
# bootstrap Secret is stale. Keep this bridge outside Flux so a deployment can
# repair that bootstrap edge before asking Flux to reconcile.

set -euo pipefail

check_only=false
if (($# > 1)); then
  echo "Usage: $0 [--check-only]" >&2
  exit 64
fi
if (($# == 1)); then
  if [[ "$1" != "--check-only" ]]; then
    echo "Usage: $0 [--check-only]" >&2
    exit 64
  fi
  check_only=true
fi

readonly SECRET_FILE="${FLUX_GHCR_SECRET_FILE:-k8s/bases/bootstrap/secret.enc.yaml}"
readonly KUBE_CONTEXT="${KUBE_CONTEXT:-admin@prod}"
readonly SYNC_ATTEMPTS="${FLUX_GHCR_SYNC_ATTEMPTS:-60}"
readonly SYNC_INTERVAL="${FLUX_GHCR_SYNC_INTERVAL:-2}"
readonly -a REQUIRED_PULL_REPOSITORIES=(
  "devantler-tech/platform/manifests"
  "devantler-tech/wedding-app/manifests"
  "devantler-tech/ascoachingogvaner/manifests"
  "devantler-tech/wedding-app"
  "devantler-tech/ascoachingogvaner"
  "devantler-tech/ksail"
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
basic_curl_config="${work_dir}/curl-basic.config"
bearer_curl_config="${work_dir}/curl-bearer.config"
token_response="${work_dir}/token.json"
patch_file="${work_dir}/patch.json"
variables_patch_file="${work_dir}/variables-patch.json"
expected_normalized="${work_dir}/expected-normalized.json"

force_sync_resource() {
  local kind="$1"
  local namespace="$2"
  local name="$3"
  local before_file="${work_dir}/${kind}-${namespace}-${name}-before.json"
  local current_file="${work_dir}/${kind}-${namespace}-${name}-current.json"
  local before_refresh
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
    >/dev/null

  for ((attempt = 1; attempt <= SYNC_ATTEMPTS; attempt++)); do
    kubectl \
      --context "${KUBE_CONTEXT}" \
      --namespace "${namespace}" \
      get "${kind}" "${name}" \
      -o json \
      > "${current_file}"
    if jq -e --arg before "${before_refresh}" '
      (.status.refreshTime // "") as $refresh
      | ($refresh != "" and $refresh != $before)
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

# KSail embeds SOPS, so the deploy uses the same pinned toolchain as workload
# reconciliation. Decrypt only the Docker config scalar and never emit it to
# stdout or place its plaintext/base64 representation in an argument.
ksail workload cipher decrypt \
  "${SECRET_FILE}" \
  --extract '["stringData"]["ghcr_dockerconfigjson"]' \
  --output "${docker_config}" \
  >/dev/null
chmod 600 "${docker_config}"

if ! jq -e '
  def non_empty_string: type == "string" and length > 0;
  (.auths["ghcr.io"] // {}) as $auth
  | ((($auth | has("username")) or ($auth | has("password"))))
      as $explicit_present
  | (($auth.username | non_empty_string)
      and ($auth.password | non_empty_string)) as $explicit_valid
  | ($auth | has("auth")) as $encoded_present
  | (if $encoded_present then
      try (
        $auth.auth
        | @base64d
        | capture("^(?<username>[^:]+):(?<password>.+)$")
      ) catch null
    else null end) as $decoded
  | (($decoded != null)
      and ($decoded.username | non_empty_string)
      and ($decoded.password | non_empty_string)) as $encoded_valid
  | ((($explicit_present | not) or $explicit_valid)
      and (($encoded_present | not) or $encoded_valid)
      and ($explicit_valid or $encoded_valid)
      and (((($explicit_present and $encoded_present) | not))
        or (($auth.username == $decoded.username)
          and ($auth.password == $decoded.password))))
' "${docker_config}" >/dev/null; then
  echo "::error::The SOPS GHCR pull credential is not a valid Docker config with non-empty, consistent ghcr.io username/password and auth fields."
  exit 1
fi
jq -S -c . "${docker_config}" > "${expected_normalized}"

# Build curl's Basic-auth config without putting the credential in argv or
# stdout. Support both Docker config representations used in this repository:
# explicit username/password and base64(username:password) in auth.
jq -r '
  def non_empty_string: type == "string" and length > 0;
  (.auths["ghcr.io"] // {}) as $auth
  | if (($auth.username | non_empty_string)
      and ($auth.password | non_empty_string)) then
      "user = " + (($auth.username + ":" + $auth.password) | @json)
    else
      "user = " + (($auth.auth | @base64d) | @json)
    end
' "${docker_config}" > "${basic_curl_config}"
chmod 600 "${basic_curl_config}"

# The same pull credential fans out to Flux OCI sources and private tenant
# workloads. GHCR permissions are package-granular, and the token endpoint can
# return only the intersection of requested and granted scopes. Therefore a
# token HTTP 200 is not proof of pull access: exchange it for a bearer token,
# then perform a real registry manifest GET for every package. Both credentials
# stay in mode-0600 files. --disable must remain curl's first argument so an
# ambient ~/.curlrc cannot enable tracing, add URLs, or otherwise expose auth.
for repository in "${REQUIRED_PULL_REPOSITORIES[@]}"; do
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
    "https://ghcr.io/v2/${repository}/manifests/latest")"; then
    echo "::error::Could not read the GHCR manifest for ${repository}; the root Flux Secret was not changed."
    exit 1
  fi
  if [[ "${http_status}" != "200" ]]; then
    echo "::error::The SOPS GHCR pull credential cannot read ${repository}:latest (GHCR HTTP ${http_status}); the root Flux Secret was not changed."
    exit 1
  fi
done

if [[ "${check_only}" == "true" ]]; then
  echo "✅ Validated every required GHCR package pull from Git/SOPS."
  exit 0
fi

# Merge only Secret data fields so ownership metadata survives. The sensitive
# payload stays in pipes/temp files and never appears in argv or logs.
base64 < "${docker_config}" \
  | tr -d '\r\n' \
  | jq -Rs '{data: {".dockerconfigjson": .}}' \
  > "${patch_file}"

kubectl \
  --context "${KUBE_CONTEXT}" \
  --namespace flux-system \
  patch secret ksail-registry-credentials \
  --type=merge \
  --patch-file="${patch_file}"

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
  echo "✅ Refreshed root Flux GHCR auth; the first reconcile will create the downstream fan-out."
  exit 0
fi

# Existing clusters must update the whole SOPS -> variables-base -> PushSecret
# -> OpenBao -> ExternalSecret chain before apps reconcile. Otherwise the root
# source recovers while tenant OCI/image pulls keep the revoked credential for
# up to their one-hour refresh interval.
jq '{data: {ghcr_dockerconfigjson: .data[".dockerconfigjson"]}}' \
  "${patch_file}" \
  > "${variables_patch_file}"
kubectl \
  --context "${KUBE_CONTEXT}" \
  --namespace flux-system \
  patch secret variables-base \
  --type=merge \
  --patch-file="${variables_patch_file}"

force_sync_resource pushsecret flux-system seed-ghcr
for namespace in "${FANOUT_NAMESPACES[@]}"; do
  force_sync_resource externalsecret "${namespace}" ghcr-auth
  verify_consumer_secret "${namespace}"
done

echo "✅ Refreshed root Flux GHCR auth and synchronised every existing consumer from Git/SOPS."
