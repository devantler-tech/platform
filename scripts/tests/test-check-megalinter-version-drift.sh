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
  {
    printf 'FAIL: fixture lost its trivy-sbom decoy line\n' >&2
    failures=$((failures + 1))
  }

# ---- Ambiguity is not resolved silently --------------------------------------------------------
# Two different versions for one tool means the log is not describing a single run.
{
  cat "$scratch/match.log"
  printf '2026-08-09T21:48:51Z - Using [checkov v3.0.0] https://megalinter.io/x\n'
} >"$scratch/dup.log"
expect 'conflicting versions for one tool fail closed' 2 "$scratch/dup.log" 'conflicting'

# ---- PROVENANCE: the managed workflow must not be definable inside this repository -------------
# The live path picks a job out of arbitrary recent runs, so it binds to the org-managed workflow's
# PATH. That binding is only proof of provenance while this repository does not define a workflow at
# that same path — if it ever does, the file could be edited to emit any versions it likes. The
# guard must then refuse rather than trust it.
#
# Uses a synthetic tree via DRIFT_REPO_ROOT, and a PATH-shadowed `gh` that fails if called: reaching
# the network here would mean the provenance check ran too late to protect anything.
mkdir -p "$scratch/fakerepo/.github/workflows" "$scratch/bin" "$scratch/fakerepo/scripts"
cp "$repo_root/scripts/megalinter-scan-counts.sh" "$scratch/fakerepo/scripts/"
printf 'name: impostor\n' >"$scratch/fakerepo/.github/workflows/validate-go-project.yaml"
printf '#!/usr/bin/env bash\necho "gh must not be reached before the provenance check" >&2\nexit 99\n' \
  >"$scratch/bin/gh"
chmod +x "$scratch/bin/gh"

out="$(DRIFT_REPO_ROOT="$scratch/fakerepo" PATH="$scratch/bin:$PATH" "$script" 2>&1)" && rc=0 || rc=$?
if [ "$rc" -ne 2 ]; then
  printf 'FAIL: in-repo managed workflow — expected exit 2, got %d\n%s\n' "$rc" "$out" >&2
  failures=$((failures + 1))
elif ! printf '%s' "$out" | grep -qF 'no longer'; then
  printf 'FAIL: in-repo managed workflow — exit 2 but message did not explain provenance\n%s\n' \
    "$out" >&2
  failures=$((failures + 1))
else
  printf 'ok: an in-repo copy of the managed workflow fails closed\n'
fi

# Negative control for the case above: with the impostor workflow REMOVED, the same synthetic tree
# must get PAST the provenance check and reach gh (which is stubbed to fail loudly). Without this,
# the assertion above would pass just as well if the guard refused for some unrelated reason.
rm "$scratch/fakerepo/.github/workflows/validate-go-project.yaml"
out="$(DRIFT_REPO_ROOT="$scratch/fakerepo" PATH="$scratch/bin:$PATH" "$script" 2>&1)" && rc=0 || rc=$?
if printf '%s' "$out" | grep -qF 'no longer'; then
  printf 'FAIL: provenance refusal fired with no in-repo workflow present\n%s\n' "$out" >&2
  failures=$((failures + 1))
else
  printf 'ok: control — without the in-repo copy the provenance check does not fire\n'
fi

