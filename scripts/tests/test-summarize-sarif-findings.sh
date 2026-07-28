#!/usr/bin/env bash
# Pin the report that scripts/summarize-sarif-findings.sh builds from a SARIF file.
#
# WHY THIS EXISTS. That script is the only thing standing between a scan's real
# findings and a CI log that says nothing was found (#2846), so a silent
# regression in its jq pipeline restores exactly the contradiction it was written
# to remove — and it restores it invisibly, because a summary that under-reports
# still looks like a healthy run. The failure mode is a missing line, never a
# crash, which is why this pins counts and grouping rather than just exit status.
#
# The SARIF shapes below are the ones measured on this repository: a result may
# carry no `ruleId` at all, and a `ruleId` may reference a rule absent from the
# driver's rule table. Both are misses that jq turns into hard errors if indexed
# naively, so each has its own case here.
#
# Bash plus the runner's jq; no cluster, no secrets, no network.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly script="${root_dir}/scripts/summarize-sarif-findings.sh"
readonly ci_workflow="${root_dir}/.github/workflows/ci.yaml"

work_dir="$(mktemp -d)"
readonly work_dir
cleanup() {
  rm -rf "${work_dir}"
}
trap cleanup EXIT

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

require_text() {
  local haystack="$1"
  local needle="$2"
  local description="$3"

  if ! grep -Fq -- "$needle" <<<"${haystack}"; then
    printf 'FAIL: %s\n--- actual output ---\n%s\n---\n' "${description}" "${haystack}" >&2
    exit 1
  fi
}

# Writes $2 to a SARIF file named $1 and echoes its path.
write_sarif() {
  local name="$1"
  local body="$2"
  local path="${work_dir}/${name}"

  printf '%s\n' "${body}" >"${path}"
  printf '%s' "${path}"
}

# Runs the script under test, capturing combined output and exit status without
# tripping `set -e`. Sets the globals `run_output` and `run_status`.
run_summary() {
  run_status=0
  run_output="$(bash "${script}" "$@" 2>&1)" || run_status=$?
}

# The 1-based line number of the first line matching $2 in $1, or empty.
line_of() {
  local haystack="$1"
  local needle="$2"

  grep -nF -- "${needle}" <<<"${haystack}" | head -1 | cut -d: -f1
}

# ---------------------------------------------------------------------------
# A scan that found nothing says so explicitly.
#
# This is the case that must never be silent: "no findings" and "the summary did
# not run" have to be distinguishable in a log, or a broken step reads as a clean
# scan.
# ---------------------------------------------------------------------------
empty_sarif="$(write_sarif empty.sarif '{"runs":[{"tool":{"driver":{"rules":[]}},"results":[]}]}')"
run_summary "${empty_sarif}"
[ "${run_status}" -eq 0 ] || fail "an empty SARIF must exit 0, got ${run_status}"
require_text "${run_output}" 'Kubescape: 0 findings in this scan.' \
  'an empty SARIF must state zero findings explicitly'

# A SARIF with no runs at all is the same story, and reaches a different jq path
# (`.runs[]?` yields nothing rather than an empty results array).
no_runs_sarif="$(write_sarif no-runs.sarif '{"version":"2.1.0"}')"
run_summary "${no_runs_sarif}"
[ "${run_status}" -eq 0 ] || fail "a SARIF with no runs must exit 0, got ${run_status}"
require_text "${run_output}" 'Kubescape: 0 findings in this scan.' \
  'a SARIF with no runs must state zero findings explicitly'

# ---------------------------------------------------------------------------
# Findings are grouped by control, counted, and ordered by count descending.
#
# Grouping is the whole point of the report: the same control on twenty files is
# one thing to fix. Ordering matters because the log is read top-down and the
# biggest group is the one worth acting on first.
# ---------------------------------------------------------------------------
multi_sarif="$(write_sarif multi.sarif '{
  "runs": [
    {
      "tool": {
        "driver": {
          "rules": [
            {
              "id": "C-0016",
              "shortDescription": { "text": "Allow privilege escalation" },
              "defaultConfiguration": { "level": "warning" }
            },
            {
              "id": "C-0009",
              "shortDescription": { "text": "Resource limits" },
              "defaultConfiguration": { "level": "error" }
            }
          ]
        }
      },
      "results": [
        { "ruleId": "C-0009" },
        { "ruleId": "C-0016" },
        { "ruleId": "C-0016" },
        { "ruleId": "C-0016" }
      ]
    }
  ]
}')"
run_summary "${multi_sarif}"
[ "${run_status}" -eq 0 ] || fail "a well-formed SARIF must exit 0, got ${run_status}"
require_text "${run_output}" 'Kubescape: 4 finding(s) across 2 control(s).' \
  'the header must carry the total finding count and the control count'
require_text "${run_output}" 'C-0016  warning  Allow privilege escalation  — 3 finding(s)' \
  'a control must report its level, description and grouped count from the rule table'
require_text "${run_output}" 'C-0009  error  Resource limits  — 1 finding(s)' \
  'every control must be reported, not only the largest'

busiest_line="$(line_of "${run_output}" 'C-0016')"
quietest_line="$(line_of "${run_output}" 'C-0009')"
[ -n "${busiest_line}" ] && [ -n "${quietest_line}" ] ||
  fail 'both controls must appear in the report'
[ "${busiest_line}" -lt "${quietest_line}" ] ||
  fail "controls must be ordered by descending count (C-0016 at line ${busiest_line}, C-0009 at line ${quietest_line})"

require_text "${run_output}" 'Security -> Code scanning -> kubescape-nsa' \
  'the report must say where the uploaded findings appear'

