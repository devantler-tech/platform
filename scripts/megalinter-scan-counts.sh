#!/usr/bin/env bash
# Reproduce MegaLinter's checkov and trivy scans outside CI.
#
# WHY THIS EXISTS
# .mega-linter.yml keeps REPOSITORY_CHECKOV and REPOSITORY_TRIVY in DISABLE_ERRORS_LINTERS while a
# backlog is worked off (#2787). Every slice of that backlog has to show the number moved, and the
# only place the number appeared was a CI log. This script produces the same numbers before pushing.
#
# WHAT "THE SAME NUMBER" MEANS, PER SCANNER — the two are NOT alike:
#
#   checkov — MegaLinter reports the SUM of "Failed checks:" across the frameworks it runs. That is
#   a real finding count, and this script reproduces it exactly.
#
#   trivy — MegaLinter reports "1 non blocking error" on a scan that actually fails 620 checks. The
#   1 is an artifact of how MegaLinter counts trivy's output, NOT a finding count. Do not read it as
#   "one finding left". This script reports what trivy actually found.
#
# HOW THE INVOCATIONS WERE DERIVED
# Both command lines are copied from MegaLinter's own log, which prints the exact command it ran:
#   - Command: [checkov --skip-path tests/ --config-file /action/lib/.automation/.checkov.yml --directory .]
#   - Command: [trivy fs --scanners vuln,misconfig --exit-code 1 --skip-dirs tests .]
# Read them from a "🧹 Lint - mega-linter" job log if they ever need re-deriving:
#   gh api repos/devantler-tech/platform/actions/jobs/<job-id>/logs | grep -aE '^\S+ - Command: '
#
# TWO DELIBERATE DIFFERENCES FROM THE CI COMMAND, both verified not to change the counts:
#
#   1. --config-file is dropped. It points inside the MegaLinter container
#      (/action/lib/.automation/.checkov.yml) and does not exist on a developer machine. Verified:
#      per-framework FAILED counts are identical with and without it.
#
#   2. --skip-framework kustomize is added. MegaLinter's checkov run reports four frameworks
#      (cloudformation, kubernetes, secrets, github_actions) and no kustomize framework; a local
#      checkov also runs kustomize, which adds 73 findings that CI never reports. Skipping it is
#      what makes the totals comparable.
#      This is an OBSERVED difference, not an explained one — it is not caused by the kustomize
#      binary being absent (it is absent on a machine that still runs the framework). If the totals
#      ever stop matching, MegaLinter's bundled .checkov.yml is the first place to look.

set -euo pipefail

# The versions CI ran when this script's equivalence was established. A different local version is
# not fatal — verified across checkov 3.3.0/3.3.2 (identical FAILED counts, different PASSED) and
# trivy 0.71.2/0.72.0 (identical) — but a large enough gap can add or retire rules, which would look
# exactly like backlog movement. Report the gap rather than silently attributing it to a fix.
readonly CI_CHECKOV_VERSION='3.3.2'
readonly CI_TRIVY_VERSION='0.71.2'

# The frameworks MegaLinter's checkov run reports. All four must appear locally or the total is not
# comparable — see the dropped-framework guard in scan_checkov.
readonly EXPECTED_CHECKOV_FRAMEWORKS=(cloudformation kubernetes secrets github_actions)

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly REPO_ROOT
OUT_DIR="$(mktemp -d)"
readonly OUT_DIR
trap 'rm -rf "$OUT_DIR"' EXIT

scanner=both

usage() {
  cat <<'EOF'
Usage: scripts/megalinter-scan-counts.sh [--checkov-only|--trivy-only]

Prints the checkov and trivy finding counts that MegaLinter's CI job scans for, so a change can be
measured before it is pushed. Always exits 0 on a successful scan — this reports, it does not gate.
EOF
}

# One mutually exclusive value rather than two booleans: passing both --checkov-only and
# --trivy-only used to disable each other and run no scan at all, printing a footer and exiting 0,
# which is indistinguishable from a completed measurement.
while [ $# -gt 0 ]; do
  case "$1" in
    --checkov-only | --trivy-only)
      local_choice="${1#--}"
      local_choice="${local_choice%-only}"
      if [ "$scanner" != both ] && [ "$scanner" != "$local_choice" ]; then
        printf '%s contradicts the earlier --%s-only\n\n' "$1" "$scanner" >&2
        usage >&2
        exit 2
      fi
      scanner="$local_choice"
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      printf 'unknown argument: %s\n\n' "$1" >&2
      usage >&2
      exit 2
      ;;
  esac
  shift
