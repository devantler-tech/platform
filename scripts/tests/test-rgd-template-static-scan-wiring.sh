#!/usr/bin/env bash
# Assert every event route invokes the nested-RGD behavioral gate. This script is run from the
# unconditional changes job, independently of the PR validation step whose presence it protects.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly REPO_ROOT
readonly CI_WORKFLOW="${RGD_WIRING_CI_WORKFLOW:-${REPO_ROOT}/.github/workflows/ci.yaml}"
readonly MAIN_WORKFLOW="${RGD_WIRING_MAIN_WORKFLOW:-${REPO_ROOT}/.github/workflows/validate-main.yaml}"
readonly SETUP_TRIVY="aquasecurity/setup-trivy@81e514348e19b6112ce2a7e3ecbafe19c1e1f567"
readonly BEHAVIORAL_RUN=$'shellcheck scripts/scan-rgd-templates.sh scripts/tests/test-rgd-template-static-scan.sh\nbash scripts/tests/test-rgd-template-static-scan.sh\n'
readonly WIRING_RUN=$'shellcheck scripts/tests/test-rgd-template-static-scan-wiring.sh\nbash scripts/tests/test-rgd-template-static-scan-wiring.sh\n'

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

command -v yq >/dev/null 2>&1 || fail "yq is required"
command -v jq >/dev/null 2>&1 || fail "jq is required"

ci_workflow_json="$(yq -o=json -I=0 '.' "$CI_WORKFLOW")"
main_workflow_json="$(yq -o=json -I=0 '.' "$MAIN_WORKFLOW")"