# ---------------------------------------------------------------------------
# A result carrying no ruleId stays counted instead of aborting the summary.
#
# jq raises "Cannot index object with null" if the rule table is indexed with a
# null id, which would take the whole report down over one malformed result —
# turning a partial scan into a silent one.
# ---------------------------------------------------------------------------
no_id_sarif="$(write_sarif no-rule-id.sarif '{
  "runs": [
    {
      "tool": { "driver": { "rules": [ { "id": "C-0009" } ] } },
      "results": [ { "ruleId": "C-0009" }, { "message": { "text": "orphan" } } ]
    }
  ]
}')"
run_summary "${no_id_sarif}"
[ "${run_status}" -eq 0 ] ||
  fail "a result with no ruleId must not abort the summary, got exit ${run_status}"
require_text "${run_output}" 'Kubescape: 2 finding(s) across 2 control(s).' \
  'a result with no ruleId must still be counted in the total'
require_text "${run_output}" '<no rule id>' \
  'a result with no ruleId must be visible in the report rather than dropped'

# ---------------------------------------------------------------------------
# A ruleId with no matching rule reports as unknown rather than blank-crashing.
# ---------------------------------------------------------------------------
orphan_rule_sarif="$(write_sarif orphan-rule.sarif '{
  "runs": [
    {
      "tool": { "driver": { "rules": [] } },
      "results": [ { "ruleId": "C-9999" } ]
    }
  ]
}')"
run_summary "${orphan_rule_sarif}"
[ "${run_status}" -eq 0 ] ||
  fail "a ruleId absent from the rule table must not abort the summary, got exit ${run_status}"
require_text "${run_output}" 'Kubescape: 1 finding(s) across 1 control(s).' \
  'a finding whose rule is missing from the table must still be counted'
require_text "${run_output}" 'C-9999  unknown' \
  'a finding whose rule is missing from the table must report an unknown level'

# ---------------------------------------------------------------------------
# The step summary carries the same report as the log.
#
# Two surfaces built from one file is the property this whole script exists for;
# if they can diverge, the summary is just another place to be wrong.
# ---------------------------------------------------------------------------
step_summary="${work_dir}/step-summary.md"
: >"${step_summary}"
summary_status=0
summary_stdout="$(GITHUB_STEP_SUMMARY="${step_summary}" bash "${script}" "${multi_sarif}" 2>&1)" ||
  summary_status=$?
[ "${summary_status}" -eq 0 ] || fail "writing a step summary must exit 0, got ${summary_status}"
summary_body="$(cat "${step_summary}")"
require_text "${summary_body}" '### Kubescape findings' \
  'the step summary must carry a heading'
require_text "${summary_body}" 'Kubescape: 4 finding(s) across 2 control(s).' \
  'the step summary must carry the same totals as the log'
require_text "${summary_body}" 'C-0016  warning  Allow privilege escalation  — 3 finding(s)' \
  'the step summary must carry the same per-control detail as the log'
require_text "${summary_body}" '```text' \
  'the step summary must fence the report so its alignment survives Markdown'
require_text "${summary_stdout}" 'Kubescape: 4 finding(s) across 2 control(s).' \
  'writing a step summary must not suppress the log output'

# Absent GITHUB_STEP_SUMMARY, nothing is written anywhere — the script must not
# assume it runs under Actions.
stray_before="$(find "${work_dir}" -type f | wc -l | tr -d ' ')"
run_summary "${multi_sarif}"
stray_after="$(find "${work_dir}" -type f | wc -l | tr -d ' ')"
[ "${stray_before}" -eq "${stray_after}" ] ||
  fail 'running without GITHUB_STEP_SUMMARY must not create files'

# ---------------------------------------------------------------------------
# Failure modes exit 2 and say why. The caller decides whether that is fatal,
# so the status has to be distinguishable from "scan was clean".
# ---------------------------------------------------------------------------
malformed_sarif="$(write_sarif malformed.sarif '{"runs": [ this is not json')"
run_summary "${malformed_sarif}"
[ "${run_status}" -eq 2 ] || fail "unparseable SARIF must exit 2, got ${run_status}"
require_text "${run_output}" 'could not parse SARIF' \
  'unparseable SARIF must name the failure'

run_summary "${work_dir}/does-not-exist.sarif"
[ "${run_status}" -eq 2 ] || fail "a missing file must exit 2, got ${run_status}"
require_text "${run_output}" 'no such file' \
  'a missing file must name the failure'

run_summary
[ "${run_status}" -eq 2 ] || fail "no argument must exit 2, got ${run_status}"
require_text "${run_output}" 'usage:' 'no argument must print usage'

run_summary "${multi_sarif}" "${empty_sarif}"
[ "${run_status}" -eq 2 ] || fail "too many arguments must exit 2, got ${run_status}"

# ---------------------------------------------------------------------------
# The wiring. A guard nothing runs is not a guard, and a guard whose own edits do
# not trigger it decays silently — so both the invocation and this file's
# presence in the path filter are pinned here.
# ---------------------------------------------------------------------------
workflow_body="$(cat "${ci_workflow}")"
require_text "${workflow_body}" 'bash scripts/tests/test-summarize-sarif-findings.sh' \
  'CI must invoke this test, or it never runs'
require_text "${workflow_body}" "- 'scripts/tests/test-summarize-sarif-findings.sh'" \
  'this test must be in the k8s path filter, or editing it alone never runs it'
require_text "${workflow_body}" "- 'scripts/summarize-sarif-findings.sh'" \
  'the script under test must be in the k8s path filter'

printf 'PASS: %s\n' "${BASH_SOURCE[0]##*/}"
