#!/usr/bin/env bash
# Measure how often the datapath DaemonSets' containers are CPU-throttled (#3790).
#
# THE QUESTION. #3790 keeps the 2-core CPU limit the kube-system LimitRange already puts on the
# datapath DaemonSets, and declares it in their templates. Cilium advises against capping the agent
# because of CFS throttling during short bursts, which average usage cannot show. The decision on
# #3790 therefore records the throttled share of CFS periods before and after the change, and
# reopens it if `cilium-agent` or `cilium-envoy` stays above 1%.
#
# WHY THE KUBELET AND NOT PROMETHEUS. The in-cluster Prometheus is Coroot's: it does not scrape the
# kubelet's cAdvisor endpoint, and its network policy admits no traffic from the API server on
# :9090. The kubelet's own cAdvisor endpoint exposes the two counters directly, per container, and
# the API server's nodes/proxy subresource reaches it without any policy or workload change.
#
# WHAT IT DOES — read-only: `get` calls only, nothing is created, changed or executed:
#   1. `get nodes`                                     — the node set.
#   2. `get --raw /api/v1/nodes/<n>/proxy/metrics/cadvisor` for every node — first sample.
#   3. sleep for the window.
#   4. `get nodes` and every node's cAdvisor again     — second sample.
# For each measured container the counters of every pod are subtracted between the samples, summed
# per container, and reported as throttled periods / CFS periods. The worst single pod is reported
# too, since one busy node can hide behind a quiet fleet.
#
# WHAT IS MEASURED. Only these DaemonSet containers in kube-system, matched on BOTH the pod name
# shape `<daemonset>-<5 chars>` and the container name, so the hcloud-csi controller's
# `liveness-probe` container is never counted as the node plugin's:
#   cilium/cilium-agent, cilium-envoy/cilium-envoy, tetragon/tetragon,
#   hcloud-csi-node/{csi-node-driver-registrar,liveness-probe,hcloud-csi-driver}
#
# THE VERDICT
#   MEASURED      every sample was read, the node set and the measured pod set did not change, no
#                 counter went backwards, and every measured container accumulated CFS periods.
#                 Each container line is then marked `ABOVE-1%` or `within-1%`; the 1% bar is the
#                 one #3790 uses to reopen its decision. The exit code does not depend on it.
#   INCONCLUSIVE  anything else: a failed read, a node or pod that appeared or disappeared (a
#                 rollout or the autoscaler), a counter reset, a container with no series or with
#                 zero CFS periods (no quota in effect, so there is no ratio to report).
#
# WHAT IT PRINTS. The workflow log of a public repository is public, so no node name, pod name or
# address is printed — only container names, counts, ratios and the verdict. kubectl's stderr is
# discarded for the same reason.
#
# EXIT CODES
#   0  MEASURED
#   1  usage error — nothing was read
#   3  INCONCLUSIVE — the run proved nothing, and a caller must not report it as green
#
# Bash 3.2 compatible so it runs on a maintainer's macOS as well as CI.
set -euo pipefail

readonly namespace='kube-system'
# <daemonset>/<container>, in report order.
readonly measured='cilium/cilium-agent
cilium-envoy/cilium-envoy
tetragon/tetragon
hcloud-csi-node/csi-node-driver-registrar
hcloud-csi-node/liveness-probe
hcloud-csi-node/hcloud-csi-driver'
# BSD awk rejects a newline inside a -v value, so awk receives the list space-separated.
measured_words="$(printf "%s" "${measured}" | tr "\n" " ")"
readonly measured_words
readonly gated_containers=' cilium/cilium-agent cilium-envoy/cilium-envoy '
readonly min_window=60
readonly max_window=1800
readonly request_timeout='30s'

usage() {
  printf 'Usage: %s --context <kube-context> [--window-seconds N (%s-%s, default 600)]\n' \
    "$(basename "$0")" "${min_window}" "${max_window}" >&2
}

