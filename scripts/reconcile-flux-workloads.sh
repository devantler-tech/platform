#!/usr/bin/env bash

# Trigger Flux reconciliation, tolerating the control-plane restart this deploy
# may itself have caused.
#
# A change to the Flux control plane (controller securityContext, resources,
# placement, a flux-operator/FluxInstance bump) restarts source-controller and
# kustomize-controller — the two components the reconcile's own readiness check
# runs through. source-controller comes back without its artifact yet rebuilt, so
# every Kustomization transiently reports ArtifactFailed, and kustomize-controller
# being terminated cancels the in-flight health checks ("context canceled").
# The single-shot reconcile then fails over a cluster that converges correctly a
# minute later. Observed on platform#3459's merge-queue deploy: the deploy failed
# in 40 s while every Kustomization reached Ready shortly afterwards (#3478).
#
# The tolerance is deliberately narrow, because this step is the gate that keeps
# a broken artifact out of production:
#
#   * a reconcile that succeeds is never retried;
#   * a reconcile that fails while the control plane did NOT restart is a genuine
#     failure and fails the deploy immediately, with no second attempt;
#   * only positive evidence that THIS deploy restarted the control plane — a
#     Deployment generation that advanced across the reconcile — buys exactly one
#     further attempt, after that rollout has been waited out.
#
# So the deploy still requires a successful reconcile in every case. What changes
# is that a failure the deploy inflicted on itself is retried once instead of
# being reported as a workload failure.

set -euo pipefail

readonly kubectl_bin="${KUBECTL:-kubectl}"
readonly reconcile_bin="${RECONCILE_BIN:-./scripts/run-ksail-prod-with-pull-auth.sh}"
readonly rollout_timeout="${FLUX_CONTROL_PLANE_ROLLOUT_TIMEOUT:-10m}"
# The snapshots run while this deploy may be restarting the API server's clients,
# so an unbounded read is exactly the wrong default: kubectl's --request-timeout
# defaults to 0 (wait forever), which would hang the deploy instead of failing it.
readonly kubectl_request_timeout="${FLUX_CONTROL_PLANE_QUERY_TIMEOUT:-30s}"
# The four controllers carry this label; flux-operator does not (it is installed
# by Helm rather than rendered by the FluxInstance) and is therefore read by name.
# tofu-controller carries neither and is deliberately out of scope: it is being
# retired and its HelmRelease is expected to stay non-Ready, so waiting on it
# would hang a healthy deploy.
readonly controller_selector='app.kubernetes.io/part-of=flux'
readonly operator_deployment='flux-operator'

kubectl_prod() {
  "${kubectl_bin}" --context admin@prod "$@"
}

# name=generation for every Flux control-plane Deployment, sorted so the snapshot
# is comparable as plain text.
control_plane_generations() {
  # Each query is captured and checked on its own. Grouping them and piping the
  # group reports only the PIPELINE's status, so a failed first query would be
  # masked by a successful second one and yield a PARTIAL snapshot. Being
  # non-empty, that partial would compare unequal to a complete one and buy a
  # retry no restart evidence supports — and it would also make the unreadable
  # case pass the emptiness guard below instead of failing closed.
  local controllers operator
  controllers="$(kubectl_prod -n flux-system get deploy \
    -l "${controller_selector}" \
    --request-timeout="${kubectl_request_timeout}" \
    -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.generation}{"\n"}{end}')" || return 1
  operator="$(kubectl_prod -n flux-system get deploy "${operator_deployment}" \
    --ignore-not-found \
    --request-timeout="${kubectl_request_timeout}" \
    -o jsonpath='{.metadata.name}={.metadata.generation}{"\n"}')" || return 1
  printf '%s\n%s\n' "${controllers}" "${operator}" | grep -v '^$' | sort
}

wait_for_control_plane_rollout() {
  local entry name
  while IFS= read -r entry; do
    [[ -n "${entry}" ]] || continue
    name="${entry%%=*}"
    printf 'Waiting for %s to finish rolling out...\n' "${name}"
    # A rollout that does not settle is itself a real problem: let it fail.
    kubectl_prod -n flux-system rollout status "deploy/${name}" \
      --timeout="${rollout_timeout}"
  done <<<"$1"
}

reconcile() {
  "${reconcile_bin}" workload reconcile
}

# `--snapshot-baseline` prints the snapshot and exits, so the deploy can capture
# a baseline BEFORE it publishes the mutable tag. It deliberately reuses the same
# producer rather than a second implementation: the comparison below is textual,
# so a baseline rendered by different code could differ for reasons that are not
# a restart and buy a retry no evidence supports.
if [[ "${1:-}" == "--snapshot-baseline" ]]; then
  # A baseline that cannot be read must not become a new way for the deploy to
  # fail; the wrapper simply falls back to reading the generations itself.
  if ! control_plane_generations; then
    printf 'Could not read the Flux control-plane baseline before publishing; the reconcile will read it itself.\n' >&2
  fi
  exit 0
fi

# Prefer a baseline captured before the mutable tag was published. Flux may
# observe the new revision before this wrapper even starts — the deploy composite
# says so where it suspends autoscaling "before the mutable artifact is
# published" — and a control-plane rollout that began in that window is already
# reflected in a snapshot taken here. `before` and `after` would then be equal,
# and the cancellation this deploy inflicted on itself would be reported as a
# genuine failure: exactly the outage this wrapper exists to prevent (#3478).
#
# A snapshot that cannot be read must not become a new way for the deploy to
# fail: without it there is simply no restart evidence, so the reconcile runs
# exactly as it did before this wrapper existed and is never retried.
before=""
baseline_file="${FLUX_CONTROL_PLANE_BASELINE_FILE:-}"
if [[ -n "${baseline_file}" && -s "${baseline_file}" ]]; then
  before="$(cat "${baseline_file}")"
  printf 'Using the Flux control-plane baseline captured before this deploy published its manifests.\n' >&2
elif ! before="$(control_plane_generations)"; then
  before=""
  printf 'Could not read the Flux control-plane generations before reconciling; a failure will not be retried.\n' >&2
fi

set +e
reconcile
reconcile_status=$?
set -e

if [[ "${reconcile_status}" -eq 0 ]]; then
  exit 0
fi

after=""
if ! after="$(control_plane_generations)"; then
  after=""
fi

# Retry only on positive evidence: both snapshots readable, non-empty, and
# different. An unreadable or empty snapshot proves nothing, so it fails closed
# onto the original single-attempt behaviour rather than buying a free retry.
if [[ -z "${before}" || -z "${after}" ]]; then
  printf 'Reconciliation failed and the control-plane generations could not be compared, so this is treated as a genuine failure — not retrying.\n' >&2
  exit "${reconcile_status}"
fi

if [[ "${before}" == "${after}" ]]; then
  printf 'Reconciliation failed and the Flux control plane did not restart, so this is a genuine failure — not retrying.\n' >&2
  exit "${reconcile_status}"
fi

printf 'Reconciliation failed while this deploy restarted the Flux control plane:\n' >&2
diff <(printf '%s\n' "${before}") <(printf '%s\n' "${after}") >&2 || true
printf 'Waiting for the control plane to settle, then retrying the reconcile once.\n' >&2

wait_for_control_plane_rollout "${after}"

reconcile
