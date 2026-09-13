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
# WHAT IT DOES — read-only: `get` calls and exactly one exec, nothing else:
#   1. `get pods` in oauth2-proxy          — the replicas' UIDs, pod IPs and nodes.
#   2. `get nodes`                         — every node (name, UID) and its addresses, to tell
#                                            host-sourced identity=6 entries apart from others.
#   3. `get pods` in kube-system           — a READY Cilium agent on a node hosting NEITHER replica.
#   4. `get daemonset cilium`              — the agent set must be fully rolled out.
#   5. `get controllerrevisions`           — the DaemonSet's current revision, which the selected
#                                            agent must be running.
#   6. `get configmap cilium-config`       — whether Cilium node encryption is on.
#   7. `exec … cilium-dbg bpf ipcache list` in that agent — a BPF map dump; it changes nothing.
#   8. `get pods` in oauth2-proxy again    — every selected pod settled, none on the probed node,
#                                            and the same pod set as the first read.
#   9. `get nodes` again                   — the topology must not have changed during the read.
#
# THE VERDICT
#   FAULT-PERSISTS   Cilium node encryption is confirmed ON, every oauth2-proxy pod entry is behind
#                    WireGuard (encryptkey != 0), EVERY remote node is covered, and every identity=6
#                    node-address entry still carries encryptkey=0.
#   PLAUSIBLY-FIXED  pods behind WireGuard, every remote node covered, and every identity=6
#                    node-address entry non-zero.
#   INCONCLUSIVE     anything else: a failed read, an unsettled rollout, no eligible agent, a
#                    partially rolled Cilium DaemonSet or an agent on an old revision, replicas or
#                    a node set that changed during the read, a pod IP missing from the ipcache, a
#                    pod entry NOT behind WireGuard, a remote node with no identity=6 entry for any
#                    of its addresses, a mixed population, an entry that would not parse — or
#                    all-zero node-address keys while node encryption is off or undetermined.
#
# NODE ENCRYPTION. With `encryption.nodeEncryption: false` — this platform's production setting —
# Cilium deliberately does not encrypt host traffic, so `encryptkey=0` on node-address entries is
# the EXPECTED state and says nothing about the #2284 fault. The chart renders the setting into the
# `cilium-config` ConfigMap as `encrypt-node: "true"` when it is on and omits the key when it is off
# (verified by rendering the 1.20.1 chart both ways); the agent defaults the flag to false. So an
# absent key or "false" reads as OFF, "true" as ON, and a failed read or any other value as
# UNDETERMINED. Only ON lets all-zero keys conclude FAULT-PERSISTS; OFF and UNDETERMINED end
# INCONCLUSIVE and point to the flagged ExternalAuth datapath test (#2284, option 2).
#
# ROLLOUT. The prod-deploy lock stops a deploy overlapping the read, but a FAILED deploy can
# release it with old and new Ready agents side by side. A verdict read from an old agent would
# describe a build that is no longer deployed, so before the exec the DaemonSet must have observed
# its current generation, scheduled and made available the updated pod on every node, and the
# selected agent must carry the DaemonSet's current `controller-revision-hash`.
#
# TOPOLOGY AND REPLICAS. The Cluster Autoscaler runs independently of the lock, and a replica can be
# replaced, rescheduled or surged at any time, so both are read again after the exec. The second
# pod read applies the same settled-state check as the first to EVERY selected oauth2-proxy pod, not
# only the Ready ones: a Pending, unready or terminating pod, or one with no UID, node or pod IP, is
# INCONCLUSIVE, and so is any pod — in any state — on the probed node, where it would break the
# no-local-replica precondition. The pod set must then match the first read exactly (UIDs, nodes and
# pod IPs), and any difference in node names, UIDs or InternalIP/ExternalIP addresses is also
# INCONCLUSIVE: the verdict would otherwise be computed from a stale address map or stale pod IPs.
#
# COVERAGE. A "remote node" is every node except the one whose agent is read. Each must have at
# least one identity=6 entry for one of its own addresses before any conclusive verdict: a node
# the autoscaler has just added can be in the node list before its ipcache entry propagates, and
# a verdict that silently ignored it would describe a source it never observed.
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
# not pod IPs, node addresses, tunnel endpoints, node names, UIDs, revision hashes or pod names.
# Only counts, identities, encryption keys, the node-encryption state and the verdict. kubectl's
# stderr is discarded for the same reason (a connection error names the API server address).
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
readonly cilium_daemonset='cilium'
readonly cilium_configmap='cilium-config'

