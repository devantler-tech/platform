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
[ "${STUB_DOCKER_RC:-0}" = 0 ] || { printf 'denied\n'; exit "$STUB_DOCKER_RC"; }
printf '%s\n' "$STUB_DIGEST"
EOF
cat >"$scratch/bin/go" <<'EOF'
#!/usr/bin/env bash
printf 'go %s\n' "$*" >>"$STUB_LOG"
printf '%s\n' "$STUB_RESOLVER_OUT"
exit "$STUB_RESOLVER_RC"
EOF
chmod +x "$scratch/bin/docker" "$scratch/bin/go"

run_case() { # <label> <expected-rc> <expected-annotation> <digest> <docker-rc> <resolver-rc> <resolver-out>
  local label="$1" want_rc="$2" want_annotation="$3" out rc
  : >"$scratch/log"
  : >"$scratch/summary"
  if out="$(cd "$repo_root" && PATH="$scratch/bin:$PATH" STUB_LOG="$scratch/log" \
    STUB_DIGEST="$4" STUB_DOCKER_RC="$5" STUB_RESOLVER_RC="$6" STUB_RESOLVER_OUT="$7" \
    GITHUB_STEP_SUMMARY="$scratch/summary" "$script" 2>&1)"; then rc=0; else rc=$?; fi

  assertions=$((assertions + 1))
  if [ "$rc" = "$want_rc" ] && [[ "$out" == *"$want_annotation"* ]] && [ -s "$scratch/summary" ]; then
    printf '  ok   %s (exit %s)\n' "$label" "$rc"
  else
    printf '  FAIL %s: expected exit %s with %s and a summary, got exit %s:\n%s\n' \
      "$label" "$want_rc" "$want_annotation" "$rc" "$out"
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
  "$good_digest" 0 0 "CONVERGED attested commit is main"
assert_log "resolver receives the digest :latest names" "--digest $good_digest"
assert_log "digest is read from :latest" "ghcr.io/devantler-tech/platform/manifests:latest"

run_case "behind warns without failing" 0 "::warning title=Production is not on main::BEHIND" \
  "$good_digest" 0 1 "BEHIND deploy input changed since"
run_case "diverged warns without failing" 0 "::warning title=Production is not on main::DIVERGED" \
  "$good_digest" 0 1 "DIVERGED not reachable from main"

run_case "unknown fails" 1 "::error title=Production convergence unknown::" \
  "$good_digest" 0 2 "UNKNOWN attestation could not be verified"
run_case "unreadable digest fails" 1 "could not read the digest" \
  "$good_digest" 1 0 "CONVERGED never asked"
run_case "malformed digest fails" 1 "not a sha256 digest" \
  "sha256:short" 0 0 "CONVERGED never asked"
run_case "exit 0 without CONVERGED fails" 1 "resolver exited 0" \
  "$good_digest" 0 0 "BEHIND contradicts exit 0"
run_case "exit 1 claiming CONVERGED fails" 1 "resolver exited 1" \
  "$good_digest" 0 1 "CONVERGED contradicts exit 1"

printf '%s assertions, %s failures\n' "$assertions" "$failures"
[ "$failures" -eq 0 ]
