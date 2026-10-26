#!/usr/bin/env bash
# Offline regression for scripts/retire-crossview-headlamp.sh against a fake
# kubectl whose objects live as files. It proves the retirement refuses before
# the removal is deployed, refuses objects Flux `apps` does not own, deletes in
# the safe order, is idempotent, never reads a failed read as "absent", and
# reports any leftover. No cluster or credential is used.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly script="${root_dir}/scripts/retire-crossview-headlamp.sh"
work_dir="$(mktemp -d)"
readonly work_dir
trap 'rm -rf "${work_dir}"' EXIT

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v jq >/dev/null || fail 'jq is required'

# The fake stores one JSON file per object under $STATE/<kind>/<namespace>/<name>
# (namespace "_" for cluster-scoped kinds) and logs every call.
cat >"${work_dir}/kubectl" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
namespace=_ verb='' kind='' names=()
while [ "$#" -gt 0 ]; do
  case "$1" in
    --request-timeout=*|--ignore-not-found|--wait|--timeout=*) ;;
    -o|--context) shift ;;
    -n) namespace="$2"; shift ;;
    get|delete) verb="$1" ;;
    *) if [ -z "${kind}" ] && [ -n "${verb}" ]; then kind="$1"; else names+=("$1"); fi ;;
  esac
  shift
done
printf '%s %s %s\n' "${verb}" "${kind}" "${names[*]}" >>"${STATE}/calls"
if [ -n "${FAIL_READ:-}" ] && [ "${verb}" = get ] && [ "${kind}" = "${FAIL_READ}" ]; then
  echo 'error: the server is currently unable to handle the request' >&2
  exit 1
fi
for name in "${names[@]}"; do
  file="${STATE}/${kind}/${namespace}/${name}"
  case "${verb}" in
    get)
      if [ "${kind}" = kustomization ]; then cat "${STATE}/${name}.json" 2>/dev/null || cat "${STATE}/apps.json"; elif [ -f "${file}" ]; then cat "${file}"; fi ;;
    delete)
      [ -n "${KEEP:-}" ] && [ "${kind}/${name}" = "${KEEP}" ] && continue
      rm -f "${file}"
      # The claim's Delete reclaim policy removes its volume.
      [ "${kind}" = pvc ] && rm -f "${STATE}/pv/_/pvc-retained"
      : ;;
  esac
done
FAKE
chmod +x "${work_dir}/kubectl"

owned='{"metadata":{"labels":{"kustomize.toolkit.fluxcd.io/name":"apps","kustomize.toolkit.fluxcd.io/namespace":"flux-system"}}}'

seed() { # <case> <inventory-ids-json>
  local state="${work_dir}/$1" id
  rm -rf "${state}"
  mkdir -p "${state}"
  jq -n --argjson ids "$2" '{metadata:{generation:4},status:{observedGeneration:4,
    conditions:[{type:"Ready",status:"True"}],inventory:{entries:[$ids[] | {id: ., v:"v1"}]}}}' \
    >"${state}/apps.json"
  for id in helmrelease/crossview/crossview helmrelease/headlamp/headlamp \
    namespace/_/crossview namespace/_/headlamp; do
    mkdir -p "${state}/$(dirname "${id}")"
    printf '%s' "${owned}" >"${state}/${id}"
  done
  mkdir -p "${state}/pvc/crossview" "${state}/pv/_"
  printf '{"spec":{"volumeName":"pvc-retained"}}' >"${state}/pvc/crossview/crossview-postgres-pvc"
  printf '{}' >"${state}/pv/_/pvc-retained"
  for kind in configurationscansummaries.spdx.softwarecomposition.kubescape.io \
    vulnerabilitysummaries.spdx.softwarecomposition.kubescape.io; do
    mkdir -p "${state}/${kind}/_"
    printf '{}' >"${state}/${kind}/_/crossview"
    printf '{}' >"${state}/${kind}/_/headlamp"
  done
  printf '%s' "${state}"
}

run() { # <state> [env...] -> sets status and output
  local state="$1"
  shift
  if output="$(env STATE="${state}" KUBECTL="${work_dir}/kubectl" RETIREMENT_POLL_SECONDS=0 \
    RETIREMENT_POLL_ATTEMPTS=2 RETIREMENT_CONFIRM=retire-crossview-headlamp-2496 "$@" \
    bash "${script}" 2>&1)"; then
    status=0
  else
    status=$?
  fi
}

expect() { # <status> <needle> <message>: the run ended with status and printed needle
  if [ "${status}" -ne "$1" ] || ! grep -Fq -- "$2" <<<"${output}"; then
    fail "$3, got ${status}: ${output}"
  fi
}

deleted() { # <state>
  grep -c '^delete ' "$1/calls" 2>/dev/null || true
}

deployed='["flux-system_apps_kustomize.toolkit.fluxcd.io_Kustomization","whoami_whoami_helm.toolkit.fluxcd.io_HelmRelease"]'

