#!/usr/bin/env bash
# Offline regression for scripts/retire-unused-apps.sh against a fake kubectl
# whose objects live as files. It proves the retirement refuses before the
# removal is deployed in every layer, refuses objects Flux `apps` does not own,
# deletes owners before namespaces and storage, deletes only storage last
# claimed in a retired namespace, waits for Longhorn volumes to detach, is
# idempotent, never reads a failed read as "absent", and reports any leftover.
# No cluster or credential is used.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly script="${root_dir}/scripts/retire-unused-apps.sh"
work_dir="$(mktemp -d)"
readonly work_dir
trap 'rm -rf "${work_dir}"' EXIT

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v jq >/dev/null || fail 'jq is required'

readonly lh=volumes.longhorn.io
readonly cnpg=clusters.postgresql.cnpg.io

# The fake stores one JSON file per object under $STATE/<kind>/<namespace>/<name>
# (namespace "_" for cluster-scoped kinds), logs every call as "<verb> <kind>
# <names>", answers a nameless get with a list, and models the side effects the
# script relies on: deleting a namespace deletes its objects, a Delete-policy
# volume goes with its claim, and a Retain volume becomes Released while its
# Longhorn volume detaches.
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
printf '%s %s %s\n' "${verb}" "${kind}" "${names[*]:-}" >>"${STATE}/calls"
if [ -n "${FAIL_READ:-}" ] && [ "${verb}" = get ] && [ "${kind}" = "${FAIL_READ}" ]; then
  echo 'error: the server is currently unable to handle the request' >&2
  exit 1
fi
if [ "${verb}" = get ] && [ "${#names[@]}" -eq 0 ]; then
  find "${STATE}/${kind}/${namespace}" -type f -exec cat {} + 2>/dev/null | jq -s '{items: .}'
  exit 0
