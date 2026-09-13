#!/usr/bin/env bash
# Read ONE Cilium agent's ipcache and decide whether the #2284 precondition is still present.
#
# THE QUESTION (devantler-tech/platform#2284). On Cilium 1.20.0-pre.3 the Gateway API
# ExternalAuth (ext_authz) subrequest to oauth2-proxy was black-holed whenever it crossed
# nodes. The recorded root cause was read from the ipcache of a node running NO oauth2-proxy
# replica: the oauth2-proxy pods sat behind WireGuard (`encryptkey=255`) while the proxy
# source — host-sourced, `identity=6` (remote-node) — carried `encryptkey=0`, so the
# plaintext subrequest could not enter the encrypted pod path cross-node. Production now runs
# a later Cilium; this script repeats that one read against the deployed build.
#
# WHAT IT DOES — read-only, four kubectl calls and nothing else:
#   1. `get pods` in oauth2-proxy  — the replicas' pod IPs and nodes, resolved at run time.
#   2. `get nodes`                 — node addresses, to tell host-sourced identity=6 entries
#                                    apart from other remote-node entries.
#   3. `get pods` in kube-system   — a READY Cilium agent on a node hosting NEITHER replica.
#   4. `exec … cilium-dbg bpf ipcache list` in that agent — a BPF map dump; it changes nothing.
#
# THE VERDICT
#   FAULT-PERSISTS   every oauth2-proxy pod entry is behind WireGuard (encryptkey != 0) AND every
#                    identity=6 entry for a node address carries encryptkey=0 — the precondition
#                    the issue blamed is unchanged.
#   PLAUSIBLY-FIXED  pods behind WireGuard AND every identity=6 node-address entry is non-zero.
#   INCONCLUSIVE     anything else: a failed read, an unsettled rollout, no eligible agent, a pod
#                    IP missing from the ipcache, a pod entry NOT behind WireGuard, no identity=6
#                    node-address entry, a mixed population, or an entry that would not parse.
#
# Only node-address entries decide the source side. A remote node's CiliumInternalIP also carries
# identity=6 and is ordinarily behind WireGuard, so counting it would turn every healthy reading
# into a mixed one; it is reported as a count and never votes.
#
# WHAT THE VERDICT IS NOT. The ipcache is a static indicator of the precondition, not a datapath
# test. PLAUSIBLY-FIXED means a cutover re-attempt behind a default-off flag is worth measuring;
# it does not prove cross-node ext_authz delivers. FAULT-PERSISTS means the blamed state is still
# present on the deployed build.
#
# WHAT IT PRINTS. The workflow log of a public repository is public, so no address is printed:
# not pod IPs, node addresses, tunnel endpoints, node names or pod names. Only counts, identities
# and encryption keys — the fields the verdict is made of. kubectl's stderr is discarded for the
# same reason (a connection error names the API server address).
#
# EXIT CODES
#   0  a conclusive verdict (FAULT-PERSISTS or PLAUSIBLY-FIXED)
#   1  usage error — nothing was read
#   3  INCONCLUSIVE — the read proved nothing, and a caller must not report it as green
#
# Bash 3.2 compatible so it runs on a maintainer's macOS as well as CI.
set -euo pipefail

readonly oauth2_namespace='oauth2-proxy'
readonly oauth2_selector='app.kubernetes.io/name=oauth2-proxy,app.kubernetes.io/instance=oauth2-proxy'
readonly cilium_namespace='kube-system'
readonly cilium_selector='k8s-app=cilium'
readonly cilium_container='cilium-agent'

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
      printf 'Unknown argument: %s\n' "$1" >&2
      usage
      exit 1
      ;;
  esac
done

# Required, never defaulted: a read against whatever the kubeconfig's current-context happens to
# be would report on a cluster that is not the one being diagnosed.
if [[ -z "${context}" ]]; then
  printf 'Refusing to run: --context is required.\n' >&2
  usage
  exit 1
