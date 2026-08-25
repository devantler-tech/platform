#!/usr/bin/env bash
#
# Pins the GitRepository commit-pin guard's verdict in all THREE directions.
#
#   exit 0  every GitRepository document carries a 40-hex spec.ref.commit
#   exit 1  at least one tracks a mutable ref, or a malformed commit
#   exit 2  the guard could not check — bad usage, missing root, unparseable
#           YAML, or NO GitRepository document found at all
#
# Two of these are anti-vacuity cases rather than behaviours. A tree with no
# GitRepository must come back exit 2, NOT a clean exit 0: a selector that
# matched nothing is indistinguishable from a healthy tree, and reporting it as
# clean is the same class of failure the guard exists to prevent. And a
# Kustomization referencing `kind: GitRepository` under `spec.sourceRef` must be
# IGNORED — it carries no ref of its own, so counting it would be a false
# refusal on a correct file.
#
# Every fixture carries its OWN namespace and name, so an assertion can only be
# satisfied by the case it belongs to.

set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
guard="$repo_root/scripts/guard-gitrepository-commit-pin.sh"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

failures=0
assertions=0

run_guard() { # <root>
  # Capture the status with `if` rather than toggling `set -e`: this file runs
  # under `set -uo pipefail` with NO errexit, so `set -e` here would enable a
  # mode the script never had.
  if GUARD_OUT="$("$guard" "$1" 2>&1)"; then
    GUARD_RC=0
  else
    GUARD_RC=$?
  fi
}

assert_rc() { # <label> <expected-rc> <actual-rc>
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
  # A here-string, NOT a pipe: `grep -q` exits on the first match, and the SIGPIPE
  # that gives the upstream `printf` becomes the pipeline status under `pipefail`.
  # A needle matching EARLY would then read as a failed assertion while a needle
  # near the end passed — the harness silently inverting its own verdict.
  if grep -qF -- "$2" <<<"$GUARD_OUT"; then
    printf '  ok   %s\n' "$1"
  else
    printf '  FAIL %s: output did not contain %s\n' "$1" "$2"
    printf '%s\n' "$GUARD_OUT" | sed 's/^/       | /'
    failures=$((failures + 1))
  fi
}

mkcase() { # <name> -> echoes the root dir
  d="$scratch/$1"
  mkdir -p "$d"
  printf '%s' "$d"
}

echo "== case: pinned commit is accepted =="
root="$(mkcase pinned)"
cat >"$root/gr.yaml" <<'YAML'
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: case-pinned
  namespace: ns-pinned
spec:
  url: https://github.com/devantler-tech/unifi
  ref:
    commit: 7b17f7e33ef507c24c395b884a433c62b92ace98
YAML
run_guard "$root"
assert_rc "pinned commit -> 0" 0 "$GUARD_RC"
assert_contains "reports the count it checked" "1 GitRepository document(s)"

echo "== case: mutable branch ref is refused =="
root="$(mkcase branchref)"
cat >"$root/gr.yaml" <<'YAML'
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: case-branch
  namespace: ns-branch
spec:
  url: https://github.com/devantler-tech/unifi
  ref:
    branch: main
YAML
run_guard "$root"
assert_rc "branch ref -> 1" 1 "$GUARD_RC"
assert_contains "names the offending resource" "ns-branch/case-branch"
assert_contains "states the fix" "spec:"

echo "== case: a short/malformed commit is refused =="
root="$(mkcase shortsha)"
cat >"$root/gr.yaml" <<'YAML'
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: case-short
  namespace: ns-short
spec:
  url: https://github.com/devantler-tech/unifi
  ref:
    commit: 7b17f7e3
YAML
run_guard "$root"
assert_rc "abbreviated sha -> 1" 1 "$GUARD_RC"
assert_contains "names the offending resource" "ns-short/case-short"

echo "== anti-false-positive: a Kustomization sourceRef is NOT a GitRepository =="
root="$(mkcase sourceref)"
cat >"$root/ks.yaml" <<'YAML'
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: case-consumer
  namespace: ns-sourceref
spec:
  sourceRef:
    kind: GitRepository
    name: case-source
YAML
cat >"$root/gr.yaml" <<'YAML'
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: case-source
  namespace: ns-sourceref
spec:
  url: https://github.com/devantler-tech/unifi
  ref:
    commit: 7b17f7e33ef507c24c395b884a433c62b92ace98
YAML
run_guard "$root"
assert_rc "sourceRef ignored, pinned source accepted -> 0" 0 "$GUARD_RC"
assert_contains "counted exactly one document, not two" "1 GitRepository document(s)"

echo "== regression: a non-canonically-spaced kind must NOT escape the prefilter =="
# The prefilter used to match the literal "kind: GitRepository", which an extra
# space defeats. Alone that produced a safe exit 2, but in a MIXED tree the
# anti-vacuity check was satisfied by the well-formed resource and the guard
# returned exit 0 -- reporting "all commit-pinned" with an unpinned GitRepository
# right beside it. Both documents must be counted, and the odd one refused.
root="$(mkcase mixedspacing)"
cat >"$root/canonical.yaml" <<'YAML'
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: case-canonical
  namespace: ns-mixed
spec:
  ref:
    commit: 7b17f7e33ef507c24c395b884a433c62b92ace98
YAML
cat >"$root/odd.yaml" <<'YAML'
apiVersion: source.toolkit.fluxcd.io/v1
kind:   GitRepository
metadata:
  name: case-spaced
  namespace: ns-mixed
spec:
  ref:
    branch: main
YAML
run_guard "$root"
assert_rc "mixed spacing, one unpinned -> 1" 1 "$GUARD_RC"
assert_contains "names the oddly-spaced resource" "ns-mixed/case-spaced"
assert_contains "counted BOTH documents, not just the canonical one" "1 of 2 GitRepository document(s)"