done
readonly scanner

# Fail closed with the fix, rather than reporting a count from a scanner that is not installed.
require_tool() {
  local tool="$1" hint="$2"
  if ! command -v "$tool" >/dev/null 2>&1; then
    printf '%s is not installed — install it with: %s\n' "$tool" "$hint" >&2
    exit 2
  fi
}

# A pipeline whose first stage legitimately matches nothing must not kill the script under
# `set -o pipefail` — and "nothing matched" is exactly the terminal state this script exists to
# demonstrate, so the zero case has to be the one that works.
count_by() {
  local pattern="$1" file="$2"
  grep -aoE "$pattern" "$file" | sort | uniq -c | sort -rn | head -5 || true
}

# Same hazard, summing form: awk still prints 0 on empty input, but the unmatched grep would fail
# the pipeline under pipefail and abort the script before the total is ever assigned.
sum_by() {
  local pattern="$1" file="$2"
  grep -aoE "$pattern" "$file" | grep -aoE '[0-9]+$' | awk '{s+=$1} END {print s+0}' || true
}

# checkov exits 0 with no findings and 1 with findings; anything else is a real failure whose
# partial output must not be reported as a count.
scan_checkov() {
  local out="$OUT_DIR/checkov.txt" rc=0
  printf 'Running checkov (this takes ~1 minute)...\n' >&2
  # --directory MUST be the literal "." from the repository root, as CI runs it. Passing an
  # absolute path instead makes checkov's kubernetes framework return NOTHING — it disappears from
  # the report entirely and the total silently drops from 73 to 31, with no error. Reproduced both
  # with and without --skip-path, so it is the absolute path itself.
  (cd "$REPO_ROOT" && checkov --skip-path tests/ --skip-framework kustomize \
    --directory . --compact --quiet) >"$out" 2>&1 || rc=$?
  if [ "$rc" -gt 1 ]; then
    printf 'checkov exited %d — refusing to report a count from an incomplete run\n' "$rc" >&2
    tail -n 5 "$out" >&2
    exit 2
  fi
  # Every framework CI reports must be present. "At least one section" is not enough: a silently
  # dropped framework is the failure mode already seen here — an absolute --directory removes the
  # whole kubernetes framework and its 42 findings while the other three still report, so the run
  # looks healthy and the total is simply 42 lower.
  local missing=''
  local fw
  for fw in "${EXPECTED_CHECKOV_FRAMEWORKS[@]}"; do
    if ! grep -aqE "^${fw} scan results:" "$out"; then
      missing="$missing $fw"
    fi
  done
  if [ -n "$missing" ]; then
    printf 'checkov did not report these frameworks CI reports:%s\n' "$missing" >&2
    printf 'Refusing to report a count — a dropped framework silently lowers the total.\n' >&2
    exit 2
  fi

  local total
  total="$(sum_by 'Failed checks: [0-9]+' "$out")"

  printf '\ncheckov — %s failing checks\n' "$total"
  printf '  per framework (failed / passed):\n'
  # Frameworks are reported as a "<name> scan results:" header followed by the counts line.
  awk '
    /scan results:/ { fw = $1; next }
    /^Passed checks:/ && fw != "" {
      split($0, f, ",")
      sub(/^Passed checks: /, "", f[1]); sub(/^ Failed checks: /, "", f[2])
      printf "    %-16s %4s / %s\n", fw, f[2], f[1]
      fw = ""
    }
  ' "$out"
  if [ "$total" -gt 0 ]; then
    printf '  top checks:\n'
    count_by 'Check: CKV[A-Z_0-9]*' "$out" | sed 's/Check: //' |
      awk '{printf "    %4s  %s\n", $1, $2}'
  fi
}

