#!/usr/bin/env bash
# Exercise the real cleanup command with a fake Kubernetes API, including races
# that must never delete a live, replacement or changed object.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin"
cat >"$work/base.json" <<'JSON'
{"apiVersion":"autoscaling.k8s.io/v1","kind":"VerticalPodAutoscaler","metadata":{"name":"old-deployment","namespace":"apps-ns","uid":"00000000-0000-0000-0000-000000000001","resourceVersion":"42","labels":{"app.kubernetes.io/managed-by":"kyverno","generate.kyverno.io/policy-name":"auto-vpa-deployment","generate.kyverno.io/trigger-group":"apps","generate.kyverno.io/trigger-version":"v1","generate.kyverno.io/trigger-kind":"Deployment","generate.kyverno.io/trigger-name":"old","generate.kyverno.io/trigger-namespace":"apps-ns","generate.kyverno.io/trigger-uid":"00000000-0000-0000-0000-000000000002"}},"spec":{"targetRef":{"apiVersion":"apps/v1","kind":"Deployment","name":"old"}}}
JSON
cat >"$work/bin/kubectl" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$CASE/calls"
while [[ "$1" == --* ]]; do
  if [[ "$1" == --context ]]; then shift 2; else shift; fi
done
verb="$1"; shift
if [[ "$verb" == delete ]]; then
  [[ "$*" == --raw\ /apis/autoscaling.k8s.io/v1/namespaces/apps-ns/verticalpodautoscalers/old-deployment\ --filename\ * ]] || exit 80
  jq -e '.apiVersion=="v1" and .kind=="DeleteOptions" and .preconditions.uid=="00000000-0000-0000-0000-000000000001" and .preconditions.resourceVersion=="42"' "${@: -1}" >/dev/null
  [[ "$SCENARIO" != delete-failed ]] || exit 1
  touch "$CASE/deleted"
  exit 0
fi
[[ "$verb" == get ]] || exit 81
resource="$1"; shift
if [[ "$resource" == generatingpolicies.policies.kyverno.io ]]; then
  if [[ "$SCENARIO" == undeployed ]]; then
    printf '{"spec":{"generate":[]}}'
  else
    yq -o=json '.' "$ROOT/k8s/bases/infrastructure/cluster-policies/best-practices/$1.yaml"
  fi
elif [[ "$resource" == verticalpodautoscalers.autoscaling.k8s.io && "$1" == --all-namespaces ]]; then
  [[ "$SCENARIO" != bad-list ]] || { printf '{"items":null}'; exit 0; }
  if [[ "$SCENARIO" == partial-read ]]; then
    jq -s '{items:[.[0],(.[0] | .metadata.name="second-deployment" |
      .spec.targetRef.name="second" | .metadata.labels["generate.kyverno.io/trigger-name"]="second")]}' "$CASE/vpa.json"
  else
    jq -s '{items:.}' "$CASE/vpa.json"
  fi
elif [[ "$resource" == verticalpodautoscalers.autoscaling.k8s.io ]]; then
  [[ ! -f "$CASE/deleted" || "$SCENARIO" == leftover ]] || exit 0
  case "$SCENARIO" in
    changed-uid) jq '.metadata.uid="00000000-0000-0000-0000-000000000099"' "$CASE/vpa.json" ;;
    changed-version) jq '.metadata.resourceVersion="43"' "$CASE/vpa.json" ;;
    *) cat "$CASE/vpa.json" ;;
  esac
elif [[ "$resource" == deployments.apps ]]; then
  count="$(cat "$CASE/target-count" 2>/dev/null || printf 0)"
  count=$((count + 1)); printf '%s' "$count" >"$CASE/target-count"
  [[ "$SCENARIO" != forbidden ]] || exit 1
  [[ "$SCENARIO" != partial-read || "$1" != second ]] || exit 1
  [[ "$SCENARIO" != malformed-target ]] || { printf '{"kind":"Deployment"}'; exit 0; }
  if [[ "$SCENARIO" == live || "$SCENARIO" == replacement || ( "$SCENARIO" == target-returned && "$count" -gt 1 ) ]]; then
    uid=00000000-0000-0000-0000-000000000002
    [[ "$SCENARIO" != replacement ]] || uid=00000000-0000-0000-0000-000000000003
    jq -n --arg uid "$uid" '{apiVersion:"apps/v1",kind:"Deployment",metadata:{name:"old",namespace:"apps-ns",uid:$uid}}'
  fi
else
  exit 82
