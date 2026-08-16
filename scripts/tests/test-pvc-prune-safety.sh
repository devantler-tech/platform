#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly base_sha="${PVC_PRUNE_BASE_SHA:-HEAD}"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v git >/dev/null 2>&1 || fail 'git is required to inspect the base revision'
command -v kubectl >/dev/null 2>&1 || fail 'kubectl is required to render the production overlays'
command -v yq >/dev/null 2>&1 || fail 'yq v4 is required to inspect rendered PVCs'
command -v jq >/dev/null 2>&1 || fail 'jq is required to inspect live Flux ownership'

git -C "${root_dir}" cat-file -e "${base_sha}^{commit}" 2>/dev/null ||
  fail "base revision ${base_sha} is not available; checkout must fetch it"

temp_dir="$(mktemp -d)"
readonly temp_dir
trap 'rm -rf "${temp_dir}"' EXIT

mkdir -p "${temp_dir}/base"
git -C "${root_dir}" archive "${base_sha}" | tar -x -C "${temp_dir}/base"

render_protected_resources() {
  local tree="$1"
  local output="$2"
  local overlay overlay_slug rendered

  : >"${output}"
  for overlay in bootstrap infrastructure/controllers infrastructure apps; do
    overlay_slug="${overlay//\//-}"
    rendered="${temp_dir}/$(basename "${tree}")-${overlay_slug}.yaml"
    kubectl kustomize "${tree}/k8s/providers/hetzner/${overlay}" >"${rendered}"
    yq eval -r '
      select(
        .kind == "PersistentVolumeClaim" or
        .kind == "HelmRelease" or
        .kind == "Namespace"
      ) |
      [
        .kind,
        (.metadata.namespace // ("-" + .kind)),
        .metadata.name,
        (.metadata.annotations."kustomize.toolkit.fluxcd.io/prune" // ""),
        (.metadata.annotations."kustomize.toolkit.fluxcd.io/force" // "")
      ] |
      @tsv
    ' "${rendered}" >>"${output}"
  done
  sort -u -o "${output}" "${output}"
}

validate_manual_deploy_gate() {
  local workflow="${root_dir}/.github/workflows/cd.yaml"
  local action="${root_dir}/.github/actions/deploy-prod/action.yml"
  local gate_base gate_run live_context live_index live_run publish_index upstream_need

  gate_run="$(yq -r '
    .jobs."validate-pvc-prune-safety".steps[]? |
    select(has("run")) |
    .run
  ' "${workflow}")"
  [[ "${gate_run}" == *'bash scripts/tests/test-pvc-prune-safety.sh'* ]] ||
    fail 'manual production deploy must run the PVC prune-safety contract'

  gate_base="$(yq -r '
    .jobs."validate-pvc-prune-safety".steps[]? |
    select(has("run")) |
    .env.PVC_PRUNE_BASE_SHA // ""
  ' "${workflow}")"
  [[ "${gate_base}" == *'github.sha'* && "${gate_base}" != *'^'* ]] ||
    fail 'manual production deploy must validate the dispatched revision without assuming its parent reached production'

  upstream_need="$(yq -r '.jobs."validate-eks-authorization".needs // ""' "${workflow}")"
  [[ "${upstream_need}" == 'validate-pvc-prune-safety' ]] ||
    fail 'manual production deploy must depend transitively on the PVC prune-safety gate'

  live_run="$(yq -r '
    .runs.steps[]? |
    select(.name == "💾 Validate live persistence retirement safety") |
    .run // ""
  ' "${action}")"
  [[ "${live_run}" == *'bash scripts/tests/test-pvc-prune-safety.sh'* ]] ||
    fail 'shared production deploy must validate removals against live Flux-owned resources'

  live_context="$(yq -r '
    .runs.steps[]? |
    select(.name == "💾 Validate live persistence retirement safety") |
    .env.PVC_PRUNE_LIVE_CONTEXT // ""
  ' "${action}")"
  [[ "${live_context}" == 'admin@prod' ]] ||
    fail 'live persistence-retirement validation must pin the production context'

  live_index="$(yq -r '
    .runs.steps | to_entries[] |
    select(.value.name == "💾 Validate live persistence retirement safety") |
    .key
  ' "${action}")"
  publish_index="$(yq -r '
    .runs.steps | to_entries[] |
    select(.value.name == "📦 Publish evidenced manifests to GHCR") |
    .key
  ' "${action}")"
  [[ "${live_index}" =~ ^[0-9]+$ && "${publish_index}" =~ ^[0-9]+$ &&
    "${live_index}" -lt "${publish_index}" ]] ||
    fail 'live persistence-retirement validation must run before the mutable production artifact is published'
}

validate_manual_deploy_gate

readonly base_resources="${temp_dir}/base-resources.tsv"
readonly current_resources="${temp_dir}/current-resources.tsv"
render_protected_resources "${temp_dir}/base" "${base_resources}"
render_protected_resources "${root_dir}" "${current_resources}"

unprotected_current="$(awk -F '\t' '
  NF >= 3 && $4 != "disabled" {print $1 " " $2 "/" $3}
' "${current_resources}")"
if [[ -n "${unprotected_current}" ]]; then
  fail "every rendered production PVC, HelmRelease, and Namespace must disable Flux pruning; missing on: ${unprotected_current//$'\n'/, }"
fi

force_enabled_current="$(awk -F '\t' '
  $1 == "PersistentVolumeClaim" && $5 != "disabled" {print $2 "/" $3}
' "${current_resources}")"
if [[ -n "${force_enabled_current}" ]]; then
  fail "every rendered production PVC must disable Flux force replacement; missing on: ${force_enabled_current//$'\n'/, }"
fi

cut -f1-3 "${base_resources}" >"${temp_dir}/base-identities.tsv"
cut -f1-3 "${current_resources}" >"${temp_dir}/current-identities.tsv"
comm -23 "${temp_dir}/base-identities.tsv" "${temp_dir}/current-identities.tsv" >"${temp_dir}/removed-identities.tsv"

unsafe_removed=""
while IFS=$'\t' read -r kind namespace name; do
  [[ -n "${name}" ]] || continue
  protection="$(awk -F '\t' -v resource_kind="${kind}" -v ns="${namespace}" -v resource_name="${name}" '
    $1 == resource_kind && $2 == ns && $3 == resource_name {print $4; exit}
  ' "${base_resources}")"
  if [[ "${protection}" != "disabled" ]]; then
    unsafe_removed+="${kind} ${namespace}/${name}"$'\n'
  fi
done <"${temp_dir}/removed-identities.tsv"

if [[ -n "${unsafe_removed}" ]]; then
  unsafe_removed="${unsafe_removed%$'\n'}"
  fail "persistence retirement requires a prior deployed revision with prune protection; base ${base_sha} is unprotected for: ${unsafe_removed//$'\n'/, }"
fi

if [[ -n "${PVC_PRUNE_LIVE_CONTEXT:-}" ]]; then
  readonly live_resources="${temp_dir}/live-resources.tsv"
  kubectl --context "${PVC_PRUNE_LIVE_CONTEXT}" get \
    persistentvolumeclaims,namespaces,helmreleases.helm.toolkit.fluxcd.io \
    --all-namespaces \
    -l 'kustomize.toolkit.fluxcd.io/namespace=flux-system' \
    -o json |
    jq -r '
      .items[] |
      select(
        .metadata.labels["kustomize.toolkit.fluxcd.io/name"] == "apps" or
        .metadata.labels["kustomize.toolkit.fluxcd.io/name"] == "infrastructure" or
        .metadata.labels["kustomize.toolkit.fluxcd.io/name"] == "infrastructure-controllers"
      ) |
      [
        .kind,
        (.metadata.namespace // ("-" + .kind)),
        .metadata.name,
        (.metadata.annotations["kustomize.toolkit.fluxcd.io/prune"] // "")
      ] |
      @tsv
    ' |
    sort -u >"${live_resources}"

  unsafe_live_removals=""
  while IFS=$'\t' read -r kind namespace name protection; do
    [[ -n "${name}" ]] || continue
    if ! awk -F '\t' -v resource_kind="${kind}" -v ns="${namespace}" -v resource_name="${name}" '
      $1 == resource_kind && $2 == ns && $3 == resource_name {found = 1}
      END {exit !found}
    ' "${current_resources}" && [[ "${protection}" != "disabled" ]]; then
      unsafe_live_removals+="${kind} ${namespace}/${name}"$'\n'
    fi
  done <"${live_resources}"

  if [[ -n "${unsafe_live_removals}" ]]; then
    unsafe_live_removals="${unsafe_live_removals%$'\n'}"
    fail "live Flux-owned resources must already be prune-protected before removal: ${unsafe_live_removals//$'\n'/, }"
  fi
fi

printf 'Production persistence-safety contract passed against base %s.\n' "${base_sha}"