echo "== anti-vacuity: no GitRepository anywhere is CANNOT-CHECK, not clean =="
root="$(mkcase empty)"
cat >"$root/other.yaml" <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: case-unrelated
  namespace: ns-empty
YAML
run_guard "$root"
assert_rc "no GitRepository -> 2" 2 "$GUARD_RC"
assert_contains "says the selector matched nothing" "matched nothing"

echo "== cannot-check: unparseable YAML is never reported as clean =="
root="$(mkcase broken)"
cat >"$root/gr.yaml" <<'YAML'
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: case-broken
  namespace: ns-broken
spec:
  ref:
    commit: "unterminated
   bad: [indent
YAML
run_guard "$root"
assert_rc "unparseable YAML -> 2" 2 "$GUARD_RC"

echo "== regression: a tree grep could not fully read is CANNOT-CHECK, not clean =="
# `grep -r` exits 2 on an unreadable path while still reporting the parts it DID
# read, and the guard discards its stderr. In a MIXED tree the anti-vacuity check
# is then satisfied by the file grep COULD read, so the pre-fix guard printed
# "all commit-pinned", exit 0, with an unpinned GitRepository in the directory it
# silently skipped. This case pins that shut.
root="$(mkcase unreadable)"
mkdir -p "$root/readable" "$root/hidden"
cat >"$root/readable/good.yaml" <<'YAML'
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: case-unreadable-good
  namespace: ns-unreadable
spec:
  ref:
    commit: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
YAML
cat >"$root/hidden/bad.yaml" <<'YAML'
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: case-unreadable-bad
  namespace: ns-unreadable
spec:
  ref:
    branch: main
YAML
chmod 000 "$root/hidden"
# Running as root defeats the mode bits entirely, so the fixture would not model
# the condition. Skipping loudly beats asserting against a fixture that did not
# build — a control that cannot fire proves nothing.
if cat "$root/hidden/bad.yaml" >/dev/null 2>&1; then
  printf '  SKIP unreadable-path case: this user can read mode-000 dirs (root?)\n'
else
  run_guard "$root"
  assert_rc "unreadable subtree -> 2" 2 "$GUARD_RC"
  assert_contains "says it could not search the tree" "refusing to report a tree it could not fully read"
fi
chmod 755 "$root/hidden"

echo "== regression: an ESCAPED kind must NOT escape selection =="
# `kind: "Git\u0052epository"` is legal YAML that parses as GitRepository, and NO raw-text
# search can see it — the file does not contain the string "GitRepository" at all. A merely
# QUOTED kind would not model this: `kind: "GitRepository"` still contains the substring, so a
# grep prefilter matches it and the fixture would pass with the bug present. With a text prefilter this document was never opened, so in a mixed tree
# the anti-vacuity check was satisfied by the pinned document beside it and the guard printed
# "all commit-pinned", exit 0, with an unpinned escaped GitRepository present. This is why
# selection is now the parser's job and there is no text prefilter at all.
root="$(mkcase escaped)"
cat >"$root/pinned.yaml" <<'YAML'
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: case-escaped-pinned
  namespace: ns-escaped
spec:
  ref:
    commit: cccccccccccccccccccccccccccccccccccccccc
YAML
cat >"$root/escaped.yaml" <<'YAML'
apiVersion: source.toolkit.fluxcd.io/v1
kind: "Git\u0052epository"
metadata:
  name: case-escaped-unpinned
  namespace: ns-escaped
spec:
  ref:
    branch: main
YAML
run_guard "$root"
assert_rc "escaped kind in a mixed tree -> 1" 1 "$GUARD_RC"
assert_contains "names the escaped resource" "case-escaped-unpinned"
assert_contains "counted BOTH documents, not just the literal one" "1 of 2"

echo "== regression: a SEQUENCE document is not an error =="
# Now that every YAML file is parsed rather than only text-matching ones, sequence documents
# are routine: a Kustomize JSON-patch file is a top-level ARRAY, and `.kind` against an array
# is a yq ERROR, not an empty result. Without the `tag == "!!map"` stage the guard exits 2 on
# this repository's own manifests. A genuinely unparseable file must STILL be cannot-check,
# which the case above this one pins.
root="$(mkcase sequence)"
cat >"$root/patch.yaml" <<'YAML'
- op: add
  path: /spec/listeners/-
  value:
    name: example
    kind: GitRepository
YAML
cat >"$root/real.yaml" <<'YAML'
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: case-sequence
  namespace: ns-sequence
spec:
  ref:
    commit: dddddddddddddddddddddddddddddddddddddddd
YAML
run_guard "$root"
assert_rc "a JSON-patch array alongside a pinned source -> 0" 0 "$GUARD_RC"
assert_contains "counted only the real document" "1 GitRepository document(s)"
echo "== cannot-check: usage and missing root =="
run_guard ""
assert_rc "empty root -> 2" 2 "$GUARD_RC"
if GUARD_OUT="$("$guard" 2>&1)"; then GUARD_RC=0; else GUARD_RC=$?; fi
assert_rc "no argument -> 2" 2 "$GUARD_RC"
if GUARD_OUT="$("$guard" "$scratch/does-not-exist" 2>&1)"; then GUARD_RC=0; else GUARD_RC=$?; fi
assert_rc "nonexistent root -> 2" 2 "$GUARD_RC"

echo
if [ "$failures" -gt 0 ]; then
  printf 'test-guard-gitrepository-commit-pin: %d of %d assertion(s) FAILED\n' "$failures" "$assertions" >&2
  exit 1
fi
printf 'test-guard-gitrepository-commit-pin: all %d assertion(s) passed\n' "$assertions"
