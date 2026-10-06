#!/usr/bin/env bash
# Read the user and group id maps of both live Actual Budget containers (#3604).
#
# THE QUESTION. The user-namespace pilot is active: the pod runs with `hostUsers: false` and has
# been healthy for weeks. What nothing has shown yet is the kernel's side of it — that each of the
# two containers really runs inside a user namespace whose root is NOT the node's root. A pod
# field is a request; the id map is the answer. A separate smoke pod would not be evidence about
# these two containers, so the maps are read from them.
#
# WHAT IT DOES. It changes nothing.
#   1. `get pods` in the actual-budget namespace — exactly one pod, Running, `hostUsers: false`,
#      with exactly the two expected containers, both ready.
#   2. For each container, `exec … -- cat /proc/self/uid_map` and `… /proc/self/gid_map`. The
#      command line is fixed; nothing read from the cluster is ever executed. A process started
#      by `exec` joins the container's namespaces, so the file it reads is that container's map.
#
# HOW A MAP IS READ. Each line is `<first id inside> <first id outside> <count>`. A container
# that shares the node's ids shows the identity map, `0 0 4294967295`. A container in its own
# user namespace shows id 0 mapped to a non-zero id outside, over a bounded range. A map counts
# as MAPPED only when every line parses, id 0 is mapped, no id maps onto itself and no range
# reaches the full id space.
#
# THE VERDICT
#   MAPPED         all four maps (two containers, user and group) are non-identity.
#   IDENTITY       at least one map is the node's own: the user namespace is not in effect there.
#   INCONCLUSIVE   anything else: a failed read, no pod or more than one, a pod that is not
#                  Running or not ready, `hostUsers` not false, unexpected containers, or a map
#                  that could not be parsed.
#
# WHAT IT PRINTS. The workflow log of a public repository is public. Only container names, map
# names, range sizes and outcomes are printed: never the ids a container maps to on the node,
# never the pod name, and never kubectl's own error text, which can carry the API server address.
#
# EXIT CODES
#   0  MAPPED
#   1  usage error — nothing was read
#   2  IDENTITY
#   3  INCONCLUSIVE — the read proved nothing, and a caller must not report it as green
#
# Bash 3.2 compatible so it runs on a maintainer's macOS as well as CI.
set -euo pipefail

readonly namespace='actual-budget'
readonly selector='app.kubernetes.io/name=actualbudget'
readonly expected_containers='actualbudget enablebanking-seed'
readonly full_id_space=4294967295

usage() {
  printf 'Usage: %s --context <kube-context>\n' "$(basename "$0")" >&2
}

context=''
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --context)
      if [[ "$#" -lt 2 || -z "$2" ]]; then
        usage
        exit 1
      fi
      context="$2"
      shift 2
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      usage
      exit 1
      ;;
  esac
done
if [[ -z "${context}" ]]; then
  usage
  exit 1
fi

for tool in kubectl jq; do
  if ! command -v "${tool}" >/dev/null 2>&1; then
    printf '%s is required.\n' "${tool}" >&2
    exit 1
  fi
done

inconclusive() {
  printf 'INCONCLUSIVE: %s\n' "$1"
  exit 3
}

# --- the pod -----------------------------------------------------------------
if ! pods="$(kubectl --context "${context}" get pods --namespace "${namespace}" \
  --selector "${selector}" --output json 2>/dev/null)"; then
  inconclusive 'the pod list could not be read.'
fi
if ! pod_count="$(jq -er '.items | length' <<<"${pods}" 2>/dev/null)"; then
  inconclusive 'the pod list was not the expected JSON.'
fi
if [[ "${pod_count}" -ne 1 ]]; then
  inconclusive "expected exactly one Actual Budget pod, found ${pod_count}."
fi

