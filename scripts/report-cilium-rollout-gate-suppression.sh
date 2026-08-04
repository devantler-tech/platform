#!/usr/bin/env bash
# Report, in the deploy's own job summary, that the Cilium rollout gate has
# suppressed the Talos machine-config sync — and warn once that suppression
# outlives its intended window.
#
# The coupling itself is deliberate and stays: KSail owns Cluster Autoscaler and
# could reconcile it back to one replica mid-rollout, racing a scale-up before
# the first Cilium canary is verified. What is not acceptable is that it was
# INVISIBLE. `cluster update` is the deploy's only Talos machine-config sync, and
# a skipped step inside a green deploy is indistinguishable from a step that ran
# and did nothing — so machine config in Git silently stopped being machine
# config on the nodes, and the divergence grew with no signal anywhere
# (platform#2951).
#
# Releasing the gate stays an operational judgement about the Cilium rollout, so
# this never fails the deploy. It makes the suppression legible and bounded.

set -euo pipefail

root_dir="${PLATFORM_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
readonly root_dir
readonly controllers_kustomization="${root_dir}/k8s/providers/hetzner/infrastructure/controllers/kustomization.yaml"
readonly activation_marker='platform.devantler.tech/rollout-gate-activated:'

# The component's runbook is "step nine nodes one at a time, verifying each, then
# soak". A week is generous for that. Past it, the rollout is not progressing as
# designed and the un-synced machine-config delta is the larger risk.
readonly warn_after_days=7

readonly gate_active="${CILIUM_ROLLOUT_GATE_ACTIVE:-false}"

fail() {
  printf '::error::%s\n' "$1"
  exit 1
}

# Nothing was suppressed, so claim nothing. Staying silent here is what keeps
# the notice meaningful when it does appear.
[[ "${gate_active}" == true ]] || exit 0

# The declared activation date lives beside the component reference, so it is
# reviewed in the same diff that activates or releases the gate.
if [[ -n "${ROLLOUT_GATE_ACTIVATED_OVERRIDE:-}" ]]; then
  activated="${ROLLOUT_GATE_ACTIVATED_OVERRIDE}"
else
  [[ -f "${controllers_kustomization}" ]] ||
    fail "cannot read ${controllers_kustomization} to date the rollout gate"
  # head -1: the marker is prose-adjacent, so take the first declaration rather
  # than letting a second mention of it anywhere in the file change the answer.
  marker_line="$(grep -F "${activation_marker}" "${controllers_kustomization}" | head -1 || true)"
  [[ -n "${marker_line}" ]] ||
    fail "the rollout gate is active but ${activation_marker} <YYYY-MM-DD> is not declared beside the component reference"
  # Parameter expansion, not sed: the marker contains '/', which would collide
  # with sed's substitution delimiter.
  activated="${marker_line##*"${activation_marker}"}"
  activated="${activated#"${activated%%[![:space:]]*}"}"
  activated="${activated%%[[:space:]]*}"
fi
readonly activated

[[ "${activated}" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] ||
  fail "the rollout gate activation date is not an ISO date: '${activated}'"

# GNU and BSD date disagree on parsing; support both so this behaves the same on
# a runner and on a maintainer's Mac.
to_epoch() {
  date -u -d "$1" +%s 2>/dev/null || date -u -j -f '%Y-%m-%d' "$1" +%s 2>/dev/null
}

activated_epoch="$(to_epoch "${activated}")" ||
  fail "could not parse the rollout gate activation date: '${activated}'"
[[ -n "${activated_epoch}" ]] ||
  fail "could not parse the rollout gate activation date: '${activated}'"
now_epoch="$(date -u +%s)"
elapsed_days=$(((now_epoch - activated_epoch) / 86400))
readonly activated_epoch now_epoch elapsed_days

# A future activation date is a typo, and it would silently buy the rollout
# unlimited extra time — the elapsed count goes negative and never trips the
# bound. Fail closed rather than report a suppression as fresh forever.
((elapsed_days >= 0)) ||
  fail "the rollout gate activation date is in the future: '${activated}' (${elapsed_days} days)"

summary="${GITHUB_STEP_SUMMARY:-/dev/stdout}"

# The backticks below are Markdown code spans for the job summary, not command
# substitution, so single quotes are deliberate — expanding them is exactly what
# must not happen.
# shellcheck disable=SC2016
{
  printf '## ⏸️ Talos machine-config sync was SUPPRESSED this deploy\n\n'
  printf 'The Cilium homogeneous-device rollout gate is active, so '
  printf '`ksail cluster update` — this deploy pipeline'"'"'s only Talos '
  printf 'machine-config sync — did not run. **Machine config in Git is not '
  printf 'necessarily machine config on the nodes.**\n\n'
  printf '| | |\n|---|---|\n'
  printf '| Gate active since | `%s` |\n' "${activated}"
  printf '| Elapsed | **%s days** |\n' "${elapsed_days}"
  printf '| Warn threshold | %s days |\n' "${warn_after_days}"
  printf '\nThis also blocks anything that depends on `cluster update` applying '
  printf 'configuration to the live cluster — including the root OCIRepository '
  printf 'cosign `verify` block (platform#2922, platform#2938).\n\n'
  printf 'Release is an operational judgement about the Cilium rollout, not '
  printf 'something a deploy decides: follow step 4–5 of the runbook in '
  printf '`k8s/providers/hetzner/infrastructure/controllers/cilium/components/homogeneous-devices/kustomization.yaml`.\n'
} >>"${summary}"

if ((elapsed_days > warn_after_days)); then
  printf '::warning::Talos machine-config sync has been suppressed by the Cilium rollout gate for %s days (threshold %s). Machine config in Git is diverging from the nodes, and platform#2922/#2938 are blocked behind it. Finish or roll back the rollout — see the component runbook.\n' \
    "${elapsed_days}" "${warn_after_days}"
fi

exit 0