# ---- PROVENANCE, PER CANDIDATE RUN -------------------------------------------------------------
# The case above only proves MAIN does not define the managed workflow. A run's `.path` is the
# workflow's path IN THE REVISION THAT RAN, so a pull request that adds a file at that same path
# produces a run matching the selector exactly while main stays clean — and its `mega-linter` job
# can then print any versions it likes. Provenance must therefore be re-checked at each candidate's
# own revision.
#
# These cases drive the live selection path (no --log-file) through a `gh` stand-in that emits what
# the real command would after its own --jq filtering.
cat >"$scratch/bin/gh" <<'STUB'
#!/usr/bin/env bash
args="$*"
case "$args" in
  *--help*) printf -- '--allow-escape-sequences\n' ;;
  *actions/jobs/*/logs*)
    job="${args##*actions/jobs/}"
    job="${job%%/logs*}"
    cat "$GH_STUB_LOGDIR/$job.log" ;;
  *actions/runs/*/jobs*)
    run="${args##*actions/runs/}"
    run="${run%%/jobs*}"
    printf 'job%s\n' "$run" ;;
  *actions/runs*per_page*)
    per_page="${args#*per_page=}"
    per_page="${per_page%%&*}"
    per_page="${per_page%% *}"
    if [ "$per_page" -ge "${GH_STUB_MIN_PER_PAGE:-0}" ]; then
      page=1
      case "$args" in
        *'&page='*)
          page="${args##*&page=}"
          page="${page%% *}"
          ;;
      esac
      if [ -n "${GH_STUB_RUN_PAGES_DIR:-}" ]; then
        page_file="$GH_STUB_RUN_PAGES_DIR/$page"
        [ ! -f "$page_file" ] || cat "$page_file"
      elif [ "$page" -eq 1 ]; then
        cat "$GH_STUB_RUNS"
      fi
    fi ;;
  *contents/.github/workflows/validate-go-project.yaml*)
    sha="${args##*ref=}"
    if grep -qxF -- "$sha" "$GH_STUB_TAINTED"; then
      printf 'HTTP/2.0 200 OK\n'
    else
      printf 'HTTP/2.0 404 Not Found\n'
    fi ;;
  *commits/*)
    sha="${args##*commits/}"
    if grep -qxF -- "$sha" "$GH_STUB_UNRESOLVABLE"; then exit 1; fi
    printf 'resolved\n' ;;
  *) printf 'unexpected gh call: %s\n' "$args" >&2; exit 99 ;;
esac
STUB
chmod +x "$scratch/bin/gh"
mkdir -p "$scratch/logs"
export GH_STUB_LOGDIR="$scratch/logs"
export GH_STUB_RUNS="$scratch/runs.txt"
export GH_STUB_TAINTED="$scratch/tainted.txt"
export GH_STUB_UNRESOLVABLE="$scratch/unresolvable.txt"
export GH_STUB_MIN_PER_PAGE=0
: >"$scratch/unresolvable.txt"

tainted_sha='deadbeefdeadbeefdeadbeefdeadbeefdeadbeef'
genuine_sha='cafebabecafebabecafebabecafebabecafebabe'

# Run 111 is attacker-controlled: its revision defines the managed workflow, and its log states
# versions that do NOT match the constants — so trusting it is visible as drift.
make_log '9.9.9' '9.9.9' '9.9.9' "$scratch/logs/job111.log"
make_log "$ci_megalinter" "$ci_checkov" "$ci_trivy" "$scratch/logs/job222.log"
printf '%s\n' "$tainted_sha" >"$scratch/tainted.txt"

run_live() {
  out="$(DRIFT_REPO_ROOT="$scratch/fakerepo" PATH="$scratch/bin:$PATH" "$script" 2>&1)" && rc=0 || rc=$?
}

# A tainted run as the ONLY candidate must never have its log consumed.
printf '111 %s\n' "$tainted_sha" >"$scratch/runs.txt"
run_live
if [ "$rc" -ne 2 ]; then
  printf 'FAIL: tainted-only candidate — expected exit 2, got %d (its log was trusted)\n%s\n' \
    "$rc" "$out" >&2
  failures=$((failures + 1))
elif ! printf '%s' "$out" | grep -qF 'CANNOT VERIFY'; then
  printf 'FAIL: tainted-only candidate — exit 2 but not a fail-closed refusal\n%s\n' "$out" >&2
  failures=$((failures + 1))
else
  printf 'ok: a run whose own revision defines the managed workflow is not trusted\n'
fi

# Negative control: the SAME candidate, now untainted, must be consumed and read as in sync. Without
# this the assertion above would pass for any unrelated refusal.
: >"$scratch/tainted.txt"
cp "$scratch/logs/job222.log" "$scratch/logs/job111.log"
run_live
if [ "$rc" -ne 0 ]; then
  printf 'FAIL: control — an untainted candidate should be consumed, got exit %d\n%s\n' \
    "$rc" "$out" >&2
  failures=$((failures + 1))
else
  printf 'ok: control — an untainted candidate is still consumed\n'
fi

# A dependency-update burst can create more than 40 repository-wide workflow runs between two
# executed MegaLinter jobs. The live incident that motivated this case buried the newest usable job
# at ordinal 99 even though it was less than an hour old. The selector must use the API's full
# single-page capacity so unrelated workflows do not make recent evidence unreachable.
GH_STUB_MIN_PER_PAGE=100
printf '111 %s\n' "$genuine_sha" >"$scratch/runs.txt"
run_live
if [ "$rc" -ne 0 ]; then
  printf 'FAIL: busy repository history — expected recent genuine run to be reachable, got %d\n%s\n' \
    "$rc" "$out" >&2
  failures=$((failures + 1))
else
  printf 'ok: a genuine run remains reachable after more than 40 unrelated workflow runs\n'
fi
GH_STUB_MIN_PER_PAGE=0

# One full page of unrelated workflows must not hide the first managed candidate on page two. This
# is the regression boundary from the review finding: `per_page=100` increases one response but does
# not paginate it. The page fixture makes the old one-request implementation fail closed, while a
# bounded multi-page lookup reaches the same genuine, provenance-checked candidate.
mkdir -p "$scratch/run-pages"
: >"$scratch/run-pages/1"
printf '111 %s\n' "$genuine_sha" >"$scratch/run-pages/2"
: >"$scratch/run-pages/3"
GH_STUB_RUN_PAGES_DIR="$scratch/run-pages"
export GH_STUB_RUN_PAGES_DIR
run_live
if [ "$rc" -ne 0 ]; then
  printf 'FAIL: paginated history — expected page-two genuine run to be reachable, got %d\n%s\n' \
    "$rc" "$out" >&2
  failures=$((failures + 1))
else
  printf 'ok: a genuine run remains reachable on the second workflow-run page\n'
fi
unset GH_STUB_RUN_PAGES_DIR

# Discrimination: the newest candidate is tainted, the next is genuine. The guard must SKIP the
# first and use the second. Consuming the first reports drift (exit 1) and aborting on it fails
# closed (exit 2), so only skipping-and-continuing yields exit 0.
make_log '9.9.9' '9.9.9' '9.9.9' "$scratch/logs/job111.log"
printf '%s\n' "$tainted_sha" >"$scratch/tainted.txt"
printf '111 %s\n222 %s\n' "$tainted_sha" "$genuine_sha" >"$scratch/runs.txt"
run_live
if [ "$rc" -ne 0 ]; then
  printf 'FAIL: discrimination — expected exit 0 from the genuine run, got %d\n%s\n' "$rc" "$out" >&2
  failures=$((failures + 1))
else
  printf 'ok: a tainted candidate is skipped and a later genuine one is used\n'
fi

# A revision that does not resolve is not evidence of absence. The contents endpoint answers 404
# for "no such file" AND for "no such commit", so without resolving the commit first an
# unresolvable revision would read as provably clean and its log would be trusted.
printf '%s\n' "$genuine_sha" >"$scratch/unresolvable.txt"
: >"$scratch/tainted.txt"
make_log '9.9.9' '9.9.9' '9.9.9' "$scratch/logs/job222.log"
printf '222 %s\n' "$genuine_sha" >"$scratch/runs.txt"
run_live
if [ "$rc" -ne 2 ]; then
  printf 'FAIL: unresolvable revision — expected exit 2, got %d (its log was trusted)\n%s\n' \
    "$rc" "$out" >&2
  failures=$((failures + 1))
else
  printf 'ok: a candidate whose revision does not resolve is not trusted\n'
fi
: >"$scratch/unresolvable.txt"

if [ "$failures" -ne 0 ]; then
  printf '\n%d check(s) failed\n' "$failures" >&2
  exit 1
fi
printf '\nmegalinter version-drift guard contract OK\n'
