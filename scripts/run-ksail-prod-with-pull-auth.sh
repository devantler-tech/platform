#!/usr/bin/env bash
# Run credential-consuming KSail lifecycle commands with Git/SOPS pull auth.

set -euo pipefail
set +x

case "${1:-} ${2:-}" in
  "cluster create" | "cluster update" | "workload push" | "workload reconcile") ;;
  *)
    echo "Usage: $0 {cluster create|cluster update|workload push|workload reconcile}" >&2
    exit 64
    ;;
esac
if (($# != 2)); then
  echo "Usage: $0 {cluster create|cluster update|workload push|workload reconcile}" >&2
  exit 64
fi

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
# shellcheck source=scripts/ghcr-auth-lib.sh
source "${SCRIPT_DIR}/ghcr-auth-lib.sh"
require_flux_ghcr_yaml_tool

readonly SECRET_FILE="${FLUX_GHCR_SECRET_FILE:-k8s/bases/bootstrap/secret.enc.yaml}"
# Viper applies this nested config override before KSail expands the two child
# environment variables. The template itself contains no credential.
# shellcheck disable=SC2016
readonly PULL_REGISTRY_TEMPLATE='${GHCR_USERNAME}:${GHCR_TOKEN}@ghcr.io/devantler-tech/platform/manifests'
pull_revision="$(flux_ghcr_revision "${SECRET_FILE}")"
readonly pull_revision

# A shell variable is required to pass the token in the child environment.
# It never enters argv or stdout, and the constrained command set prevents an
# arbitrary process from inheriting it. The Actions GHCR_TOKEN remains in the
# workload-push step; only lifecycle reads override it. The non-secret SOPS
# ciphertext revision defeats KSail's deliberate credential redaction in
# machine-config fingerprints, so autoscaler templates still refresh on
# token-only rotations.
if [[ "$1 $2" == "workload push" ]]; then
  if [[ -z "${GHCR_TOKEN:-}" || -z "${GITHUB_ACTOR:-}" ]]; then
    echo "::error::The Actions GHCR publish token and actor are required for workload push."
    exit 1
  fi
  GHCR_USERNAME="${GITHUB_ACTOR}" \
    GHCR_PULL_REVISION="${pull_revision}" \
    ksail --config ksail.prod.yaml "$1" "$2"
else
  work_dir="$(mktemp -d)"
  trap 'rm -rf "${work_dir}"' EXIT
  chmod 700 "${work_dir}"
  umask 077

  docker_config="${work_dir}/config.json"
  credentials_file="${work_dir}/credentials.json"
  decrypt_flux_ghcr_docker_config "${docker_config}" "${SECRET_FILE}"
  write_flux_ghcr_credentials "${docker_config}" "${credentials_file}"
  pull_username="$(jq -er '.username' "${credentials_file}")"
  pull_token="$(jq -er '.password' "${credentials_file}")"
  KSAIL_SPEC_CLUSTER_LOCALREGISTRY_REGISTRY="${PULL_REGISTRY_TEMPLATE}" \
    GHCR_USERNAME="${pull_username}" \
    GHCR_TOKEN="${pull_token}" \
    GHCR_PULL_REVISION="${pull_revision}" \
    ksail --config ksail.prod.yaml "$1" "$2"
fi