# One canonical, order-insensitive description of the node topology: name, UID and the sorted
# InternalIP/ExternalIP set of every node. Compared before and after the exec.
readonly node_fingerprint_filter='
  [.items[]?
    | (.metadata.name // "") + "|" + (.metadata.uid // "") + "|"
      + ([.status.addresses[]? | select(.type == "InternalIP" or .type == "ExternalIP")
          | .type + "=" + .address] | sort | join(","))]
  | sort | join(";")'

# Shared jq definitions for the oauth2-proxy pod reads, so the first and second read apply the SAME
# settled-state test. `replicas` keeps EVERY selected pod, whatever its state; `fingerprint` is an
# order-insensitive description of the set (UID, node and sorted pod IPs of each pod).
readonly replica_jq_defs='
  def ready: ([.status.conditions[]? | select(.type == "Ready") | .status] == ["True"]);
  def replicas: [.items[]? | {
      ok: (.status.phase == "Running" and .metadata.deletionTimestamp == null and ready),
      uid: (.metadata.uid // ""),
      node: (.spec.nodeName // ""),
      ips: (([.status.podIPs[]?.ip] + [.status.podIP // empty]) | map(select(. != "")) | unique)
    }];
  def settled: .ok and .uid != "" and .node != "" and (.ips | length) > 0;
  def fingerprint: map(.uid + "|" + .node + "|" + (.ips | join(","))) | sort | join(";");
'

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

is_revision_hash() {
  [[ "$1" =~ ^[a-z0-9]+$ ]]
}

# ---------------------------------------------------------------------------
# 1. oauth2-proxy replicas. A replica that is Pending, terminating or not Ready means a rollout
#    is in flight: its IP may or may not be in the ipcache yet, and a replica may be about to land
#    on the node we would read. Neither is a verdict, so it fails closed.
# ---------------------------------------------------------------------------
if ! oauth2_json="$(kc -n "${oauth2_namespace}" get pods -l "${oauth2_selector}" -o json)"; then
  inconclusive 'could not list the oauth2-proxy pods'
fi

if ! oauth2_lines="$(jq -r "${replica_jq_defs}"'
    replicas
    | if length == 0 then "ERR none"
      elif any(.[]; .ok | not) then "ERR unsettled"
      elif any(.[]; settled | not) then "ERR incomplete"
      else (.[] | ("NODE " + .node), (.ips[] | "IP " + .))
      end
  ' <<<"${oauth2_json}")"; then
  inconclusive 'the oauth2-proxy pod list did not parse'
fi

case "${oauth2_lines}" in
  'ERR none') inconclusive 'no oauth2-proxy pods were found' ;;
  'ERR unsettled') inconclusive 'an oauth2-proxy replica is not Running and Ready (rollout in flight)' ;;
  'ERR incomplete') inconclusive 'an oauth2-proxy replica has no UID, node or pod IP yet' ;;
esac

if ! replica_fingerprint_before="$(jq -r "${replica_jq_defs} replicas | fingerprint" <<<"${oauth2_json}")"; then
  inconclusive 'the oauth2-proxy pod list did not parse'
fi

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

if [[ "${replica_count}" -eq 0 || -z "${pod_ips}" || -z "${replica_fingerprint_before}" ]]; then
  inconclusive 'no oauth2-proxy pod IPs were resolved'
fi

distinct_replica_nodes="$(printf '%s' "${replica_nodes}" | sort -u | grep -c . || true)"
printf 'oauth2-proxy replicas: %s (on %s node(s))\n' "${replica_count}" "${distinct_replica_nodes}"

# ---------------------------------------------------------------------------
# 2. Nodes and their addresses. Both address types are host addresses; which one a node uses as
#    its Cilium node IP depends on the provider, so both count. Every node is kept, indexed by
#    its position in name order, so coverage can be checked per node without printing a name.
# ---------------------------------------------------------------------------
if ! nodes_json="$(kc get nodes -o json)"; then
  inconclusive 'could not list the nodes'
fi
if ! node_address_lines="$(jq -r '
    [.items[]? | {
      name: (.metadata.name // ""),
      uid: (.metadata.uid // ""),
      addrs: ([.status.addresses[]? | select(.type == "InternalIP" or .type == "ExternalIP") | .address] | unique)
    }]
    | if length == 0 then "ERR none"
      elif any(.[]; .name == "" or .uid == "") then "ERR identity"
      elif any(.[]; (.addrs | length) == 0) then "ERR incomplete"
      else sort_by(.name) | .[] | .name as $n | .addrs[] | $n + " " + .
      end
  ' <<<"${nodes_json}")"; then
  inconclusive 'the node list did not parse'
fi

case "${node_address_lines}" in
  'ERR none') inconclusive 'no nodes were found' ;;
  'ERR identity') inconclusive 'a node reported no name or UID' ;;
  'ERR incomplete') inconclusive 'a node reported no InternalIP or ExternalIP address' ;;
esac

if ! node_fingerprint_before="$(jq -r "${node_fingerprint_filter}" <<<"${nodes_json}")"; then
  inconclusive 'the node list did not parse'
fi

node_names=''
node_count=0
node_map=''
last_node=''
while IFS=' ' read -r node address; do
  [[ -z "${node}" ]] && continue
  is_k8s_name "${node}" || inconclusive 'a node reported a malformed name'
  is_address "${address}" || inconclusive 'a node reported a malformed address'
  if [[ "${node}" != "${last_node}" ]]; then
    node_count=$((node_count + 1))
    node_names="${node_names}${node}"$'\n'
    last_node="${node}"
  fi
  case " ${node_map}" in
    *" ${address}="*) inconclusive 'two nodes reported the same address' ;;
  esac
  node_map="${node_map}${address}=${node_count} "
done <<<"${node_address_lines}"
if [[ "${node_count}" -eq 0 || -z "${node_map}" ]]; then
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
      | {name: .metadata.name, node: (.spec.nodeName // ""),
         hash: (.metadata.labels["controller-revision-hash"] // "")}
      | select(.node != "")]
    | sort_by(.node, .name) | .[] | .node + " " + .name + " " + .hash
  ' <<<"${cilium_json}")"; then
  inconclusive 'the Cilium agent pod list did not parse'
fi

agent_pod=''
agent_node=''
agent_hash=''
while IFS=' ' read -r node name hash; do
  [[ -z "${node}" ]] && continue
  if printf '%s' "${replica_nodes}" | grep -qxF -- "${node}"; then
    continue
  fi
  if is_k8s_name "${node}" && is_k8s_name "${name}"; then
    agent_pod="${name}"
    agent_node="${node}"
    agent_hash="${hash}"
    break
  fi
done <<<"${agent_lines}"

if [[ -z "${agent_pod}" ]]; then
  inconclusive 'no Ready Cilium agent runs on a node hosting neither oauth2-proxy replica'
fi

# The probed node's own addresses are `host` in its ipcache, not remote-node, so it is the one
# node not expected to be covered. It must itself be a listed node, or coverage cannot be judged.
agent_node_index="$(printf '%s' "${node_names}" | grep -nxF -- "${agent_node}" | cut -d: -f1 || true)"
if [[ -z "${agent_node_index}" ]]; then
  inconclusive 'the selected Cilium agent runs on a node missing from the node list'
fi
remote_node_count=$((node_count - 1))
if [[ "${remote_node_count}" -lt 1 ]]; then
  inconclusive 'there is no remote node whose proxy source could be observed'
fi
printf 'Cilium agent: selected a Ready agent on a node hosting neither replica\n'
printf 'remote nodes expected in the ipcache: %s\n' "${remote_node_count}"

# ---------------------------------------------------------------------------
# 4. The agent set must be fully rolled out, and the selected agent on the current revision.
#    Checked BEFORE the exec, so a partial rollout reads nothing at all.
# ---------------------------------------------------------------------------
if ! daemonset_json="$(kc -n "${cilium_namespace}" get daemonset "${cilium_daemonset}" -o json)"; then
  inconclusive 'could not read the Cilium DaemonSet'
fi
if ! daemonset_state="$(jq -r '
    (.metadata.generation // null) as $generation
    | (.status.desiredNumberScheduled // null) as $desired
    | if (.metadata.uid // "") == "" then "ERR uid"
      elif $generation == null or .status.observedGeneration != $generation then "ERR generation"
      elif ($desired | type) != "number" or $desired < 1 then "ERR desired"
      elif .status.updatedNumberScheduled != $desired then "ERR updated"
      elif .status.numberAvailable != $desired then "ERR available"
      else "OK " + .metadata.uid
      end
  ' <<<"${daemonset_json}")"; then
  inconclusive 'the Cilium DaemonSet did not parse'
fi

case "${daemonset_state}" in
  'ERR uid') inconclusive 'the Cilium DaemonSet reported no UID' ;;
  'ERR generation') inconclusive 'the Cilium DaemonSet has not observed its current generation' ;;
  'ERR desired') inconclusive 'the Cilium DaemonSet schedules no pods' ;;
  'ERR updated') inconclusive 'the Cilium DaemonSet is partially rolled out (not every node runs the updated agent)' ;;
  'ERR available') inconclusive 'the Cilium DaemonSet is partially rolled out (not every agent is available)' ;;
  'OK '*) ;;
  *) inconclusive 'the Cilium DaemonSet produced an unexpected state' ;;
