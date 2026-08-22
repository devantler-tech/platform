#!/usr/bin/env bash

# Assert that the node image verifier can enforce AT ALL.
#
# `talos/cluster/verify-first-party-images.yaml` declares ImageVerificationConfig
# rules. Those rules are inert on their own: containerd does not verify images
# itself, it delegates to the `io.containerd.image-verifier.v1.bindir` plugin,
# which executes every binary in a configured `bin_dir`. containerd's documented
# behaviour when that directory is unset, absent, or empty is to permit the pull:
#
#   "If bin_dir does not exist or contains no files, the image verifier does not
#    block image pulls."
#
# So a cluster with perfect rules and no verifier binary reads EXACTLY like a
# compliant one from every surface we routinely check — the rules are present,
# the node config matches the repository, CI is green — while every image pulls
# unverified. That is not hypothetical: it was the live state of this cluster,
# found only by hand (devantler-tech/platform#2856).
#
# This check asserts the ENFORCEMENT PATH, not the configuration. Asserting that
# the rules exist would NOT have caught #2856 — the rules were correct
# throughout. It is deliberately the weaker of the two signals that issue asks
# for; the strong one is a behavioural test that a known-unsigned image is
# actually refused (#3101). This one's value is that it needs no node rollout
# and fails loudly while the verifier is missing.
#
# It reads LIVE NODE STATE on purpose. A variant that read the repository would
# re-create the exact blind spot it exists to close.
#
# SECURITY: the rendered `/etc/cri/conf.d/cri.toml` carries registry
# credentials. Every read of it is piped straight into a key extractor; the
# file's contents are never stored in a variable, echoed, or included in an
# error message. Keep it that way — this script's output goes to CI logs.

set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: validate-image-verifier-liveness.sh [--nodes <ip>[,<ip>...]]

Asserts, for every node, that containerd's image-verifier bin_dir is configured,
exists, and holds at least one executable.

Nodes are discovered from the cluster when --nodes/TALOS_NODES is not given, so
autoscaled nodes are covered without anyone maintaining a list.

Environment overrides: TALOSCTL, KUBECTL, TALOS_NODES, CONTAINERD_CONFIG_FILES,
IMAGE_VERIFIER_CONVERGENCE_ATTEMPTS (default 3; how many times discovery re-reads
a node inventory that changed while the fleet was being checked, before giving up)
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

