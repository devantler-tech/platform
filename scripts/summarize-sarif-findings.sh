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
# .runs[].tool.driver.rules and the findings in .runs[].results, and both are
# scoped to their run — so each result resolves its rule against its OWN run and
# carries the answer forward, rather than against a table flattened across runs.
# Results are then grouped by control rather than listed one per line: the same
# control on twenty files is one thing to fix, and a per-result list would bury
# that.
report="$(
  jq -r '
    # A SARIF document MUST carry .runs as an array. Without this check `{}`,
    # `{"version":"2.1.0"}` and `{"runs":null}` all flow through the optional
    # iteration below and report "0 findings" — a structurally unusable file
    # reported as a clean scan, which is the exact ambiguity this script exists
    # to remove. `"runs": []` is a different thing and stays a legitimate zero.
    if (.runs | type) != "array" then
      error("not a SARIF document: .runs is absent or not an array")
    else . end
    | [.runs[]] as $runs
    # SARIF lets a result name its rule EITHER by .ruleId or by .ruleIndex into
    # its OWN run.tool.driver.rules, and two runs may define the same rule id
    # with different metadata. Both facts make rule resolution run-scoped, so
    # each result is resolved against its own run and carries the answer with it.
    #
    # A global id-keyed table cannot do this: `from_entries` keeps the LAST
    # definition of a duplicate key, so every finding for that id would be
    # reported with the level and description from the LAST run — a wrong attribution,
    # which is worse than an absent one. Same reason the index is bounds-checked:
    # jq indexes backwards from -1, so an out-of-range index would silently
    # borrow the last rule in the table.
    | [ $runs[]
        | ( .tool.driver.rules // [] ) as $runRules
        | ( $runRules | map({key: .id, value: .}) | from_entries ) as $runRuleTable
        | ( .results // [] )[]
        | ( .ruleId
            // ( if (.ruleIndex | type) == "number"
                   and .ruleIndex >= 0
                   and .ruleIndex < ( $runRules | length )
                 then $runRules[.ruleIndex].id
                 else null end ) ) as $id
        | ( if $id == null then null else $runRuleTable[$id] end ) as $rule
        | {
            id:    ( $id // "<no rule id>" ),
            level: ( $rule.defaultConfiguration.level // "unknown" ),
            desc:  ( $rule.shortDescription.text // "" )
          }
      ] as $findings
    | ( $findings | length ) as $total
    | if $total == 0 then
        "Kubescape: 0 findings in this scan."
      else
        # Grouped on the resolved triple, not on the id alone. Runs that agree
        # about a rule still merge into one control (the ordinary case, and the
        # only one the single-run scans in this repository produce); runs that
        # disagree are reported separately rather than one silently overwriting
        # the other.
        ( $findings
          | group_by([.id, .level, .desc])
          | map( .[0] + { count: length } )
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
  } >>"$GITHUB_STEP_SUMMARY"
fi