esac
daemonset_uid="${daemonset_state#OK }"

# The current update revision is the ControllerRevision owned (as controller) by THIS DaemonSet
# with the highest revision number. Revisions of other DaemonSets in the namespace are ignored.
if ! revisions_json="$(kc -n "${cilium_namespace}" get controllerrevisions -o json)"; then
  inconclusive 'could not list the ControllerRevisions'
fi
if ! current_hash="$(jq -r --arg uid "${daemonset_uid}" '
    [.items[]?
      | select(any(.metadata.ownerReferences[]?;
          .uid == $uid and .kind == "DaemonSet" and .controller == true))]
    | if length == 0 then "ERR none"
      elif any(.[]; (.revision | type) != "number") then "ERR revision"
      else (map(.revision) | max) as $top
        | [.[] | select(.revision == $top)]
        | if length != 1 then "ERR ambiguous"
          else (.[0].metadata.labels["controller-revision-hash"] // "")
            | if . == "" then "ERR hash" else . end
          end
      end
  ' <<<"${revisions_json}")"; then
  inconclusive 'the ControllerRevisions did not parse'
fi

if ! is_revision_hash "${current_hash}"; then
  inconclusive 'the Cilium DaemonSet current revision could not be resolved'
fi
if [[ "${agent_hash}" != "${current_hash}" ]]; then
  inconclusive 'the selected Cilium agent is not on the DaemonSet current revision'
fi
printf 'Cilium DaemonSet: fully rolled out; selected agent is on the current revision\n'

# ---------------------------------------------------------------------------
# 5. Node encryption. Read here and applied only to the all-zero branch below. A failed read does
#    not stop the diagnostic: it makes the setting UNDETERMINED, which can never conclude the fault.
# ---------------------------------------------------------------------------
node_encryption='undetermined'
if cilium_config_json="$(kc -n "${cilium_namespace}" get configmap "${cilium_configmap}" -o json)" &&
  encrypt_node="$(jq -r '
      if (.data | type) != "object" then "ERR data"
      else (.data["encrypt-node"] // "ABSENT")
      end
    ' <<<"${cilium_config_json}")"; then
  case "${encrypt_node}" in
    true) node_encryption='on' ;;
    false | ABSENT) node_encryption='off' ;;
    *) node_encryption='undetermined' ;;
  esac
