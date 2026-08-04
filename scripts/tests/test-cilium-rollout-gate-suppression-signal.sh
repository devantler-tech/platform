#!/usr/bin/env bash
# The Cilium rollout gate suppresses the deploy's only Talos machine-config
# sync. That coupling is deliberate; its INVISIBILITY was not. A skipped step
# inside a green deploy is indistinguishable from a step that ran and did
# nothing, which is how the suppression survived ten days unnoticed and
# misdirected the diagnosis of two unrelated issues (platform#2951).
#
# These assertions pin that the gate can never suppress machine-config sync
# without saying so, and that the suppression is bounded rather than open-ended.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly report_script="${root_dir}/scripts/report-cilium-rollout-gate-suppression.sh"
readonly deploy_action="${root_dir}/.github/actions/deploy-prod/action.yml"
readonly controllers_kustomization="${root_dir}/k8s/providers/hetzner/infrastructure/controllers/kustomization.yaml"
readonly component_kustomization="${root_dir}/k8s/providers/hetzner/infrastructure/controllers/cilium/components/homogeneous-devices/kustomization.yaml"
readonly activation_marker='platform.devantler.tech/rollout-gate-activated:'

passed=0
fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}
ok() {
  passed=$((passed + 1))
}

line_of() {
  grep -nF -- "$2" "$1" | head -1 | cut -d: -f1
}

# --- the reporter exists and is wired into the deploy -----------------------

[[ -x "${report_script}" ]] ||
  fail 'the gate-suppression reporter must be an executable script'
ok

grep -Fq './scripts/report-cilium-rollout-gate-suppression.sh' "${deploy_action}" ||
  fail 'the deploy action must invoke the gate-suppression reporter'
ok

# It must report AFTER the step it describes, or it would announce a
# suppression before the deploy has actually reached that decision.
report_line="$(line_of "${deploy_action}" './scripts/report-cilium-rollout-gate-suppression.sh')"
cluster_update_line="$(line_of "${deploy_action}" 'run: ./scripts/run-ksail-prod-with-pull-auth.sh cluster update')"
readonly report_line cluster_update_line
[[ -n "${report_line}" && -n "${cluster_update_line}" ]] ||
  fail 'could not locate both the reporter and the cluster update step'
((cluster_update_line < report_line)) ||
  fail 'the reporter must run after the cluster update step it reports on'
ok

# The gate's own output is the single source of truth for "is it active".
# Re-deriving it in the reporter would let the two drift apart silently.
# shellcheck disable=SC2016
grep -Fq 'CILIUM_ROLLOUT_GATE_ACTIVE: ${{ steps.cilium_rollout_gate.outputs.active }}' "${deploy_action}" ||
  fail 'the reporter must take the gate state from the gate step output'
ok

# always(): a failure earlier in the deploy must not be able to hide the fact
# that machine-config sync was suppressed.
grep -Fq 'always() && steps.cilium_rollout_gate.outputs.active' "${deploy_action}" ||
  fail 'the reporter must run under always() so an earlier failure cannot hide the suppression'
ok

# --- the activation marker exists while the gate is active ------------------

component_referenced=false
if grep -Eq '^[[:space:]]*-[[:space:]]*cilium/components/homogeneous-devices/?[[:space:]]*(#.*)?$' \
  "${controllers_kustomization}"; then
  component_referenced=true
fi
on_delete=false
if grep -Eq '^[[:space:]]*type:[[:space:]]*OnDelete[[:space:]]*(#.*)?$' "${component_kustomization}"; then
  on_delete=true
fi

