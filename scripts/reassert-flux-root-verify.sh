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
  "${kubectl_bin}" --context admin@prod -n "${namespace}" --request-timeout=10s "$@"
}

command -v jq >/dev/null 2>&1 || die 'jq is required but not on PATH'
command -v yq >/dev/null 2>&1 || die 'yq is required but not on PATH'
[[ -r "${declared_file}" ]] || die "cannot read ${declared_file}"

# Prints the canonical JSON of the single /spec/verify value a JSON6902 patch
# (YAML text on stdin) sets. Nested operations can change that value after the
# root operation, so they make the live patch ambiguous and trigger replacement.
verify_of_patch() {
  local ops
  ops="$(yq -o=json '.')" || return 1
  jq -e -S -c '
    if type != "array" then error("expected a JSON6902 operation list") else . end
    | [.[] | select((.path // "") as $p
        | $p == "/spec/verify"
          or ($p | startswith("/spec/verify/"))
          or ("/spec/verify" | startswith($p + "/")))]
    | if length == 1 and .[0].path == "/spec/verify" and (.[0].op == "add" or .[0].op == "replace") and (.[0].value | type) == "object"
    then .[0].value else error("expected exactly one add/replace of an object at /spec/verify") end' <<<"${ops}"
}

readonly source_patch_filter='.target.kind == "OCIRepository" and .target.name == "flux-system"'

declared_patches="$(yq -o=json '[.spec.kustomize.patches[] | select('"${source_patch_filter}"')]' "${declared_file}")" ||
  die "could not parse ${declared_file}"
[[ "$(jq 'length' <<<"${declared_patches}")" -eq 1 ]] ||
  die "${declared_file} must declare exactly one OCIRepository/flux-system patch"
declared_patch="$(jq -r '.[0].patch' <<<"${declared_patches}")"
declared_target="$(jq -S -c '.[0].target' <<<"${declared_patches}")"
declared_verify="$(verify_of_patch <<<"${declared_patch}")" ||
  die "the declared OCIRepository/flux-system patch does not set exactly one /spec/verify object"

instance_json="$(kubectl_prod get fluxinstance "${instance}" -o json)" ||
  die "could not read FluxInstance ${namespace}/${instance}"
indices="$(jq -c '[.spec.kustomize.patches // [] | to_entries[] | select(.value | '"${source_patch_filter}"') | .key]' <<<"${instance_json}")"
[[ "$(jq 'length' <<<"${indices}")" -eq 1 ]] ||
  die "live FluxInstance ${namespace}/${instance} must carry exactly one OCIRepository/flux-system patch"
index="$(jq '.[0]' <<<"${indices}")"
live_patch="$(jq -r --argjson i "${index}" '.spec.kustomize.patches[$i].patch // ""' <<<"${instance_json}")"
live_target="$(jq -S -c --argjson i "${index}" '.spec.kustomize.patches[$i].target' <<<"${instance_json}")"
# An unparseable live patch is the bad state this exists to repair, not a reason to stop.
live_instance_verify="$(verify_of_patch <<<"${live_patch}" 2>/dev/null || printf 'unparseable')"

source_verify() {
  local source_json
  source_json="$(kubectl_prod get ocirepository "${source_name}" -o json)" || return 1
  jq -S -c '.spec.verify // null' <<<"${source_json}"
}

live_source_verify="$(source_verify)" || die "could not read OCIRepository ${namespace}/${source_name}"

if [[ "${live_patch}" == "${declared_patch}" && "${live_target}" == "${declared_target}" && "${live_source_verify}" == "${declared_verify}" ]]; then
  summary 'CURRENT — the FluxInstance and the root source already carry the declared verify; nothing written.'
  exit 0
fi

patched_generation=''
if [[ "${live_patch}" != "${declared_patch}" || "${live_instance_verify}" != "${declared_verify}" || "${live_target}" != "${declared_target}" ]]; then
  printf 'FluxInstance %s/%s has a different root-source patch than this commit; replacing that patch entry.\n' \
    "${namespace}" "${instance}"
  json_patch="$(jq -n -c --argjson i "${index}" --argjson live "${live_target}" --argjson declared "${declared_target}" --arg patch "${declared_patch}" '[
    {op: "test", path: "/spec/kustomize/patches/\($i)/target", value: $live},
    {op: "replace", path: "/spec/kustomize/patches/\($i)/target", value: $declared},
    {op: "replace", path: "/spec/kustomize/patches/\($i)/patch", value: $patch}
  ]')"
  kubectl_prod patch fluxinstance "${instance}" --type=json \
    --field-manager=kustomize-controller -p "${json_patch}" >/dev/null ||
    die "could not patch FluxInstance ${namespace}/${instance}"
  # The source can already carry the declared verify while the operator still applies the
  # old patch, so a repaired FluxInstance counts only once it is Ready at this generation.
  patched_generation="$(kubectl_prod get fluxinstance "${instance}" -o json | jq -er '.metadata.generation')" ||
    die "could not read the generation of the patched FluxInstance ${namespace}/${instance}"
fi

# instance_rendered -> true once flux-operator has reconciled the patched generation (or
# nothing was patched), false while it has not.
instance_rendered() {
  [[ -z "${patched_generation}" ]] && { printf 'true'; return 0; }
  kubectl_prod get fluxinstance "${instance}" -o json |
    jq -r --argjson g "${patched_generation}" \
      'any(.status.conditions[]?; .type == "Ready" and .status == "True" and (.observedGeneration // -1) >= $g)'
}

requested_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
kubectl_prod annotate fluxinstance "${instance}" --overwrite \
  "reconcile.fluxcd.io/requestedAt=${requested_at}" >/dev/null ||
  die "could not request a reconcile of FluxInstance ${namespace}/${instance}"

deadline=$((SECONDS + render_timeout))
while :; do
  rendered="$(instance_rendered)" || die "could not re-read FluxInstance ${namespace}/${instance}"
  live_source_verify="$(source_verify)" || die "could not re-read OCIRepository ${namespace}/${source_name}"
  [[ "${rendered}" == true && "${live_source_verify}" == "${declared_verify}" ]] && break
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
