#!/usr/bin/env bash
# Proves, on a throwaway cluster, that the production DeletingPolicy
# (k8s/bases/infrastructure/deleting-policies/prune-stale-policy-reports.yaml)
# removes a Kyverno result that a name exclusion left stale, that the next scan
# recreates the report with only its current results, that a report whose
# results are all current is left alone, and that a report the scan no longer
# writes any current result to keeps its failures.
#
# Requires kubectl, helm and a cluster in the current kubeconfig context with
# nothing else on it. CI creates one with kind (.github/workflows/prove-kyverno-stale-report-prune.yaml).
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dir="$root/tests/kyverno-stale-report-prune"
# The shipped policy and its delete grant themselves, so this proof cannot drift
# from what deploys. Only the policy's two windows and schedule are shortened below.
policy="$root/k8s/bases/infrastructure/deleting-policies/prune-stale-policy-reports.yaml"
cleanup_role="$root/k8s/bases/infrastructure/cluster-roles/kyverno-cleanup-policy-reports.yaml"
chart_version="${KYVERNO_CHART_VERSION:?set KYVERNO_CHART_VERSION}"
# Short enough to finish in minutes, still several one-minute scan intervals.
stale_after="${STALE_AFTER:-4m}"
# Two one-minute scan intervals, as production uses two one-hour intervals.
rescanned_within="${RESCANNED_WITHIN:-2m}"
ns=prune-test

log() { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
fail() {
  log "FAIL: $*"
  kubectl get policyreports -n "$ns" -o yaml || true
  kubectl get deletingpolicies.policies.kyverno.io -o yaml || true
  kubectl -n kyverno logs deploy/kyverno-cleanup-controller --tail=200 || true
  kubectl -n kyverno logs deploy/kyverno-reports-controller --tail=100 || true
  exit 1
}

# Prints "<report uid> <space-separated policy/rule list>" for a ConfigMap's report,
# or nothing when the report does not exist.
report_state() {
  local cm_uid
  cm_uid="$(kubectl -n "$ns" get configmap "$1" -o jsonpath='{.metadata.uid}')"
  kubectl -n "$ns" get policyreport "$cm_uid" -o json 2>/dev/null |
    jq -r '[.metadata.uid, ([.results[]? | "\(.policy)/\(.rule)"] | sort | join(" "))] | join(" ")' || true
}

# Prints the unix seconds of one result's timestamp in a ConfigMap's report.
result_timestamp() { # <configmap> <policy> <rule>
  local cm_uid
  cm_uid="$(kubectl -n "$ns" get configmap "$1" -o jsonpath='{.metadata.uid}')"
  kubectl -n "$ns" get policyreport "$cm_uid" -o json 2>/dev/null |
    jq -r --arg policy "$2" --arg rule "$3" \
      'first(.results[]? | select(.policy == $policy and .rule == $rule) | .timestamp.seconds) // empty' ||
    true
}

# wait_for <description> <seconds> <command...>: retries the command every 10s.
wait_for() {
  local what="$1" budget="$2"
  shift 2
  local deadline=$((SECONDS + budget))
  until "$@"; do
    ((SECONDS < deadline)) || fail "timed out after ${budget}s waiting for: $what"
    sleep 10
  done
  log "ok: $what"
}

has_results() { # <configmap> <expected sorted policy/rule list>
  local state
  state="$(report_state "$1")"
  [[ "${state#* }" == "$2" ]]
}

log "installing kyverno chart $chart_version"
helm repo add kyverno https://kyverno.github.io/kyverno/ >/dev/null
helm upgrade --install kyverno kyverno/kyverno --version "$chart_version" \
  -n kyverno --create-namespace -f "$dir/values.yaml" --wait --timeout 10m >/dev/null

kubectl apply -f "$cleanup_role"
kubectl apply -f "$dir/fixtures.yaml"

both="require-owner-label/owner-label require-team-label/team-label"
wait_for "both results reported for excluded-later" 600 has_results excluded-later "$both"
wait_for "both results reported for never-rescanned" 600 has_results never-rescanned "$both"
wait_for "both results reported for always-current" 600 has_results always-current "$both"

team_before="$(result_timestamp excluded-later require-team-label team-label)"
owner_before="$(result_timestamp excluded-later require-owner-label owner-label)"
[[ -n "$team_before" && -n "$owner_before" ]] || fail "could not read the result timestamps to compare against"

never_owner_before="$(result_timestamp never-rescanned require-owner-label owner-label)"
[[ -n "$never_owner_before" ]] || fail "could not read never-rescanned's owner-label timestamp"

log "excluding excluded-later from require-team-label, and never-rescanned from both policies"
kubectl patch clusterpolicy require-team-label --type=json -p '[{"op":"add","path":"/spec/rules/0/exclude","value":{"any":[{"resources":{"names":["excluded-later","never-rescanned"]}}]}}]'
kubectl patch clusterpolicy require-owner-label --type=json -p '[{"op":"add","path":"/spec/rules/0/exclude","value":{"any":[{"resources":{"names":["never-rescanned"]}}]}}]'

# Reproduce the defect before relying on the fix. A scan that has actually run
# since the exclusion rewrites the still-evaluated result, so waiting for the
# owner-label timestamp to advance proves a scan completed — a plain sleep would
# not, and the stale result could then simply be one the scanner never reached.
scan_ran() {
  local now
  now="$(result_timestamp excluded-later require-owner-label owner-label)"
  [[ -n "$now" ]] && ((now > owner_before))
}

wait_for "a background scan to complete after the exclusion" 600 scan_ran

team_after="$(result_timestamp excluded-later require-team-label team-label)"
[[ "$team_after" == "$team_before" ]] ||
  fail "the excluded rule's result was rewritten ($team_before -> $team_after), so it is not stale"
has_results excluded-later "$both" || fail "defect did not reproduce: the stale result cleared without the policy"
[[ "$(result_timestamp never-rescanned require-owner-label owner-label)" == "$never_owner_before" ]] ||
  fail "never-rescanned's owner-label result was rewritten, so that report is still being scanned"
has_results never-rescanned "$both" || fail "never-rescanned lost its results without the policy"
log "ok: defect reproduced, the excluded rule's result survives a completed scan unchanged"

control_before="$(report_state always-current)"
control_before="${control_before%% *}"
stale_report_uid="$(report_state excluded-later)"
stale_report_uid="${stale_report_uid%% *}"
never_before="$(report_state never-rescanned)"
never_before="${never_before%% *}"

log "applying the deleting policy (stale after $stale_after, rescanned within $rescanned_within, every minute)"
grep -qF "duration('6h')" "$policy" || fail "the production policy no longer carries duration('6h'); update this substitution"
grep -qF "duration('2h')" "$policy" || fail "the production policy no longer carries duration('2h'); update this substitution"
sed -e "s/duration('6h')/duration('$stale_after')/" -e "s/duration('2h')/duration('$rescanned_within')/" \
  -e 's#schedule: ".*"#schedule: "* * * * *"#' "$policy" | kubectl apply -f -

wait_for "excluded-later's stale result pruned and its report recreated with only the current result" 900 \
  has_results excluded-later "require-owner-label/owner-label"

recreated_uid="$(report_state excluded-later)"
[[ "${recreated_uid%% *}" != "$stale_report_uid" ]] ||
  fail "the report still carries its original uid ($stale_report_uid), so it was never deleted and recreated"
log "ok: report deleted and recreated ($stale_report_uid -> ${recreated_uid%% *})"

control_after="$(report_state always-current)"
[[ "${control_after%% *}" == "$control_before" ]] ||
  fail "the control report was deleted although all its results were current ($control_before -> ${control_after%% *})"
has_results always-current "$both" || fail "the control report lost a current result"
log "ok: control report untouched"

# excluded-later was pruned by a run that also evaluated never-rescanned, whose
# failures are at least as old. Let a few more runs pass before judging it.
sleep 180
never_after="$(report_state never-rescanned)"
[[ "${never_after%% *}" == "$never_before" ]] ||
  fail "never-rescanned was deleted although no scan wrote a current result to it ($never_before -> ${never_after%% *})"
has_results never-rescanned "$both" || fail "never-rescanned lost a failing result"
log "ok: a report with no current result keeps its failures"

log "PASS"