fi

summary() {
  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    printf '%s\n' "$1" >>"${GITHUB_STEP_SUMMARY}" || true
  fi
}

inconclusive() {
  printf 'VERDICT: INCONCLUSIVE\n'
  printf 'Reason: %s\n' "$1"
  summary "### Cilium ExternalAuth ipcache diagnostic (#2284)"
  summary ""
  summary "**Verdict:** INCONCLUSIVE — $1"
  exit 3
}

conclude() {
  printf 'VERDICT: %s\n' "$1"
  printf 'Reason: %s\n' "$2"
  summary "### Cilium ExternalAuth ipcache diagnostic (#2284)"
  summary ""
  summary "**Verdict:** $1 — $2"
  exit 0
}

kc() {
  kubectl --context "${context}" "$@" 2>/dev/null
}

is_address() {
  [[ "$1" =~ ^[0-9A-Fa-f:.]+$ ]]
}

is_k8s_name() {
  [[ "$1" =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ ]]
}

# ---------------------------------------------------------------------------
# 1. oauth2-proxy replicas. A replica that is Pending, terminating or not Ready means a rollout
#    is in flight: its IP may or may not be in the ipcache yet, and a replica may be about to land
#    on the node we would read. Neither is a verdict, so it fails closed.
# ---------------------------------------------------------------------------
if ! oauth2_json="$(kc -n "${oauth2_namespace}" get pods -l "${oauth2_selector}" -o json)"; then
  inconclusive 'could not list the oauth2-proxy pods'
fi

if ! oauth2_lines="$(jq -r '
    def ready: ([.status.conditions[]? | select(.type == "Ready") | .status] == ["True"]);
    [.items[]? | {
      ok: (.status.phase == "Running" and .metadata.deletionTimestamp == null and ready),
      node: (.spec.nodeName // ""),
      ips: (([.status.podIPs[]?.ip] + [.status.podIP // empty]) | map(select(. != "")) | unique)
    }]
    | if length == 0 then "ERR none"
      elif any(.[]; .ok | not) then "ERR unsettled"
      elif any(.[]; .node == "" or (.ips | length) == 0) then "ERR incomplete"
      else (.[] | ("NODE " + .node), (.ips[] | "IP " + .))
      end
  ' <<<"${oauth2_json}")"; then
  inconclusive 'the oauth2-proxy pod list did not parse'
fi

case "${oauth2_lines}" in
  'ERR none') inconclusive 'no oauth2-proxy pods were found' ;;
  'ERR unsettled') inconclusive 'an oauth2-proxy replica is not Running and Ready (rollout in flight)' ;;
  'ERR incomplete') inconclusive 'an oauth2-proxy replica has no node or no pod IP yet' ;;
esac

replica_nodes=''
pod_ips=''
replica_count=0
while IFS=' ' read -r kind value; do
  case "${kind}" in
    NODE)
      is_k8s_name "${value}" || inconclusive 'an oauth2-proxy replica reported a malformed node name'
      replica_count=$((replica_count + 1))
      replica_nodes="${replica_nodes}${value}"$'\n'
      ;;
    IP)
      is_address "${value}" || inconclusive 'an oauth2-proxy replica reported a malformed pod IP'
      pod_ips="${pod_ips}${value} "
      ;;
    *) inconclusive 'the oauth2-proxy pod list produced an unexpected record' ;;
  esac
done <<<"${oauth2_lines}"

if [[ "${replica_count}" -eq 0 || -z "${pod_ips}" ]]; then
  inconclusive 'no oauth2-proxy pod IPs were resolved'
fi

distinct_replica_nodes="$(printf '%s' "${replica_nodes}" | sort -u | grep -c . || true)"
printf 'oauth2-proxy replicas: %s (on %s node(s))\n' "${replica_count}" "${distinct_replica_nodes}"

