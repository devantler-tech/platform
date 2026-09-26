#!/usr/bin/env bash
#
# Reassert the root OCIRepository's cosign verification from the value this
# commit declares, before the deploy reconciles, so a bad live value stays
# recoverable (platform#3014).
#
# WHY. The trust configuration for the root source is delivered THROUGH that
# source: flux-instance.yaml → Kustomization infrastructure-controllers →
# FluxInstance/flux → rendered by flux-operator onto OCIRepository/flux-system,
# which must verify the artifact carrying that very config. Once the live
# matcher is wrong, main can hold the corrected value and never deliver it:
# the source refuses every new artifact, so kustomize-controller keeps applying
# the old FluxInstance, and flux-operator keeps rendering the old matcher. The
# 2026-08-07 delivery freeze lasted ~35h for exactly this reason, and the heal
# shares the deploy path, so it failed alongside.
#
# HOW, and why this writes the FluxInstance rather than the OCIRepository.
# flux-operator is the Apply field manager of the source and renders spec.verify
# from the FluxInstance's kustomize patch. A write to the source alone would be
# reverted by the operator's next reconcile, because the live FluxInstance still
# carries the bad patch. So:
#   1. When the live FluxInstance's OCIRepository patch declares a different
#      verify than this commit, replace that one patch entry, under
#      kustomize-controller (the manager that applies the FluxInstance). It is a
#      JSON patch, not an apply: an apply under that manager with a partial
#      object would drop every field kustomize-controller owns and this object
#      omits, including the labels it stamps.
#   2. When the source's rendered verify differs from this commit, ask
#      flux-operator to reconcile and wait until it has re-rendered the source,
#      then ask the source to fetch. The source itself is never written, so it
#      keeps a single writer.
# Run it after the new artifact is published: the source must then be able to
# verify and fetch the artifact carrying the corrected FluxInstance, which is
# what makes kustomize-controller's next apply agree with this repair.
#
# A healthy cluster takes no write at all.
#
# Exit: 0  verify already current on both objects, or repaired and re-rendered
#       1  repaired the FluxInstance, but the source was not re-rendered in time
#       2  could not check or repair — unreadable or ambiguous declared or live
#          state, or a failed write

set -euo pipefail

readonly kubectl_bin="${KUBECTL:-kubectl}"
readonly namespace='flux-system'
readonly instance='flux'
readonly source_name='flux-system'
root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly root_dir
readonly declared_file="${FLUX_INSTANCE_FILE:-${root_dir}/k8s/providers/hetzner/infrastructure/controllers/flux-instance/flux-instance.yaml}"
readonly render_timeout="${FLUX_VERIFY_RENDER_TIMEOUT_SECONDS:-180}"
readonly poll_interval="${FLUX_VERIFY_POLL_INTERVAL_SECONDS:-5}"

die() {
  printf 'reassert-flux-root-verify: %s\n' "$1" >&2
  exit 2
}

summary() {
  printf '%s\n' "$1"
  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    printf -- '- Root source verify: %s\n' "$1" >>"${GITHUB_STEP_SUMMARY}"
  fi
}

kubectl_prod() {
  "${kubectl_bin}" --context admin@prod -n "${namespace}" "$@"
}

command -v jq >/dev/null 2>&1 || die 'jq is required but not on PATH'
command -v yq >/dev/null 2>&1 || die 'yq is required but not on PATH'
[[ -r "${declared_file}" ]] || die "cannot read ${declared_file}"

# Prints the canonical JSON of the single /spec/verify value a JSON6902 patch
# (YAML text on stdin) sets. Fails unless there is exactly one such operation.
verify_of_patch() {
  local ops
  ops="$(yq -o=json '[.[] | select(.path == "/spec/verify")]')" || return 1
  jq -e -S -c 'if length == 1 and (.[0].op == "add" or .[0].op == "replace") and (.[0].value | type) == "object"
    then .[0].value else error("expected exactly one add/replace of an object at /spec/verify") end' <<<"${ops}"
}

readonly source_patch_filter='.target.kind == "OCIRepository" and .target.name == "flux-system"'

