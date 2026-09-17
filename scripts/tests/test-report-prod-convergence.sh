#!/usr/bin/env bash
#
# Pins scripts/report-prod-convergence.sh against stub `docker` and `go` commands.
#
# Each case gives the stubs its own digest and resolver answer, so an assertion
# can only be satisfied by the case it belongs to. The fail-closed cases matter
# most: an unreadable digest, an UNKNOWN verdict, and resolver output that
# contradicts its exit status must all fail, never read as converged.

set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="$repo_root/scripts/report-prod-convergence.sh"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

failures=0
assertions=0

[ -x "$script" ] || { printf 'FAIL: %s is not executable\n' "$script"; exit 1; }

good_digest="sha256:$(printf 'a%.0s' {1..64})"

mkdir -p "$scratch/bin"
cat >"$scratch/bin/docker" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
[ "${STUB_DOCKER_RC:-0}" = 0 ] || { printf 'denied\n' >&2; exit "$STUB_DOCKER_RC"; }
printf 'WARNING: a harmless notice on stderr\n' >&2
printf '%s\n' "$STUB_DIGEST"
EOF
cat >"$scratch/bin/gh" <<'EOF'
#!/usr/bin/env bash
printf 'gh %s\n' "$*" >>"$STUB_LOG"
[ -n "${STUB_ATTESTED:-}" ] || exit 1
printf '%s\n' $STUB_ATTESTED
EOF
cat >"$scratch/bin/git" <<'EOF'
#!/usr/bin/env bash
printf 'git %s\n' "$*" >>"$STUB_LOG"
case "$1" in
  cat-file) [[ " ${STUB_PRESENT:-} " == *" ${3%^\{commit\}} "* ]] ;;
  fetch) exit 0 ;;
esac
EOF
cat >"$scratch/bin/go" <<'EOF'
#!/usr/bin/env bash
printf 'go %s\n' "$*" >>"$STUB_LOG"
printf '%s\n' "$STUB_RESOLVER_OUT"
exit "$STUB_RESOLVER_RC"
EOF
chmod +x "$scratch/bin/docker" "$scratch/bin/go" "$scratch/bin/gh" "$scratch/bin/git"
export STUB_ATTESTED="" STUB_PRESENT=""

run_case() { # <label> <expected-rc> <expected-annotation> <digest> <docker-rc> <resolver-rc> <resolver-out> <expected-output>
  local label="$1" want_rc="$2" want_annotation="$3" want_output="$8" out rc got_output
  : >"$scratch/log"
  : >"$scratch/summary"
  : >"$scratch/output"
  if out="$(cd "$repo_root" && PATH="$scratch/bin:$PATH" STUB_LOG="$scratch/log" \
    STUB_DIGEST="$4" STUB_DOCKER_RC="$5" STUB_RESOLVER_RC="$6" STUB_RESOLVER_OUT="$7" \
    GITHUB_STEP_SUMMARY="$scratch/summary" GITHUB_OUTPUT="$scratch/output" "$script" 2>&1)"; then rc=0; else rc=$?; fi

  assertions=$((assertions + 1))
  if [ "$rc" = "$want_rc" ] && [[ "$out" == *"$want_annotation"* ]] && [ -s "$scratch/summary" ]; then
    printf '  ok   %s (exit %s)\n' "$label" "$rc"
  else
    printf '  FAIL %s: expected exit %s with %s and a summary, got exit %s:\n%s\n' \
      "$label" "$want_rc" "$want_annotation" "$rc" "$out"
    failures=$((failures + 1))
  fi

  # The workflow redeploys main on this output, so it must be exactly the verdict,
  # and empty whenever the verdict could not be established.
  got_output="$(cat "$scratch/output")"
  assertions=$((assertions + 1))
  if [ "$got_output" = "$want_output" ]; then
    printf '  ok   %s writes output [%s]\n' "$label" "$want_output"
  else
    printf '  FAIL %s: expected output [%s], got [%s]\n' "$label" "$want_output" "$got_output"
    failures=$((failures + 1))
  fi
}

assert_log() { # <label> <needle>
  assertions=$((assertions + 1))
  if grep -Fq -- "$2" "$scratch/log"; then
    printf '  ok   %s\n' "$1"
  else
    printf '  FAIL %s: %s not in stub log:\n%s\n' "$1" "$2" "$(cat "$scratch/log")"
    failures=$((failures + 1))
  fi
}

run_case "converged is a notice" 0 "::notice title=Production converged::CONVERGED" \
  "$good_digest" 0 0 "CONVERGED attested commit is main" "verdict=CONVERGED"
assert_log "resolver receives the digest :latest names" "--digest $good_digest"
assert_log "digest is read from :latest" "ghcr.io/devantler-tech/platform/manifests:latest"

