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

# Emits the configured bin_dir, or nothing when no config declares one.
# The config file is piped directly into sed: its contents never land anywhere
# this script could print them. See the SECURITY note above.
extract_bin_dir() {
  local node="$1" file value
  for file in "${config_files[@]}"; do
    # INTENT: a config file that does not exist on this node image is skipped so
    # the next one still gets its turn. The `|| true` states that explicitly
    # rather than relying on how `set -e` and `pipefail` interact with a failing
    # first stage inside a command substitution — behaviour subtle enough that
    # two isolated probes of it disagreed, so it is not something this script
    # should depend on either way. The missing-config case in the test pins the
    # behaviour itself.
    value="$(
      { "${talosctl_bin}" -n "${node}" read "${file}" 2>/dev/null || true; } |
        sed -n "s/^[[:space:]]*bin_dir[[:space:]]*=[[:space:]]*['\"]\([^'\"]*\)['\"].*/\1/p" |
        head -n 1
    )"
    if [[ -n "${value}" ]]; then
      printf '%s' "${value}"
      return 0
    fi
  done
  return 0
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

failures=0
for node in "${nodes[@]}"; do
  [[ -n "${node}" ]] || continue

  bin_dir="$(extract_bin_dir "${node}")"

  if [[ -z "${bin_dir}" ]]; then
    printf 'FAIL %s: no bin_dir configured in %s — the image-verifier plugin has no directory to run, so containerd permits every pull\n' \
      "${node}" "${config_files[*]}"
    failures=$((failures + 1))
    continue
  fi

  if ! directory_exists "${node}" "${bin_dir}"; then
    printf 'FAIL %s: bin_dir %s is configured but does not exist — containerd permits every pull\n' \
      "${node}" "${bin_dir}"
    failures=$((failures + 1))
    continue
  fi

  executables="$(count_executables "${node}" "${bin_dir}")"
  if [[ "${executables}" -eq 0 ]]; then
    printf 'FAIL %s: bin_dir %s holds no executable — containerd permits every pull\n' \
      "${node}" "${bin_dir}"
    failures=$((failures + 1))
    continue
  fi

  printf 'OK   %s: bin_dir %s holds %s executable(s)\n' "${node}" "${bin_dir}" "${executables}"
done

if [[ "${failures}" -gt 0 ]]; then
  printf '\n%s of %s node(s) cannot enforce image verification.\n' "${failures}" "${#nodes[@]}" >&2
  printf 'The ImageVerificationConfig rules in talos/cluster/verify-first-party-images.yaml are not being evaluated on those nodes.\n' >&2
  printf 'See devantler-tech/platform#2856 (finding) and #3101 (installing the verifier).\n' >&2
  exit 1
fi

printf '\nAll %s node(s) can enforce image verification.\n' "${#nodes[@]}"