pod="$(jq -r '.items[0].metadata.name // ""' <<<"${pods}")"
phase="$(jq -r '.items[0].status.phase // ""' <<<"${pods}")"
host_users="$(jq -r '.items[0].spec.hostUsers | if . == null then "unset" else tostring end' <<<"${pods}")"
containers="$(jq -r '[.items[0].spec.containers[]?.name] | sort | join(" ")' <<<"${pods}")"
not_ready="$(jq -r '[.items[0].status.containerStatuses[]? | select(.ready != true)] | length' <<<"${pods}")"
status_count="$(jq -r '[.items[0].status.containerStatuses[]?] | length' <<<"${pods}")"

# The name goes on a kubectl command line, so it must look like a pod name before it is used.
if [[ ! "${pod}" =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ ]]; then
  inconclusive 'the pod name was not a valid object name.'
fi
if [[ "${phase}" != Running ]]; then
  inconclusive 'the pod is not Running.'
fi
if [[ "${host_users}" != false ]]; then
  inconclusive "the pod does not request a user namespace (hostUsers is ${host_users})."
fi
if [[ "${containers}" != "${expected_containers}" ]]; then
  inconclusive 'the pod does not have exactly the two expected containers.'
fi
if [[ "${status_count}" -ne 2 || "${not_ready}" -ne 0 ]]; then
  inconclusive 'not every container is ready.'
fi
printf 'Pod: one, Running, hostUsers false, both containers ready.\n'

# --- the maps ----------------------------------------------------------------
# Prints `mapped <total count>` or `identity`, and returns 1 when the map cannot be judged.
classify_map() {
  local map="$1" inside outside count extra lines=0 total=0 zero_mapped=0 identity=0
  while read -r inside outside count extra; do
    [[ -z "${inside}${outside}${count}${extra}" ]] && continue
    if [[ -n "${extra}" || ! "${inside}" =~ ^[0-9]+$ || ! "${outside}" =~ ^[0-9]+$ ||
      ! "${count}" =~ ^[1-9][0-9]*$ ]]; then
      return 1
    fi
    # Longer than any id the kernel prints; refuse rather than let bash arithmetic wrap.
    if [[ "${#inside}" -gt 10 || "${#outside}" -gt 10 || "${#count}" -gt 10 ]]; then
      return 1
    fi
    lines=$((lines + 1))
    total=$((total + count))
    if [[ "${inside}" -eq 0 ]]; then
      zero_mapped=1
    fi
    if [[ "${inside}" -eq "${outside}" || "${count}" -ge "${full_id_space}" ]]; then
      identity=1
    fi
  done <<<"${map}"
  if [[ "${lines}" -eq 0 ]]; then
    return 1
  fi
  if [[ "${identity}" -eq 1 ]]; then
    printf 'identity\n'
    return 0
  fi
  if [[ "${zero_mapped}" -ne 1 ]]; then
    return 1
  fi
  printf 'mapped %s\n' "${total}"
}

identity_found=0
unknown_found=0
for container in ${expected_containers}; do
  for map_name in uid_map gid_map; do
    if ! map="$(kubectl --context "${context}" exec --namespace "${namespace}" "${pod}" \
      --container "${container}" -- cat "/proc/self/${map_name}" 2>/dev/null)"; then
      printf '%s %s: could not be read.\n' "${container}" "${map_name}"
      unknown_found=1
      continue
    fi
    if ! result="$(classify_map "${map}")"; then
      printf '%s %s: not a map this script can judge.\n' "${container}" "${map_name}"
      unknown_found=1
      continue
    fi
    if [[ "${result}" == identity ]]; then
      printf '%s %s: IDENTITY — the container shares ids with the node.\n' "${container}" "${map_name}"
      identity_found=1
    else
      printf '%s %s: mapped — id 0 is a non-zero id on the node, %s ids in range.\n' \
        "${container}" "${map_name}" "${result#mapped }"
    fi
  done
done

if [[ "${identity_found}" -eq 1 ]]; then
  printf 'IDENTITY: at least one map is the node'"'"'s own; the user namespace is not in effect there.\n'
  exit 2
fi
if [[ "${unknown_found}" -eq 1 ]]; then
  inconclusive 'at least one map could not be read or judged.'
fi
printf 'MAPPED: both containers run in a user namespace with non-identity user and group maps.\n'