run_case "behind warns without failing" 0 "::warning title=Production is not on main::BEHIND" \
  "$good_digest" 0 1 "BEHIND deploy input changed since" "verdict=BEHIND"
run_case "diverged warns without failing" 0 "::warning title=Production is not on main::DIVERGED" \
  "$good_digest" 0 1 "DIVERGED not reachable from main" "verdict=DIVERGED"

run_case "unknown fails" 1 "::error title=Production convergence unknown::" \
  "$good_digest" 0 2 "UNKNOWN attestation could not be verified" ""
run_case "unreadable digest fails" 1 "could not read the digest" \
  "$good_digest" 1 0 "CONVERGED never asked" ""
run_case "malformed digest fails" 1 "not a sha256 digest" \
  "sha256:short" 0 0 "CONVERGED never asked" ""
run_case "exit 0 without CONVERGED fails" 1 "resolver exited 0" \
  "$good_digest" 0 0 "BEHIND contradicts exit 0" ""
run_case "exit 1 claiming CONVERGED fails" 1 "resolver exited 1" \
  "$good_digest" 0 1 "CONVERGED contradicts exit 1" ""

assert_no_log() { # <label> <needle>
  assertions=$((assertions + 1))
  if grep -Fq -- "$2" "$scratch/log"; then
    printf '  FAIL %s: %s unexpectedly in stub log\n' "$1" "$2"
    failures=$((failures + 1))
  else
    printf '  ok   %s\n' "$1"
  fi
}

missing_commit="$(printf 'b%.0s' {1..40})"
present_commit="$(printf 'c%.0s' {1..40})"
STUB_ATTESTED="$missing_commit $present_commit" STUB_PRESENT="$present_commit"
export STUB_ATTESTED STUB_PRESENT
run_case "attested commits are prepared before resolving" 0 "CONVERGED" \
  "$good_digest" 0 0 "CONVERGED healed artifact" "verdict=CONVERGED"
assert_log "a missing attested commit is fetched" "git fetch --quiet --no-tags origin $missing_commit"
assert_no_log "a present attested commit is not fetched" "origin $present_commit"

STUB_ATTESTED="not-a-commit"
run_case "a malformed attested value is never fetched" 0 "CONVERGED" \
  "$good_digest" 0 0 "CONVERGED ignored" "verdict=CONVERGED"
assert_no_log "malformed value reaches no fetch" "git fetch"

# The workflow acts on the verdict written above. Only BEHIND may deploy: DIVERGED is
# what a merge group that deployed before merging looks like, and a failed check writes
# no verdict at all. The observer must also wait on the prod-deploy lock, so it cannot
# read `:latest` while a heal is still about to overwrite it.
workflow="$repo_root/.github/workflows/validate-main.yaml"
assert_workflow() { # <label> <yq-expression> <expected>
  local got
  assertions=$((assertions + 1))
  if got="$(yq -r "$2" "$workflow")" && [ "$got" = "$3" ]; then
    printf '  ok   %s\n' "$1"
  else
    printf '  FAIL %s: expected [%s], got [%s]\n' "$1" "$3" "${got:-}"
    failures=$((failures + 1))
  fi
}
# shellcheck disable=SC2016 # a literal workflow expression, not a shell expansion
assert_workflow "the observer exposes the report's verdict" \
  '.jobs.observe-prod-convergence.outputs.verdict' '${{ steps.report.outputs.verdict }}'
assert_workflow "the report step carries the id the output reads" \
  '.jobs.observe-prod-convergence.steps[] | select(.run == "./scripts/report-prod-convergence.sh") | .id' 'report'
assert_workflow "the observer waits on the prod-deploy lock" \
  '.jobs.observe-prod-convergence.concurrency.group' 'prod-deploy'
assert_workflow "the observer never cancels a queued deploy" \
  '.jobs.observe-prod-convergence.concurrency.cancel-in-progress' 'false'
assert_workflow "the redeploy waits for the observer" \
  '.jobs.redeploy-prod-when-behind.needs' 'observe-prod-convergence'
assert_workflow "only BEHIND redeploys" \
  '.jobs.redeploy-prod-when-behind.if' "needs.observe-prod-convergence.outputs.verdict == 'BEHIND'"
# shellcheck disable=SC2016 # a literal workflow command, not a shell expansion
assert_workflow "the redeploy goes through the gated CD workflow on main" \
  '.jobs.redeploy-prod-when-behind.steps[-1].run' 'gh workflow run cd.yaml --repo "${REPOSITORY}" --ref main'

printf '%s assertions, %s failures\n' "$assertions" "$failures"
[ "$failures" -eq 0 ]
