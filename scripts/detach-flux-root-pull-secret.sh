#!/usr/bin/env bash
#
# Make the root Flux source pull the public manifests package anonymously by
# removing spec.sync.pullSecret from the FluxInstance (platform#6272).
#
# WHY. The root package ghcr.io/devantler-tech/platform/manifests is public, so
# the root source needs no credential. Keeping one is a liability, not a
# fallback: GHCR answers a revoked basic-auth credential with 403 on the token
# exchange instead of falling back to anonymous, so a stale pull Secret breaks
# the fetch of a PUBLIC package too.
#
# HOW, and why this writes the FluxInstance rather than the OCIRepository.
# KSail wrote spec.sync (field manager `ksail`, at bootstrap) and only re-writes
# it when another FluxInstance field drifts. The repository's FluxInstance does
# not declare spec.sync, so a deploy can never remove the field through Flux.
# flux-operator renders the root OCIRepository's spec.secretRef from it, so a
# write to the source alone would be reverted. So:
#   1. Prove the declared root artifact is anonymously readable BEFORE any
#      write. A package that is still private leaves the cluster untouched.
#   2. Remove spec.sync.pullSecret under the `ksail` field manager, guarded by
#      a JSON-patch test of the exact live value.
#   3. Ask flux-operator to reconcile, wait until it has applied the patched
#      generation and the source carries no secretRef, then ask the source to
#      fetch again. spec.verify is never touched.
#      This deliberately does NOT wait for the source to be Ready. A bad live
#      cosign matcher keeps the source not Ready whatever its credential, and
#      reassert-flux-root-verify.sh repairs that only after publication, which
#      runs after this step. Waiting here would time the deploy out before the
#      repair could run. Step 1 already proved the anonymous pull; the deploy's
#      later steps prove the source fetches and verifies the published artifact.
# KSail itself stops setting pullSecret once ksail.prod.yaml carries no
# localRegistry credential, so a later KSail re-assert agrees with this.
#
# A cluster that is already detached takes no write at all.
#
# Exit: 0  already detached, or detached and re-rendered
#       1  detached the FluxInstance, but the source did not converge in time
#       2  could not check or detach: unreadable or unexpected live state, the
#          package is not anonymously readable, or a failed write

set -euo pipefail

readonly kubectl_bin="${KUBECTL:-kubectl}"
readonly curl_bin="${CURL:-curl}"
readonly namespace='flux-system'
readonly instance='flux'
readonly source_name='flux-system'
readonly expected_url='oci://ghcr.io/devantler-tech/platform/manifests'
readonly repository='devantler-tech/platform/manifests'
readonly render_timeout="${FLUX_PULL_SECRET_RENDER_TIMEOUT_SECONDS:-300}"
readonly poll_interval="${FLUX_PULL_SECRET_POLL_INTERVAL_SECONDS:-5}"

die() {
  printf 'detach-flux-root-pull-secret: %s\n' "$1" >&2
  exit 2
}

summary() {
  printf '%s\n' "$1"
  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    printf -- '- Root source pull secret: %s\n' "$1" >>"${GITHUB_STEP_SUMMARY}"
  fi
}

kubectl_prod() {
  "${kubectl_bin}" --context admin@prod -n "${namespace}" --request-timeout=10s "$@"
}

command -v jq >/dev/null 2>&1 || die 'jq is required but not on PATH'

# anonymous_pull_ok <ref> succeeds only when GHCR issues an anonymous pull token
# for the root repository and serves the manifest at <ref> with it. No
# credential is read or sent; an anonymous token grants only public reads.
anonymous_pull_ok() {
  local ref="$1" token status
  token="$("${curl_bin}" --silent --show-error --fail --max-time 20 \
    "https://ghcr.io/token?scope=repository:${repository}:pull&service=ghcr.io" |
    jq -er '.token | select(type == "string" and length > 0)')" || return 1
  status="$("${curl_bin}" --silent --output /dev/null --write-out '%{http_code}' \
    --max-time 20 --head \
    --header "Authorization: Bearer ${token}" \
    --header 'Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json' \
    "https://ghcr.io/v2/${repository}/manifests/${ref}")" || return 1
  [[ "${status}" == '200' ]]
}