fi
printf 'Cilium node encryption: %s\n' "${node_encryption}"

# ---------------------------------------------------------------------------
# 6. The one exec. `cilium-dbg bpf ipcache list` dumps a BPF map; it writes nothing.
# ---------------------------------------------------------------------------
if ! ipcache="$(kc -n "${cilium_namespace}" exec "${agent_pod}" -c "${cilium_container}" -- \
  cilium-dbg bpf ipcache list)"; then
  inconclusive 'the ipcache read in the Cilium agent failed'
fi
if [[ -z "${ipcache//[[:space:]]/}" ]]; then
  inconclusive 'the ipcache read returned nothing'
fi

# ---------------------------------------------------------------------------
# 7. The oauth2-proxy pods after the read. EVERY selected pod — not only the Ready ones — is held to
#    the same settled-state test as the first read, none may sit on the probed node in any state,
#    and the set must match the first read exactly. A surge or replacement pod that is still Pending
#    or unready would otherwise be invisible here.
# ---------------------------------------------------------------------------
if ! oauth2_after_json="$(kc -n "${oauth2_namespace}" get pods -l "${oauth2_selector}" -o json)"; then
  inconclusive 'could not re-list the oauth2-proxy pods after the read'
fi
if ! replica_state_after="$(jq -r --arg agent_node "${agent_node}" "${replica_jq_defs}"'
    (replicas)
    | if any(.[]; .node == $agent_node) then "LOCAL"
      elif any(.[]; settled | not) then "UNSETTLED"
      else "OK " + fingerprint
      end
  ' <<<"${oauth2_after_json}")"; then
  inconclusive 'the oauth2-proxy pod re-list did not parse'
