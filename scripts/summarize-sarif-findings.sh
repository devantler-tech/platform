#!/usr/bin/env bash
# Print a human-readable summary of the findings in a SARIF file.
#
# WHY THIS EXISTS. Kubescape's own console table shows only a subset of what it
# finds: measured on this repository, a scan whose SARIF carried 4 findings
# printed "All controls passed. No issues found", and a scan whose SARIF carried
# 70 findings listed a single control. The compliance-score gate is orthogonal —
# it answers "did posture regress", not "what was found" — so a finding that does
# not move the score below the floor is invisible in the log entirely.
#
# Since the SARIF is uploaded to Code Scanning (#2829), that silence became a
# contradiction: the CI log says there is nothing to fix while the Security tab
# raises alerts for the same run. A developer reading the log — which is what a
# developer actually reads on a PR — draws the wrong conclusion. This script
# closes that gap by reporting from the SARIF itself, so the log and the alerts
# always agree because they come from one source. See #2846.
#
# CONTRACT
#   - Reports what is IN the file; it never re-scans and never filters.
#   - Zero findings is stated explicitly, so "no findings" and "the summary did
#     not run" can never look alike in a log.
#   - Purely informational: it does NOT gate. The compliance threshold remains
#     the only merge gate, and this script's exit status never fails a build for
#     the content of a scan.
#   - Exits non-zero ONLY on a usage error or a file it cannot parse — a broken
#     summary is worth surfacing, but the caller decides whether that is fatal.
#   - Writes to $GITHUB_STEP_SUMMARY as well when that variable is set, so the
#     result is visible on the run page without opening the log.
set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "usage: ${0##*/} <sarif-file>" >&2
  exit 2
fi

sarif_file="$1"

if [ ! -f "$sarif_file" ]; then
  echo "${0##*/}: no such file: $sarif_file" >&2
  exit 2
fi

# One jq pass builds the whole report. Rule metadata lives in
# .runs[].tool.driver.rules and the findings in .runs[].results, so the rule
# table is indexed by id first and each result counted against it. Results are
# grouped by control rather than listed one per line: the same control on twenty
# files is one thing to fix, and a per-result list would bury that.
report="$(
  jq -r '
    [.runs[]? ] as $runs
    | ( [ $runs[].tool.driver.rules[]? ] | map({key: .id, value: .}) | from_entries ) as $rules
    | [ $runs[].results[]? ] as $results
    | ($results | length) as $total
    | if $total == 0 then
        "Kubescape: 0 findings in this scan."
      else
        ( $results
          | group_by(.ruleId)
          | map(
              # A result is not required to carry a ruleId, and indexing the rule
              # table with null is an error rather than a miss — so resolve the id
              # once and look the rule up only when there is one. A finding with no
              # id stays counted and visible instead of aborting the summary.
              ( .[0].ruleId ) as $id
              | ( if $id == null then null else $rules[$id] end ) as $rule
              | {
                  id:    ( $id // "<no rule id>" ),
                  count: length,
                  level: ( $rule.defaultConfiguration.level // "unknown" ),
                  desc:  ( $rule.shortDescription.text // "" )
                }
            )
          | sort_by(-.count)
        ) as $byControl
        | ( "Kubescape: \($total) finding(s) across \($byControl | length) control(s)."
          , ( $byControl[]
              | "  \(.id)  \(.level)  \(.desc)  — \(.count) finding(s)" )
          , "Where these are uploaded, they appear under Security -> Code scanning -> kubescape-nsa, with file and line."
          )
      end
  ' "$sarif_file"
)" || {
  echo "${0##*/}: could not parse SARIF: $sarif_file" >&2
  exit 2
}

printf '%s\n' "$report"

# The step summary renders Markdown, so the body goes in a fenced block to keep
# the alignment the plain-text report relies on.
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  {
    echo "### Kubescape findings"
    echo ''
    echo '```text'
    printf '%s\n' "$report"
    echo '```'
  } >> "$GITHUB_STEP_SUMMARY"
fi