instance_json="$(kubectl_prod get fluxinstance "${instance}" -o json)" ||
  die "could not read FluxInstance ${namespace}/${instance}"
source_json="$(kubectl_prod get ocirepository "${source_name}" -o json)" ||
  die "could not read OCIRepository ${namespace}/${source_name}"

live_url="$(jq -r '.spec.sync.url // ""' <<<"${instance_json}")"
live_ref="$(jq -r '.spec.sync.ref // ""' <<<"${instance_json}")"
pull_secret="$(jq -r '.spec.sync.pullSecret // ""' <<<"${instance_json}")"
source_secret="$(jq -r '.spec.secretRef.name // ""' <<<"${source_json}")"

[[ "${live_url}" == "${expected_url}" ]] ||
  die "FluxInstance ${namespace}/${instance} syncs from an unexpected URL; refusing to change its pull auth"
[[ "${live_ref}" =~ ^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$ ]] ||
  die "FluxInstance ${namespace}/${instance} has no valid sync ref"

if [[ -z "${pull_secret}" && -z "${source_secret}" ]]; then
  summary 'CURRENT — the root source already pulls the public package anonymously; nothing written.'
  exit 0
fi

anonymous_pull_ok "${live_ref}" ||
  die "ghcr.io/${repository}:${live_ref} is not anonymously readable; leaving the root pull secret in place"

patched_generation=''
if [[ -n "${pull_secret}" ]]; then
  printf 'FluxInstance %s/%s still names a root pull secret; removing it.\n' "${namespace}" "${instance}"
  json_patch="$(jq -n -c --arg secret "${pull_secret}" '[
    {op: "test", path: "/spec/sync/pullSecret", value: $secret},
    {op: "remove", path: "/spec/sync/pullSecret"}
  ]')"
  kubectl_prod patch fluxinstance "${instance}" --type=json \
    --field-manager=ksail -p "${json_patch}" >/dev/null ||
    die "could not patch FluxInstance ${namespace}/${instance}"
  patched_generation="$(kubectl_prod get fluxinstance "${instance}" -o json | jq -er '.metadata.generation')" ||
    die "could not read the generation of the patched FluxInstance ${namespace}/${instance}"
fi

# converged -> true once flux-operator has reconciled the patched generation (or
# nothing was patched) and the rendered source names no secret. Source readiness
# is not required here; see step 3 in the header.
converged() {
  local fi_json src_json
  fi_json="$(kubectl_prod get fluxinstance "${instance}" -o json)" || return 1
  src_json="$(kubectl_prod get ocirepository "${source_name}" -o json)" || return 1
  jq -n -r --argjson instance "${fi_json}" --argjson src "${src_json}" --arg g "${patched_generation}" '
    (($g == "") or any($instance.status.conditions[]?; .type == "Ready" and .status == "True"
      and (.observedGeneration // -1) >= ($g | tonumber)))
    and (($instance.spec.sync.pullSecret // "") == "")
    and (($src.spec.secretRef.name // "") == "")'
}

requested_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
kubectl_prod annotate fluxinstance "${instance}" --overwrite \
  "reconcile.fluxcd.io/requestedAt=${requested_at}" >/dev/null ||
  die "could not request a reconcile of FluxInstance ${namespace}/${instance}"

deadline=$((SECONDS + render_timeout))
while :; do
  state="$(converged)" || die "could not re-read FluxInstance ${namespace}/${instance} or OCIRepository ${namespace}/${source_name}"
  [[ "${state}" == true ]] && break
  if ((SECONDS >= deadline)); then
    printf 'detach-flux-root-pull-secret: OCIRepository %s/%s was not re-rendered without its pull secret within %ss\n' \
      "${namespace}" "${source_name}" "${render_timeout}" >&2
    summary 'DETACH INCOMPLETE — the FluxInstance names no pull secret, but the root source has not been re-rendered without it yet.'
    exit 1
  fi
  sleep "${poll_interval}"
done

kubectl_prod annotate ocirepository "${source_name}" --overwrite \
  "reconcile.fluxcd.io/requestedAt=${requested_at}" >/dev/null ||
  die "could not request a fetch of OCIRepository ${namespace}/${source_name}"

summary 'DETACHED — the root source names no pull secret and was asked to fetch the public package anonymously.'
