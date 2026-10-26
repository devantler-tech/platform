#!/usr/bin/env bash
# Deletes what Flux deliberately leaves behind once Crossview and Headlamp are
# gone from Git (#2496).
#
# Production disables Flux pruning on every Namespace, HelmRelease and claim, so
# removing the manifests stops reconciling them but keeps them running. AGENTS.md
# ("Persistence retirement is always two-stage") makes the final deletion an
# explicit step. This is that step, for exactly this object set:
#
#   * HelmReleases crossview/crossview and headlamp/headlamp (the chart uninstall
#     removes their workloads);
#   * claim crossview/crossview-postgres-pvc, which the chart keeps on uninstall,
#     and the volume its Delete reclaim policy then removes;
#   * Namespaces crossview and headlamp, with everything still inside them;
#   * the Kubescape scan summaries named after those namespaces, which outlive
#     the namespaces they describe.
#
# It refuses to delete anything until the infrastructure-controllers,
# infrastructure and apps Kustomizations are Ready for their current generations
# and their inventories no longer own any object of either app: that is the
# proof the removal has deployed, so nothing recreates them.
# Each object is checked for Flux ownership by `apps` before deletion. Re-running
# after a partial run is safe: absent objects are skipped. The run ends by
# proving every listed object, the volume and the retired bindings are gone.
#
# PUBLIC LOG: workflow logs are public, so kubectl's own error text, which can
# quote the server it reached, is discarded and each failure is named here.
#
# Exit codes: 0 everything is gone, 1 a refusal or leftover, 2 an input could not
# be read.
set -euo pipefail

readonly confirm_value='retire-crossview-headlamp-2496'
readonly context='admin@prod'
kubectl_bin="${KUBECTL:-kubectl}"
poll_seconds="${RETIREMENT_POLL_SECONDS:-10}"
poll_attempts="${RETIREMENT_POLL_ATTEMPTS:-90}"
readonly kubectl_bin poll_seconds poll_attempts
readonly releases=(crossview/crossview headlamp/headlamp)
readonly claim_namespace=crossview claim_name=crossview-postgres-pvc
readonly namespaces=(crossview headlamp)
readonly summary_kinds=(
  configurationscansummaries.spdx.softwarecomposition.kubescape.io
  vulnerabilitysummaries.spdx.softwarecomposition.kubescape.io
)
readonly retired_bindings=(crossview-cluster-reader crossview-view headlamp-cluster-reader)

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

unknown() {
  printf 'UNKNOWN: %s\n' "$1" >&2
  exit 2
}

k() {
  "${kubectl_bin}" --context "${context}" --request-timeout=30s "$@" 2>/dev/null
}

# Prints the object's JSON, or nothing when it does not exist. A failed read is
# UNKNOWN, never "absent".
read_object() {
  local out
  out="$(k get "$@" --ignore-not-found -o json)" || unknown "cannot read $*"
  printf '%s' "${out}"
}

require_apps_owned() {
  local description="$1" object="$2"
  jq -e '.metadata.labels["kustomize.toolkit.fluxcd.io/name"] == "apps" and
    .metadata.labels["kustomize.toolkit.fluxcd.io/namespace"] == "flux-system"' \
    <<<"${object}" >/dev/null ||
    fail "${description} is not owned by the apps Kustomization; refusing to delete it"
}

[ "${RETIREMENT_CONFIRM:-}" = "${confirm_value}" ] ||
  unknown "set RETIREMENT_CONFIRM=${confirm_value} to run this retirement"
command -v jq >/dev/null || unknown 'jq is required'

