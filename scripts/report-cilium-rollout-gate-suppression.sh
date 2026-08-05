#!/usr/bin/env bash
# Report, in the deploy's own job summary, that the Cilium rollout gate has
# suppressed the Talos machine-config sync — and escalate, from a warning to a
# failed deploy, once that suppression outlives its intended window.
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
# the deploy asks before it insists: it warns from `warn_after_days`, and only
# once the suppression passes `fail_after_days` does it fail. That escalation is
# the point — a warning inside an otherwise-green deploy is not a forcing
# function, and the suppression has to end in something other than silence.

set -euo pipefail

root_dir="${PLATFORM_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
readonly root_dir
readonly controllers_kustomization="${root_dir}/k8s/providers/hetzner/infrastructure/controllers/kustomization.yaml"
readonly activation_marker='platform.devantler.tech/rollout-gate-activated:'

# The component's runbook is "step nine nodes one at a time, verifying each, then
# soak". A week is generous for that. Past it, the rollout is not progressing as
# designed and the un-synced machine-config delta is the larger risk.
readonly warn_after_days=7

# Past this, the deploy FAILS rather than warning inside a green run. A warning
# in a green job is not a forcing function: the gate ran 10 days over its warn
# threshold while every deploy stayed green and silently skipped its only Talos
# machine-config sync, which is exactly how that went unnoticed (platform#2963).
# The gap between the two thresholds is deliberate — a week of warnings first,
# so the escalation is never a surprise — and it is measured from the reviewed
# activation marker in Git, so completing or rolling back the rollout is the
# only way to clear it. Raising this constant is not a resolution.
readonly fail_after_days="${CILIUM_ROLLOUT_FAIL_AFTER_DAYS_OVERRIDE:-14}"

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
  # Require EXACTLY one declaration. Silently taking the first would let a second
  # marker — a stale one left behind by a previous rollout, say — sit in the file
  # while the reporter quietly dates the suppression from whichever came first.
  marker_lines="$(grep -F "${activation_marker}" "${controllers_kustomization}" || true)"
  marker_count="$(printf '%s' "${marker_lines}" | grep -c . || true)"
  [[ "${marker_count}" -ge 1 ]] ||
    fail "the rollout gate is active but ${activation_marker} <YYYY-MM-DD> is not declared beside the component reference"
  [[ "${marker_count}" -eq 1 ]] ||
    fail "the rollout gate activation date is declared ${marker_count} times; exactly one ${activation_marker} is required"
  marker_line="${marker_lines}"
  # Parameter expansion, not sed: the marker contains '/', which would collide
  # with sed's substitution delimiter.
  activated="${marker_line##*"${activation_marker}"}"
  activated="${activated#"${activated%%[![:space:]]*}"}"
  activated="${activated%%[[:space:]]*}"
fi
readonly activated

[[ "${activated}" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] ||
  fail "the rollout gate activation date is not an ISO date: '${activated}'"

# Resolve a YYYY-MM-DD to its UTC MIDNIGHT epoch, on both GNU and BSD date.
#
# The explicit 00:00:00 is load-bearing, not decoration. BSD `date -j -f
# '%Y-%m-%d'` fills unspecified fields from the CURRENT time, so a bare date
# parses as that date at the present time-of-day, while GNU `date -d` parses it
# as midnight. Left implicit, the same marker yields day counts that differ by
# one between a maintainer's Mac and the Linux runner. Anchoring both sides to
# midnight makes the elapsed count an exact whole-day difference everywhere.
to_utc_midnight_epoch() {
  date -u -d "$1T00:00:00Z" +%s 2>/dev/null ||
    date -u -j -f '%Y-%m-%d %H:%M:%S' "$1 00:00:00" +%s 2>/dev/null
}

activated_epoch="$(to_utc_midnight_epoch "${activated}")" ||
  fail "could not parse the rollout gate activation date: '${activated}'"
[[ -n "${activated_epoch}" ]] ||
  fail "could not parse the rollout gate activation date: '${activated}'"
today_epoch="$(to_utc_midnight_epoch "$(date -u +%Y-%m-%d)")"
[[ -n "${today_epoch}" ]] || fail 'could not resolve the current UTC date'
readonly activated_epoch today_epoch

# A future activation date is a typo, and it would silently buy the rollout extra
# time. Compare the EPOCHS, before any division: bash truncates integer division
# toward zero, so a date one day ahead would otherwise yield elapsed_days == 0 —
# indistinguishable from "activated today".
((today_epoch >= activated_epoch)) ||
  fail "the rollout gate activation date is in the future: '${activated}'"

elapsed_days=$(((today_epoch - activated_epoch) / 86400))
readonly elapsed_days

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
  # State the hard bound too. The summary is where an operator meets this gate,
  # and a deploy that will start FAILING on a known day is exactly the thing
  # they need to see before it happens rather than on the day it does.
  printf '| Fail threshold | %s days |\n' "${fail_after_days}"
  printf '\nThis also blocks anything that depends on `cluster update` applying '
  printf 'configuration to the live cluster — including the root OCIRepository '
  printf 'cosign `verify` block (platform#2922, platform#2938).\n\n'
  printf 'Release is an operational judgement about the Cilium rollout, not '
  printf 'something a deploy decides: follow step 4–5 of the runbook in '
  printf '`k8s/providers/hetzner/infrastructure/controllers/cilium/components/homogeneous-devices/kustomization.yaml`.\n'
} >>"${summary}"

# >=, not >: warn_after_days=7 means "warn once this has run seven days", so the
# warning belongs on day seven. A strict > would silently make the real bound
# eight days, which is not what the constant says.
if ((elapsed_days >= warn_after_days)); then
  printf '::warning::Talos machine-config sync has been suppressed by the Cilium rollout gate for %s days (threshold %s). Machine config in Git is diverging from the nodes, and platform#2922/#2938 are blocked behind it. Finish or roll back the rollout — see the component runbook.\n' \
    "${elapsed_days}" "${warn_after_days}"
fi

# Escalate past the hard bound. Same `>=` reasoning as the warning above:
# fail_after_days=14 means "fail once this has run fourteen days", so the failure
# belongs ON day fourteen.
if ((elapsed_days >= fail_after_days)); then
  fail "the Cilium rollout gate has suppressed every Talos machine-config sync (\`ksail cluster update\`) for ${elapsed_days} days, beyond its ${fail_after_days}-day bound. A deploy will not silently skip its own config sync any longer. Resolve it by stepping the remaining Cilium agents onto the current DaemonSet revision, or by rolling the homogeneous-devices component back — see the component runbook. Raising the bound is not a resolution."
fi

exit 0
