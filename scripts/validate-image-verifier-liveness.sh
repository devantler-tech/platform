#!/usr/bin/env bash

# Assert that node-level image verification can enforce AT ALL.
#
# `talos/cluster/verify-first-party-images.yaml` declares ImageVerificationConfig
# rules. Talos v1.13 verifies images ITSELF: no external verifier binary is
# involved, and containerd's `io.containerd.image-verifier.v1.bindir` plugin is
# not the mechanism on this platform. Two node-local controllers own it:
#
#   * `security.ImageVerificationConfigController` materialises each declared
#     rule into an `ImageVerificationRules.security.talos.dev` resource, and
#   * `security.TUFTrustedRootController` maintains the Sigstore TUF trust root
#     (`TUFTrustedRoots.security.talos.dev`) that keyless verification needs.
#
# A rule that never reached `phase: running`, or a node with no trust root, is a
# node where the declared rules decide nothing — while the repository, the
# rendered manifests and CI all still read as compliant. That is the silent
# enforcement gap this check exists to break (devantler-tech/platform#2856).
#
# It reads LIVE NODE STATE on purpose. A variant that read the repository would
# re-create the exact blind spot it exists to close.
#
# WHAT THIS DOES NOT PROVE. That the controllers hold running rules and trust
# material is necessary for enforcement, not sufficient: it does not establish
# that a matched rule has ever produced a verification DECISION, because a
# cached image is never re-pulled. That behavioural proof — a known-unsigned
# image demonstrably refused at pull — is devantler-tech/platform#3336's job.
# This check's value is that it needs no rollout and fails loudly when the
# mechanism itself is not running.
#
# HISTORY, because it changes how a reader should read a green result: until
# 2026-08-24 this script asserted a containerd `bin_dir`. No `bin_dir` is
# configured anywhere on this cluster, so it reported a permanent, confident
# FAIL on nodes that were verifying correctly (#3108). Trust its verdict only
# from that date forward.
#
# SECURITY: this check reads Talos resources only. It deliberately does NOT read
# `/etc/cri/conf.d/cri.toml`, which carries registry credentials on this
# cluster — the previous bin_dir implementation had to, and this script's output
# goes to CI logs. Keep it that way: no node file is read here, so no credential
# can reach an error message.

set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: validate-image-verifier-liveness.sh [--nodes <ip>[,<ip>...]]

Asserts, for every node, that Talos' own image verification is live: at least one
ImageVerificationRules resource in phase "running", and a TUFTrustedRoots trust
root in phase "running" to verify against.

Nodes are discovered from the cluster when --nodes/TALOS_NODES is not given, so
autoscaled nodes are covered without anyone maintaining a list.

Environment overrides: TALOSCTL, KUBECTL, KUBECTL_CONTEXT, TALOS_NODES
Exit: 0 every node can enforce; 1 at least one cannot; 2 usage/infrastructure error.
USAGE
  exit 2
}

readonly talosctl_bin="${TALOSCTL:-talosctl}"
readonly kubectl_bin="${KUBECTL:-kubectl}"

nodes_arg="${TALOS_NODES:-}"

# Whether an explicit fleet was REQUESTED is tracked separately from whether the
# list is non-empty. `--nodes "$TALOS_NODES"` with the variable unset expands to
# an empty string: the option WAS supplied and named nothing. Falling back to
# cluster discovery there checks a different fleet than the caller asked for and
# still exits 0 — and explicit addresses are usually explicit precisely because
# the current kube context is not that fleet.
#
# `${VAR+set}` distinguishes set-but-empty from unset, so an exported
# `TALOS_NODES=` is caught too; that is how CI passes the list.
nodes_requested=0
[[ -n "${TALOS_NODES+set}" ]] && nodes_requested=1
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --nodes)
      [[ "$#" -ge 2 ]] || usage
      nodes_arg="$2"
      nodes_requested=1
      shift 2
      ;;
    -h | --help) usage ;;
    *) usage ;;
  esac
done

fail_infra() {
  printf 'ERROR: %s\n' "$1" >&2
  exit 2
}