# Without the exact confirmation nothing is read.
state="$(seed unconfirmed "${deployed}")"
run "${state}" RETIREMENT_CONFIRM=yes
[ "${status}" -eq 2 ] || fail "an unconfirmed run must be UNKNOWN, got ${status}: ${output}"
[ ! -e "${state}/calls" ] || fail 'an unconfirmed run must not call the cluster'

# Before the removal deploys, the inventory still owns the apps: refuse.
state="$(seed undeployed '["crossview_crossview_helm.toolkit.fluxcd.io_HelmRelease","_headlamp-cluster-reader_rbac.authorization.k8s.io_ClusterRoleBinding"]')"
run "${state}"
expect 1 'still owns' 'an undeployed removal must be refused'
[ "$(deleted "${state}")" -eq 0 ] || fail 'an undeployed removal must delete nothing'

# Every layer that carried part of an app must have deployed the removal.
state="$(seed infrastructure-undeployed "${deployed}")"
jq '.status.inventory.entries += [{id: "observability_crossview-login-alerter_batch_CronJob", v: "v1"}]' \
  "${state}/apps.json" >"${state}/infrastructure.json"
run "${state}"
expect 1 'deployed infrastructure inventory still owns observability_crossview-login-alerter' \
  'a removal not yet deployed to infrastructure must be refused'
[ "$(deleted "${state}")" -eq 0 ] || fail 'an undeployed infrastructure removal must delete nothing'

# A Kustomization that is not Ready for its generation proves nothing.
state="$(seed stale "${deployed}")"
jq '.status.observedGeneration = 3' "${state}/apps.json" >"${state}/apps.tmp" && mv "${state}/apps.tmp" "${state}/apps.json"
run "${state}"
[ "${status}" -eq 1 ] && [ "$(deleted "${state}")" -eq 0 ] ||
  fail "a stale apps Kustomization must be refused, got ${status}: ${output}"

# An object of the same name that Flux apps does not own is never deleted.
state="$(seed foreign "${deployed}")"
printf '{"metadata":{"labels":{}}}' >"${state}/namespace/_/headlamp"
run "${state}"
expect 1 'Namespace headlamp is not owned' 'a foreign namespace must be refused'
[ -f "${state}/namespace/_/headlamp" ] || fail 'a foreign namespace must survive'

# A failed read is UNKNOWN and never reads as "already gone".
state="$(seed failed-read "${deployed}")"
run "${state}" FAIL_READ=helmrelease
[ "${status}" -eq 2 ] && [ "$(deleted "${state}")" -eq 0 ] ||
  fail "a failed read must stop the run as UNKNOWN, got ${status}: ${output}"

# The same holds for the final proof: bindings are only read there.
state="$(seed failed-proof "${deployed}")"
run "${state}" FAIL_READ=clusterrolebinding
[ "${status}" -eq 2 ] || fail "a failed read in the final proof must be UNKNOWN, got ${status}: ${output}"

# The deployed removal retires every object, releases before the claim before
# the namespaces, and proves the set is gone.
state="$(seed deployed "${deployed}")"
run "${state}"
expect 0 'PASS:' 'the deployed retirement must pass'
order="$(grep '^delete ' "${state}/calls" | awk '{print $2}' | tr '\n' ' ')"
[ "${order}" = 'helmrelease helmrelease pvc namespace namespace configurationscansummaries.spdx.softwarecomposition.kubescape.io vulnerabilitysummaries.spdx.softwarecomposition.kubescape.io ' ] ||
  fail "unexpected deletion order: ${order}"
[ -z "$(find "${state}" -path "${state}/calls" -prune -o -path "${state}/apps.json" -prune -o -type f -print)" ] ||
  fail 'the deployed retirement must leave no object behind'

# A second run finds nothing to delete but still verifies.
rm -f "${state}/calls"
run "${state}"
expect 0 'already gone' 'a repeat run must pass'
[ "$(grep -c '^delete ' "${state}/calls" | tr -d ' ')" -eq 2 ] ||
  fail 'a repeat run may only re-issue the idempotent summary deletions'

# An object that survives its deletion is reported, never passed.
state="$(seed leftover "${deployed}")"
run "${state}" KEEP=configurationscansummaries.spdx.softwarecomposition.kubescape.io/headlamp
expect 1 'still present: configurationscansummaries.spdx.softwarecomposition.kubescape.io headlamp' \
  'a surviving object must fail the run'

# A retired binding Flux failed to prune is reported too.
state="$(seed binding "${deployed}")"
mkdir -p "${state}/clusterrolebinding/_"
printf '{}' >"${state}/clusterrolebinding/_/crossview-view"
run "${state}"
expect 1 'ClusterRoleBinding crossview-view' 'a surviving binding must fail the run'

printf 'PASS: the Crossview and Headlamp retirement waits for the deployed removal, deletes only owned leftovers and proves nothing remains\n'
