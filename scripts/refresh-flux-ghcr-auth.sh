#!/usr/bin/env bash
# Refresh the KSail-managed root Flux pull Secret from the Git/SOPS source.
#
# Flux cannot fetch the artifact containing a rotated credential while its
# bootstrap Secret is stale. Keep this bridge outside Flux so a deployment can
# repair that bootstrap edge before asking Flux to reconcile.

set -euo pipefail

readonly SECRET_FILE="${FLUX_GHCR_SECRET_FILE:-k8s/bases/bootstrap/secret.enc.yaml}"
readonly IMAGE="${FLUX_GHCR_IMAGE:-ghcr.io/devantler-tech/platform/manifests:latest}"
readonly KUBE_CONTEXT="${KUBE_CONTEXT:-admin@prod}"

work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT
chmod 700 "${work_dir}"
umask 077

docker_config="${work_dir}/config.json"
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
  (.auths["ghcr.io"] // {})
  | (.username | type == "string" and length > 0)
    and (.password | type == "string" and length > 0)
' "${docker_config}" >/dev/null; then
  echo "::error::The SOPS GHCR pull credential is not a valid Docker config with non-empty ghcr.io username/password fields."
  exit 1
fi

# Prove the decrypted credential can actually pull the production artifact
# before mutating the cluster. DOCKER_CONFIG isolates it from the workflow's
# separate push credential in ~/.docker/config.json.
if ! DOCKER_CONFIG="${work_dir}" docker buildx imagetools inspect "${IMAGE}" >/dev/null; then
  echo "::error::The SOPS GHCR pull credential cannot read ${IMAGE}; the root Flux Secret was not changed."
  exit 1
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