fi
case "${replica_state_after}" in
  LOCAL) inconclusive 'an oauth2-proxy pod is on the selected Cilium node after the read' ;;
  UNSETTLED) inconclusive 'an oauth2-proxy pod is not settled after the read (Pending, unready, terminating, or missing a UID, node or pod IP)' ;;
  'OK '*) ;;
  *) inconclusive 'the oauth2-proxy pod re-list produced an unexpected state' ;;
esac
if [[ "${replica_state_after#OK }" != "${replica_fingerprint_before}" ]]; then
  inconclusive 'the oauth2-proxy replicas changed during the read'
fi
printf 'oauth2-proxy replicas: unchanged across the read\n'

# ---------------------------------------------------------------------------
# 8. The topology must be unchanged across the read: same node names, UIDs and addresses.
# ---------------------------------------------------------------------------
if ! nodes_after_json="$(kc get nodes -o json)"; then
  inconclusive 'could not re-list the nodes after the read'
fi
if ! node_fingerprint_after="$(jq -r "${node_fingerprint_filter}" <<<"${nodes_after_json}")"; then
  inconclusive 'the node re-list did not parse'
fi
if [[ "${node_fingerprint_after}" != "${node_fingerprint_before}" ]]; then
  inconclusive 'the node set or a node address changed during the read'
fi
printf 'node topology: unchanged across the read\n'