declared_patches="$(yq -o=json '[.spec.kustomize.patches[] | select('"${source_patch_filter}"') | .patch]' "${declared_file}")" ||
  die "could not parse ${declared_file}"
[[ "$(jq 'length' <<<"${declared_patches}")" -eq 1 ]] ||
  die "${declared_file} must declare exactly one OCIRepository/flux-system patch"
declared_patch="$(jq -r '.[0]' <<<"${declared_patches}")"
declared_verify="$(verify_of_patch <<<"${declared_patch}")" ||
  die "the declared OCIRepository/flux-system patch does not set exactly one /spec/verify object"

instance_json="$(kubectl_prod get fluxinstance "${instance}" -o json)" ||
  die "could not read FluxInstance ${namespace}/${instance}"
indices="$(jq -c '[.spec.kustomize.patches // [] | to_entries[] | select(.value | '"${source_patch_filter}"') | .key]' <<<"${instance_json}")"
[[ "$(jq 'length' <<<"${indices}")" -eq 1 ]] ||
  die "live FluxInstance ${namespace}/${instance} must carry exactly one OCIRepository/flux-system patch"
index="$(jq '.[0]' <<<"${indices}")"
live_patch="$(jq -r --argjson i "${index}" '.spec.kustomize.patches[$i].patch // ""' <<<"${instance_json}")"
# An unparseable live patch is the bad state this exists to repair, not a reason to stop.
live_instance_verify="$(verify_of_patch <<<"${live_patch}" 2>/dev/null || printf 'unparseable')"

source_verify() {
  local source_json
  source_json="$(kubectl_prod get ocirepository "${source_name}" -o json)" || return 1
  jq -S -c '.spec.verify // null' <<<"${source_json}"
}

live_source_verify="$(source_verify)" || die "could not read OCIRepository ${namespace}/${source_name}"

if [[ "${live_instance_verify}" == "${declared_verify}" && "${live_source_verify}" == "${declared_verify}" ]]; then
  summary 'CURRENT — the FluxInstance and the root source already carry the declared verify; nothing written.'
  exit 0
fi

if [[ "${live_instance_verify}" != "${declared_verify}" ]]; then
  printf 'FluxInstance %s/%s declares a different root-source verify than this commit; replacing that patch entry.\n' \
    "${namespace}" "${instance}"
  json_patch="$(jq -n -c --argjson i "${index}" --arg patch "${declared_patch}" '[
    {op: "test", path: "/spec/kustomize/patches/\($i)/target/kind", value: "OCIRepository"},
    {op: "test", path: "/spec/kustomize/patches/\($i)/target/name", value: "flux-system"},
    {op: "replace", path: "/spec/kustomize/patches/\($i)/patch", value: $patch}
  ]')"
  kubectl_prod patch fluxinstance "${instance}" --type=json \
    --field-manager=kustomize-controller -p "${json_patch}" >/dev/null ||
    die "could not patch FluxInstance ${namespace}/${instance}"
fi

requested_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
kubectl_prod annotate fluxinstance "${instance}" --overwrite \
  "reconcile.fluxcd.io/requestedAt=${requested_at}" >/dev/null ||
  die "could not request a reconcile of FluxInstance ${namespace}/${instance}"

deadline=$((SECONDS + render_timeout))
while :; do
  live_source_verify="$(source_verify)" || die "could not re-read OCIRepository ${namespace}/${source_name}"
  [[ "${live_source_verify}" == "${declared_verify}" ]] && break
  if ((SECONDS >= deadline)); then
    printf 'reassert-flux-root-verify: flux-operator did not re-render OCIRepository %s/%s within %ss\n' \
      "${namespace}" "${source_name}" "${render_timeout}" >&2
    summary 'REPAIR INCOMPLETE — the FluxInstance carries the declared verify, but the root source was not re-rendered.'
    exit 1
  fi
  sleep "${poll_interval}"
done

kubectl_prod annotate ocirepository "${source_name}" --overwrite \
  "reconcile.fluxcd.io/requestedAt=${requested_at}" >/dev/null ||
  die "could not request a fetch of OCIRepository ${namespace}/${source_name}"

summary 'REPAIRED — the root source was re-rendered with the declared verify and asked to fetch.'
