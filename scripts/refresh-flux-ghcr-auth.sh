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
readonly IMAGE="${FLUX_GHCR_IMAGE:-ghcr.io/devantler-tech/platform/manifests:latest}"
readonly KUBE_CONTEXT="${KUBE_CONTEXT:-admin@prod}"
readonly -a REQUIRED_PULL_REPOSITORIES=(
  "devantler-tech/platform/manifests"
  "devantler-tech/wedding-app/manifests"
  "devantler-tech/ascoachingogvaner/manifests"
)

work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT
chmod 700 "${work_dir}"
umask 077

docker_config="${work_dir}/config.json"
curl_config="${work_dir}/curl.config"
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
' "${docker_config}" > "${curl_config}"
chmod 600 "${curl_config}"

# The same pull credential fans out to Platform and both private tenant OCI
# sources. Prove GHCR grants every required package scope before changing the
# cluster; checking only Platform would let a partially-authorized rotation
# break the tenants later.
for repository in "${REQUIRED_PULL_REPOSITORIES[@]}"; do
  if ! http_status="$(curl \
    --config "${curl_config}" \
    --silent \
    --show-error \
    --output /dev/null \
    --write-out '%{http_code}' \
    --get \
    --data-urlencode 'service=ghcr.io' \
    --data-urlencode "scope=repository:${repository}:pull" \
    'https://ghcr.io/token')"; then
    echo "::error::Could not validate the SOPS GHCR pull credential for ${repository}; the root Flux Secret was not changed."
    exit 1
  fi
  if [[ "${http_status}" != "200" ]]; then
    echo "::error::The SOPS GHCR pull credential cannot read ${repository} (GHCR HTTP ${http_status}); the root Flux Secret was not changed."
    exit 1
  fi
done

# Also prove the production artifact exists and can be resolved through a real
# OCI client. DOCKER_CONFIG isolates this check from the workflow's separate
# push credential in ~/.docker/config.json.
if ! DOCKER_CONFIG="${work_dir}" docker buildx imagetools inspect "${IMAGE}" >/dev/null; then
  echo "::error::The SOPS GHCR pull credential cannot read ${IMAGE}; the root Flux Secret was not changed."
  exit 1
fi

if [[ "${check_only}" == "true" ]]; then
  echo "✅ Validated every required GHCR pull scope from Git/SOPS."
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
