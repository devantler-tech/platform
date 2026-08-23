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
#   trivy — MegaLinter reports "1 non blocking error" on a scan that actually fails 612 checks. The
#   1 is an artifact of how MegaLinter counts trivy's output, NOT a finding count. Do not read it as
#   "one finding left". This script reports what trivy actually found.
#
# HOW THE INVOCATIONS WERE DERIVED
# Both command lines are copied from MegaLinter's own log, which prints the exact command it ran:
#   - Command: [checkov --skip-path tests/ --config-file /action/lib/.automation/.checkov.yml
#     --skip-path .agents --skip-path megalinter-reports --directory . --skip-framework secrets]
#   - Command: [trivy fs --scanners vuln,misconfig --exit-code 1 --ignorefile
#     .trivyignore.yaml --skip-dirs tests --config-data .trivy/data .]
# Read them from a "🧹 Lint - mega-linter" job log if they ever need re-deriving:
#   gh api repos/devantler-tech/platform/actions/jobs/<job-id>/logs | grep -aE '^\S+ - Command: '
#
# ⚠️ SCAN THE REPOSITORY ROOT. Narrowing trivy's target to k8s/ INFLATES the count, silently.
# .trivyignore.yaml's dispositions are path-scoped, and every one of them is written relative to the
# repository root (k8s/bases/infrastructure/**, k8s/clusters/prod/bootstrap/config-map.yaml, ...).
# Trivy matches those globs against paths relative to the SCAN ROOT, so a scan rooted at k8s/ sees
# bases/infrastructure/... instead, and the path-scoped entries stop matching. They deactivate with
# no warning, and the run re-reports findings this repository has already dispositioned.
#
# Measured on this tree with trivy v0.74.0, same subcommand and same flags, scan root the ONLY
# variable: `... .` reports 66 findings across 16 targets, `... k8s/` reports 133 across 62 — the
# count doubles. Every check ID that appears only in the k8s/-rooted arm (KSV-0022, KSV-0037,
# KSV-0053, KSV-0109, KSV-0114, KSV-0117) is already dispositioned there; KSV-0037 alone
# accounts for 47 of the 67 extra. A few IDs rise without being new to that arm (KSV-0041, KSV-0046,
# KSV-0056) because their disposition covers only some paths, so the scoped subset deactivates while
# their other instances legitimately remain.
#
# The failure direction is what makes this dangerous: it looks like more backlog to work off, not
# like a broken measurement, so it reads as plausible and gets believed. Run the command above from
# the repository root, or the number is not comparable to CI's.
#
# TWO DELIBERATE DIFFERENCES FROM THE CI COMMAND, both verified not to change the counts.
# (--skip-framework secrets is NOT one of them: CI passes it too since MegaLinter 10.0.0, so
# mirroring it keeps the invocations aligned rather than diverging them. The two --skip-path
# entries CI adds cover directories that do not exist here.)
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
readonly CI_CHECKOV_VERSION='3.3.9'
readonly CI_TRIVY_VERSION='0.73.0'

# The MegaLinter release those two versions came from. It is not a local concept — nothing here runs
# MegaLinter — but it is what makes the pair above checkable: MegaLinter bundles both scanners AND
# the .checkov.yml this script deliberately drops, all inside one immutable image tag. So the image
# version is the single provenance for every CI-side input this script models, and the only one of
# them that a log states directly.
#
# Nothing in THIS repository selects it: the MegaLinter action is pinned in devantler-tech/actions,
# so all three constants go stale here when that pin moves, silently and in both directions.
# scripts/check-megalinter-version-drift.sh is the CI-side guard that catches it (#2853).
readonly CI_MEGALINTER_VERSION='10.0.0'

# The frameworks MegaLinter's checkov run reports, and their current contribution to the CI total.
#
# A missing framework is REPORTED, never refused, because the two causes are indistinguishable from
# checkov's output alone: a framework whose last finding is fixed stops emitting a section entirely
# (it does not print a zero), and a framework dropped by a broken invocation also takes its findings
# with it. Both look like "section absent, total lower". Gating on presence would therefore refuse at
# exactly the cleared-backlog state this helper exists to certify — the same self-defeating shape as
# demanding trivy detail rows from a clean scan.
#
# What protects against the known defect is structural rather than a check: this script always runs
# checkov from the repository root with a literal ".", the invocation whose absence caused the
# kubernetes framework to vanish in the first place.
# MegaLinter 10.0.0 reports THREE frameworks, not four: its own checkov invocation now passes
# --skip-framework secrets, so CI emits no secrets section at all. `secrets` is therefore removed
# from this list — it records what CI reports, and a framework CI has stopped running is not a
# framework whose absence should be reported against a local run. The local invocation below mirrors
# that skip so the totals stay comparable, which is the whole point of both lists.
#
# kubernetes moved 15 -> 3 in the SAME step that took MegaLinter 9.6.0 -> 10.0.0 (checkov
# 3.3.2 -> 3.3.9). That drop is NOT recorded as backlog progress, and #2787 must not read it as any:
# a major scanner bump adds and retires rules, which is indistinguishable from findings being fixed
# unless the two versions are run against the same tree. Nobody has done that here.
#
# Every framework CI runs is now at zero. The three that were left are all dispositioned with
# scoped, resource-level skips naming their reason:
#   CKV_K8S_38  CronJob.umami.umami-provision-tenants         (SA token mounted)   #3198
#   CKV_K8S_40  CronJob.openbao.vault-snapshot                (image-defined UID)  #3282
#   CKV_K8S_40  Job.openbao.vault-snapshot-init               (image-defined UID)  #3282
# The two CKV_K8S_40 skips are narrow permanent dispositions: each pod defaults to high UID 65532,
# and the identity-boundary guard permits UID 100 only on the container whose OpenBao image defines it.
readonly CI_CHECKOV_FRAMEWORKS=(cloudformation:0 kubernetes:0 github_actions:0)