# Both containerd instances matter: the CRI one pulls workload images, the system
# one pulls Talos' own. A verifier wired into only one leaves the other open.
readonly default_config_files='/etc/cri/conf.d/cri.toml /etc/containerd/config.toml'
read -r -a config_files <<<"${CONTAINERD_CONFIG_FILES:-${default_config_files}}"

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
        uid: ((.metadata.uid // "") | tostring),
        ips: [(.status.addresses // [])[]
              | select(.type == "InternalIP" and ((.address // "") | tostring) != "")
              | .address]
      }] as $nodes
      | ($nodes | map(select(.ips | length != 1))
          | map("\(.name) (has \(.ips | length) InternalIP addresses, expected 1)")),
        ($nodes | map(select(.uid == "" ))
          | map("\(.name) (has no metadata.uid)")),
        ($nodes | map(select(.ips | length == 1))
          | group_by(.ips[0]) | map(select(length > 1))
          | map("\(map(.name) | join(" and ")) share InternalIP \(.[0].ips[0])")),
        ($nodes | map(select(.uid != "")) | group_by(.uid) | map(select(length > 1))
          | map("\(map(.name) | join(" and ")) share UID \(.[0].uid)"))
      | .[]
    ' 2>/dev/null)" ||
    fail_infra 'could not parse node addresses from kubectl output'
  if [[ -n "${bad_nodes}" ]]; then
    fail_infra "unusable node inventory — refusing to report a fleet that was only partly enumerated: $(printf '%s' "${bad_nodes}" | tr '\n' ';' | sed 's/;$//')"
  fi

  # Emitted as UID and address together. An address alone cannot tell a node
  # apart from its own replacement: the autoscaler can retire a machine and
  # bring up a new one that reuses the address, and comparing addresses would
  # call that pair identical -- so a machine that was never inspected would be
  # reported as one that passed. The UID is what makes "the same node" mean the
  # same node.
  printf '%s' "${json}" |
    jq -r '
      .items[]
      | . as $node
      | (.status.addresses // [])[]
      | select(.type == "InternalIP" and ((.address // "") | tostring) != "")
      | "\(($node.metadata.uid // "") | tostring)\t\(.address)"
    ' 2>/dev/null ||
    fail_infra 'could not parse node addresses from kubectl output'
}

# `ls` reports a path's EXISTENCE without emitting its contents, so its exit
# status is safe to branch on; `read` emits the whole config, so a failed read
# must never be the thing that tells "absent" apart from "unreachable".
#
# That distinction is load-bearing. Inferring absence from a failed read means a
# node the runner cannot reach — expired talosconfig, network partition, API
# down — looks exactly like a node that simply does not ship that config file.
# Such a node would be skipped silently, and if any OTHER containerd config
# declared a verifier the fleet would pass while nothing had been checked: the
# same fail-open shape this script exists to detect, reintroduced in the
# detector.
# Three-state probe: 'present', 'absent', or 'error'.
#
# A failed `ls` is ambiguous — the path may not be there, or the node may have
# hiccuped — and only the first of those is a verdict. Collapsing both into
# "this node does not ship that config" lets a transient fault silently drop one
# containerd from the check; if the OTHER one is wired up, `configs_present`
# stays nonzero and the node passes with half its image paths never inspected.
#
# Only a CONFIRMED not-found counts as absent, and the discriminator has to be
# the stderr text because talosctl exits 1 for both. `ls` on a path emits only
# that path's name and never file contents, so matching its stderr cannot expose
# the registry credentials that `/etc/cri/conf.d/cri.toml` carries.
path_probe() {
  local node="$1" path="$2" err status=0
  err="$("${talosctl_bin}" -n "${node}" ls "${path}" 2>&1 >/dev/null)" || status=$?
  if [[ "${status}" -eq 0 ]]; then
    printf 'present\n'
    return 0
  fi
  case "${err}" in
    *'no such file or directory'* | *'NotFound'* | *'not found'*) printf 'absent\n' ;;
    *) printf 'error\n' ;;
  esac
}

node_reachable() {
  "${talosctl_bin}" -n "$1" ls / >/dev/null 2>&1
}

# Emits the bin_dir declared by ONE config file's image-verifier plugin, or
# nothing when that file declares none.
#
# Scoped to the `io.containerd.image-verifier.v1.bindir` table on purpose.
# `bin_dir` is a generic key name, and an unscoped match accepts one from an
# unrelated plugin's table — reporting a node as enforcing on the strength of a
# setting containerd never consults for image verification.
#
# The config is piped straight into awk and only the extracted value is ever
# printed. `/etc/cri/conf.d/cri.toml` carries registry credentials and this
# script's output goes to CI logs, so the file must never be captured — not into
# a variable, not into a temp file, not into an error message. See the SECURITY
# note at the top.
#
# awk rather than sed because the table scoping needs state across lines, and
# because awk consumes all input: a `sed ... | head -n 1` pipeline closes the
# pipe early, and under `pipefail` the resulting SIGPIPE is indistinguishable
# from a genuine read failure.
extract_bin_dir_from() {
  local node="$1" file="$2"
  "${talosctl_bin}" -n "${node}" read "${file}" 2>/dev/null | awk '
    BEGIN { SQ = sprintf("%c", 39); DQ = sprintf("%c", 34); want = 0; found = 0 }
    found { next }
    /^[ \t]*\[/ {
      header = $0
      gsub(/[ \t]/, "", header)
      gsub(SQ, "", header)
      gsub(DQ, "", header)
      want = (header == "[plugins.io.containerd.image-verifier.v1.bindir]")
      next
    }
    want && match($0, /^[ \t]*bin_dir[ \t]*=/) {
      value = substr($0, RSTART + RLENGTH)
      sub(/^[ \t]*/, "", value)
      quote = substr(value, 1, 1)
      if (quote != SQ && quote != DQ) next
      value = substr(value, 2)
      end = index(value, quote)
      if (end == 0) next
      print substr(value, 1, end - 1)
      found = 1
    }
  '
}

# Reports 'disabled' when this config file switches the image-verifier plugin
# OFF, and nothing otherwise.
#
# A bin_dir that is configured, exists and holds an executable proves nothing if
# containerd never loads the plugin that runs it. `disabled_plugins` is the
# cheap half of that question: it lives in the SAME file the bin_dir came from,
# so reading it costs nothing extra and closes the case where the configuration
# looks complete and no verifier handles a single pull -- the same silent
# enforcement failure this script exists to detect, one layer up. (Whether a
# plugin that is NOT disabled actually loaded and stayed healthy needs a live
# plugin-status query or a behavioural probe; that is #3101's job.)
#
# Scoped to TOP-LEVEL keys. `disabled_plugins` is a root key in containerd's
# config, and TOML puts root keys before the first table header -- so once a
# '[' header is seen, any later match belongs to some other table and must not
# count. An unscoped match would let an unrelated key disable the check.
#
# Only the marker is ever printed. The array is assembled in awk and searched
# there; the config itself never reaches a variable or an error message, because
# /etc/cri/conf.d/cri.toml carries registry credentials and this output goes to
# CI logs. See the SECURITY note at the top.
plugin_disabled_in() {
  local node="$1" file="$2"
  "${talosctl_bin}" -n "${node}" read "${file}" 2>/dev/null | awk '
    BEGIN {
      SQ = sprintf("%c", 39); DQ = sprintf("%c", 34)
      toplevel = 1; in_array = 0; buf = ""; disabled = 0
    }
    # Records the verdict rather than printing and exiting on it. Exiting here
    # would close the pipe while `talosctl read` is still writing, and under
    # `set -o pipefail` the resulting SIGPIPE is indistinguishable from a real
    # read failure -- so a genuinely disabled node on a large config would be
    # reported as an infrastructure error instead of the FAIL it is. Consume to
    # EOF and print from END. (extract_bin_dir_from documents the same hazard.)
    function verdict(text,   probe) {
      probe = text
      gsub(SQ, "", probe); gsub(DQ, "", probe); gsub(/[ \t]/, "", probe)
      if (index(probe, "io.containerd.image-verifier.v1.bindir") > 0) {
        disabled = 1
      }
    }
    END { if (disabled) print "disabled" }
    # A continuation of the array is consumed before the header rule below, so a
    # value that happens to start with a bracket cannot be mistaken for a table.
    disabled { next }
    in_array {
      buf = buf $0
      if (index($0, "]") > 0) { in_array = 0; verdict(buf) }
      next
    }
    /^[ \t]*\[/ { toplevel = 0 }
    !toplevel { next }
    match($0, /^[ \t]*disabled_plugins[ \t]*=/) {
      buf = substr($0, RSTART + RLENGTH)
      if (index(buf, "]") > 0) { verdict(buf) } else { in_array = 1 }
    }
  '
}

# Counts executable entries, skipping the directory's own '.' entry. MODE is
# always column 2; NAME is always last. The LABEL column is empty for some files
# on this cluster, so positional parsing from the left past column 2 is unsafe.
#
# The MODE's first character is the entry TYPE, and it must be checked: a
# directory's mode ('drwxr-xr-x') carries an 'x' too, so counting any entry with
# an 'x' would let a bin_dir holding nothing but a subdirectory report as
# enforcing. containerd executes FILES in bin_dir and does not descend into
# subdirectories, so such a node permits every pull while this check calls it
# healthy — the precise fail-open this script exists to detect. Regular files
# ('-') and symlinks ('l') count; everything else does not.
#
# The listing's exit status is preserved, not discarded. The caller only reaches
# this after the directory was proven to exist, so a failure here means the node
# stopped answering mid-check — which must be an infrastructure error, never the
# "holds no executable" verdict. `awk` on empty input prints 0 quite happily, so
# swallowing the status turns an unreachable node into a confident FAIL line.
count_executables() {
  local node="$1" dir="$2" listing status=0
  listing="$("${talosctl_bin}" -n "${node}" ls -l "${dir}" 2>/dev/null)" || status=$?
  [[ "${status}" -eq 0 ]] ||
    fail_infra "could not list ${dir} on ${node} (the directory exists but talosctl ls failed)"
  printf '%s\n' "${listing}" |
    awk 'NR > 1 && $NF != "." && substr($2, 1, 1) ~ /^[-l]$/ && $2 ~ /x/ { n++ } END { print n + 0 }'
}

directory_exists() {
  local node="$1" dir="$2"
  "${talosctl_bin}" -n "${node}" ls "${dir}" >/dev/null 2>&1
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
  discovered_identities="$(discover_nodes)" ||
    fail_infra 'node discovery failed — refusing to report a fleet that was only partly enumerated'
  while IFS=$'\t' read -r _discovered_uid discovered; do
    [[ -n "${discovered}" ]] || continue
    nodes+=("${discovered}")
  done <<<"${discovered_identities}"
fi

[[ "${#nodes[@]}" -gt 0 ]] || fail_infra 'no nodes to check'

# Verdict for ONE containerd instance on one node. Returns 0 when that instance
# can enforce, 1 when it cannot.
check_config() {
  local node="$1" file="$2" bin_dir status=0 executables exec_status=0

  # Asked FIRST: a disabled plugin makes the rest of this verdict irrelevant.
  # bin_dir could be configured, present and full of executables and containerd
  # would still run none of them, so reporting on the directory before checking
  # whether the plugin is switched on would describe a path that is not taken.
  local disabled disabled_status=0
  disabled="$(plugin_disabled_in "${node}" "${file}")" || disabled_status=$?
  [[ "${disabled_status}" -eq 0 ]] ||
    fail_infra "could not read ${file} on ${node} (the file exists but talosctl read failed)"
  if [[ "${disabled}" == 'disabled' ]]; then
    printf 'FAIL %s [%s]: the io.containerd.image-verifier.v1.bindir plugin is in disabled_plugins — containerd loads no verifier, so it permits every pull whatever bin_dir says\n' \
      "${node}" "${file}"
    return 1
  fi

  bin_dir="$(extract_bin_dir_from "${node}" "${file}")" || status=$?
  # The file's existence was proven before this call, so a read failure here is
  # an infrastructure fault, never a verdict. Reporting it as "cannot enforce"
  # would be a misdiagnosis; swallowing it would be a fail-open.
  [[ "${status}" -eq 0 ]] ||
    fail_infra "could not read ${file} on ${node} (the file exists but talosctl read failed)"

  if [[ -z "${bin_dir}" ]]; then
    printf 'FAIL %s [%s]: no bin_dir in the io.containerd.image-verifier.v1.bindir table — the plugin has no directory to run, so containerd permits every pull\n' \
      "${node}" "${file}"
    return 1
  fi

  if ! directory_exists "${node}" "${bin_dir}"; then
    # "The directory is not there" and "the node stopped answering" are the same
    # failed `ls` from here. Only the first is a verdict; reporting the second as
    # one would blame the cluster for a runner-side fault.
    node_reachable "${node}" ||
      fail_infra "cannot reach node ${node} while checking ${bin_dir} (talosctl ls / failed)"
    printf 'FAIL %s [%s]: bin_dir %s is configured but does not exist — containerd permits every pull\n' \
      "${node}" "${file}" "${bin_dir}"
    return 1
  fi

  # `fail_infra` inside count_executables would exit only the command
  # substitution, so its status is carried out explicitly — the same subshell
  # trap that made partial node discovery pass silently.
  executables="$(count_executables "${node}" "${bin_dir}")" || exec_status=$?
  [[ "${exec_status}" -eq 0 ]] ||
    fail_infra "could not list ${bin_dir} on ${node} (the directory exists but talosctl ls failed)"

  if [[ "${executables}" -eq 0 ]]; then
    printf 'FAIL %s [%s]: bin_dir %s holds no executable — containerd permits every pull\n' \
      "${node}" "${file}" "${bin_dir}"
    return 1
  fi

  printf 'OK   %s [%s]: bin_dir %s holds %s executable(s)\n' \
    "${node}" "${file}" "${bin_dir}" "${executables}"
  return 0
}

# One full sweep of the fleet currently in `nodes`. Kept as a function so the
# discovery path can run it again on a changed inventory; it is deliberately NOT
# invoked in a command substitution, because `fail_infra` inside one would exit
# only the subshell and let a fleet that was never checked report a verdict --
# the same trap the discovery path documents above.
run_check_pass() {
  failures=0
  for node in "${nodes[@]}"; do
    # Every containerd instance present on the node is evaluated INDEPENDENTLY.
    # Accepting the first config that declares a bin_dir would let a wired-up CRI
    # containerd mask an unprotected system containerd (or the reverse) — and the
    # two pull different images: CRI pulls workload images, the system instance
    # pulls Talos' own. A verifier on one leaves the other open, which is the
    # whole point of checking both.
    configs_present=0
    node_failures=0
    for config_file in "${config_files[@]}"; do
      case "$(path_probe "${node}" "${config_file}")" in
        present) ;;
        absent)
          # A confirmed not-found normally proves the node answered — but the same
          # phrase can come from a client-side fault (a missing talosconfig, say),
          # which would otherwise read as "this node does not ship that config".
          # The reachability probe is what tells those two apart.
          node_reachable "${node}" ||
            fail_infra "cannot reach node ${node} (talosctl ls / failed) — refusing to report a fleet that was not checked"
          continue
          ;;
        *)
          # Both "the node is gone" and "the node answered but this one probe
          # failed" arrive here, and they deserve different diagnoses: the first
          # is a fleet-wide fault, the second is specific to one containerd.
          # Neither is a verdict, so both stop the run either way.
          node_reachable "${node}" ||
            fail_infra "cannot reach node ${node} (talosctl ls / failed) — refusing to report a fleet that was not checked"
          fail_infra "could not determine whether ${config_file} exists on ${node} (talosctl ls failed for a reason other than the file being absent) — refusing to report a node whose containerd was never inspected"
          ;;
      esac
      configs_present=$((configs_present + 1))
      check_config "${node}" "${config_file}" || node_failures=$((node_failures + 1))
    done

    # No containerd configuration at all is not a clean node — it means this check
    # looked at nothing and would otherwise pass the node by default.
    [[ "${configs_present}" -gt 0 ]] ||
      fail_infra "no containerd configuration found on ${node} (looked in ${config_files[*]})"

    [[ "${node_failures}" -eq 0 ]] || failures=$((failures + 1))
  done
}

# Rebuilds `nodes` from a UID/address identity list.
nodes_from_identities() {
  nodes=()
  while IFS=$'\t' read -r _uid identity_address; do
    [[ -n "${identity_address}" ]] || continue
    nodes+=("${identity_address}")
  done <<<"$1"
}

# The inventory is a snapshot taken BEFORE a serial pass, and Cluster Autoscaler
# can add or replace workers while that pass runs. A node that joins mid-run --
# or a replacement reusing a retired address -- would then go completely
# uninspected while every node in the stale snapshot reported healthy: a green
# verdict for a fleet that was never fully enumerated, which is the same
# fail-open shape this script exists to detect.
#
# So the discovery path re-reads the inventory AFTER the checks and only reports
# when the fleet it just checked is still the fleet that exists. A change is
# ordinary autoscaling rather than a fault, so it costs a re-run, not a failure;
# only an inventory that will not settle within the bound is an error, which
# keeps a continuously-scaling cluster from either flapping or looping forever.
# `sync_talos_registry_auth` in scripts/refresh-flux-ghcr-auth.sh converges the
# same way and for the same reason.
#
# An explicitly requested fleet is NOT re-read: the caller named those nodes, so
# a cluster changing under them does not change what was asked for.
convergence_attempts="${IMAGE_VERIFIER_CONVERGENCE_ATTEMPTS:-3}"
case "${convergence_attempts}" in
  '' | *[!0-9]*) fail_infra "IMAGE_VERIFIER_CONVERGENCE_ATTEMPTS must be a positive integer, got '${convergence_attempts}'" ;;
esac
[[ "${convergence_attempts}" -ge 1 ]] ||
  fail_infra 'IMAGE_VERIFIER_CONVERGENCE_ATTEMPTS must be at least 1'

if [[ "${nodes_requested}" -eq 1 ]]; then
  run_check_pass
else
  attempt=0
  while :; do
    attempt=$((attempt + 1))
    run_check_pass

    inventory_after="$(discover_nodes)" ||
      fail_infra 'could not re-read the node inventory after the checks — refusing to report a fleet that may have changed underneath the pass'

    # Compare the identity SET, not the stream. `kubectl get nodes` gives no
    # ordering guarantee, so a plain string compare would call a re-ordered but
    # otherwise identical fleet "changed", re-run the whole pass, and can burn
    # the attempt bound into an exit 2 for a cluster that never moved.
    [[ "$(printf '%s\n' "${inventory_after}" | LC_ALL=C sort)" \
      != "$(printf '%s\n' "${discovered_identities}" | LC_ALL=C sort)" ]] || break

    [[ "${attempt}" -lt "${convergence_attempts}" ]] ||
      fail_infra "the node inventory changed during each of ${convergence_attempts} attempt(s) — refusing to report a fleet that never held still long enough to be checked"

    printf '\nThe node set changed while it was being checked (a node joined, left, or was replaced). Re-running over the current fleet — attempt %s of %s.\n' \
      "$((attempt + 1))" "${convergence_attempts}"
    discovered_identities="${inventory_after}"
    nodes_from_identities "${discovered_identities}"
    [[ "${#nodes[@]}" -gt 0 ]] || fail_infra 'no nodes to check after the inventory changed'
  done
fi

if [[ "${failures}" -gt 0 ]]; then
  printf '\n%s of %s node(s) cannot enforce image verification.\n' "${failures}" "${#nodes[@]}" >&2
  printf 'The ImageVerificationConfig rules in talos/cluster/verify-first-party-images.yaml are not being evaluated on those nodes.\n' >&2
  printf 'See devantler-tech/platform#2856 (finding) and #3101 (installing the verifier).\n' >&2
  exit 1
fi

printf '\nAll %s node(s) can enforce image verification.\n' "${#nodes[@]}"