# trivy is given --exit-code 1 by CI, so 1 means findings and 0 means clean; anything else is a real
# failure. Both scanner categories are counted, because CI asks for both.
scan_trivy() {
  local out="$OUT_DIR/trivy.txt" rc=0
  printf '\nRunning trivy (this takes ~1 minute)...\n' >&2
  (cd "$REPO_ROOT" && trivy fs --scanners vuln,misconfig --exit-code 1 --skip-dirs tests .) \
    >"$out" 2>"$OUT_DIR/trivy.err" || rc=$?
  if [ "$rc" -gt 1 ]; then
    printf 'trivy exited %d — refusing to report a count from an incomplete run\n' "$rc" >&2
    tail -n 5 "$OUT_DIR/trivy.err" >&2
    exit 2
  fi
  # The exit status alone CANNOT distinguish "findings found" from "the scan broke": --exit-code 1
  # sets the status for findings, and trivy also exits 1 on operational failures such as a failed
  # vulnerability-database download. A broken scan would otherwise be reported as
  # "0 misconfigurations across 0 targets" — indistinguishable from a cleared backlog, which is the
  # single most dangerous wrong answer this script could give. Require positive evidence that the
  # scan actually ran instead.
  if grep -aqE '\bFATAL\b' "$OUT_DIR/trivy.err"; then
    printf 'trivy reported a fatal error — refusing to report a count\n' >&2
    grep -aE '\bFATAL\b' "$OUT_DIR/trivy.err" | tail -n 3 >&2
    exit 2
  fi
  # The discriminator is the exit status, NOT the presence of detail rows: trivy prints a
  # "Tests:" summary only for targets that HAVE failures (measured — the current 458-target report
  # contains zero "FAILURES: 0" lines), so a cleared backlog legitimately has no detail at all.
  #   rc 0 — ran, found nothing. That IS the terminal state this script exists to report.
  #   rc 1 — findings, OR an operational failure using the same status; demand the findings.
  if [ "$rc" -eq 1 ] && ! grep -aqE '^(Tests: [0-9]+ \(SUCCESSES|Total: [0-9]+ \()' "$out"; then
    printf 'trivy exited 1 but reported no findings — refusing to report a count.\n' >&2
    printf 'Exit 1 means findings OR an operational failure, so with neither present the scan\n' >&2
    printf 'cannot be assumed to have run. A genuinely clean scan exits 0.\n' >&2
    tail -n 5 "$OUT_DIR/trivy.err" >&2
    exit 2
  fi

  local misconfig vulns targets
  # Misconfiguration results are summarised per target as "Tests: N (SUCCESSES: n, FAILURES: n)".
  misconfig="$(sum_by 'FAILURES: [0-9]+' "$out")"
  targets="$(grep -acE '^Tests: [0-9]+ \(SUCCESSES' "$out" || true)"
  # Vulnerabilities are summarised in a DIFFERENT shape — "Total: N (UNKNOWN: …)" — so a count that
  # only sums FAILURES reports a clean trivy backlog while CI still finds vulnerabilities.
  vulns="$(sum_by '^Total: [0-9]+' "$out")"

  printf '\ntrivy — %s misconfigurations across %s targets, %s vulnerabilities\n' \
    "$misconfig" "$targets" "$vulns"
  printf '  NOTE: MegaLinter reports this scan as "1 non blocking error". That 1 is a counting\n'
  printf '        artifact, not a finding count. The numbers above are what trivy actually found.\n'
  if [ "$misconfig" -gt 0 ]; then
    printf '  top misconfiguration checks:\n'
    count_by '(KSV|AVD)-[0-9A-Z-]+' "$out" | awk '{printf "    %4s  %s\n", $1, $2}'
  fi
}

# A scanner version far from CI's can add or retire rules, which looks identical to backlog
# movement. Report the comparison instead of leaving it to be discovered as a mystery delta.
report_version() {
  local tool="$1" ci_version="$2" local_version="$3"
  if [ "$local_version" = "$ci_version" ]; then
    printf '  %-8s %s (matches CI)\n' "$tool" "$local_version"
  else
    printf '  %-8s %s — CI ran %s; a rule-set difference between these can look like backlog movement\n' \
      "$tool" "$local_version" "$ci_version"
  fi
}

printf 'Scanner versions:\n'
if [ "$scanner" != trivy ]; then
  require_tool checkov 'brew install checkov'
  report_version checkov "$CI_CHECKOV_VERSION" "$(checkov --version 2>/dev/null | tr -d '[:space:]')"
fi
if [ "$scanner" != checkov ]; then
  require_tool trivy 'brew install trivy'
  report_version trivy "$CI_TRIVY_VERSION" \
    "$(trivy --version 2>/dev/null | awk '/^Version:/ {print $2; exit}')"
fi

# `if`, not `cond && scan_…`: a false condition makes the AND-list the script's last exit status, so
# a --checkov-only run would end non-zero having done everything right.
if [ "$scanner" != trivy ]; then
  scan_checkov
fi
if [ "$scanner" != checkov ]; then
  scan_trivy
fi

printf '\nBaselines these are measured against are recorded on #2787.\n'
