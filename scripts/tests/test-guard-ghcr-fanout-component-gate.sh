#!/usr/bin/env bash
#
# Pins the GHCR fan-out / apps-base component-gate parity guard in all THREE
# directions.
#
#   exit 0  every ghcr-auth app's presence in FANOUT_NAMESPACES matches whether
#           it is enabled in k8s/bases/apps/kustomization.yaml
#   exit 1  the two disagree in EITHER direction, or a listed namespace is
#           neither a ghcr-auth app nor a declared non-app namespace
#   exit 2  the guard could not check — missing inputs, NO ghcr-auth app found,
#           or an empty FANOUT_NAMESPACES
#
# Both drift directions are asserted separately because they are different
# failures with different consequences, and a guard that caught only one would
# still have let platform#3476 through:
#
#   * staged off but still listed  — the post-reconcile reassertion can never
#     find the ExternalSecret, so EVERY prod deploy fails. This is the direction
#     that actually shipped.
#   * enabled but unlisted        — a running consumer's pull credential is
#     never proven.
#
# The anti-vacuity cases matter more than they look: a tree with no ghcr-auth
# app, or an array that parsed empty, produces exactly the same "no failures
# found" shape as a healthy tree. An empty result from a filtered read is a
# claim about the filter, so both must be exit 2 and never exit 0.
#
# The reference-vs-declaration case is the third silent-pass risk: a
# ServiceAccount imagePullSecret and an OCIRepository secretRef both carry a
# bare `name: ghcr-auth` without declaring the ExternalSecret, so a grep-based
# selector would treat an app whose credential is declared elsewhere as an
# in-scope consumer and report a false failure on a correct tree.
#
# Every fixture uses its OWN app names, so an assertion can only be satisfied by
# the case it belongs to.

set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
guard="$repo_root/scripts/guard-ghcr-fanout-component-gate.sh"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

failures=0
assertions=0

[ -x "$guard" ] || { printf 'FAIL: %s is not executable\n' "$guard"; exit 1; }

run_guard() { # <root>
  # Capture the status with `if`: this file runs under `set -uo pipefail` with
  # NO errexit, so `set -e` here would enable a mode the script never had.
  if GUARD_OUT="$("$guard" "$1" 2>&1)"; then GUARD_RC=0; else GUARD_RC=$?; fi
}

assert_rc() { # <label> <expected> <actual>
  assertions=$((assertions + 1))
  if [ "$2" = "$3" ]; then
    printf '  ok   %s (exit %s)\n' "$1" "$3"
  else
    printf '  FAIL %s: expected exit %s, got %s\n' "$1" "$2" "$3"
    printf '%s\n' "$GUARD_OUT" | sed 's/^/       | /'
    failures=$((failures + 1))
  fi
}

assert_contains() { # <label> <needle>
  assertions=$((assertions + 1))
  # A here-string, NOT a pipe: `grep -q` exits on first match and the SIGPIPE it
  # gives the upstream writer becomes the pipeline status under pipefail.
  if grep -qF -- "$2" <<<"$GUARD_OUT"; then
    printf '  ok   %s\n' "$1"
  else
    printf '  FAIL %s: output did not contain %s\n' "$1" "$2"
    printf '%s\n' "$GUARD_OUT" | sed 's/^/       | /'
    failures=$((failures + 1))
  fi
}

# --- fixture builders -------------------------------------------------------

# make_root <name> -> echoes the root path
make_root() {
  local root="$scratch/$1"
  mkdir -p "$root/k8s/bases/apps" "$root/scripts"
  printf '%s\n' "$root"
}

# add_app <root> <app> <declares-ghcr-auth: yes|no|reference-only>
add_app() {
  local root="$1" app="$2" mode="$3"
  mkdir -p "$root/k8s/bases/apps/$app"
  case "$mode" in
    yes)
      cat >"$root/k8s/bases/apps/$app/external-secret.yaml" <<YAML
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: ghcr-auth
  namespace: $app