# 1. The removal is deployed: every layer that carried part of either app
#    applies its current revision without owning anything of them.
for layer in infrastructure-controllers infrastructure apps; do
  kustomization="$(k -n flux-system get kustomization "${layer}" -o json)" ||
    unknown "cannot read the ${layer} Kustomization"
  jq -e '
    (.status.conditions // [] | any(.type == "Ready" and .status == "True")) and
    .status.observedGeneration == .metadata.generation and
    (.status.inventory.entries | type == "array" and length > 0)
  ' <<<"${kustomization}" >/dev/null ||
    fail "the ${layer} Kustomization is not Ready for its current generation with a non-empty inventory"
  owned="$(jq -r '[.status.inventory.entries[].id | select(test("crossview|headlamp"))] | join(" ")' <<<"${kustomization}")"
  [ -z "${owned}" ] ||
    fail "the deployed ${layer} inventory still owns ${owned}; deploy the removal before retiring the leftovers"
done

# 2. Uninstall the releases. The chart keeps the claim, so no data goes yet.
for release in "${releases[@]}"; do
  namespace="${release%%/*}" name="${release#*/}"
  object="$(read_object helmrelease -n "${namespace}" "${name}")"
  if [ -z "${object}" ]; then
    printf 'HelmRelease %s is already gone\n' "${release}"
    continue
  fi
  require_apps_owned "HelmRelease ${release}" "${object}"
  printf 'Uninstalling HelmRelease %s\n' "${release}"
  k -n "${namespace}" delete helmrelease "${name}" --ignore-not-found --wait --timeout=10m >/dev/null ||
    fail "HelmRelease ${release} did not finish uninstalling"
done

# 3. Delete the retained claim, then wait for its volume to go with it.
volume=''
object="$(read_object pvc -n "${claim_namespace}" "${claim_name}")"
if [ -n "${object}" ]; then
  volume="$(jq -r '.spec.volumeName // ""' <<<"${object}")"
  printf 'Deleting claim %s/%s\n' "${claim_namespace}" "${claim_name}"
  k -n "${claim_namespace}" delete pvc "${claim_name}" --ignore-not-found --wait --timeout=10m >/dev/null ||
    fail "claim ${claim_namespace}/${claim_name} was not deleted"
else
  printf 'Claim %s/%s is already gone\n' "${claim_namespace}" "${claim_name}"
fi

# 4. Delete the namespaces and everything still inside them.
for namespace in "${namespaces[@]}"; do
  object="$(read_object namespace "${namespace}")"
  if [ -z "${object}" ]; then
    printf 'Namespace %s is already gone\n' "${namespace}"
    continue
  fi
  require_apps_owned "Namespace ${namespace}" "${object}"
  printf 'Deleting Namespace %s\n' "${namespace}"
  k delete namespace "${namespace}" --ignore-not-found --wait --timeout=15m >/dev/null ||
    fail "Namespace ${namespace} did not finish terminating"
done

# 5. The scan summaries are cluster-scoped and named after the namespaces.
for kind in "${summary_kinds[@]}"; do
  k delete "${kind}" "${namespaces[@]}" --ignore-not-found >/dev/null ||
    fail "the ${kind} records could not be deleted"
done

# 6. Prove the whole set is gone. Every read is a plain assignment so a failed
#    read stops the run under errexit; inside `[ ]` it would read as "absent".
#    The volume is reclaimed asynchronously, so it is polled.
if [ -n "${volume}" ]; then
  attempt=0
  while :; do
    object="$(read_object pv "${volume}")"
    [ -n "${object}" ] || break
    attempt=$((attempt + 1))
    [ "${attempt}" -lt "${poll_attempts}" ] || fail "volume ${volume} was not reclaimed"
    sleep "${poll_seconds}"
  done
fi
leftovers=()
check_absent() {
  local description="$1" object
  shift
  object="$(read_object "$@")"
  [ -z "${object}" ] || leftovers+=("${description}")
}
for release in "${releases[@]}"; do
  check_absent "HelmRelease ${release}" helmrelease -n "${release%%/*}" "${release#*/}"
done
check_absent "claim ${claim_namespace}/${claim_name}" pvc -n "${claim_namespace}" "${claim_name}"
for namespace in "${namespaces[@]}"; do
  check_absent "Namespace ${namespace}" namespace "${namespace}"
  for kind in "${summary_kinds[@]}"; do
    check_absent "${kind} ${namespace}" "${kind}" "${namespace}"
  done
done
for binding in "${retired_bindings[@]}"; do
  check_absent "ClusterRoleBinding ${binding}" clusterrolebinding "${binding}"
done
[ "${#leftovers[@]}" -eq 0 ] || fail "still present: $(printf '%s; ' "${leftovers[@]}")"

printf 'PASS: Crossview and Headlamp leave nothing behind in production\n'