fi
for name in "${names[@]}"; do
  file="${STATE}/${kind}/${namespace}/${name}"
  case "${verb}" in
    get)
      if [ "${kind}" = kustomization ]; then cat "${STATE}/${name}.json" 2>/dev/null || cat "${STATE}/apps.json"; elif [ -f "${file}" ]; then cat "${file}"; fi ;;
    delete)
      [ -n "${KEEP:-}" ] && [ "${kind}/${name}" = "${KEEP}" ] && continue
      rm -f "${file}"
      if [ "${kind}" = namespace ]; then
        rm -rf "${STATE}"/*/"${name}"
        for pv in "${STATE}"/pv/_/*; do
          [ -f "${pv}" ] && [ "$(jq -r .spec.claimRef.namespace "${pv}")" = "${name}" ] || continue
          volume="${STATE}/volumes.longhorn.io/longhorn-system/$(basename "${pv}")"
          if [ "$(jq -r .spec.persistentVolumeReclaimPolicy "${pv}")" = Delete ]; then
            rm -f "${pv}" "${volume}"
          else
            jq '.status.phase = "Released"' "${pv}" >"${pv}.tmp" && mv "${pv}.tmp" "${pv}"
            [ -n "${STAY_ATTACHED:-}" ] || { jq '.status.state = "detached"' "${volume}" >"${volume}.tmp" && mv "${volume}.tmp" "${volume}"; }
          fi
        done
      fi ;;
  esac
done
FAKE
chmod +x "${work_dir}/kubectl"

owned='{"metadata":{"labels":{"kustomize.toolkit.fluxcd.io/name":"apps","kustomize.toolkit.fluxcd.io/namespace":"flux-system"}}}'

put() { # <state> <kind> <namespace> <name> <json>
  mkdir -p "$1/$2/$3"
  printf '%s' "$5" >"$1/$2/$3/$4"
}

volume() { # <state> <name> <claim-namespace> <claim> <policy>
  put "$1" pv _ "$2" "$(jq -nc --arg n "$2" --arg ns "$3" --arg c "$4" --arg p "$5" \
    '{metadata:{name:$n},spec:{claimRef:{namespace:$ns,name:$c},persistentVolumeReclaimPolicy:$p},status:{phase:"Bound"}}')"
  put "$1" "${lh}" longhorn-system "$2" "$(jq -nc --arg n "$2" --arg ns "$3" --arg c "$4" \
    '{metadata:{name:$n},status:{state:"attached",kubernetesStatus:{namespace:$ns,pvcName:$c,pvName:$n}}}')"
}

seed() { # <case> <inventory-ids-json>
  local state="${work_dir}/$1" kind
  rm -rf "${state}"
  mkdir -p "${state}"
  jq -n --argjson ids "$2" '{metadata:{generation:4},status:{observedGeneration:4,
    conditions:[{type:"Ready",status:"True"}],inventory:{entries:[$ids[] | {id: ., v:"v1"}]}}}' \
    >"${state}/apps.json"
  put "${state}" helmrelease crossview crossview "${owned}"
  put "${state}" helmrelease headlamp headlamp "${owned}"
  put "${state}" "${cnpg}" backstage backstage-db "${owned}"
  for namespace in crossview headlamp backstage; do
    put "${state}" namespace _ "${namespace}" "${owned}"
  done
  put "${state}" pvc crossview crossview-postgres-pvc '{}'
  volume "${state}" pvc-crossview crossview crossview-postgres-pvc Delete
  put "${state}" pvc backstage backstage-db-3 '{}'
  volume "${state}" pvc-backstage-3 backstage backstage-db-3 Retain
  volume "${state}" pvc-backstage-4 backstage backstage-db-4 Retain
  # Unrelated storage must survive every run.
  volume "${state}" pvc-wedding wedding-app wedding-db-1 Retain
  for kind in configurationscansummaries.spdx.softwarecomposition.kubescape.io \
    vulnerabilitysummaries.spdx.softwarecomposition.kubescape.io; do
    for namespace in crossview headlamp backstage; do
      put "${state}" "${kind}" _ "${namespace}" '{}'
    done
  done
  printf '%s' "${state}"
}

run() { # <state> [env...] -> sets status and output
  local state="$1"
  shift
  if output="$(env STATE="${state}" KUBECTL="${work_dir}/kubectl" RETIREMENT_POLL_SECONDS=0 \
    RETIREMENT_POLL_ATTEMPTS=3 RETIREMENT_CONFIRM=retire-crossview-headlamp-backstage "$@" \
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

survivors() { # <state>: every object file left, relative to the state
  (cd "$1" && find . -path ./calls -prune -o -name '*.json' -prune -o -type f -print | sort)
}

deployed='["flux-system_apps_kustomize.toolkit.fluxcd.io_Kustomization","whoami_whoami_helm.toolkit.fluxcd.io_HelmRelease"]'

# Without the exact confirmation nothing is read.
state="$(seed unconfirmed "${deployed}")"
run "${state}" RETIREMENT_CONFIRM=retire-crossview-headlamp-2496
[ "${status}" -eq 2 ] || fail "an unconfirmed run must be UNKNOWN, got ${status}: ${output}"
[ ! -e "${state}/calls" ] || fail 'an unconfirmed run must not call the cluster'

# Before the removal deploys, an inventory still owns an app: refuse.
for id in crossview_crossview_helm.toolkit.fluxcd.io_HelmRelease _headlamp-cluster-reader_rbac.authorization.k8s.io_ClusterRoleBinding \
  _backstage__Namespace; do
  state="$(seed undeployed "[\"${id}\"]")"
  run "${state}"
  expect 1 'still owns' "an undeployed removal owning ${id} must be refused"
  [ "$(deleted "${state}")" -eq 0 ] || fail "an undeployed removal owning ${id} must delete nothing"
done

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
expect 1 'is not Ready for its current generation' 'a stale apps Kustomization must be refused'
[ "$(deleted "${state}")" -eq 0 ] || fail 'a stale apps Kustomization must delete nothing'

# An object of the same name that Flux apps does not own is never deleted.
state="$(seed foreign "${deployed}")"
put "${state}" "${cnpg}" backstage backstage-db '{"metadata":{"labels":{}}}'
run "${state}"
expect 1 'Cluster backstage/backstage-db is not owned' 'a foreign database must be refused'
[ -f "${state}/${cnpg}/backstage/backstage-db" ] || fail 'a foreign database must survive'
[ -f "${state}/namespace/_/backstage" ] || fail 'a refusal must stop before the namespaces'

# A failed read is UNKNOWN and never reads as "already gone".
state="$(seed failed-read "${deployed}")"
run "${state}" FAIL_READ=helmrelease
expect 2 'UNKNOWN: cannot read helmrelease' 'a failed read must stop the run as UNKNOWN'
[ "$(deleted "${state}")" -eq 0 ] || fail 'a failed read must delete nothing'

# The same holds for the storage listings and the final proof.
for kind in pv "${lh}" clusterrolebinding; do
  state="$(seed "failed-${kind}" "${deployed}")"
  run "${state}" FAIL_READ="${kind}"
  [ "${status}" -eq 2 ] || fail "a failed ${kind} read must be UNKNOWN, got ${status}: ${output}"
  if [ "${kind}" = pv ] && grep -q "^delete ${lh} " "${state}/calls"; then
    fail 'a failed volume read must stop the run before any Longhorn volume is deleted'
  fi
done

# A Longhorn volume that never detaches is not deleted under a workload.
state="$(seed attached "${deployed}")"
run "${state}" STAY_ATTACHED=1
expect 1 'Longhorn volumes did not detach' 'an attached Longhorn volume must stop the run'
[ -f "${state}/${lh}/longhorn-system/pvc-backstage-3" ] || fail 'an attached Longhorn volume must survive'

# The deployed removal retires everything in order and leaves only unrelated
# storage behind.
state="$(seed deployed "${deployed}")"
run "${state}"
expect 0 'PASS:' 'the deployed retirement must pass'
order="$(awk '$1 == "delete" {print $2}' "${state}/calls" | uniq | tr '\n' ' ')"
[ "${order}" = "helmrelease ${cnpg} namespace pv ${lh} configurationscansummaries.spdx.softwarecomposition.kubescape.io vulnerabilitysummaries.spdx.softwarecomposition.kubescape.io " ] ||
  fail "unexpected deletion order: ${order}"
[ "$(survivors "${state}")" = "$(printf './pv/_/pvc-wedding\n./%s/longhorn-system/pvc-wedding' "${lh}")" ] ||
  fail "only unrelated storage may survive, got: $(survivors "${state}")"

# A second run finds nothing to delete but still verifies.
rm -f "${state}/calls"
run "${state}"
expect 0 'already gone' 'a repeat run must pass'
[ "$(deleted "${state}")" -eq 2 ] || fail 'a repeat run may only re-issue the idempotent summary deletions'

# A rerun after the volume objects went still finds the retained Longhorn data.
state="$(seed partial "${deployed}")"
run "${state}" KEEP="${lh}/pvc-backstage-4"
expect 1 'Longhorn volumes did not detach or were not deleted: pvc-backstage-4 detached' \
  'a Longhorn volume that outlives its deletion must fail the run'
[ ! -e "${state}/pv/_/pvc-backstage-4" ] || fail 'its volume object must already be gone for this case'
run "${state}"
expect 0 'Deleting Longhorn volume pvc-backstage-4' 'a rerun must delete the Longhorn volume found by its claim namespace'

# An object that survives its deletion is reported, never passed.
state="$(seed leftover "${deployed}")"
run "${state}" KEEP=configurationscansummaries.spdx.softwarecomposition.kubescape.io/headlamp
expect 1 'still present: configurationscansummaries.spdx.softwarecomposition.kubescape.io headlamp' \
  'a surviving object must fail the run'

# A retired binding Flux failed to prune is reported too.
state="$(seed binding "${deployed}")"
put "${state}" clusterrolebinding _ crossview-view '{}'
run "${state}"
expect 1 'ClusterRoleBinding crossview-view' 'a surviving binding must fail the run'

printf 'PASS: the unused-app retirement waits for the deployed removal, deletes only owned leftovers and their storage, and proves nothing remains\n'
