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
#
# Scanner versions differ between CI and a developer machine (CI ran checkov 3.3.2 / trivy 0.71.2).
# Verified across that gap: identical per-framework failed counts for checkov, and an identical
# check-id distribution for trivy. Passed-check totals do differ, which is expected and harmless.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly REPO_ROOT
OUT_DIR="$(mktemp -d)"
readonly OUT_DIR
trap 'rm -rf "$OUT_DIR"' EXIT

run_checkov=true
run_trivy=true

usage() {
  cat <<'EOF'
Usage: scripts/megalinter-scan-counts.sh [--checkov-only|--trivy-only]

Prints the checkov and trivy finding counts that MegaLinter's CI job scans for, so a change can be
measured before it is pushed. Always exits 0 on a successful scan — this reports, it does not gate.
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --checkov-only) run_trivy=false ;;
    --trivy-only) run_checkov=false ;;
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

# Fail closed with the fix, rather than reporting a count from a scanner that is not installed.
require_tool() {
  local tool="$1" hint="$2"
  if ! command -v "$tool" >/dev/null 2>&1; then
    printf '%s is not installed — install it with: %s\n' "$tool" "$hint" >&2
    exit 2
  fi
}

# checkov exits non-zero whenever it has findings, which is the normal case here, so the exit status
# is captured rather than allowed to abort the script.
scan_checkov() {
  local out="$OUT_DIR/checkov.txt" rc=0
  printf 'Running checkov (this takes ~1 minute)...\n' >&2
  # --directory MUST be the literal "." from the repository root, as CI runs it. Passing an
  # absolute path instead makes checkov's kubernetes framework return NOTHING — it disappears from
  # the report entirely and the total silently drops from 73 to 31, with no error. Reproduced both
  # with and without --skip-path, so it is the absolute path itself.
  (cd "$REPO_ROOT" && checkov --skip-path tests/ --skip-framework kustomize \
    --directory . --compact --quiet) >"$out" 2>&1 || rc=$?
  if [ ! -s "$out" ]; then
    printf 'checkov produced no output (exit %d) — cannot report a count\n' "$rc" >&2
    exit 2
  fi

  local total
  total="$(grep -aoE 'Failed checks: [0-9]+' "$out" | awk -F': ' '{s+=$2} END {print s+0}')"

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
  printf '  top checks:\n'
  grep -aoE 'Check: (CKV[A-Z_0-9]*)' "$out" | sed 's/Check: //' | sort | uniq -c | sort -rn |
    head -5 | awk '{printf "    %4s  %s\n", $1, $2}'
}

# trivy also exits non-zero on findings, by way of the --exit-code 1 that CI passes.
scan_trivy() {
  local out="$OUT_DIR/trivy.txt" rc=0
  printf '\nRunning trivy (this takes ~1 minute)...\n' >&2
  (cd "$REPO_ROOT" && trivy fs --scanners vuln,misconfig --exit-code 1 --skip-dirs tests .) \
    >"$out" 2>"$OUT_DIR/trivy.err" || rc=$?
  if [ ! -s "$out" ]; then
    printf 'trivy produced no output (exit %d) — cannot report a count\n' "$rc" >&2
    sed -n '1,5p' "$OUT_DIR/trivy.err" >&2
    exit 2
  fi

  local failures targets
  failures="$(grep -aoE 'FAILURES: [0-9]+' "$out" | awk -F': ' '{s+=$2} END {print s+0}')"
  targets="$(grep -acE '^Tests: [0-9]+ \(SUCCESSES' "$out" || true)"

  printf '\ntrivy — %s failing checks across %s targets\n' "$failures" "$targets"
  printf '  NOTE: MegaLinter reports this scan as "1 non blocking error". That 1 is a counting\n'
  printf '        artifact, not a finding count. The number above is what trivy actually found.\n'
  printf '  top checks:\n'
  grep -aoE '(KSV|AVD)-[0-9A-Z-]+' "$out" | sort | uniq -c | sort -rn |
    head -5 | awk '{printf "    %4s  %s\n", $1, $2}'
}

if [ "$run_checkov" = true ]; then
  require_tool checkov 'brew install checkov'
  scan_checkov
fi

if [ "$run_trivy" = true ]; then
  require_tool trivy 'brew install trivy'
  scan_trivy
fi

printf '\nBaselines these are measured against are recorded on #2787.\n'
