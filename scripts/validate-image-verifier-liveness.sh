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

Asserts, for every node, that Talos' own image verification is live: every
declared ImageVerificationRules pattern is materialised in order and in phase
"running", and a TUFTrustedRoots trust root is in phase "running" to verify
against.

Nodes are discovered from the cluster when --nodes/TALOS_NODES is not given, so
autoscaled nodes are covered without anyone maintaining a list. A discovered
fleet is read again after the sweep and the verdict is only reported once two
consecutive readings agree, because a serial sweep is not instantaneous and a
node that joins or is replaced midway would otherwise go uninspected under a
green summary. Nodes are identified by UID and address together, so a
replacement reusing a departed node's address is not mistaken for it. An
explicitly pinned fleet is the caller's claim about what to check and is not
re-read.

Environment overrides: TALOSCTL, KUBECTL, KUBECTL_CONTEXT, TALOS_NODES
Exit: 0 every node can enforce; 1 at least one cannot; 2 usage/infrastructure error.
USAGE
  exit 2
}

readonly talosctl_bin="${TALOSCTL:-talosctl}"
readonly kubectl_bin="${KUBECTL:-kubectl}"
root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly root_dir
readonly policy_manifest="${root_dir}/talos/cluster/verify-first-party-images.yaml"

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

# Read the ordered `rules[].image` policy without adding a yq dependency to the
# production workflow. This manifest intentionally uses one plain or quoted
# scalar per `- image:` row; if that shape disappears, an empty result fails
# closed below instead of inventing an expected policy.
declared_patterns_text="$(awk '
  /^[[:space:]]*-[[:space:]]+image:[[:space:]]*/ {
    line = $0
    sub(/^[[:space:]]*-[[:space:]]+image:[[:space:]]*/, "", line)
    sub(/[[:space:]]+#.*$/, "", line)
    gsub(/^[[:space:]]+|[[:space:]]+$/, "", line)
    if ((substr(line, 1, 1) == "\"" && substr(line, length(line), 1) == "\"") ||
        (substr(line, 1, 1) == sprintf("%c", 39) && substr(line, length(line), 1) == sprintf("%c", 39))) {
      line = substr(line, 2, length(line) - 2)
    }
    if (line != "") print line
  }
' "${policy_manifest}")" || fail_infra "could not read declared image-verification rules from ${policy_manifest}"

declared_rule_patterns=()
while IFS= read -r pattern; do
  [[ -n "${pattern}" ]] || continue
  declared_rule_patterns+=("${pattern}")
done <<<"${declared_patterns_text}"
[[ "${#declared_rule_patterns[@]}" -gt 0 ]] ||
  fail_infra "no image-verification rules found in ${policy_manifest}"
readonly declared_rule_count="${#declared_rule_patterns[@]}"
declared_patterns_json="$(printf '%s\n' "${declared_rule_patterns[@]}" |
  jq -R -s -c 'split("\n") | map(select(length > 0))')" ||
  fail_infra 'could not encode the declared image-verification rule set'
readonly declared_patterns_json

