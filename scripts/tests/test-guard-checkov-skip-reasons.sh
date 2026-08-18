#!/usr/bin/env bash
#
# Pins the checkov skip-reason guard's verdict in BOTH directions.
#
# The guard fails by PASSING: a `checkov.io/skipN` value whose reason parsed away
# still suppresses its finding, because the check id sits before the `#`. Nothing
# else in CI notices — the suppression simply reads as reviewed. So the cases that
# matter most here are the REJECTIONS, and every one of them is asserted to fire on
# a fixture that differs from a passing fixture in exactly one way.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
guard="$repo_root/scripts/guard-checkov-skip-reasons.sh"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

failures=0

# Run the guard against a fixture tree and report its exit status without letting
# `set -e` abort the test on the (expected) non-zero ones.
run_guard() { # <tree>
  set +e
  GUARD_OUT="$("$guard" "$1" 2>&1)"
  GUARD_RC=$?
  set -e
}

# Build a one-file fixture tree whose single annotation value is supplied verbatim,
# so each case differs from its neighbour in exactly one scalar.
fixture() { # <name> <raw-scalar-text>
  local dir="$scratch/$1"
  mkdir -p "$dir"
  cat >"$dir/resource.yaml" <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: sample
  annotations:
    checkov.io/skip1: $2
spec:
  replicas: 1
YAML
  printf '%s' "$dir"
}

expect() { # <case> <expected-rc> <tree> [<substring the output must name>]
  local case_name=$1 want=$2 tree=$3 needle=${4-}
  run_guard "$tree"
  if [ "$GUARD_RC" -ne "$want" ]; then
    printf 'FAIL %s: expected exit %s, got %s\n%s\n' \
      "$case_name" "$want" "$GUARD_RC" "$GUARD_OUT" >&2
    failures=$((failures + 1))
    return
  fi
  if [ -n "$needle" ] && ! printf '%s' "$GUARD_OUT" | grep -qF -- "$needle"; then
    printf 'FAIL %s: exit %s was right, but the report never named %s\n%s\n' \
      "$case_name" "$want" "$needle" "$GUARD_OUT" >&2
    failures=$((failures + 1))
    return
  fi
  printf 'ok   %s\n' "$case_name"
}

# ---------------------------------------------------------------- REJECTIONS ---

# The #3204 case: an unquoted value whose ` #` opens a YAML comment. checkov still
# suppresses (the id precedes the `#`), so only this guard can see it.
expect 'truncated: unquoted value containing " #"' 1 \
  "$(fixture truncated 'CKV_K8S_40=deferred to #3202 -- the whole stated reason')" \
  'resource.yaml'

# The second invisible shape named in #3204: a value with nothing after `=`.
expect 'empty reason: nothing after "="' 1 \
  "$(fixture empty 'CKV_K8S_40=')" \
  'resource.yaml'

# Whitespace is not a reason.
expect 'empty reason: only whitespace after "="' 1 \
  "$(fixture blank '"CKV_K8S_40=   "')" \
  'resource.yaml'

# A bare id with no `=` at all carries no reason either, and the `.mega-linter.yml`
# rule is about the reason being present to check.
expect 'malformed: no "=" separator' 1 \
  "$(fixture noeq 'CKV_K8S_40')" \
  'resource.yaml'

# ----------------------------------------------------------- NEGATIVE CONTROLS ---
# Each of these is one character away from a rejection above. Without them the
# guard could pass every test by simply rejecting anything containing a "#".

# The correct fix for the truncation case — quoting — must NOT be flagged.
expect 'accepted: quoted value containing " #"' 0 \
  "$(fixture quoted '"CKV_K8S_40=deferred to #3202 -- the whole stated reason"')"

# In a YAML plain scalar a `#` only opens a comment when preceded by whitespace, so
# this one is NOT truncated and must not be reported.
expect 'accepted: unquoted "#" with no preceding space' 0 \
  "$(fixture hashnospace 'CKV_K8S_40=see issue#3202 for the deferral rationale')"

# The ordinary shape, which most annotations in the tree use.
expect 'accepted: plain value with no "#"' 0 \
  "$(fixture plain 'CKV_K8S_38=the container reads its SA token through the API server')"

# A tree with no annotations at all is vacuously clean, not an error.
expect 'accepted: tree with no skip annotations' 0 "$(mkdir -p "$scratch/bare" && printf '%s' "$scratch/bare")"

# ------------------------------------------------------------- THE REAL TREE ---
# #3204's acceptance criterion: the guard passes on the current tree. This is what
# makes the rejections above a regression gate rather than a lint nobody can adopt.
expect 'accepted: the repository k8s/ tree as it stands' 0 "$repo_root/k8s"

# ------------------------------------------------------ FAIL-CLOSED ON TOOLING ---
# A guard that exits 0 when its parser is missing protects nothing: CI would go
# green having checked no annotation at all. It must be distinguishable from a
# clean run, and from a finding.
missing_yq_dir="$scratch/no-yq-bin"
mkdir -p "$missing_yq_dir"
set +e
no_yq_out="$(PATH="$missing_yq_dir" /bin/bash "$guard" "$(fixture plainagain 'CKV_K8S_38=fine')" 2>&1)"
no_yq_rc=$?
set -e
if [ "$no_yq_rc" -ne 2 ]; then
  printf 'FAIL fail-closed: expected exit 2 without yq, got %s\n%s\n' "$no_yq_rc" "$no_yq_out" >&2
  failures=$((failures + 1))
else
  printf 'ok   fail-closed: missing yq is exit 2, not a silent pass\n'
fi

# A directory that does not exist is an infrastructure error, never "nothing to check".
set +e
"$guard" "$scratch/does-not-exist" >/dev/null 2>&1
absent_rc=$?
set -e
if [ "$absent_rc" -ne 2 ]; then
  printf 'FAIL fail-closed: expected exit 2 for a missing root, got %s\n' "$absent_rc" >&2
  failures=$((failures + 1))
else
  printf 'ok   fail-closed: a missing root is exit 2\n'
fi

# ------------------------------------------------------------------ ABLATION ---
# Proves the truncation check is what rejects the truncated fixture, rather than
# some unrelated property of it. Quoting the SAME scalar must flip 1 to 0; if both
# arms agreed, every rejection above could be vacuous.
run_guard "$(fixture ablate_bad 'CKV_K8S_40=deferred to #3202 -- reason text')"
bad_rc=$GUARD_RC
run_guard "$(fixture ablate_ok '"CKV_K8S_40=deferred to #3202 -- reason text"')"
ok_rc=$GUARD_RC
if [ "$bad_rc" -eq 1 ] && [ "$ok_rc" -eq 0 ]; then
  printf 'ok   ablation: quoting the identical scalar flips the verdict (1 -> 0)\n'
else
  printf 'FAIL ablation: quoting did not flip the verdict (unquoted=%s quoted=%s)\n' \
    "$bad_rc" "$ok_rc" >&2
  failures=$((failures + 1))
fi

if [ "$failures" -ne 0 ]; then
  printf '\ncheckov skip-reason guard: %d case(s) failed\n' "$failures" >&2
  exit 1
fi
printf '\ncheckov skip-reason guard contract OK\n'