# KUBECTL must name a bare binary, never a command with flags: it is invoked
# quoted, so "kubectl --context x" would be looked up as a single executable
# name and fail. Pin the context with KUBECTL_CONTEXT instead — the deploy
# composite pins `--context admin@prod` for the same reason, because the
# restored kubeconfig's current-context is not guaranteed to be prod.
discover_nodes() {
  local json bad_nodes
  local -a context_args=()
  [[ -n "${KUBECTL_CONTEXT:-}" ]] && context_args=(--context "${KUBECTL_CONTEXT}")
  json="$("${kubectl_bin}" "${context_args[@]+"${context_args[@]}"}" get nodes -o json 2>/dev/null)" ||
    fail_infra 'could not list cluster nodes (kubectl get nodes failed)'

  # Every node must map to EXACTLY ONE InternalIP, and those addresses must be
  # unique across the fleet. Both halves are fail-opens the flattening hides:
  #
  #   * a node whose addresses hold only a Hostname or an ExternalIP matches
  #     nothing and the select SUCCEEDS, so that node silently leaves the fleet;
  #   * two nodes publishing the SAME InternalIP — a malformed or stale cloud
  #     registration — emit that address twice, so both passes inspect the same
  #     machine while the other node is never looked at.
  #
  # Either way the remaining nodes report OK and the run prints a green verdict
  # for a fleet it never fully enumerated: the same fail-open this script exists
  # to detect, one layer earlier. `scripts/refresh-flux-ghcr-auth.sh`'s
  # `validate_talos_node_inventory` refuses the same two shapes before it mutates
  # anything, for the same reason.
  #
  # Checked as its own pass so the offending nodes can be NAMED. Deriving it from
  # a jq error instead would either lose the names or push kubectl's output into
  # an error message.
  bad_nodes="$(printf '%s' "${json}" |
    jq -r '
      [.items[] | {
        name: (.metadata.name // "<unnamed node>"),
        ips: [(.status.addresses // [])[]
              | select(.type == "InternalIP" and ((.address // "") | tostring) != "")
              | .address]
      }] as $nodes
      | ($nodes | map(select(.ips | length != 1))
          | map("\(.name) (has \(.ips | length) InternalIP addresses, expected 1)")),
        ($nodes | map(select(.ips | length == 1))
          | group_by(.ips[0]) | map(select(length > 1))
          | map("\(map(.name) | join(" and ")) share InternalIP \(.[0].ips[0])"))
      | .[]
    ' 2>/dev/null)" ||
    fail_infra 'could not parse node addresses from kubectl output'
  if [[ -n "${bad_nodes}" ]]; then
    fail_infra "unusable node inventory — refusing to report a fleet that was only partly enumerated: $(printf '%s' "${bad_nodes}" | tr '\n' ';' | sed 's/;$//')"
  fi

  printf '%s' "${json}" |
    jq -r '
      .items[]
      | (.status.addresses // [])[]
      | select(.type == "InternalIP" and ((.address // "") | tostring) != "")
      | .address
    ' 2>/dev/null ||
    fail_infra 'could not parse node addresses from kubectl output'
}

node_reachable() {
  "${talosctl_bin}" -n "$1" ls / >/dev/null 2>&1
}

# Built with a read loop rather than `mapfile` so the script and its tests run
# on bash 3.2 (macOS) as well as CI's bash 5. A check nobody can run locally is
# a check nobody verifies before shipping.
nodes=()
if [[ "${nodes_requested}" -eq 1 && -z "${nodes_arg}" ]]; then
  printf 'ERROR: --nodes/TALOS_NODES was supplied but names no node — refusing to fall back to cluster discovery, which would check a different fleet than was asked for\n' >&2
  usage
fi

if [[ -n "${nodes_arg}" ]]; then
  # A malformed list ('a,,b', a leading or trailing comma, a shell variable that
  # expanded to nothing) is refused rather than quietly shortened: checking FEWER
  # nodes than were asked for and still exiting 0 is a checker reporting a clean
  # fleet it never looked at — the same silence this script exists to break.
  #
  # Validate the RAW STRING, before splitting. `read -a` discards a trailing
  # empty field, so 'worker-1,' splits to exactly one element and a
  # post-split scan for empty entries cannot see the malformation at all.
  if [[ "${nodes_arg}" == ,* || "${nodes_arg}" == *, || "${nodes_arg}" == *,,* ]]; then
    printf 'ERROR: --nodes/TALOS_NODES has an empty entry (leading, trailing or doubled comma): %s\n' \
      "${nodes_arg}" >&2
    usage
  fi
  IFS=',' read -r -a nodes <<<"${nodes_arg}"
else
  # Discovery runs in a command substitution so its exit status reaches this
  # shell. A process substitution would NOT: `fail_infra` inside it exits only
  # that subshell, the loop keeps whatever partial output jq already emitted,
  # and if those nodes happen to be healthy the script exits 0 having silently
  # dropped the rest of the fleet. jq emitting a valid address and then failing
  # on a malformed one is exactly that case.
  discovered_nodes="$(discover_nodes)" ||
    fail_infra 'node discovery failed — refusing to report a fleet that was only partly enumerated'
  while IFS= read -r discovered; do
    [[ -n "${discovered}" ]] || continue
    nodes+=("${discovered}")
  done <<<"${discovered_nodes}"
fi

[[ "${#nodes[@]}" -gt 0 ]] || fail_infra 'no nodes to check'

readonly rules_type='imageverificationrules.security.talos.dev'
readonly trustroot_type='tuftrustedroots.security.talos.dev'
readonly rules_owner='security.ImageVerificationConfigController'
readonly trustroot_owner='security.TUFTrustedRootController'

# Emits ONE resource type's rows on one node as a compact JSON array, or returns
# non-zero. `talosctl get -o json` emits a STREAM of objects (not an array), and
# emits nothing at all when the type has no instances, so `jq -s` is what turns
# both shapes into something countable.
#
# An empty result and a failed query are NOT the same thing, and this returns
# them differently on purpose: "the node holds no rules" is a verdict, "the
# query failed" is an infrastructure fault. Collapsing them would let an
# expired talosconfig or a partitioned node read as "verification is not
# configured here" — a confident wrong answer, which is the failure mode this
# script was rewritten to remove rather than reintroduce one level down.
get_resource_json() {
  local node="$1" type="$2" out status=0
  out="$("${talosctl_bin}" -n "${node}" get "${type}" -o json 2>/dev/null)" || status=$?
  [[ "${status}" -eq 0 ]] || return "${status}"
  # `jq -s` on empty input yields `[]`, which is the "none present" verdict.
  printf '%s' "${out}" | jq -s -c '[.[] | {id: (.metadata.id|tostring), phase: (.metadata.phase // ""), owner: (.metadata.owner // "")}]' 2>/dev/null
}

# Verdict for ONE node. Returns 0 when that node's verification mechanism is
# live, 1 when it is not.
check_node() {
  local node="$1" rules trustroots status=0
  local total_rules running_rules total_roots running_roots

  rules="$(get_resource_json "${node}" "${rules_type}")" || status=$?
  if [[ "${status}" -ne 0 || -z "${rules}" ]]; then
    # "The node is gone" and "the node answered but this query failed" deserve
    # different diagnoses, and neither is a verdict.
    node_reachable "${node}" ||
      fail_infra "cannot reach node ${node} (talosctl ls / failed) — refusing to report a fleet that was not checked"
    fail_infra "could not read ${rules_type} on ${node} (the node is reachable but the query failed) — refusing to report a node whose verification state was never inspected"
  fi

  total_rules="$(printf '%s' "${rules}" | jq -r 'length')"
  running_rules="$(printf '%s' "${rules}" | jq -r --arg owner "${rules_owner}" \
    '[.[] | select(.phase == "running" and .owner == $owner)] | length')"

  if [[ "${total_rules}" -eq 0 ]]; then
    printf 'FAIL %s: no %s resources — the declared ImageVerificationConfig rules were never materialised on this node, so nothing constrains a pull\n' \
      "${node}" "${rules_type}"
    return 1
  fi

  if [[ "${running_rules}" -eq 0 ]]; then
    printf 'FAIL %s: %s rule(s) present but none owned by %s in phase "running" — the rules exist as resources and are not being enforced\n' \
      "${node}" "${total_rules}" "${rules_owner}"
    return 1
  fi

  status=0
  trustroots="$(get_resource_json "${node}" "${trustroot_type}")" || status=$?
  if [[ "${status}" -ne 0 || -z "${trustroots}" ]]; then
    node_reachable "${node}" ||
      fail_infra "cannot reach node ${node} (talosctl ls / failed) — refusing to report a fleet that was not checked"
    fail_infra "could not read ${trustroot_type} on ${node} (the node is reachable but the query failed) — refusing to report a node whose verification state was never inspected"
  fi

  total_roots="$(printf '%s' "${trustroots}" | jq -r 'length')"
  running_roots="$(printf '%s' "${trustroots}" | jq -r --arg owner "${trustroot_owner}" \
    '[.[] | select(.phase == "running" and .owner == $owner)] | length')"

  # The trust root is checked SECOND and separately because its absence is a
  # different failure from a missing rule: the rules can be perfectly live and
  # still decide nothing if there is no Sigstore trust material to verify a
  # keyless signature against.
  if [[ "${total_roots}" -eq 0 ]]; then
    printf 'FAIL %s: %s running rule(s) but no %s — keyless verification has no trust material, so a signature cannot be checked\n' \
      "${node}" "${running_rules}" "${trustroot_type}"
    return 1
  fi

  if [[ "${running_roots}" -eq 0 ]]; then
    printf 'FAIL %s: trust root present but not owned by %s in phase "running" — keyless verification has no usable trust material\n' \
      "${node}" "${trustroot_owner}"
    return 1
  fi

  printf 'OK   %s: %s rule(s) in phase running, %s trust root(s) running\n' \
    "${node}" "${running_rules}" "${running_roots}"
  return 0
}

failures=0
for node in "${nodes[@]}"; do
  check_node "${node}" || failures=$((failures + 1))
done

if [[ "${failures}" -gt 0 ]]; then
  printf '\n%s of %s node(s) cannot enforce image verification.\n' "${failures}" "${#nodes[@]}" >&2
  printf 'The ImageVerificationConfig rules in talos/cluster/verify-first-party-images.yaml are declared, but Talos is not enforcing them on those nodes.\n' >&2
  printf 'See devantler-tech/platform#2856 (finding) and #3336 (proving a refusal at pull).\n' >&2
  exit 1
fi

printf '\nAll %s node(s) can enforce image verification.\n' "${#nodes[@]}"