if [[ "${component_referenced}" == true && "${on_delete}" == true ]]; then
  marker_line="$(grep -F "${activation_marker}" "${controllers_kustomization}" || true)"
  [[ -n "${marker_line}" ]] ||
    fail "the rollout gate is ACTIVE, so ${activation_marker} <YYYY-MM-DD> must be declared beside the component reference"
  ok
  # Parameter expansion, not sed: the marker contains '/'.
  declared_date="${marker_line##*"${activation_marker}"}"
  declared_date="${declared_date#"${declared_date%%[![:space:]]*}"}"
  declared_date="${declared_date%%[[:space:]]*}"
  [[ "${declared_date}" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] ||
    fail "the activation marker must be an ISO date, got: '${declared_date}'"
  ok
else
  # Gate inactive: the marker must NOT be left behind claiming an active
  # rollout, or the next activation inherits a stale start date.
  ! grep -Fq "${activation_marker}" "${controllers_kustomization}" ||
    fail 'the rollout gate is inactive, so the activation marker must be removed'
  ok
  ok
fi

# --- the bound is real and enforced ----------------------------------------

grep -Eq '^readonly warn_after_days=[0-9]+$' "${report_script}" ||
  fail 'the reporter must declare an explicit warn_after_days threshold'
ok

threshold="$(sed -nE 's/^readonly warn_after_days=([0-9]+)$/\1/p' "${report_script}")"
readonly threshold
[[ -n "${threshold}" && "${threshold}" -gt 0 && "${threshold}" -le 30 ]] ||
  fail "the threshold must be a bound in (0,30] days, got: '${threshold}'"
ok

# --- behaviour: the reporter itself --------------------------------------

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

run_reporter() {
  # $1 active, $2 declared date; emits summary to $tmp/summary, stdout captured
  : >"${tmp}/summary"
  CILIUM_ROLLOUT_GATE_ACTIVE="$1" \
    ROLLOUT_GATE_ACTIVATED_OVERRIDE="$2" \
    GITHUB_STEP_SUMMARY="${tmp}/summary" \
    "${report_script}" >"${tmp}/out" 2>"${tmp}/err"
}

today="$(date -u +%Y-%m-%d)"
long_ago="$(date -u -v-90d +%Y-%m-%d 2>/dev/null || date -u -d '90 days ago' +%Y-%m-%d)"

# Inactive gate: nothing suppressed, so nothing claimed.
run_reporter false "${today}" || fail 'reporter must exit 0 when the gate is inactive'
[[ ! -s "${tmp}/summary" ]] ||
  fail 'an inactive gate must not write a suppression notice'
ok

# Active + fresh: must still announce the suppression in the job summary.
run_reporter true "${today}" || fail 'reporter must exit 0 for an active, in-bound gate'
grep -Fq 'machine-config sync' "${tmp}/summary" ||
  fail 'an active gate must name the suppressed sync in the job summary'
ok
grep -Fq "${today}" "${tmp}/summary" ||
  fail 'the summary must state when the suppression began'
ok
# In-bound: informative, but NOT a warning annotation.
! grep -q '^::warning' "${tmp}/out" ||
  fail 'an in-bound suppression must not emit a warning annotation'
ok

# Active + long overdue: must warn loudly, and still exit 0 (releasing the
# gate is an operational judgement, not something a deploy may decide).
run_reporter true "${long_ago}" || fail 'an over-threshold gate must warn, not fail the deploy'
grep -q '^::warning' "${tmp}/out" ||
  fail 'a suppression past the threshold must emit a ::warning:: annotation'
ok
grep -Fq '90' "${tmp}/summary" ||
  fail 'the summary must state the elapsed day count'
ok

# Fail closed: an active gate with an unreadable start date must NOT pass
# silently — that is exactly the invisible-suppression class this guards.
if run_reporter true 'not-a-date'; then
  fail 'an active gate with an unparseable activation date must fail closed'
fi
ok

# A future date would make elapsed negative, so the bound could never trip and
# the gate would buy itself unlimited time from a single typo.
future="$(date -u -v+30d +%Y-%m-%d 2>/dev/null || date -u -d '30 days' +%Y-%m-%d)"
if run_reporter true "${future}"; then
  fail 'an activation date in the future must fail closed, not read as perpetually fresh'
fi
ok

# The elapsed count must be an EXACT whole-day difference and identical on GNU
# and BSD date. BSD `date -j -f '%Y-%m-%d'` fills unspecified fields from the
# current time while GNU parses a bare date as midnight, so an implicit parse
# makes this off by one between a Mac and the Linux runner.
for probe_days in 1 3 9 40; do
  probe_date="$(date -u -v-"${probe_days}"d +%Y-%m-%d 2>/dev/null || date -u -d "${probe_days} days ago" +%Y-%m-%d)"
  run_reporter true "${probe_date}" || fail "reporter must exit 0 for a ${probe_days}-day-old gate"
  grep -Fq "**${probe_days} days**" "${tmp}/summary" ||
    fail "elapsed must be exactly ${probe_days} days for ${probe_date}, summary said otherwise"
done
ok

# NEXT-DAY specifically: bash truncates integer division toward zero, so a date
# one day ahead yields elapsed_days == 0 and reads as "activated today". The
# 30-day case above does NOT cover this — the guard must compare epochs, not the
# divided day count.
tomorrow="$(date -u -v+1d +%Y-%m-%d 2>/dev/null || date -u -d '1 day' +%Y-%m-%d)"
if run_reporter true "${tomorrow}"; then
  fail 'an activation date one day ahead must fail closed (integer division hides it as 0 days)'
fi
ok

# EXACT threshold: warn_after_days=7 means the warning belongs on day seven, not
# day eight. A strict > would silently make the real bound one day longer.
exactly_threshold="$(date -u -v-"${threshold}"d +%Y-%m-%d 2>/dev/null || date -u -d "${threshold} days ago" +%Y-%m-%d)"
run_reporter true "${exactly_threshold}" ||
  fail 'a suppression exactly at the threshold must still exit 0'
grep -q '^::warning' "${tmp}/out" ||
  fail 'the warning must fire ON the threshold day, not the day after'
ok

# One day INSIDE the bound must stay quiet, or the assertion above would pass
# for a reporter that simply always warns.
inside="$(date -u -v-"$((threshold - 1))"d +%Y-%m-%d 2>/dev/null || date -u -d "$((threshold - 1)) days ago" +%Y-%m-%d)"
run_reporter true "${inside}" || fail 'an in-bound suppression must exit 0'
! grep -q '^::warning' "${tmp}/out" ||
  fail 'a suppression one day inside the bound must not warn'
ok

# Duplicate marker: the reporter must not silently date the suppression from
# whichever declaration happens to come first.
fixture_root="${tmp}/root"
fixture_kustomization="${fixture_root}/k8s/providers/hetzner/infrastructure/controllers/kustomization.yaml"
mkdir -p "$(dirname "${fixture_kustomization}")"
{
  printf '  # %s 2026-07-26\n' "${activation_marker}"
  printf '  # %s 2026-01-01\n' "${activation_marker}"
  printf '  - cilium/components/homogeneous-devices/\n'
} >"${fixture_kustomization}"
: >"${tmp}/summary"
if CILIUM_ROLLOUT_GATE_ACTIVE=true PLATFORM_ROOT="${fixture_root}" \
  GITHUB_STEP_SUMMARY="${tmp}/summary" "${report_script}" >"${tmp}/out" 2>&1; then
  fail 'a duplicated activation marker must fail closed, not silently take the first'
fi
ok

# Control for the fixture: with exactly one marker the same path succeeds, so the
# assertion above is about duplication and not about the fixture being unreadable.
printf '  # %s 2026-07-26\n' "${activation_marker}" >"${fixture_kustomization}"
: >"${tmp}/summary"
CILIUM_ROLLOUT_GATE_ACTIVE=true PLATFORM_ROOT="${fixture_root}" \
  GITHUB_STEP_SUMMARY="${tmp}/summary" "${report_script}" >"${tmp}/out" 2>&1 ||
  fail 'a single marker read from PLATFORM_ROOT must succeed'
grep -Fq '2026-07-26' "${tmp}/summary" ||
  fail 'the fixture summary must carry the declared date'
ok

printf 'passed: %s failed: 0\n' "${passed}"
