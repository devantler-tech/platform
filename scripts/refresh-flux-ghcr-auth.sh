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
readonly -a REQUIRED_PULL_REPOSITORIES=(
  "devantler-tech/platform/manifests"
  "devantler-tech/wedding-app/manifests"
  "devantler-tech/ascoachingogvaner/manifests"
  "devantler-tech/wedding-app"
  "devantler-tech/ascoachingogvaner"
)

work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT
chmod 700 "${work_dir}"
umask 077

docker_config="${work_dir}/config.json"
basic_curl_config="${work_dir}/curl-basic.config"
bearer_curl_config="${work_dir}/curl-bearer.config"
token_response="${work_dir}/token.json"
patch_file="${work_dir}/patch.json"

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
  | (
      (($auth.username | non_empty_string)
        and ($auth.password | non_empty_string))
      or
      (try (
        $auth.auth
        | @base64d
        | capture("^(?<username>[^:]+):(?<password>.+)$")
        | ((.username | non_empty_string)
          and (.password | non_empty_string))
      ) catch false)
    )
' "${docker_config}" >/dev/null; then
  echo "::error::The SOPS GHCR pull credential is not a valid Docker config with non-empty ghcr.io username/password or auth fields."
  exit 1
fi

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

# Merge only the Secret data field so KSail's ownership label and any unrelated
# metadata survive. The sensitive payload stays in the pipe/temp file and never
# appears in argv or logs.
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

echo "✅ Refreshed the root Flux GHCR pull credential from Git/SOPS."
