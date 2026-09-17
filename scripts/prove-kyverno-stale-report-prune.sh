#!/usr/bin/env bash
# Proves, on a throwaway cluster, that tests/kyverno-stale-report-prune/deleting-policy.yaml
# removes a Kyverno result that a name exclusion left stale, that the next scan
# recreates the report with only its current results, and that a report whose
# results are all current is left alone.
#
# Requires kubectl, helm and a cluster in the current kubeconfig context with
# nothing else on it. CI creates one with kind (.github/workflows/prove-kyverno-stale-report-prune.yaml).
set -euo pipefail

dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/tests/kyverno-stale-report-prune"
chart_version="${KYVERNO_CHART_VERSION:?set KYVERNO_CHART_VERSION}"
# Short enough to finish in minutes, still several one-minute scan intervals.
stale_after="${STALE_AFTER:-4m}"
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

kubectl apply -f "$dir/cleanup-controller-role.yaml"
kubectl apply -f "$dir/fixtures.yaml"

both="require-owner-label/owner-label require-team-label/team-label"
wait_for "both results reported for excluded-later" 600 has_results excluded-later "$both"
wait_for "both results reported for always-current" 600 has_results always-current "$both"

log "excluding excluded-later from require-team-label"
kubectl patch clusterpolicy require-team-label --type=json -p '[{"op":"add","path":"/spec/rules/0/exclude","value":{"any":[{"resources":{"names":["excluded-later"]}}]}}]'

# Reproduce the defect before relying on the fix: three scans later the
# excluded rule's result must still be there.
sleep 180
has_results excluded-later "$both" || fail "defect did not reproduce: the stale result cleared without the policy"
log "ok: defect reproduced, stale result survives three scans"

control_before="$(report_state always-current)"
control_before="${control_before%% *}"

log "applying the deleting policy (stale after $stale_after, every minute)"
sed -e "s/STALE_AFTER/$stale_after/" -e 's#schedule: ".*"#schedule: "* * * * *"#' "$dir/deleting-policy.yaml" | kubectl apply -f -

wait_for "excluded-later's stale result pruned and its report recreated with only the current result" 900 \
  has_results excluded-later "require-owner-label/owner-label"

control_after="$(report_state always-current)"
[[ "${control_after%% *}" == "$control_before" ]] ||
  fail "the control report was deleted although all its results were current ($control_before -> ${control_after%% *})"
has_results always-current "$both" || fail "the control report lost a current result"
log "ok: control report untouched"

log "PASS"