# ---------------------------------------------------------------------------
# 2. Node addresses. Both address types are host addresses; which one a node uses as its
#    Cilium node IP depends on the provider, so both count.
# ---------------------------------------------------------------------------
if ! nodes_json="$(kc get nodes -o json)"; then
  inconclusive 'could not list the nodes'
fi
if ! node_address_lines="$(jq -r '
    [.items[]?.status.addresses[]? | select(.type == "InternalIP" or .type == "ExternalIP") | .address]
    | unique | .[]
  ' <<<"${nodes_json}")"; then
  inconclusive 'the node list did not parse'
fi

node_addresses=''
while IFS= read -r address; do
  [[ -z "${address}" ]] && continue
  is_address "${address}" || inconclusive 'a node reported a malformed address'
  node_addresses="${node_addresses}${address} "
done <<<"${node_address_lines}"
if [[ -z "${node_addresses}" ]]; then
  inconclusive 'no node addresses were resolved'
fi

# ---------------------------------------------------------------------------
# 3. A Ready Cilium agent on a node hosting neither replica. Sorted by node so the choice is
#    deterministic across runs.
# ---------------------------------------------------------------------------
if ! cilium_json="$(kc -n "${cilium_namespace}" get pods -l "${cilium_selector}" -o json)"; then
  inconclusive 'could not list the Cilium agent pods'