YAML
      ;;
    reference-only)
      # References the secret without declaring the ExternalSecret — the exact
      # shape a grep-based selector would misread as a declaration.
      cat >"$root/k8s/bases/apps/$app/service-account.yaml" <<YAML
apiVersion: v1
kind: ServiceAccount
metadata:
  name: $app
imagePullSecrets:
  - name: ghcr-auth
YAML
      ;;
    no)
      cat >"$root/k8s/bases/apps/$app/deployment.yaml" <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $app
YAML
      ;;
  esac
}

# write_kustomization <root> <enabled-app>...   (apps not listed are staged off)
write_kustomization() {
  local root="$1"; shift
  {
    printf -- '---\napiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n'
    local a
    for a in "$@"; do printf -- '  - %s/\n' "$a"; done
    # A staged-off entry is a comment, exactly as the real base writes it.
    local dir app
    for dir in "$root"/k8s/bases/apps/*/; do
      app="$(basename "$dir")"
      case " $* " in *" $app "*) ;; *) printf -- '  # - %s/\n' "$app" ;; esac
    done
  } >"$root/k8s/bases/apps/kustomization.yaml"
}

# write_fanout <root> <namespace>...
write_fanout() {
  local root="$1"; shift
  {
    printf '#!/usr/bin/env bash\nreadonly -a FANOUT_NAMESPACES=(\n'
    local n
    for n in "$@"; do printf '  "%s"\n' "$n"; done
    printf ')\n'
  } >"$root/scripts/refresh-flux-ghcr-auth.sh"
}

# --- case 1: parity holds ---------------------------------------------------
printf 'case: parity holds (one enabled consumer, one staged off)\n'
root="$(make_root parity)"
add_app "$root" alpha-shop yes
add_app "$root" bravo-staged yes
write_kustomization "$root" alpha-shop
write_fanout "$root" alpha-shop kyverno
run_guard "$root"
assert_rc 'parity holds' 0 "$GUARD_RC"
assert_contains 'reports the staged-off app as correctly absent' 'bravo-staged — staged off'

# --- case 2: staged off but still listed (the shipped regression) -----------
printf 'case: staged off but still listed\n'
root="$(make_root staged-but-listed)"
add_app "$root" charlie-ghost yes
# A plain enabled app so the base still declares resources: a kustomization with
# an EMPTY resource list is a degenerate tree the guard refuses as UNKNOWN, and
# that refusal would mask the drift this case exists to assert.
add_app "$root" charlie-neighbour no
write_kustomization "$root" charlie-neighbour
write_fanout "$root" charlie-ghost kyverno
run_guard "$root"
assert_rc 'staged off + listed is drift' 1 "$GUARD_RC"
assert_contains 'names the app' 'charlie-ghost is COMMENTED OUT'
assert_contains 'names the deploy consequence' 'every prod deploy would fail'

# --- case 3: enabled but unlisted ------------------------------------------
printf 'case: enabled but unlisted\n'
root="$(make_root enabled-unlisted)"
add_app "$root" delta-unproven yes
write_kustomization "$root" delta-unproven
write_fanout "$root" kyverno
run_guard "$root"
assert_rc 'enabled + unlisted is drift' 1 "$GUARD_RC"
assert_contains 'names the app' 'delta-unproven is ENABLED'

# --- case 4: orphan / typo in the fan-out list ------------------------------
printf 'case: orphaned fan-out entry\n'
root="$(make_root orphan)"
add_app "$root" echo-real yes
write_kustomization "$root" echo-real
write_fanout "$root" echo-real echo-rael kyverno
run_guard "$root"
assert_rc 'orphan entry is drift' 1 "$GUARD_RC"
assert_contains 'names the orphan' 'echo-rael is in FANOUT_NAMESPACES'

# --- case 5: a bare reference is NOT a declaration --------------------------
printf 'case: reference-only app is out of scope\n'
root="$(make_root reference-only)"
add_app "$root" foxtrot-consumer yes
add_app "$root" golf-referencer reference-only
write_kustomization "$root" foxtrot-consumer golf-referencer
write_fanout "$root" foxtrot-consumer kyverno
run_guard "$root"
assert_rc 'a referencing app does not require a fan-out entry' 0 "$GUARD_RC"
assertions=$((assertions + 1))
if grep -qF -- 'golf-referencer' <<<"$GUARD_OUT"; then
  printf '  FAIL reference-only app was treated as a ghcr-auth consumer\n'
  printf '%s\n' "$GUARD_OUT" | sed 's/^/       | /'
  failures=$((failures + 1))
else
  printf '  ok   reference-only app is excluded from the ghcr-auth set\n'
fi

# --- case 6: anti-vacuity — no ghcr-auth app at all -------------------------
printf 'case: no ghcr-auth app (anti-vacuity)\n'
root="$(make_root no-consumer)"
add_app "$root" hotel-plain no
write_kustomization "$root" hotel-plain
write_fanout "$root" kyverno
run_guard "$root"
assert_rc 'an empty consumer set is UNKNOWN, not clean' 2 "$GUARD_RC"
assert_contains 'says it refused rather than passed' 'refusing to report parity on an empty set'

# --- case 7: anti-vacuity — empty FANOUT_NAMESPACES -------------------------
printf 'case: empty FANOUT_NAMESPACES (anti-vacuity)\n'
root="$(make_root empty-fanout)"
add_app "$root" india-consumer yes
write_kustomization "$root" india-consumer
write_fanout "$root"
run_guard "$root"
assert_rc 'an empty fan-out list is UNKNOWN, not clean' 2 "$GUARD_RC"
assert_contains 'says the array parsed empty' 'parsed as EMPTY'

# --- case 8: missing inputs -------------------------------------------------
printf 'case: missing apps base (anti-vacuity)\n'
run_guard "$scratch/does-not-exist"
assert_rc 'a missing root is UNKNOWN' 2 "$GUARD_RC"

# --- case 9: an entry the parser cannot represent is REJECTED, not dropped --
# `foo` and 'foo' are equally valid Bash array entries, but this guard only
# understands a "double-quoted" token. Silently dropping the others would make
# it compare against a SMALLER namespace set than the array really expands to,
# and so report parity for a staged-off app whose ExternalSecret production
# still demands — the every-deploy failure this guard exists to catch,
# reintroduced through the guard itself.
printf 'case: an unquoted fan-out entry is rejected, never silently dropped\n'
root="$(make_root unquoted-entry)"
add_app "$root" juliet-consumer yes
add_app "$root" kilo-staged yes
write_kustomization "$root" juliet-consumer
{
  printf '#!/usr/bin/env bash\nreadonly -a FANOUT_NAMESPACES=(\n'
  printf '  kilo-staged\n'
  printf '  "juliet-consumer"\n'
  printf '  "kyverno"\n'
  printf ')\n'
} >"$root/scripts/refresh-flux-ghcr-auth.sh"
# Premise first: assert the unquoted entry really IS in the array Bash builds,
# so this case cannot pass by testing a value that was never live.
assertions=$((assertions + 1))
# A here-string, NOT a pipe: `grep -q` exits on first match and the SIGPIPE it
# gives the upstream writer becomes the pipeline status under pipefail.
fanout_runtime="$(bash -c 'source "$1"; printf "%s\n" "${FANOUT_NAMESPACES[@]}"' _ \
  "$root/scripts/refresh-flux-ghcr-auth.sh")"
if grep -qxF -- 'kilo-staged' <<<"$fanout_runtime"; then
  printf '  ok   premise: the unquoted entry is a live value in the runtime array\n'
else
  printf '  FAIL premise: kilo-staged is absent from the runtime array\n'
  failures=$((failures + 1))
fi
run_guard "$root"
assert_rc 'an unparseable entry is UNKNOWN, not clean' 2 "$GUARD_RC"
# `kilo-staged` alone does NOT discriminate — the pre-fix guard printed that
# same name in its (wrong) "ok ... absent from FANOUT_NAMESPACES" line. The
# load-bearing assertion is that the guard did not claim parity at all.
assertions=$((assertions + 1))
if grep -qF -- 'follows the apps-base component gate' <<<"$GUARD_OUT"; then
  printf '  FAIL claimed parity over an entry it could not parse\n'
  failures=$((failures + 1))
else
  printf '  ok   refuses rather than claiming parity\n'
fi
assert_contains 'says it could not parse it' 'cannot parse'

# --- case 10: the merge-group guard is wired into the REQUIRED check --------
# Gating deploy-prod is necessary but NOT sufficient. When this guard fails,
# deploy-prod is SKIPPED, and aggregate-job-checks treats `skipped` as
# acceptable — so a required check that only sees deploy-prod stays GREEN and
# the merge group merges a revision the guard rejected. The sibling RGD gate is
# listed in ci-required-checks for exactly this reason; a weaker wiring here
# would make the guard block the deploy while letting the bad revision land.
printf 'case: the merge-group guard is wired into the required check\n'
ci_workflow="${repo_root}/.github/workflows/ci.yaml"
guard_job='validate-ghcr-fanout-merge-group'
# Fail CLOSED on a missing tool or unreadable workflow: an unchecked wiring
# assertion must never render as a pass.
assertions=$((assertions + 1))
if ! command -v yq >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
  printf '  FAIL cannot check wiring: yq and jq are both required\n'
  failures=$((failures + 1))
elif ! ci_json="$(yq -o=json -I=0 '.' "$ci_workflow" 2>/dev/null)" || [ -z "$ci_json" ]; then
  printf '  FAIL cannot check wiring: %s is unreadable as YAML\n' "$ci_workflow"
  failures=$((failures + 1))
else
  printf '  ok   premise: ci.yaml parsed and both tools present\n'

  assertions=$((assertions + 1))
  if jq -e --arg j "$guard_job" '
      (.jobs["deploy-prod"].needs | if type == "array" then . else [.] end)
      | index($j) != null' <<<"$ci_json" >/dev/null; then
    printf '  ok   deploy-prod depends on the guard\n'
  else
    printf '  FAIL deploy-prod does not depend on %s\n' "$guard_job"
    failures=$((failures + 1))
  fi

  assertions=$((assertions + 1))
  if jq -e --arg j "$guard_job" '
      (.jobs["ci-required-checks"].needs | if type == "array" then . else [.] end)
      | index($j) != null' <<<"$ci_json" >/dev/null; then
    printf '  ok   ci-required-checks depends on the guard\n'
  else
    printf '  FAIL ci-required-checks does not depend on %s — a guard failure\n' "$guard_job"
    printf '       skips deploy-prod, which the aggregate accepts, so the\n'
    printf '       required check stays green and the revision merges\n'
    failures=$((failures + 1))
  fi

  assertions=$((assertions + 1))
  if jq -e --arg j "$guard_job" '
      ([.jobs["ci-required-checks"].steps[]?.with["job-results"]? // empty] | join(" "))
      | contains("needs." + $j + ".result")' <<<"$ci_json" >/dev/null; then
    printf '  ok   the guard result is aggregated into the required check\n'
  else
    printf '  FAIL %s.result is absent from ci-required-checks job-results, so\n' "$guard_job"
    printf '       its failure is never evaluated even once the job is a dependency\n'
    failures=$((failures + 1))
  fi
fi

printf '\n%s assertion(s), %s failure(s)\n' "$assertions" "$failures"
[ "$failures" -eq 0 ] || exit 1