context=''
window='600'
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --context | --window-seconds)
      if [[ "$#" -lt 2 || -z "$2" ]]; then
        usage
        exit 1
      fi
      case "$1" in
        --context) context="$2" ;;
        --window-seconds) window="$2" ;;
      esac
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
if [[ ! "${window}" =~ ^[0-9]+$ ]] || ((10#${window} < min_window || 10#${window} > max_window)); then
  printf 'The window must be a whole number of seconds from %s to %s.\n' "${min_window}" "${max_window}" >&2
  exit 1
fi
window=$((10#${window}))

work_dir="$(mktemp -d)"
readonly work_dir
trap 'rm -rf "${work_dir}"' EXIT

report() {
  printf '%s\n' "$1"
  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    printf '%s\n' "$1" >>"${GITHUB_STEP_SUMMARY}"
  fi
}

inconclusive() {
  report "Verdict: INCONCLUSIVE — $1"
  exit 3
}

kube() {
  kubectl --context "${context}" --request-timeout="${request_timeout}" "$@" 2>/dev/null
}

# Reads every node's cAdvisor counters for the measured containers into <out> as
#   <daemonset>/<container>|<pod>|<metric>|<value>
# and the sorted node names into <out>.nodes. Returns non-zero on any failed read.
sample() {
  local out="$1" nodes node raw
  nodes="$(kube get nodes -o 'jsonpath={.items[*].metadata.name}')" || return 1
  [[ -n "${nodes}" ]] || return 1
  printf '%s\n' "${nodes}" | tr ' ' '\n' | grep -v '^$' | LC_ALL=C sort >"${out}.nodes"
  : >"${out}"
  while IFS= read -r node; do
    [[ -n "${node}" ]] || continue
    raw="$(kube get --raw "/api/v1/nodes/${node}/proxy/metrics/cadvisor")" || return 1
    printf '%s\n' "${raw}" | awk -v ns="${namespace}" -v measured="${measured_words}" '
      function label(name,   re, s) {
        re = "[{,]" name "=\"[^\"]*\""
        if (!match($0, re)) return ""
        s = substr($0, RSTART + length(name) + 3, RLENGTH - length(name) - 4)
        return s
      }
      BEGIN {
        n = split(measured, rows, " ")
        for (i = 1; i <= n; i++) {
          split(rows[i], parts, "/")
          want[rows[i]] = parts[1]
        }
      }
      $1 ~ /^container_cpu_cfs_(throttled_)?periods_total\{/ {
        metric = $1
        sub(/\{.*/, "", metric)
        if (label("namespace") != ns) next
        pod = label("pod"); container = label("container")
        for (key in want) {
          ds = want[key]
          if (key != ds "/" container) continue
          suffix = substr(pod, length(ds) + 2)
          if (substr(pod, 1, length(ds) + 1) != ds "-" || suffix !~ /^[a-z0-9][a-z0-9][a-z0-9][a-z0-9][a-z0-9]$/) continue
          value = $2
          if (value !~ /^[0-9]+(\.[0-9]+)?([eE][+]?[0-9]+)?$/) { print "BAD|" key; next }
          print key "|" pod "|" metric "|" value
        }
      }' >>"${out}" || return 1
  done <"${out}.nodes"
  if grep -q '^BAD|' "${out}"; then
    return 1
  fi
  LC_ALL=C sort -o "${out}" "${out}"
}

report "Datapath CPU throttling over ${window}s (#3790)"

sample "${work_dir}/first" || inconclusive 'the first cAdvisor read failed'
sleep "${window}"
sample "${work_dir}/second" || inconclusive 'the second cAdvisor read failed'

cmp -s "${work_dir}/first.nodes" "${work_dir}/second.nodes" ||
  inconclusive 'the node set changed during the window'
# The same pods, containers and metrics must appear in both samples; a new or missing series means a
# pod was replaced and its counters cannot be subtracted.
cut -d'|' -f1-3 "${work_dir}/first" >"${work_dir}/first.keys"
cut -d'|' -f1-3 "${work_dir}/second" >"${work_dir}/second.keys"
cmp -s "${work_dir}/first.keys" "${work_dir}/second.keys" ||
  inconclusive 'the measured pod set changed during the window (a rollout or a replaced pod)'

# Duplicate series for one pod and metric would be double-counted; refuse rather than guess.
if [[ -n "$(uniq -d "${work_dir}/first.keys")" ]]; then
  inconclusive 'a pod reported the same counter twice'
fi

results="$(paste -d'|' "${work_dir}/first" "${work_dir}/second" | awk -F'|' -v measured="${measured_words}" '
  BEGIN { bad = 0 }
  {
    key = $1; pod = $2; metric = $3; before = $4 + 0; after = $8 + 0
    if (after < before) { bad = 1; exit }
    delta = after - before
    podkey = key "|" pod
    pods[podkey] = key
    if (metric == "container_cpu_cfs_periods_total") { periods[key] += delta; podperiods[podkey] += delta }
    else { throttled[key] += delta; podthrottled[podkey] += delta }
  }
  END {
    if (bad) { print "RESET"; exit }
    n = split(measured, rows, " ")
    for (i = 1; i <= n; i++) {
      key = rows[i]; count = 0; worst = -1
      for (pk in pods) {
        if (pods[pk] != key) continue
        count++
        if (podperiods[pk] > 0) {
          r = 100 * podthrottled[pk] / podperiods[pk]
          if (r > worst) worst = r
        }
      }
      if (count == 0) { print "MISSING|" key; continue }
      if (periods[key] + 0 == 0) { print "NOPERIODS|" key "|" count; continue }
      printf "OK|%s|%d|%d|%d|%.3f|%.3f\n", key, count, periods[key], throttled[key] + 0, 100 * throttled[key] / periods[key], (worst < 0 ? 0 : worst)
    }
  }')"

if grep -q '^RESET' <<<"${results}"; then
  inconclusive 'a counter went backwards during the window (a container restarted)'
fi

report ''
report '| DaemonSet/container | Pods | CFS periods | Throttled | Throttled % | Worst pod % | 1% bar |'
report '|---|---|---|---|---|---|---|'
problems=''
while IFS='|' read -r state key count periods throttled ratio worst; do
  [[ -n "${state}" ]] || continue
  case "${state}" in
    MISSING)
      report "| ${key} | 0 | — | — | — | — | no series |"
      problems="${problems} ${key}:no-series"
      ;;
    NOPERIODS)
      report "| ${key} | ${count} | 0 | — | — | — | no quota in effect |"
      problems="${problems} ${key}:no-periods"
      ;;
    OK)
      bar='within-1%'
      if awk -v w="${worst}" -v r="${ratio}" 'BEGIN { exit !(r > 1) }'; then
        bar='ABOVE-1%'
      fi
      if [[ "${gated_containers}" != *" ${key} "* ]]; then
        bar="${bar} (not gated)"
      fi
      report "| ${key} | ${count} | ${periods} | ${throttled} | ${ratio} | ${worst} | ${bar} |"
      ;;
  esac
done <<<"${results}"
report ''

if [[ -n "${problems}" ]]; then
  inconclusive "not every container could be measured:${problems}"
fi
report 'Verdict: MEASURED'
