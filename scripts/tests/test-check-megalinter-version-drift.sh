#!/usr/bin/env bash
# Contract tests for scripts/check-megalinter-version-drift.sh.
#
# Every case runs against a FIXTURE log via --log-file, so the suite is offline and does not depend
# on a live MegaLinter run existing.
#
# The fixture is built from the real thing: the four marker lines below are copied verbatim from the
# "🧹 Lint - mega-linter" job of platform run 31337531893, including the trivy-sbom line, which is
# present in every real log and is the reason the trivy matcher has to be anchored.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="$repo_root/scripts/check-megalinter-version-drift.sh"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

failures=0

# Read the recorded constants from the helper that owns them, so this suite keeps testing the real
# contract after a legitimate version bump instead of pinning a second copy that goes stale.
read_const() {
  sed -n "s/^readonly $1='\([^']*\)'.*/\1/p" "$repo_root/scripts/megalinter-scan-counts.sh"
}
ci_megalinter="$(read_const CI_MEGALINTER_VERSION)"
ci_checkov="$(read_const CI_CHECKOV_VERSION)"
ci_trivy="$(read_const CI_TRIVY_VERSION)"

for name in ci_megalinter ci_checkov ci_trivy; do
  if [ -z "${!name}" ]; then
    printf 'FAIL: could not read %s from scripts/megalinter-scan-counts.sh\n' "$name" >&2
    exit 1
  fi
done

# Build a log fixture. Args: megalinter, checkov, trivy version (empty string omits that marker).
make_log() {
  local ml="$1" ck="$2" tv="$3" out="$4"
  {
    printf '2026-08-09T21:42:59Z ##[group]Pull down action image\n'
    [ -n "$ml" ] && printf "2026-08-09T21:42:59Z ghcr.io/oxsecurity/megalinter-go:v%s\n" "$ml"
    printf '2026-08-09T21:48:50Z - Using [actionlint v1.7.12] https://megalinter.io/x\n'
    [ -n "$ck" ] && printf '2026-08-09T21:48:50Z - Using [checkov v%s] https://megalinter.io/x\n' "$ck"
    [ -n "$tv" ] && printf '2026-08-09T21:48:50Z - Using [trivy v%s] https://megalinter.io/x\n' "$tv"
    # Always present in a real log, and one character away from the trivy marker.
    printf '2026-08-09T21:48:50Z - Using [trivy-sbom v9.9.9] https://megalinter.io/x\n'
    printf '2026-08-09T21:48:57Z done\n'
  } >"$out"
}

expect() {
  local desc="$1" want_rc="$2" logfile="$3" want_msg="${4-}"
  local out rc=0
  out="$("$script" --log-file "$logfile" 2>&1)" || rc=$?
  if [ "$rc" -ne "$want_rc" ]; then
    printf 'FAIL: %s — expected exit %d, got %d\n%s\n' "$desc" "$want_rc" "$rc" "$out" >&2
    failures=$((failures + 1))
    return
  fi
  if [ -n "$want_msg" ] && ! printf '%s' "$out" | grep -qF "$want_msg"; then
    printf 'FAIL: %s — exit %d correct but message lacked %q\n%s\n' "$desc" "$rc" "$want_msg" "$out" >&2
    failures=$((failures + 1))
    return
  fi
  printf 'ok: %s\n' "$desc"
}

# ---- GREEN: a log matching every recorded constant --------------------------------------------
make_log "$ci_megalinter" "$ci_checkov" "$ci_trivy" "$scratch/match.log"
expect 'matching versions pass' 0 "$scratch/match.log"

# ---- RED: each version drifts INDEPENDENTLY ----------------------------------------------------
# Asserted one at a time so a matcher that reads the wrong line cannot pass by coincidence: each
# case leaves the other two markers correct, so only the named tool can be responsible.
make_log '9.99.0' "$ci_checkov" "$ci_trivy" "$scratch/ml.log"
expect 'megalinter drift is caught' 1 "$scratch/ml.log" 'CI_MEGALINTER_VERSION'

make_log "$ci_megalinter" '3.99.9' "$ci_trivy" "$scratch/ck.log"
expect 'checkov drift is caught' 1 "$scratch/ck.log" 'CI_CHECKOV_VERSION'

make_log "$ci_megalinter" "$ci_checkov" '0.99.9' "$scratch/tv.log"
expect 'trivy drift is caught' 1 "$scratch/tv.log" 'CI_TRIVY_VERSION'

# The failure message has to name the file to edit, or the check is a puzzle rather than a signal.
expect 'failure names the file to edit' 1 "$scratch/ck.log" 'scripts/megalinter-scan-counts.sh'

# ---- FAIL-CLOSED: an unreadable log must never read as "no drift" ------------------------------
# This is the important direction. A MegaLinter log-format change removes the markers; if that were
# reported as success, the guard would go quietly dead at exactly the moment it stopped working.
make_log '' "$ci_checkov" "$ci_trivy" "$scratch/no-ml.log"
expect 'absent megalinter marker fails closed' 2 "$scratch/no-ml.log" 'could not find'

make_log "$ci_megalinter" '' "$ci_trivy" "$scratch/no-ck.log"
expect 'absent checkov marker fails closed' 2 "$scratch/no-ck.log" 'could not find'

make_log "$ci_megalinter" "$ci_checkov" '' "$scratch/no-tv.log"
expect 'absent trivy marker fails closed' 2 "$scratch/no-tv.log" 'could not find'

: >"$scratch/empty.log"
expect 'empty log fails closed' 2 "$scratch/empty.log"

expect 'missing log file fails closed' 2 "$scratch/does-not-exist.log"

# ---- The trivy matcher must not be satisfied by trivy-sbom -------------------------------------
# trivy-sbom carries its own version and sits directly beside the trivy line in every real log. A
# substring matcher reads 9.9.9 from it; with the real trivy marker absent the run must fail closed
# rather than compare against the sbom's version.
grep -q 'trivy-sbom v9.9.9' "$scratch/no-tv.log" ||
  { printf 'FAIL: fixture lost its trivy-sbom decoy line\n' >&2; failures=$((failures + 1)); }

# ---- Ambiguity is not resolved silently --------------------------------------------------------
# Two different versions for one tool means the log is not describing a single run.
{
  cat "$scratch/match.log"
  printf '2026-08-09T21:48:51Z - Using [checkov v3.0.0] https://megalinter.io/x\n'
} >"$scratch/dup.log"
expect 'conflicting versions for one tool fail closed' 2 "$scratch/dup.log" 'conflicting'

if [ "$failures" -ne 0 ]; then
  printf '\n%d check(s) failed\n' "$failures" >&2
  exit 1
fi
printf '\nmegalinter version-drift guard contract OK\n'
