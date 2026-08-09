#!/usr/bin/env bash
# Fail when the scanner versions recorded in scripts/megalinter-scan-counts.sh no longer match the
# ones MegaLinter actually runs in CI (#2853).
#
# WHY THIS EXISTS
# megalinter-scan-counts.sh reproduces CI's checkov and trivy counts locally, and its equivalence is
# EMPIRICAL — established against one CI run at three specific versions. Nothing in this repository
# selects those versions: the MegaLinter action is pinned in devantler-tech/actions. When that pin
# moves, the constants here go stale with no signal at all, and the helper's advisory note starts
# misleading in both directions — claiming a mismatch that no longer exists, or staying silent on a
# real one. A rule-set change between scanner versions looks exactly like backlog movement, which is
# the one thing #2787 must be able to tell apart.
#
# WHY IT LIVES IN CI AND NOT IN THE HELPER
# The helper's whole value is being an offline answer available before pushing. Deriving the live
# versions costs an authenticated API call and log download, so folding it in would put the network
# on the ordinary pre-push path. This runs in CI instead, where that cost is already paid.
#
# WHERE THE LIVE VERSIONS COME FROM
# The "🧹 Lint - mega-linter" job log prints them itself:
#   ghcr.io/oxsecurity/megalinter-go:v9.6.0
#   - Using [checkov v3.3.2] https://megalinter.io/9.6.0/descriptors/repository_checkov
#   - Using [trivy v0.71.2] https://megalinter.io/9.6.0/descriptors/repository_trivy
#
# FINDING THAT JOB IS THE AWKWARD PART, and the reason this walks runs instead of reading one:
#   * The workflow that runs mega-linter ("✅ Validate Go Project") is NOT in this repository — it is
#     org-managed, absent from the repo's workflow list, and its workflow id 404s. So this cannot be
#     a job with `needs:` on it, and it cannot be located by workflow file either.
#   * mega-linter is `skipped` on main. Measured 2026-08-09: skipped on all 25 most recent main
#     commits, including ones that changed k8s/. Reading "the latest run on main" therefore finds a
#     job that ran nothing and states no versions.
# So: walk recent runs newest-first and take the first mega-linter job that actually executed.
#
# Exit codes: 0 in sync · 1 drift (act on it) · 2 could not verify (fail closed).

set -euo pipefail

REPO="${GITHUB_REPOSITORY:-devantler-tech/platform}"
MEGALINTER_JOB_MATCH='mega-linter'
MAX_RUNS=40

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly REPO_ROOT
CONSTANTS_FILE="$REPO_ROOT/scripts/megalinter-scan-counts.sh"
readonly CONSTANTS_FILE
CONSTANTS_REL='scripts/megalinter-scan-counts.sh'
readonly CONSTANTS_REL

log_file=''
while [ $# -gt 0 ]; do
  case "$1" in
    --log-file)
      log_file="${2:?--log-file needs a path}"
      shift 2
      ;;
    --repo)
      REPO="${2:?--repo needs owner/name}"
      shift 2
      ;;
    -h | --help)
      sed -n '2,30p' "${BASH_SOURCE[0]}"
      exit 0
      ;;
    *)
      printf 'unknown argument: %s\n' "$1" >&2
      exit 2
      ;;
  esac
done

die_unverifiable() {
  printf 'CANNOT VERIFY: %s\n' "$1" >&2
  printf 'Failing closed: an unreadable log is not evidence that the versions still match.\n' >&2
  exit 2
}

# Read a constant from the file that owns it, rather than keeping a second copy that can itself go
# stale — the exact failure this guard exists to prevent.
read_const() {
  local name="$1" value
  value="$(sed -n "s/^readonly $name='\([^']*\)'.*/\1/p" "$CONSTANTS_FILE")"
  [ -n "$value" ] || die_unverifiable "could not read $name from $CONSTANTS_REL"
  printf '%s' "$value"
}

# Extract exactly one version for a tool. Fails closed on absent OR conflicting matches: a log with
# two different versions for one tool is not describing a single run, and silently taking one of
# them would report a comparison that means nothing.
#
# The pattern is anchored per tool because the real log contains `- Using [trivy-sbom v0.71.2]`
# directly beside `- Using [trivy v0.71.2]`; an unanchored match reads the sbom's version, which
# tracks a different component and would drift independently.
extract_one() {
  local label="$1" pattern="$2" file="$3" found
  found="$(grep -aoE "$pattern" "$file" | grep -aoE '[0-9]+\.[0-9]+\.[0-9]+[A-Za-z0-9.+-]*' |
    sort -u || true)"
  if [ -z "$found" ]; then
    die_unverifiable "could not find the $label version marker in the mega-linter log"
  fi
  if [ "$(printf '%s\n' "$found" | wc -l | tr -d ' ')" -ne 1 ]; then
    printf 'CANNOT VERIFY: conflicting %s versions in one log: %s\n' \
      "$label" "$(printf '%s' "$found" | tr '\n' ' ')" >&2
    exit 2
  fi
  printf '%s' "$found"
}

