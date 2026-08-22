#!/usr/bin/env bash
# Assert every event route invokes the nested-RGD behavioral gate. This script is run from the
# unconditional changes job, independently of the PR validation step whose presence it protects.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly REPO_ROOT
readonly CI_WORKFLOW="${REPO_ROOT}/.github/workflows/ci.yaml"
readonly MAIN_WORKFLOW="${REPO_ROOT}/.github/workflows/validate-main.yaml"
readonly SETUP_TRIVY="aquasecurity/setup-trivy@81e514348e19b6112ce2a7e3ecbafe19c1e1f567"
readonly BEHAVIORAL_COMMAND="bash scripts/tests/test-rgd-template-static-scan.sh"
readonly WIRING_COMMAND="bash scripts/tests/test-rgd-template-static-scan-wiring.sh"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

command -v yq >/dev/null 2>&1 || fail "yq is required"
command -v jq >/dev/null 2>&1 || fail "jq is required"

ci_workflow_json="$(yq -o=json -I=0 '.' "$CI_WORKFLOW")"
main_workflow_json="$(yq -o=json -I=0 '.' "$MAIN_WORKFLOW")"

jq -e --arg wiring "$WIRING_COMMAND" '
  any(.jobs.changes.steps[]; (.run // "") | contains($wiring))
' <<<"$ci_workflow_json" >/dev/null ||
  fail "the unconditional changes job does not independently validate RGD workflow wiring"

jq -e '
  any(.jobs.changes.steps[];
      .id == "filter"
      and (.with.filters | contains(".trivy/data/**"))
      and (.with.filters | contains("scripts/tests/test-rgd-template-static-scan-wiring.sh")))
' <<<"$ci_workflow_json" >/dev/null ||
  fail "ci.yaml does not route every RGD gate input through k8s validation"

jq -e --arg setup "$SETUP_TRIVY" --arg behavior "$BEHAVIORAL_COMMAND" '
  .jobs.validate as $gate
  | (($gate.if // "") | contains("github.event_name == '\''pull_request'\''"))
  and any($gate.steps[];
      (.uses // "") == $setup and .with.version == "v0.74.0")
  and any($gate.steps[]; (.run // "") | contains($behavior))
' <<<"$ci_workflow_json" >/dev/null ||
  fail "the pull-request validate job does not run the pinned RGD behavioral gate"

jq -e --arg setup "$SETUP_TRIVY" --arg behavior "$BEHAVIORAL_COMMAND" '
  .jobs["validate-rgd-templates-merge-group"] as $gate
  | (($gate.if // "") | contains("github.event_name == '\''merge_group'\''"))
  and (($gate.needs | if type == "array" then . else [.] end) | index("changes") != null)
  and any($gate.steps[];
      (.uses // "") == $setup and .with.version == "v0.74.0")
  and any($gate.steps[]; (.run // "") | contains($behavior))
  and ((.jobs["deploy-prod"].needs | if type == "array" then . else [.] end)
      | index("validate-rgd-templates-merge-group") != null)
  and ((.jobs["ci-required-checks"].needs | if type == "array" then . else [.] end)
      | index("validate-rgd-templates-merge-group") != null)
' <<<"$ci_workflow_json" >/dev/null ||
  fail "ci.yaml does not gate merge-group deployment on the exact RGD behavioral scan"

jq -e --arg setup "$SETUP_TRIVY" --arg behavior "$BEHAVIORAL_COMMAND" '
  .jobs["validate-rgd-templates"] as $gate
  | any($gate.steps[];
      (.uses // "") == $setup and .with.version == "v0.74.0")
  and any($gate.steps[]; (.run // "") | contains($behavior))
' <<<"$main_workflow_json" >/dev/null ||
  fail "validate-main.yaml does not run the pinned RGD behavioral gate"

echo "PASS: PR, merge-group, and direct-main routes retain the nested-RGD behavioral gate."
