#!/usr/bin/env bash
# Deletes what Flux deliberately leaves behind once Crossview, Headlamp (#2496)
# and the retained Backstage database (#4266) are gone from Git.
#
# Production disables Flux pruning on every Namespace, HelmRelease, claim and
# CloudNativePG Cluster, so removing the manifests stops reconciling them but
# keeps them running. AGENTS.md ("Persistence retirement is always two-stage")
# makes the final deletion an explicit step. This is that step, for exactly this
# set, deleted in this order:
#
#   1. HelmReleases crossview/crossview and headlamp/headlamp: the chart
#      uninstall removes their workloads;
#   2. CloudNativePG Cluster backstage/backstage-db: its instances stop;
#   3. Namespaces crossview, headlamp and backstage, with every claim and object
#      still inside them;
#   4. volumes that were bound to claims in those namespaces. A Delete policy
#      reclaims them on its own; a Retain volume (the Backstage database) is
#      deleted once Released;
#   5. Longhorn volumes last used by claims in those namespaces, once detached:
#      a Retain volume's backing data survives its volume object otherwise;
#   6. the Kubescape scan summaries named after the namespaces, which outlive
#      the namespaces they describe.
#
# The Backstage data has no backup: deleting it was the maintainer's decision
# of 26 October 2026, which replaced keeping a recovery copy (#5122).
#
# Nothing is deleted until the infrastructure-controllers, infrastructure and
# apps Kustomizations are Ready for their current generations and their
# inventories own no object of the three apps: that is the proof the removal
# has deployed, so nothing recreates them. Each Namespace, HelmRelease and
# Cluster must be owned by the apps Kustomization, and every volume and Longhorn
# volume must name one of the three namespaces as its last claim. Re-running
# after a partial run is safe: absent objects are skipped, and storage is found
# again by its recorded claim namespace. The run ends by proving the whole set,
# and the retired bindings, are gone.
#
# PUBLIC LOG: workflow logs are public, so kubectl's own error text, which can
# quote the server it reached, is discarded and each failure is named here.
#
# Exit codes: 0 everything is gone, 1 a refusal or leftover, 2 an input could not
# be read.
set -euo pipefail

readonly confirm_value='retire-crossview-headlamp-backstage'
readonly context='admin@prod'
kubectl_bin="${KUBECTL:-kubectl}"
poll_seconds="${RETIREMENT_POLL_SECONDS:-10}"
poll_attempts="${RETIREMENT_POLL_ATTEMPTS:-90}"
readonly kubectl_bin poll_seconds poll_attempts
readonly releases=(crossview/crossview headlamp/headlamp)
readonly clusters=(backstage/backstage-db)
readonly namespaces=(crossview headlamp backstage)
readonly retired_pattern='crossview|headlamp|backstage'
# Crossview's Vault seed chain stays declared until this retirement has run, so
# a healed failed deploy never regenerates credentials that Crossview's retained
# database was initialised with. A later revision removes it (#5391).
readonly retained_seed_pattern='^openbao_(generated-|push-)?crossview-(db-password|session-secret|admin-password)_'
readonly longhorn_namespace=longhorn-system
readonly summary_kinds=(
  configurationscansummaries.spdx.softwarecomposition.kubescape.io
  vulnerabilitysummaries.spdx.softwarecomposition.kubescape.io
)
readonly retired_bindings=(crossview-cluster-reader crossview-view headlamp-cluster-reader)
namespaces_json="$(printf '%s\n' "${namespaces[@]}" | jq -R . | jq -sc .)"
readonly namespaces_json

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
# UNKNOWN, never "absent". Call it only in a plain assignment: inside `[ ]` or
# a condition its failure would be read as "absent".
read_object() {
  local out
  out="$(k get "$@" --ignore-not-found -o json)" || unknown "cannot read $*"
  printf '%s' "${out}"
}

# Prints the names of the volumes whose claim was in a retired namespace.
retired_volumes() {
  local out
  out="$(k get pv -o json)" || unknown 'cannot list volumes'
  jq -r --argjson ns "${namespaces_json}" \
    '.items[] | select(.spec.claimRef.namespace as $n | $ns | index($n)) | .metadata.name' <<<"${out}"
}

# Prints "<name> <state>" for the Longhorn volumes last claimed in a retired namespace.
retired_longhorn_volumes() {
  local out
  out="$(k -n "${longhorn_namespace}" get volumes.longhorn.io -o json)" ||
    unknown 'cannot list Longhorn volumes'
  jq -r --argjson ns "${namespaces_json}" '
    .items[] | select(.status.kubernetesStatus.namespace as $n | $ns | index($n)) |
    "\(.metadata.name) \(.status.state // "unknown")"' <<<"${out}"
}

require_apps_owned() {
  local description="$1" object="$2"
  jq -e '.metadata.labels["kustomize.toolkit.fluxcd.io/name"] == "apps" and
    .metadata.labels["kustomize.toolkit.fluxcd.io/namespace"] == "flux-system"' \
    <<<"${object}" >/dev/null ||
    fail "${description} is not owned by the apps Kustomization; refusing to delete it"
}

# Deletes an apps-owned object and waits for its finalizers.
delete_owned() { # <description> <timeout> <kubectl get/delete args...>
  local description="$1" timeout="$2" object
  shift 2
  object="$(read_object "$@")"
  if [ -z "${object}" ]; then
    printf '%s is already gone\n' "${description}"
    return
  fi
  require_apps_owned "${description}" "${object}"
  printf 'Deleting %s\n' "${description}"
  k delete "$@" --ignore-not-found --wait --timeout="${timeout}" >/dev/null ||
    fail "${description} was not deleted"
}