resolve_log() {
  command -v gh >/dev/null 2>&1 || die_unverifiable 'gh is not installed'
  local dest="$1" run_id job_id runs
  runs="$(gh api "repos/$REPO/actions/runs?per_page=$MAX_RUNS" \
    --jq '.workflow_runs[].id' 2>/dev/null)" ||
    die_unverifiable "could not list recent runs for $REPO"
  [ -n "$runs" ] || die_unverifiable "no recent workflow runs found for $REPO"

  for run_id in $runs; do
    # A skipped job states no versions, so require one that actually executed.
    job_id="$(gh api "repos/$REPO/actions/runs/$run_id/jobs?per_page=100" \
      --jq ".jobs[]
            | select(.name | test(\"$MEGALINTER_JOB_MATCH\"))
            | select(.conclusion == \"success\" or .conclusion == \"failure\")
            | .id" 2>/dev/null | head -1)" || continue
    if [ -n "$job_id" ]; then
      printf 'Reading mega-linter job %s (run %s) in %s\n' "$job_id" "$run_id" "$REPO" >&2
      gh api --allow-escape-sequences "repos/$REPO/actions/jobs/$job_id/logs" >"$dest" 2>/dev/null ||
        die_unverifiable "could not download the log for job $job_id"
      return 0
    fi
  done
  die_unverifiable "no completed '$MEGALINTER_JOB_MATCH' job in the last $MAX_RUNS runs of $REPO"
}

ci_megalinter="$(read_const CI_MEGALINTER_VERSION)"
ci_checkov="$(read_const CI_CHECKOV_VERSION)"
ci_trivy="$(read_const CI_TRIVY_VERSION)"

scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

if [ -n "$log_file" ]; then
  [ -r "$log_file" ] || die_unverifiable "log file not readable: $log_file"
  cp "$log_file" "$scratch/job.log"
else
  resolve_log "$scratch/job.log"
fi
[ -s "$scratch/job.log" ] || die_unverifiable 'the mega-linter log is empty'

live_megalinter="$(extract_one megalinter 'ghcr\.io/oxsecurity/megalinter[a-z-]*:v[0-9][^[:space:]"]*' "$scratch/job.log")"
live_checkov="$(extract_one checkov '\[checkov v[0-9][^]]*\]' "$scratch/job.log")"
live_trivy="$(extract_one trivy '\[trivy v[0-9][^]]*\]' "$scratch/job.log")"

drifted=''
compare() {
  local label="$1" constant="$2" recorded="$3" live="$4"
  if [ "$recorded" = "$live" ]; then
    printf '  %-11s %-10s matches CI\n' "$label" "$recorded"
  else
    printf '  %-11s %-10s CI now runs %s   <-- DRIFT (%s)\n' "$label" "$recorded" "$live" "$constant"
    drifted="$drifted $constant=$live"
  fi
}

printf 'Recorded vs live MegaLinter scanner versions:\n'
compare megalinter "CI_MEGALINTER_VERSION" "$ci_megalinter" "$live_megalinter"
compare checkov "CI_CHECKOV_VERSION" "$ci_checkov" "$live_checkov"
compare trivy "CI_TRIVY_VERSION" "$ci_trivy" "$live_trivy"

if [ -n "$drifted" ]; then
  cat >&2 <<EOF

DRIFT: MegaLinter in CI no longer runs the versions $CONSTANTS_REL records.

Why this fails the build rather than warning: a scanner rule-set change adds or retires findings, and
that is indistinguishable from someone fixing or introducing them. Leaving the constants stale means
the next #2787 slice cannot tell which it measured.

To fix, set these in $CONSTANTS_REL:
EOF
  for pair in $drifted; do
    printf "  readonly %s='%s'\n" "${pair%%=*}" "${pair##*=}" >&2
  done
  cat >&2 <<EOF

Then re-measure the checkov baseline on the new version and record it on #2787 — the count may move
for reasons that are not backlog progress. If CI_CHECKOV_FRAMEWORKS' per-framework baselines change
too, update them in the same commit.
EOF
  exit 1
fi

printf '\nIn sync with MegaLinter %s.\n' "$live_megalinter"
