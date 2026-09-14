#!/usr/bin/env bash
#
# Restart the Talos-bootstrapped CoreDNS Deployment when its running pods carry no
# CPU limit, and only then.
#
# WHY. On prod, Talos applies CoreDNS from its own bootstrap manifests at cluster
# init — before KSail installs Flux and before the `bootstrap` layer creates the
# kube-system `default-limitrange`. Flux manages no CoreDNS Deployment there, so
# nothing recreates those pods, and a LimitRange binds at pod admission only. A
# fresh cluster therefore runs CoreDNS without a CPU limit until the next Talos
# Kubernetes upgrade happens to roll it (platform#3567).
#
# WHY SO NARROW. Every other kube-system workload is either Flux-managed or a
# datapath DaemonSet whose rollout this repository sequences deliberately, so a
# generic "restart whatever is uncapped" would roll Cilium on a fresh cluster.
# This touches `deployment/coredns` and nothing else.
#
# Run it after every Flux Kustomization reports Ready, so the LimitRange exists.
#
# Exit: 0  CoreDNS already capped (nothing restarted), or capped by the restart
#       1  restarted, but a running CoreDNS container still has no CPU limit
#       2  could not check — unreadable LimitRange, Deployment or pods, no default
#          CPU limit to supply, or no running CoreDNS pod to judge

set -euo pipefail

readonly kubectl_bin="${KUBECTL:-kubectl}"
readonly rollout_timeout="${COREDNS_ROLLOUT_TIMEOUT:-5m}"
readonly namespace='kube-system'
readonly deployment='coredns'
readonly limit_range='default-limitrange'

die() {
  printf 'restart-uncapped-talos-coredns: %s\n' "$1" >&2
  exit 2
}

kubectl_prod() {
  "${kubectl_bin}" --context admin@prod "$@"
}

command -v jq >/dev/null 2>&1 || die 'jq is required but not on PATH'

# A restart only helps when admission will supply a limit. Without one, restarting
# CoreDNS would cost a rollout and change nothing, so refuse rather than pretend.
if ! default_cpu="$(kubectl_prod -n "${namespace}" get limitrange "${limit_range}" \
  -o jsonpath='{.spec.limits[?(@.type=="Container")].default.cpu}')"; then
  die "could not read LimitRange ${namespace}/${limit_range}"
fi
[[ -n "${default_cpu}" ]] ||
  die "LimitRange ${namespace}/${limit_range} supplies no default CPU limit"

if ! deployment_json="$(kubectl_prod -n "${namespace}" get deployment "${deployment}" -o json)"; then
  die "could not read Deployment ${namespace}/${deployment}"
fi
selector="$(jq -r '.spec.selector.matchLabels // {}
  | to_entries | map("\(.key)=\(.value)") | join(",")' <<<"${deployment_json}")"
[[ -n "${selector}" ]] || die "Deployment ${namespace}/${deployment} has no matchLabels selector"

# Prints "<running pods> <running containers without a CPU limit>". Terminating pods
# are excluded: after a restart they linger uncapped and say nothing about the new ones.
count_uncapped() {
  local pods_json
  pods_json="$(kubectl_prod -n "${namespace}" get pods -l "${selector}" -o json)" || return 1
  jq -r '[.items[] | select(.metadata.deletionTimestamp == null)]
    | "\(length) \([.[].spec.containers[] | select((.resources.limits.cpu // "") == "")] | length)"' \
    <<<"${pods_json}"
}

if ! counts="$(count_uncapped)"; then
  die "could not read ${deployment} pods (selector ${selector})"
fi
read -r running uncapped <<<"${counts}"
[[ "${running}" -gt 0 ]] || die "no running ${deployment} pod to judge (selector ${selector})"

if [[ "${uncapped}" -eq 0 ]]; then
  printf 'CoreDNS already carries a CPU limit on all %s running pod(s); nothing restarted.\n' "${running}"
  exit 0
fi

printf 'CoreDNS has %s container(s) without a CPU limit; restarting so admission applies the %s default.\n' \
  "${uncapped}" "${default_cpu}"
kubectl_prod -n "${namespace}" rollout restart "deployment/${deployment}"
kubectl_prod -n "${namespace}" rollout status "deployment/${deployment}" --timeout="${rollout_timeout}"

if ! counts="$(count_uncapped)"; then
  die "could not re-read ${deployment} pods after the restart"
fi
read -r running uncapped <<<"${counts}"
if [[ "${uncapped}" -ne 0 ]]; then
  printf 'restart-uncapped-talos-coredns: %s container(s) still lack a CPU limit after the restart\n' \
    "${uncapped}" >&2
  exit 1
fi

printf 'CoreDNS restarted; all %s running pod(s) now carry a CPU limit.\n' "${running}"
