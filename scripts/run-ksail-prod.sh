#!/usr/bin/env bash
# Run the production KSail commands the deploy and DR paths use.
#
# First-party GHCR packages are public (platform#6272), so no lifecycle
# command needs a pull credential: the root Flux source, tenants and nodes all
# pull anonymously. Only `workload push` writes to GHCR, so only push gets a
# credentialed registry, and only the Actions write token.

set -euo pipefail
set +x

usage() {
  echo "Usage: $0 {cluster create|cluster update|workload reconcile|workload push oci://ghcr.io/devantler-tech/platform/manifests:staging-<sha>-<run>-<attempt>}" >&2
  exit 64
}

case "${1:-} ${2:-}" in
  "cluster create" | "cluster update" | "workload reconcile")
    (($# == 2)) || usage
    ;;
  "workload push")
    (($# == 3)) || usage
    [[ "$3" =~ ^oci://ghcr\.io/devantler-tech/platform/manifests:staging-[0-9a-f]{40}-[0-9]+-[0-9]+$ ]] || usage
    ;;
  *) usage ;;
esac

if [[ "$1 $2" != "workload push" ]]; then
  exec ksail --config ksail.prod.yaml "$1" "$2"
fi

if [[ -z "${GHCR_TOKEN:-}" ]]; then
  echo "::error::The Actions GHCR publish token is required for workload push."
  exit 1
fi
# ksail.prod.yaml's localRegistry carries no credential. Viper applies this
# nested config override before KSail expands the child environment variable,
# so the token never enters argv; the template itself holds no credential.
# shellcheck disable=SC2016
KSAIL_SPEC_CLUSTER_LOCALREGISTRY_REGISTRY='devantler:${GHCR_TOKEN}@ghcr.io/devantler-tech/platform/manifests' \
  exec ksail --config ksail.prod.yaml "$1" "$2" "$3"