# KUBECTL must name a bare binary, never a command with flags: it is invoked
# quoted, so "kubectl --context x" would be looked up as a single executable
# name and fail. Pin the context with KUBECTL_CONTEXT instead — the deploy
# composite pins `--context admin@prod` for the same reason, because the
# restored kubeconfig's current-context is not guaranteed to be prod.
# Emits one `<uid>\t<InternalIP>` row per node, sorted, or fails closed.
#
# Identity is the UID *and* the address, never the address alone. Cluster
# Autoscaler replaces a worker by deleting the Node object and creating a new
# one, and the replacement can reuse the departed node's InternalIP. An
# address-only identity reads that replacement as the original — so a node this
# script never inspected is reported healthy under a familiar address. The UID
# is the only field that distinguishes them, and the API server always sets it.
discover_node_identities() {
  local json bad_nodes
  local -a context_args=()
  [[ -n "${KUBECTL_CONTEXT:-}" ]] && context_args=(--context "${KUBECTL_CONTEXT}")
  json="$("${kubectl_bin}" "${context_args[@]+"${context_args[@]}"}" get nodes -o json 2>/dev/null)" ||
    fail_infra 'could not list cluster nodes (kubectl get nodes failed)'

  # Every node must map to EXACTLY ONE InternalIP and carry a non-empty UID, and
  # both must be unique across the fleet. Each is a fail-open the flattening
  # hides:
  #
  #   * a node with NO InternalIP — it vanishes from the list entirely;
  #   * a node with SEVERAL — it is checked more than once under different
  #     addresses while another is missed;
  #   * two nodes publishing the SAME InternalIP — a malformed or stale cloud
  #     inventory, where one silently masks the other;
  #   * a node with NO UID, or two sharing one — identity collapses back to the
  #     address, which is precisely the confusion this pass exists to refuse.
  #
  # Either way the remaining nodes report OK and the run prints a green verdict
  # for a fleet it only partly enumerated. `validate_talos_node_inventory`
  # refuses the same shapes before it mutates anything, for the same reason.
  #
  # Checked as its own pass so the offending nodes can be NAMED. Deriving it from
  # a jq error instead would either lose the names or push kubectl's output into
  # an error message.
  bad_nodes="$(printf '%s' "${json}" |
    jq -r '
      [.items[] | {
        name: (.metadata.name // "<unnamed node>"),
        uid: ((.metadata.uid // "") | tostring),
        ips: [(.status.addresses // [])[]
              | select(.type == "InternalIP" and ((.address // "") | tostring) != "")
              | .address]
      }] as $nodes
      | ($nodes | map(select(.ips | length != 1))
          | map("\(.name) (has \(.ips | length) InternalIP addresses, expected 1)")),
        ($nodes | map(select(.uid == ""))
          | map("\(.name) (has no metadata.uid, so it cannot be told apart from a replacement reusing its address)")),
        ($nodes | map(select(.ips | length == 1))
          | group_by(.ips[0]) | map(select(length > 1))
          | map("\(map(.name) | join(" and ")) share InternalIP \(.[0].ips[0])")),
        ($nodes | map(select(.uid != ""))
          | group_by(.uid) | map(select(length > 1))
          | map("\(map(.name) | join(" and ")) share UID \(.[0].uid)"))
      | .[]
    ' 2>/dev/null)" ||
    fail_infra 'could not parse the node inventory from kubectl output'
  if [[ -n "${bad_nodes}" ]]; then
    fail_infra "unusable node inventory — refusing to report a fleet that was only partly enumerated: $(printf '%s' "${bad_nodes}" | tr '\n' ';' | sed 's/;$//')"
  fi

  # Sorted so two readings of an unchanged fleet compare equal regardless of the
  # order the API server happened to return them in.
  printf '%s' "${json}" |
    jq -r '
      .items[]
      | ((.metadata.uid // "") | tostring) as $uid
      | (.status.addresses // [])[]
      | select(.type == "InternalIP" and ((.address // "") | tostring) != "")
      | "\($uid)\t\(.address)"
    ' 2>/dev/null |
    LC_ALL=C sort ||
    fail_infra 'could not parse the node inventory from kubectl output'
}

node_reachable() {
  "${talosctl_bin}" -n "$1" ls / >/dev/null 2>&1
}

# Built with a read loop rather than `mapfile` so the script and its tests run
# on bash 3.2 (macOS) as well as CI's bash 5. A check nobody can run locally is
# a check nobody verifies before shipping.
nodes=()
inventory_signature=''
if [[ "${nodes_requested}" -eq 1 && -z "${nodes_arg}" ]]; then
  printf 'ERROR: --nodes/TALOS_NODES was supplied but names no node — refusing to fall back to cluster discovery, which would check a different fleet than was asked for\n' >&2
  usage
fi

# Fills `nodes` and `inventory_signature` from the live cluster. Called once per
# convergence attempt, so the fleet under test is re-read rather than inherited.
load_discovered_nodes() {
  local discovered_identities identity
  # Discovery runs in a command substitution so its exit status reaches this
  # shell. A process substitution would NOT: `fail_infra` inside it exits only
  # that subshell, the loop keeps whatever partial output jq already emitted,
  # and if those nodes happen to be healthy the script exits 0 having silently
  # dropped the rest of the fleet. jq emitting a valid address and then failing
  # on a malformed one is exactly that case.
  discovered_identities="$(discover_node_identities)" ||
    fail_infra 'node discovery failed — refusing to report a fleet that was only partly enumerated'
  nodes=()
  inventory_signature="${discovered_identities}"
  while IFS= read -r identity; do
    [[ -n "${identity}" ]] || continue
    # `<uid>\t<address>` — the address is what talosctl is pointed at; the UID
    # only ever participates in the identity comparison.
    nodes+=("${identity#*$'\t'}")
  done <<<"${discovered_identities}"
}

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
  load_discovered_nodes
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
  printf '%s' "${out}" | jq -s -c '[.[] | {
    id: (.metadata.id // ""),
    phase: (.metadata.phase // ""),
    owner: (.metadata.owner // ""),
    imagePattern: (.spec.imagePattern // "")
  }]' 2>/dev/null
}

# Verdict for ONE node. Returns 0 when that node's verification mechanism is
# live, 1 when it is not.
check_node() {
  local node="$1" rules trustroots status=0
  local total_rules running_rules running_patterns total_roots running_roots

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

  # Talos assigns IDs 0000, 0001, ... in declaration order, and first match
  # wins. Count and order are therefore both policy: one running rule is not a
  # healthy substitute for three declared rules, and three stale patterns are
  # not the declared policy merely because the totals happen to agree.
  running_patterns="$(printf '%s' "${rules}" | jq -c --arg owner "${rules_owner}" \
    '[sort_by(.id)[] | select(.phase == "running" and .owner == $owner) | .imagePattern]')" ||
    fail_infra "could not compare materialised image-verification rules on ${node}"
  if [[ "${total_rules}" -ne "${declared_rule_count}" ||
    "${running_patterns}" != "${declared_patterns_json}" ]]; then
    printf 'FAIL %s: materialised rules do not match the declared rule set — expected %s ordered running pattern(s) %s, found %s resource(s) with %s running pattern(s) %s\n' \
      "${node}" "${declared_rule_count}" "${declared_patterns_json}" \
      "${total_rules}" "${running_rules}" "${running_patterns}"
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

# One serial sweep of the fleet. Verdict lines go to stdout so a caller can hold
# them back until the inventory they describe is known to be current; returns 0
# when every node enforces and 1 when any does not. `check_node`'s `fail_infra`
# exits 2, and because this runs in a command substitution that status reaches
# the caller instead of being swallowed.
run_pass() {
  local node failed=0
  for node in "$@"; do
    check_node "${node}" || failed=$((failed + 1))
  done
  [[ "${failed}" -eq 0 ]]
}

# How many times a changed inventory is re-checked before the run gives up.
# Ordinary autoscaling changes the fleet occasionally, so a single change is a
# retry rather than a failure; a fleet that changes on every attempt is churning
# faster than this check can observe it, and reporting a verdict for one of
# those transient snapshots would be exactly the stale-snapshot claim this pass
# exists to refuse.
readonly max_inventory_attempts=3

pass_output=''
pass_status=0
attempt=1
while :; do
  pass_status=0
  pass_output="$(run_pass "${nodes[@]}")" || pass_status=$?
  # 2 is `fail_infra` from inside the pass: it has already explained itself on
  # stderr, and it is not a verdict, so it is re-raised rather than retried.
  [[ "${pass_status}" -le 1 ]] || exit "${pass_status}"

  # An explicitly pinned fleet is the caller's claim about what to check, not a
  # snapshot of a moving cluster, so there is nothing to converge on.
  [[ -z "${nodes_arg}" ]] || break

  # Re-read the inventory AFTER the pass. The snapshot the pass ran against was
  # taken before it started, and a serial sweep of a real fleet is not
  # instantaneous — a worker that joins, is replaced, or is removed midway is
  # never inspected, yet the summary below would count only the nodes in the
  # stale snapshot and call the fleet clean. Two consecutive identical readings
  # are what make the verdict a statement about the fleet that exists now.
  previous_signature="${inventory_signature}"
  load_discovered_nodes
  [[ "${inventory_signature}" != "${previous_signature}" ]] || break

  attempt=$((attempt + 1))
  if [[ "${attempt}" -gt "${max_inventory_attempts}" ]]; then
    fail_infra "the node inventory changed on every one of ${max_inventory_attempts} attempts — refusing to report a fleet that will not hold still long enough to be checked"
  fi
  printf 'The node inventory changed while the fleet was being checked; re-checking (attempt %s of %s).\n' \
    "${attempt}" "${max_inventory_attempts}" >&2
done

printf '%s\n' "${pass_output}"

if [[ "${pass_status}" -ne 0 ]]; then
  failures="$(printf '%s\n' "${pass_output}" | grep -c '^FAIL ' || true)"
  printf '\n%s of %s node(s) cannot enforce image verification.\n' "${failures}" "${#nodes[@]}" >&2
  printf 'The ImageVerificationConfig rules in talos/cluster/verify-first-party-images.yaml are declared, but Talos is not enforcing them on those nodes.\n' >&2
  printf 'See devantler-tech/platform#2856 (finding) and #3336 (proving a refusal at pull).\n' >&2
  exit 1
fi

printf '\nAll %s node(s) can enforce image verification.\n' "${#nodes[@]}"
