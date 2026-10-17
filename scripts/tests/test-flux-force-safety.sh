#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
render_dir="$(mktemp -d)"
readonly render_dir
trap 'rm -rf "${render_dir}"' EXIT

cd "${root_dir}"
# Source checks cover both KRO tenant branches; rendered checks cover patches.
kubectl kustomize k8s/clusters/prod >"${render_dir}/cluster.yaml"
for overlay in bootstrap infrastructure/controllers infrastructure apps; do
  kubectl kustomize "k8s/providers/hetzner/${overlay}" >"${render_dir}/${overlay//\//-}.yaml"
done
go run ./scripts/validate-flux-force-safety k8s "${render_dir}"