fi
MOCK
chmod +x "$work/bin/kubectl"
export ROOT="$root" PATH="$work/bin:$PATH"
export GITHUB_REPOSITORY=devantler-tech/platform GITHUB_REF=refs/heads/main
export GITHUB_EVENT_NAME=workflow_dispatch GITHUB_RUN_ATTEMPT=1
export VPA_CLEANUP_CONFIRM=cleanup-orphan-generated-vpas
run_case() {
  local scenario="$1" mode="$2" expected="$3" deleted="$4" rc=0
  export CASE="$work/$scenario-$mode" SCENARIO="$scenario"
  mkdir -p "$CASE"; cp "$work/base.json" "$CASE/vpa.json"
  case "$scenario" in
    wrong-label) jq '.metadata.labels["generate.kyverno.io/trigger-namespace"]="other"' "$CASE/vpa.json" >"$CASE/new"; mv "$CASE/new" "$CASE/vpa.json" ;;
    wrong-target) jq '.spec.targetRef.name="other"' "$CASE/vpa.json" >"$CASE/new"; mv "$CASE/new" "$CASE/vpa.json" ;;
    invalid-uid) jq '.metadata.labels["generate.kyverno.io/trigger-uid"]=""' "$CASE/vpa.json" >"$CASE/new"; mv "$CASE/new" "$CASE/vpa.json" ;;
    owned) jq '.metadata.ownerReferences=[{"apiVersion":"apps/v1","kind":"Deployment","name":"old","uid":"00000000-0000-0000-0000-000000000002"}]' "$CASE/vpa.json" >"$CASE/new"; mv "$CASE/new" "$CASE/vpa.json" ;;
  esac
  bash "$root/scripts/cleanup-orphan-generated-vpas.sh" "$mode" >"$CASE/output" 2>&1 || rc=$?
  [[ "$rc" == "$expected" ]] || { cat "$CASE/output"; echo "FAIL: $scenario/$mode exit $rc expected $expected"; exit 1; }
  if [[ "$deleted" == yes ]]; then [[ -f "$CASE/deleted" ]]; else [[ ! -f "$CASE/deleted" ]]; fi
  if grep -Eq 'apps-ns|old-deployment|00000000-|Forbidden|https?://' "$CASE/output"; then
    echo "FAIL: $scenario exposed private input"; exit 1
  fi
  echo "ok: $scenario/$mode"
}
run_case orphan assess 0 no
run_case orphan apply 0 yes
run_case live apply 0 no
run_case replacement apply 0 no
run_case owned apply 0 no
for scenario in forbidden bad-list partial-read malformed-target; do run_case "$scenario" apply 2 no; done
for scenario in wrong-label wrong-target invalid-uid changed-uid changed-version target-returned undeployed; do run_case "$scenario" apply 1 no; done
run_case delete-failed apply 2 no
run_case leftover apply 1 yes
export GITHUB_REF=refs/heads/unreviewed
run_case unreviewed apply 1 no
[[ ! -f "$CASE/calls" ]] || { echo 'FAIL: unreviewed dispatch reached the API'; exit 1; }
echo 'OK: generated VPA cleanup preserves live and changed objects'

# Execute the actual workflow guard, including every branch before checkout or
# credentials, rather than accepting a reassuring substring in the workflow.
workflow="$root/.github/workflows/cleanup-orphan-generated-vpas.yaml"
yq -o=json '.' "$workflow" | jq -e '(.on | keys) == ["workflow_dispatch"] and .permissions == {} and
  .concurrency.group == "prod-deploy" and .concurrency["cancel-in-progress"] == false and
  .jobs.cleanup.environment == "prod" and .jobs.cleanup.permissions == {"contents":"read"} and
  .jobs.cleanup.steps[1].with["persist-credentials"] == false and
  .jobs.cleanup.steps[1].with.ref == "${{ github.sha }}"' >/dev/null
yq -r '.jobs.cleanup.steps[0].run' "$workflow" >"$work/guard.sh"
export GITHUB_REF=refs/heads/main VPA_CLEANUP_MODE=assess
bash "$work/guard.sh"
for invalid in branch repository event attempt confirm mode; do
  if (
    case "$invalid" in
      branch) export GITHUB_REF=refs/heads/unreviewed ;;
      repository) export GITHUB_REPOSITORY=outside/platform ;;
      event) export GITHUB_EVENT_NAME=push ;;
      attempt) export GITHUB_RUN_ATTEMPT=2 ;;
      confirm) export VPA_CLEANUP_CONFIRM=wrong ;;
      mode) export VPA_CLEANUP_MODE=delete-everything ;;
    esac
    bash "$work/guard.sh"
  ) >/dev/null 2>&1; then
    echo "FAIL: the workflow accepted $invalid"; exit 1
  fi
done
echo 'OK: the main-only workflow guard refuses every invalid dispatch control'