# Parse. Addresses are matched EXACTLY after the prefix length is stripped, never as substrings:
# a pod at 10.244.22.23 must not be satisfied by an entry for 10.244.22.235. Values reach awk
# through ENVIRON rather than -v, which would interpret backslash escapes.
records="$(POD_IPS="${pod_ips}" NODE_MAP="${node_map}" awk '
  BEGIN {
    n = split(ENVIRON["POD_IPS"], pods, " ")
    for (i = 1; i <= n; i++) pod_index[pods[i]] = i
    m = split(ENVIRON["NODE_MAP"], pairs, " ")
    for (i = 1; i <= m; i++) {
      eq = index(pairs[i], "=")
      node_of[substr(pairs[i], 1, eq - 1)] = substr(pairs[i], eq + 1)
    }
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
      if (address in node_of) print "NODE", node_of[address], key
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

# Source side: identity=6 entries for REMOTE node addresses decide; the probed node's own
# addresses never vote, and other identity=6 entries are only counted.
node_zero=0
node_nonzero=0
while IFS=' ' read -r kind index key; do
  [[ "${kind}" == 'NODE' ]] || continue
  [[ "${index}" == "${agent_node_index}" ]] && continue
  if ! [[ "${key}" =~ ^[0-9]+$ ]]; then
    inconclusive 'an identity=6 node-address entry has no parseable encryptkey'
  fi
  if [[ "${key}" -eq 0 ]]; then
    node_zero=$((node_zero + 1))
  else
    node_nonzero=$((node_nonzero + 1))
  fi
done <<<"${records}"

# Coverage: every remote node needs at least one identity=6 entry for one of its own addresses.
covered=0
first_uncovered=''
k=1
while [[ "${k}" -le "${node_count}" ]]; do
  if [[ "${k}" != "${agent_node_index}" ]]; then
    if awk -v idx="${k}" '$1 == "NODE" && $2 == idx { found = 1 } END { exit !found }' <<<"${records}"; then
      covered=$((covered + 1))
    elif [[ -z "${first_uncovered}" ]]; then
      first_uncovered="${k}"
    fi
  fi
  k=$((k + 1))
done
printf 'remote nodes covered: %s of %s\n' "${covered}" "${remote_node_count}"

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

if [[ -n "${first_uncovered}" ]]; then
  inconclusive "remote node #${first_uncovered} has no identity=6 entry for any of its addresses ($((remote_node_count - covered)) of ${remote_node_count} uncovered)"
fi
if [[ $((node_zero + node_nonzero)) -eq 0 ]]; then
  inconclusive 'no identity=6 entry for a node address was found, so the proxy source is unobserved'
fi
if [[ "${node_zero}" -gt 0 && "${node_nonzero}" -gt 0 ]]; then
  inconclusive 'identity=6 node-address entries disagree on encryptkey'
fi

# All-zero node-address keys can show the fault ONLY when node encryption is confirmed on; with it
# off they are the expected state, and with it undetermined nothing distinguishes the two.
if [[ "${node_zero}" -gt 0 ]]; then
  if [[ "${node_encryption}" == 'on' ]]; then
    conclude 'FAULT-PERSISTS' 'Cilium node encryption is on, yet the host-sourced identity=6 proxy source is still plaintext (encryptkey=0) toward oauth2-proxy pods behind WireGuard'
  fi
  if [[ "${node_encryption}" == 'off' ]]; then
    inconclusive 'identity=6 node-address entries are plaintext (encryptkey=0), but Cilium node encryption is off, so that is the expected state and cannot show the fault; run the flagged ExternalAuth datapath test (#2284, option 2)'
  fi
  inconclusive 'identity=6 node-address entries are plaintext (encryptkey=0), but Cilium node encryption could not be determined, so that cannot show the fault; run the flagged ExternalAuth datapath test (#2284, option 2)'
fi
conclude 'PLAUSIBLY-FIXED' 'oauth2-proxy pods are behind WireGuard and the host-sourced identity=6 proxy source carries a non-zero encryptkey'