[ "${RETIREMENT_CONFIRM:-}" = "${confirm_value}" ] ||
  unknown "set RETIREMENT_CONFIRM=${confirm_value} to run this retirement"
command -v jq >/dev/null || unknown 'jq is required'

# 0. The removal is deployed: every layer that carried part of an app applies
#    its current revision without owning anything of them. Flux records a
#    layer's inventory right after a successful apply, before health checks,
#    so a layer whose only failure is HealthCheckFailed still proves what its
#    current revision applied; an unrelated unhealthy workload must not block
#    this retirement. Any other not-Ready reason, such as a failed build or
#    apply, leaves the inventory unproven and is refused.
for layer in infrastructure-controllers infrastructure apps; do
  kustomization="$(k -n flux-system get kustomization "${layer}" -o json)" ||
    unknown "cannot read the ${layer} Kustomization"
  jq -e '
    (.status.conditions // [] | any(.type == "Ready" and
      (.status == "True" or (.status == "False" and .reason == "HealthCheckFailed")))) and
    .status.observedGeneration == .metadata.generation and
    (.status.inventory.entries | type == "array" and length > 0)
  ' <<<"${kustomization}" >/dev/null ||
    fail "the ${layer} Kustomization has not applied its current generation with a non-empty inventory"
  if jq -e '.status.conditions // [] | any(.type == "Ready" and .status == "False")' <<<"${kustomization}" >/dev/null; then
    printf 'Note: the %s Kustomization applied its revision but an unrelated health check is failing\n' "${layer}"
  fi
  owned="$(jq -r --arg p "${retired_pattern}" --arg seed "${retained_seed_pattern}" \
    '[.status.inventory.entries[].id | select(test($p) and (test($seed) | not))] | join(" ")' <<<"${kustomization}")"
  [ -z "${owned}" ] ||
    fail "the deployed ${layer} inventory still owns ${owned}; deploy the removal before retiring the leftovers"
done

# 1–3. Owners first, so no controller recreates what the namespaces hold.
for release in "${releases[@]}"; do
  delete_owned "HelmRelease ${release}" 10m helmrelease -n "${release%%/*}" "${release#*/}"
done
for cluster in "${clusters[@]}"; do
  delete_owned "Cluster ${cluster}" 10m clusters.postgresql.cnpg.io -n "${cluster%%/*}" "${cluster#*/}"
done
for namespace in "${namespaces[@]}"; do
  delete_owned "Namespace ${namespace}" 15m namespace "${namespace}"
done

# 4. Volumes: wait until each is reclaimed or Released, then delete the Released.
#    Every pass counts, so a volume that outlives its deletion cannot spin.
attempt=0
while :; do
  volumes="$(retired_volumes)"
  [ -n "${volumes}" ] || break
  attempt=$((attempt + 1))
  [ "${attempt}" -le "${poll_attempts}" ] ||
    fail "volumes were neither reclaimed nor deleted: ${volumes//$'\n'/ }"
  for volume in ${volumes}; do
    object="$(read_object pv "${volume}")"
    [ -n "${object}" ] || continue
    phase="$(jq -r '.status.phase // ""' <<<"${object}")"
    [ "${phase}" = Released ] || continue
    printf 'Deleting released volume %s\n' "${volume}"
    k delete pv "${volume}" --ignore-not-found --wait --timeout=5m >/dev/null ||
      fail "volume ${volume} was not deleted"
  done
  sleep "${poll_seconds}"
done

# 5. Longhorn volumes: delete each once detached. A Delete-policy volume may
#    already be on its way out, which deleting again does not disturb.
attempt=0
while :; do
  backing="$(retired_longhorn_volumes)"
  [ -n "${backing}" ] || break
  attempt=$((attempt + 1))
  [ "${attempt}" -le "${poll_attempts}" ] ||
    fail "Longhorn volumes did not detach or were not deleted: ${backing//$'\n'/; }"
  while read -r name state; do
    [ "${state}" = detached ] || continue
    printf 'Deleting Longhorn volume %s\n' "${name}"
    k -n "${longhorn_namespace}" delete volumes.longhorn.io "${name}" --ignore-not-found --wait --timeout=5m >/dev/null ||
      fail "Longhorn volume ${name} was not deleted"
  done <<<"${backing}"
  sleep "${poll_seconds}"
done

# 6. The scan summaries are cluster-scoped and named after the namespaces.
for kind in "${summary_kinds[@]}"; do
  k delete "${kind}" "${namespaces[@]}" --ignore-not-found >/dev/null ||
    fail "the ${kind} records could not be deleted"
done

# Prove the whole set is gone.
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
for cluster in "${clusters[@]}"; do
  check_absent "Cluster ${cluster}" clusters.postgresql.cnpg.io -n "${cluster%%/*}" "${cluster#*/}"
done
for namespace in "${namespaces[@]}"; do
  check_absent "Namespace ${namespace}" namespace "${namespace}"
  for kind in "${summary_kinds[@]}"; do
    check_absent "${kind} ${namespace}" "${kind}" "${namespace}"
  done
done
for binding in "${retired_bindings[@]}"; do
  check_absent "ClusterRoleBinding ${binding}" clusterrolebinding "${binding}"
done
volumes="$(retired_volumes)"
[ -z "${volumes}" ] || leftovers+=("volumes ${volumes//$'\n'/ }")
backing="$(retired_longhorn_volumes)"
[ -z "${backing}" ] || leftovers+=("Longhorn volumes ${backing//$'\n'/; }")
[ "${#leftovers[@]}" -eq 0 ] || fail "still present: $(printf '%s; ' "${leftovers[@]}")"

printf 'PASS: Crossview, Headlamp and Backstage leave nothing behind in production\n'
