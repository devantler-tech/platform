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

Environment overrides: TALOSCTL, KUBECTL, TALOS_NODES, CONTAINERD_CONFIG_FILES
Exit: 0 every node can enforce; 1 at least one cannot; 2 usage/infrastructure error.
USAGE
  exit 2
}

readonly talosctl_bin="${TALOSCTL:-talosctl}"
readonly kubectl_bin="${KUBECTL:-kubectl}"

nodes_arg="${TALOS_NODES:-}"
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --nodes)
      [[ "$#" -ge 2 ]] || usage
      nodes_arg="$2"
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
  local json
  local -a context_args=()
  [[ -n "${KUBECTL_CONTEXT:-}" ]] && context_args=(--context "${KUBECTL_CONTEXT}")
  json="$("${kubectl_bin}" "${context_args[@]+"${context_args[@]}"}" get nodes -o json 2>/dev/null)" ||
    fail_infra 'could not list cluster nodes (kubectl get nodes failed)'
  printf '%s' "${json}" |
    jq -r '.items[].status.addresses[] | select(.type == "InternalIP") | .address' 2>/dev/null ||
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
path_exists() {
  "${talosctl_bin}" -n "$1" ls "$2" >/dev/null 2>&1
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
count_executables() {
  local node="$1" dir="$2"
  "${talosctl_bin}" -n "${node}" ls -l "${dir}" 2>/dev/null |
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
if [[ -n "${nodes_arg}" ]]; then
  IFS=',' read -r -a nodes <<<"${nodes_arg}"
  # An empty element means the caller's list was malformed ('a,,b', a trailing
  # comma, a shell variable that expanded to nothing). Skipping it silently
  # would check FEWER nodes than were asked for and still exit 0 — a checker
  # reporting a clean fleet it never looked at, which is the same silence this
  # script exists to break. Refuse the list instead.
  for node in "${nodes[@]}"; do
    [[ -n "${node}" ]] || {
      printf 'ERROR: --nodes/TALOS_NODES contains an empty entry: %s\n' "${nodes_arg}" >&2
      usage
    }
  done
else
  while IFS= read -r discovered; do
    [[ -n "${discovered}" ]] || continue
    nodes+=("${discovered}")
  done < <(discover_nodes)
fi

[[ "${#nodes[@]}" -gt 0 ]] || fail_infra 'no nodes to check'

# Verdict for ONE containerd instance on one node. Returns 0 when that instance
# can enforce, 1 when it cannot.
check_config() {
  local node="$1" file="$2" bin_dir status=0 executables

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
    printf 'FAIL %s [%s]: bin_dir %s is configured but does not exist — containerd permits every pull\n' \
      "${node}" "${file}" "${bin_dir}"
    return 1
  fi

  executables="$(count_executables "${node}" "${bin_dir}")"
  if [[ "${executables}" -eq 0 ]]; then
    printf 'FAIL %s [%s]: bin_dir %s holds no executable — containerd permits every pull\n' \
      "${node}" "${file}" "${bin_dir}"
    return 1
  fi

  printf 'OK   %s [%s]: bin_dir %s holds %s executable(s)\n' \
    "${node}" "${file}" "${bin_dir}" "${executables}"
  return 0
}

failures=0
for node in "${nodes[@]}"; do
  [[ -n "${node}" ]] || continue

  # Every containerd instance present on the node is evaluated INDEPENDENTLY.
  # Accepting the first config that declares a bin_dir would let a wired-up CRI
  # containerd mask an unprotected system containerd (or the reverse) — and the
  # two pull different images: CRI pulls workload images, the system instance
  # pulls Talos' own. A verifier on one leaves the other open, which is the
  # whole point of checking both.
  configs_present=0
  node_failures=0
  for config_file in "${config_files[@]}"; do
    if ! path_exists "${node}" "${config_file}"; then
      node_reachable "${node}" ||
        fail_infra "cannot reach node ${node} (talosctl ls / failed) — refusing to report a fleet that was not checked"
      continue
    fi
    configs_present=$((configs_present + 1))
    check_config "${node}" "${config_file}" || node_failures=$((node_failures + 1))
  done

  # No containerd configuration at all is not a clean node — it means this check
  # looked at nothing and would otherwise pass the node by default.
  [[ "${configs_present}" -gt 0 ]] ||
    fail_infra "no containerd configuration found on ${node} (looked in ${config_files[*]})"

  [[ "${node_failures}" -eq 0 ]] || failures=$((failures + 1))
done

if [[ "${failures}" -gt 0 ]]; then
  printf '\n%s of %s node(s) cannot enforce image verification.\n' "${failures}" "${#nodes[@]}" >&2
  printf 'The ImageVerificationConfig rules in talos/cluster/verify-first-party-images.yaml are not being evaluated on those nodes.\n' >&2
  printf 'See devantler-tech/platform#2856 (finding) and #3101 (installing the verifier).\n' >&2
  exit 1
fi

printf '\nAll %s node(s) can enforce image verification.\n' "${#nodes[@]}"