fi
if ! agent_lines="$(jq -r --arg container "${cilium_container}" '
    [.items[]?
      | select(.status.phase == "Running" and .metadata.deletionTimestamp == null)
      | select(any(.status.containerStatuses[]?; .name == $container and .ready == true))
      | {name: .metadata.name, node: (.spec.nodeName // "")}
      | select(.node != "")]
    | sort_by(.node, .name) | .[] | .node + " " + .name
  ' <<<"${cilium_json}")"; then
  inconclusive 'the Cilium agent pod list did not parse'
fi

agent_pod=''
while IFS=' ' read -r node name; do
  [[ -z "${node}" ]] && continue
  if printf '%s' "${replica_nodes}" | grep -qxF -- "${node}"; then
    continue
  fi
  if is_k8s_name "${node}" && is_k8s_name "${name}"; then
    agent_pod="${name}"
    break
  fi
done <<<"${agent_lines}"

if [[ -z "${agent_pod}" ]]; then
  inconclusive 'no Ready Cilium agent runs on a node hosting neither oauth2-proxy replica'
fi
printf 'Cilium agent: selected a Ready agent on a node hosting neither replica\n'

# ---------------------------------------------------------------------------
# 4. The one exec. `cilium-dbg bpf ipcache list` dumps a BPF map; it writes nothing.
# ---------------------------------------------------------------------------
if ! ipcache="$(kc -n "${cilium_namespace}" exec "${agent_pod}" -c "${cilium_container}" -- \
  cilium-dbg bpf ipcache list)"; then
  inconclusive 'the ipcache read in the Cilium agent failed'
fi
if [[ -z "${ipcache//[[:space:]]/}" ]]; then
  inconclusive 'the ipcache read returned nothing'
fi

# Parse. Addresses are matched EXACTLY after the prefix length is stripped, never as substrings:
# a pod at 10.244.22.23 must not be satisfied by an entry for 10.244.22.235. Values reach awk
# through ENVIRON rather than -v, which would interpret backslash escapes.
records="$(POD_IPS="${pod_ips}" NODE_ADDRESSES="${node_addresses}" awk '
  BEGIN {
    n = split(ENVIRON["POD_IPS"], pods, " ")
    for (i = 1; i <= n; i++) pod_index[pods[i]] = i
    m = split(ENVIRON["NODE_ADDRESSES"], nodes, " ")
    for (i = 1; i <= m; i++) node_set[nodes[i]] = 1
  }
  {
    identity = ""; key = "?"
    for (f = 2; f <= NF; f++) {
      if ($f ~ /^identity=/) identity = substr($f, 10)
      else if ($f ~ /^encryptkey=/) key = substr($f, 12)
    }
    if (identity == "") next
    address = $1
    sub(/\/[0-9]+$/, "", address)
    parsed++
    if (address in pod_index) print "POD", pod_index[address], identity, key
    if (identity == "6") {
      if (address in node_set) print "NODE", key
      else print "OTHER6", key
    }
  }
  END { print "PARSED", parsed + 0 }
' <<<"${ipcache}")"

parsed="$(awk '$1 == "PARSED" { print $2 }' <<<"${records}")"
if [[ -z "${parsed}" || "${parsed}" -eq 0 ]]; then
  inconclusive 'no ipcache entry could be parsed'
fi
printf 'ipcache entries parsed: %s\n' "${parsed}"

# Pod side: every resolved pod IP must be present, and every entry for it behind WireGuard.
pod_total="$(printf '%s' "${pod_ips}" | wc -w | tr -d ' ')"
i=1
while [[ "${i}" -le "${pod_total}" ]]; do
  pod_records="$(awk -v idx="${i}" '$1 == "POD" && $2 == idx { print $3, $4 }' <<<"${records}")"
  if [[ -z "${pod_records}" ]]; then
    inconclusive "oauth2-proxy pod IP #${i} has no ipcache entry on the selected node"
  fi
  while IFS=' ' read -r identity key; do
    if ! [[ "${identity}" =~ ^[0-9]+$ && "${key}" =~ ^[0-9]+$ ]]; then
      inconclusive "the ipcache entry for oauth2-proxy pod IP #${i} did not parse"
    fi
    printf 'oauth2-proxy pod IP #%s: identity=%s encryptkey=%s\n' "${i}" "${identity}" "${key}"
    if [[ "${key}" -eq 0 ]]; then
      inconclusive "oauth2-proxy pod IP #${i} is not behind WireGuard (encryptkey=0), so the #2284 precondition does not apply"
    fi
  done <<<"${pod_records}"
  i=$((i + 1))
done

# Source side: identity=6 entries for node addresses decide; other identity=6 entries are counted.
node_zero=0
node_nonzero=0
while IFS=' ' read -r kind key; do
  [[ "${kind}" == 'NODE' ]] || continue
  if ! [[ "${key}" =~ ^[0-9]+$ ]]; then
    inconclusive 'an identity=6 node-address entry has no parseable encryptkey'
  fi
  if [[ "${key}" -eq 0 ]]; then
    node_zero=$((node_zero + 1))
  else
    node_nonzero=$((node_nonzero + 1))
  fi
done <<<"${records}"

other_zero=0
other_nonzero=0
while IFS=' ' read -r kind key; do
  [[ "${kind}" == 'OTHER6' ]] || continue
  if [[ "${key}" == '0' ]]; then
    other_zero=$((other_zero + 1))
  else
    other_nonzero=$((other_nonzero + 1))
  fi
done <<<"${records}"

printf 'identity=6 node-address entries (decide): encryptkey=0: %s, encryptkey!=0: %s\n' \
  "${node_zero}" "${node_nonzero}"
printf 'identity=6 other entries (informational): encryptkey=0: %s, other: %s\n' \
  "${other_zero}" "${other_nonzero}"

if [[ $((node_zero + node_nonzero)) -eq 0 ]]; then
  inconclusive 'no identity=6 entry for a node address was found, so the proxy source is unobserved'
fi
if [[ "${node_zero}" -gt 0 && "${node_nonzero}" -gt 0 ]]; then
  inconclusive 'identity=6 node-address entries disagree on encryptkey'
fi

if [[ "${node_zero}" -gt 0 ]]; then
  conclude 'FAULT-PERSISTS' 'oauth2-proxy pods are behind WireGuard while the host-sourced identity=6 proxy source is plaintext (encryptkey=0)'
fi
conclude 'PLAUSIBLY-FIXED' 'oauth2-proxy pods are behind WireGuard and the host-sourced identity=6 proxy source carries a non-zero encryptkey'