jq -e --arg wiring_run "$WIRING_RUN" '
  any(.jobs.changes.steps[]; (.run // "") == $wiring_run)
' <<<"$ci_workflow_json" >/dev/null ||
  fail "the unconditional changes job does not independently validate RGD workflow wiring"

jq -e --argjson required_inputs '[
    ".trivy/data/**",
    "scripts/scan-rgd-templates.sh",
    "scripts/rgd-template-static-scan-baseline.tsv",
    "scripts/tests/test-rgd-template-static-scan.sh",
    "scripts/tests/test-rgd-template-static-scan-wiring.sh"
  ]' '
  any(.jobs.changes.steps[];
      .id == "filter"
      and (.with.filters as $filters
        | [$required_inputs[] as $input | $filters | contains($input)] | all))
' <<<"$ci_workflow_json" >/dev/null ||
  fail "ci.yaml does not route every RGD gate input through k8s validation"

jq -e --arg setup "$SETUP_TRIVY" --arg behavior_run "$BEHAVIORAL_RUN" '
  .jobs.validate as $gate
  | (($gate.if // "") ==
      "github.event_name == '\''pull_request'\'' && (needs.changes.outputs.k8s == '\''true'\'' || needs.changes.outputs.bridge_validation == '\''true'\'')")
  and (($gate["continue-on-error"] // false) == false)
  and any($gate.steps[];
      (.uses // "") == $setup and .with.version == "v0.74.0")
  and any($gate.steps[];
      (.run // "") == $behavior_run
      and ((.["continue-on-error"] // false) == false)
      and ((.if // "") == "needs.changes.outputs.k8s == '\''true'\''"))
' <<<"$ci_workflow_json" >/dev/null ||
  fail "the pull-request validate job does not run the pinned RGD behavioral gate"

jq -e --arg setup "$SETUP_TRIVY" --arg behavior_run "$BEHAVIORAL_RUN" '
  .jobs["validate-rgd-templates-merge-group"] as $gate
  | (($gate.if // "") ==
      "github.event_name == '\''merge_group'\'' && needs.changes.outputs.k8s == '\''true'\''")
  and (($gate["continue-on-error"] // false) == false)
  and (($gate.needs | if type == "array" then . else [.] end) | index("changes") != null)
  and any($gate.steps[];
      (.uses // "") == $setup and .with.version == "v0.74.0")
  and any($gate.steps[];
      (.run // "") == $behavior_run
      and ((.["continue-on-error"] // false) == false)
      and (has("if") | not))
  and ((.jobs["deploy-prod"].needs | if type == "array" then . else [.] end)
      | index("validate-rgd-templates-merge-group") != null)
  and ((.jobs["ci-required-checks"].needs | if type == "array" then . else [.] end)
      | index("validate-rgd-templates-merge-group") != null)
' <<<"$ci_workflow_json" >/dev/null ||
  fail "ci.yaml does not gate merge-group deployment on the exact RGD behavioral scan"

jq -e --arg setup "$SETUP_TRIVY" --arg behavior_run "$BEHAVIORAL_RUN" '
  .jobs["validate-rgd-templates"] as $gate
  | (($gate["continue-on-error"] // false) == false)
  and ($gate | has("if") | not)
  and any($gate.steps[];
      (.uses // "") == $setup and .with.version == "v0.74.0")
  and any($gate.steps[];
      (.run // "") == $behavior_run
      and ((.["continue-on-error"] // false) == false)
      and (has("if") | not))
' <<<"$main_workflow_json" >/dev/null ||
  fail "validate-main.yaml does not run the pinned RGD behavioral gate"

jq -e --arg wiring_run "$WIRING_RUN" '
  any(
    .jobs | to_entries[];
    .key != "validate-rgd-templates"
    and ((.value["continue-on-error"] // false) == false)
    and (.value | has("if") | not)
    and any(.value.steps[]?;
      (.run // "") == $wiring_run
      and ((.["continue-on-error"] // false) == false)
      and (has("if") | not))
  )
' <<<"$main_workflow_json" >/dev/null ||
  fail "the direct-main route does not independently protect the RGD gate from self-removal"

if [ "${RGD_WIRING_SKIP_ABLATIONS:-false}" != "true" ]; then
  ablation_work="$(mktemp -d)"
  trap 'rm -rf "$ablation_work"' EXIT

  suppressed_workflow="${ablation_work}/ci-continue-on-error.yaml"
  cp "$CI_WORKFLOW" "$suppressed_workflow"
  yq -i '(.jobs."validate-rgd-templates-merge-group".steps[] |
    select((.run // "") | contains("bash scripts/tests/test-rgd-template-static-scan.sh"))).continue-on-error = true' \
    "$suppressed_workflow"
  if RGD_WIRING_CI_WORKFLOW="$suppressed_workflow" \
    RGD_WIRING_MAIN_WORKFLOW="$MAIN_WORKFLOW" \
    RGD_WIRING_SKIP_ABLATIONS=true "$0" >"${ablation_work}/continue-on-error.log" 2>&1; then
    fail "the wiring validator accepted a failure-suppressed merge-group RGD scan"
  fi

  suppressed_job_workflow="${ablation_work}/ci-job-continue-on-error.yaml"
  cp "$CI_WORKFLOW" "$suppressed_job_workflow"
  yq -i '.jobs."validate-rgd-templates-merge-group".continue-on-error = true' \
    "$suppressed_job_workflow"
  if RGD_WIRING_CI_WORKFLOW="$suppressed_job_workflow" \
    RGD_WIRING_MAIN_WORKFLOW="$MAIN_WORKFLOW" \
    RGD_WIRING_SKIP_ABLATIONS=true "$0" >"${ablation_work}/job-continue-on-error.log" 2>&1; then
    fail "the wiring validator accepted a failure-suppressed merge-group RGD job"
  fi

  conditional_workflow="${ablation_work}/ci-false-condition.yaml"
  cp "$CI_WORKFLOW" "$conditional_workflow"
  yq -i '(.jobs."validate-rgd-templates-merge-group".steps[] |
    select((.run // "") | contains("bash scripts/tests/test-rgd-template-static-scan.sh"))).if = false' \
    "$conditional_workflow"
  if RGD_WIRING_CI_WORKFLOW="$conditional_workflow" \
    RGD_WIRING_MAIN_WORKFLOW="$MAIN_WORKFLOW" \
    RGD_WIRING_SKIP_ABLATIONS=true "$0" >"${ablation_work}/false-condition.log" 2>&1; then
    fail "the wiring validator accepted a conditionally skipped merge-group RGD scan"
  fi

  conditional_job_workflow="${ablation_work}/ci-false-job-condition.yaml"
  cp "$CI_WORKFLOW" "$conditional_job_workflow"
  yq -i '.jobs."validate-rgd-templates-merge-group".if = false' "$conditional_job_workflow"
  if RGD_WIRING_CI_WORKFLOW="$conditional_job_workflow" \
    RGD_WIRING_MAIN_WORKFLOW="$MAIN_WORKFLOW" \
    RGD_WIRING_SKIP_ABLATIONS=true "$0" >"${ablation_work}/false-job-condition.log" 2>&1; then
    fail "the wiring validator accepted a conditionally skipped merge-group RGD job"
  fi

  shell_suppressed_workflow="${ablation_work}/ci-shell-suppressed.yaml"
  cp "$CI_WORKFLOW" "$shell_suppressed_workflow"
  yq -i '(.jobs."validate-rgd-templates-merge-group".steps[] |
    select((.run // "") | contains("bash scripts/tests/test-rgd-template-static-scan.sh"))).run += " || true"' \
    "$shell_suppressed_workflow"
  if RGD_WIRING_CI_WORKFLOW="$shell_suppressed_workflow" \
    RGD_WIRING_MAIN_WORKFLOW="$MAIN_WORKFLOW" \
    RGD_WIRING_SKIP_ABLATIONS=true "$0" >"${ablation_work}/shell-suppressed.log" 2>&1; then
    fail "the wiring validator accepted a shell-suppressed merge-group RGD scan"
  fi

  echoed_command_workflow="${ablation_work}/ci-echoed-command.yaml"
  cp "$CI_WORKFLOW" "$echoed_command_workflow"
  yq -i '(.jobs."validate-rgd-templates-merge-group".steps[] |
    select((.run // "") | contains("bash scripts/tests/test-rgd-template-static-scan.sh"))).run =
    "echo bash scripts/tests/test-rgd-template-static-scan.sh"' "$echoed_command_workflow"
  if RGD_WIRING_CI_WORKFLOW="$echoed_command_workflow" \
    RGD_WIRING_MAIN_WORKFLOW="$MAIN_WORKFLOW" \
    RGD_WIRING_SKIP_ABLATIONS=true "$0" >"${ablation_work}/echoed-command.log" 2>&1; then
    fail "the wiring validator accepted an echoed merge-group RGD scan command"
  fi

  required_filter_inputs=(
    ".trivy/data/**"
    "scripts/scan-rgd-templates.sh"
    "scripts/rgd-template-static-scan-baseline.tsv"
    "scripts/tests/test-rgd-template-static-scan.sh"
    "scripts/tests/test-rgd-template-static-scan-wiring.sh"
  )
  filter_ablation_index=0
  for filter_input in "${required_filter_inputs[@]}"; do
    filter_ablation_index=$((filter_ablation_index + 1))
    filtered_workflow="${ablation_work}/ci-filter-${filter_ablation_index}.yaml"
    cp "$CI_WORKFLOW" "$filtered_workflow"
    FILTER_INPUT="$filter_input" yq -i \
      '(.jobs.changes.steps[] | select(.id == "filter").with.filters) |=
        (split("\n") | map(select(contains(strenv(FILTER_INPUT)) | not)) | join("\n"))' \
      "$filtered_workflow"
    if RGD_WIRING_CI_WORKFLOW="$filtered_workflow" \
      RGD_WIRING_MAIN_WORKFLOW="$MAIN_WORKFLOW" \
      RGD_WIRING_SKIP_ABLATIONS=true "$0" >"${ablation_work}/filter-${filter_ablation_index}.log" 2>&1; then
      fail "the wiring validator accepted removal of required RGD filter input: $filter_input"
    fi
  done
fi

echo "PASS: PR, merge-group, and direct-main routes retain the nested-RGD behavioral gate."