# A parsing error means a file was NOT analysed, so findings can hide behind it. CI's run has
# exactly one (in the cloudformation framework) and so does a correct local run, which is why this
# is a ceiling rather than a zero check — refusing on any parsing error would refuse on the current,
# CI-matching state. Anything above the ceiling is local-only and breaks comparability.
readonly CI_CHECKOV_PARSING_ERRORS=1

# TWO RESIDUAL DRIFT RISKS, REVIEWED AND ACCEPTED (#2853). Recorded here rather than fixed, because
# in both cases the fix costs more than the failure it prevents. Revisit if either assumption moves.
#
#   Parsing-error IDENTITY, not just count. The check above compares a count, so one file becoming
#   parsable while another stops would net to zero and pass. Detecting that needs per-file parse
#   errors, which --compact suppresses — so closing it means dropping --compact and parsing full
#   output, a large rewrite of every count path here. Accepted because the blast radius is small and
#   self-limiting: the swap would have to happen between two runs, and any findings hidden behind the
#   newly-unparsable file still surface in CI, which does not use this script. The count remains a
#   ceiling, so the common direction — a NEW parse error appearing — is still caught.
#
#   Trivy's vulnerability DATABASE revision moves independently of the CLI version, so
#   check-megalinter-version-drift.sh cannot pin it. No effect today: this repository reports 0
#   vulnerabilities, and the misconfiguration counts that #2787 tracks come from built-in policies
#   rather than the DB. Accepted on that basis, and the reason the two categories are reported
#   SEPARATELY below is to keep it visible — a vulnerability count that moves without a code change
#   is the signal that this assumption has expired.

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
  # the report entirely and, in the historical pre-#2899 reproduction, silently dropped the total
  # from 73 to 31 with no error. Reproduced both with and without --skip-path, so it is the absolute
  # path itself.
  (cd "$REPO_ROOT" && checkov --skip-path tests/ --skip-framework kustomize \
    --skip-framework secrets --directory . --compact --quiet) >"$out" 2>&1 || rc=$?
  if [ "$rc" -gt 1 ]; then
    printf 'checkov exited %d — refusing to report a count from an incomplete run\n' "$rc" >&2
    tail -n 5 "$out" >&2
    exit 2
  fi
  # Every framework CI reports must be present. "At least one section" is not enough: a silently
  # dropped framework is the failure mode already seen here — an absolute --directory removes the
  # whole kubernetes framework and its then-current 42 findings while the other three still report,
  # so the historical run looked healthy and the total was simply 42 lower.
  local absent='' entry fw baseline
  for entry in "${CI_CHECKOV_FRAMEWORKS[@]}"; do
    fw="${entry%%:*}"
    baseline="${entry##*:}"
    if ! grep -aqE "^${fw} scan results:" "$out"; then
      absent="$absent ${fw}(${baseline})"
    fi
  done

  local parse_errors
  parse_errors="$(sum_by 'Parsing errors: [0-9]+' "$out")"
  if [ "$parse_errors" -gt "$CI_CHECKOV_PARSING_ERRORS" ]; then
    printf 'checkov reported %d parsing errors, above CI'"'"'s %d — refusing to report a count.\n' \
      "$parse_errors" "$CI_CHECKOV_PARSING_ERRORS" >&2
    printf 'An unparsed file is not analysed, so findings can hide behind it.\n' >&2
    exit 2
  fi

  local total
  total="$(sum_by 'Failed checks: [0-9]+' "$out")"

  printf '\ncheckov — %s failing checks (%s parsing error(s); CI has %s)\n' \
    "$total" "$parse_errors" "$CI_CHECKOV_PARSING_ERRORS"
  if [ -n "$absent" ]; then
    printf '  ⚠ framework(s) CI reports are absent here, with their CI finding counts:%s\n' "$absent"
    printf '    Either those findings are genuinely gone, or the scan did not cover them. The two\n'
    printf '    are indistinguishable from this output, so check before recording a lower total:\n'
    printf '    re-run from the repository root, and confirm the drop matches the counts above.\n'
  fi
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
  (cd "$REPO_ROOT" && trivy fs --scanners vuln,misconfig --exit-code 1 \
    --ignorefile .trivyignore.yaml --skip-dirs tests --config-data .trivy/data .) \
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

printf 'Scanner versions (CI runs them from MegaLinter %s):\n' "$CI_MEGALINTER_VERSION"
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
