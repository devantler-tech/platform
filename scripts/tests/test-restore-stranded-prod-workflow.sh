#!/usr/bin/env bash
# Pins the decision wiring of .github/workflows/restore-stranded-prod.yaml (#4186).
#
# scripts/prod-stranded-gate.sh and scripts/redeploy-main-when-behind.sh are tested on
# their own; what only the workflow decides is WHEN the redeploy runs. Each way of getting
# that wrong is silent: a redeploy on BEHIND or CONVERGED, a redeploy that ignores the gate
# (rolling back a group that has deployed but not merged), a gate read after the report,
# a job outside the prod-deploy lock, or a lost schedule.
#
# Every assertion is ABLATED: a copy of the workflow is mutated in exactly one place and the
# check must fail naming THAT assertion.
#
# yq (mikefarah v4) reads the YAML; no network, no secrets. Bash 3.2 compatible.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly workflow="${root_dir}/.github/workflows/restore-stranded-prod.yaml"
readonly job='.jobs["restore-stranded-prod"]'

work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v yq >/dev/null 2>&1 || fail 'yq is required'
[ -f "$workflow" ] || fail "missing ${workflow}"

# Prints the first violated assertion's name, or nothing when the workflow holds.
violation() {
  local file="$1" gate report redeploy
  [ "$(yq '.on | has("schedule")' "$file")" = true ] || { echo schedule; return; }
  [ "$(yq '.on | has("workflow_dispatch")' "$file")" = true ] || { echo dispatch; return; }
  [ "$(yq '.on | (has("push") or has("pull_request") or has("merge_group"))' "$file")" = false ] ||
    { echo event-triggers; return; }
  [ "$(yq "${job}.concurrency.group" "$file")" = prod-deploy ] || { echo lock; return; }
  [ "$(yq "${job}.concurrency.cancel-in-progress" "$file")" = false ] || { echo lock-cancel; return; }
  [ "$(yq "${job}.concurrency.queue" "$file")" = max ] || { echo lock-queue; return; }
  gate="$(yq "${job}.steps | to_entries | map(select(.value.id == \"gate\")) | .[0].key // -1" "$file")"
  report="$(yq "${job}.steps | to_entries | map(select(.value.id == \"report\")) | .[0].key // -1" "$file")"
  if [ "$gate" -lt 0 ] || [ "$report" -le "$gate" ]; then echo gate-before-report; return; fi
  [ "$(yq "${job}.steps[${gate}].run" "$file")" = ./scripts/prod-stranded-gate.sh ] || { echo gate-script; return; }
  [ "$(yq "${job}.steps[${report}].if" "$file")" = "steps.gate.outputs.stranded == 'true'" ] ||
    { echo report-gated; return; }
  redeploy="$(yq "[${job}.steps[] | select(.run == \"./scripts/redeploy-main-when-behind.sh\")] | length" "$file")"
  [ "$redeploy" = 1 ] || { echo single-redeploy; return; }
  [ "$(yq "${job}.steps[] | select(.run == \"./scripts/redeploy-main-when-behind.sh\") | .if" "$file")" = \
    "steps.gate.outputs.stranded == 'true' && steps.report.outputs.verdict == 'DIVERGED'" ] ||
    { echo redeploy-condition; return; }
}

got="$(violation "$workflow")"
[ -z "$got" ] || fail "the workflow violates '${got}'"
printf '  ok   the workflow holds every assertion\n'

# <assertion> <yq mutation>
ablate() {
  local want="$1" copy="${work_dir}/ablated.yaml" got
  cp "$workflow" "$copy"
  yq -i "$2" "$copy"
  got="$(violation "$copy")"
  [ "$got" = "$want" ] || fail "mutation '$2' should violate '${want}', got '${got:-nothing}'"
  printf '  ok   ablation caught: %s\n' "$want"
}

ablate schedule 'del(.on.schedule)'
ablate dispatch 'del(.on.workflow_dispatch)'
ablate event-triggers '.on.push = {"branches": ["main"]}'
ablate lock "${job}.concurrency.group = \"stranded\""
ablate lock-cancel "${job}.concurrency.cancel-in-progress = true"
ablate lock-queue "del(${job}.concurrency.queue)"
ablate gate-before-report "${job}.steps |= (map(select(.id == \"report\")) + map(select(.id != \"report\")))"
ablate gate-script "(${job}.steps[] | select(.id == \"gate\") | .run) = \"true\""
ablate report-gated "del(${job}.steps[] | select(.id == \"report\") | .if)"
ablate single-redeploy "${job}.steps += [{\"run\": \"./scripts/redeploy-main-when-behind.sh\"}]"
ablate redeploy-condition "(${job}.steps[] | select(.run == \"./scripts/redeploy-main-when-behind.sh\") | .if) = \"steps.report.outputs.verdict == 'DIVERGED'\""

printf 'restore-stranded-prod workflow: all assertions hold and every ablation is caught\n'
