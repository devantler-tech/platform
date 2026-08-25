#!/usr/bin/env bash
#
# Fail when a Flux `GitRepository` tracks a MUTABLE ref instead of a pinned commit.
#
# THE RULE THIS ENFORCES: every GitRepository document under the given root must
# carry `spec.ref.commit` as a full 40-hex SHA.
#
# Why a pin rather than a signature. A GitRepository that names `branch: main`
# reconciles whatever the branch tip is, so the trust boundary is entirely the
# source repository's own enforcement. #2707 closed that on the `unifi` source by
# pinning the full commit, and stated the reason in the manifest: a
# source-repository compromise then cannot change resources already authorized
# for patch/update without a separately reviewed Platform change. A 40-hex commit
# fixes the CONTENT cryptographically — Flux fetches that object or fails — which
# is strictly stronger than verifying who signed a tip that can still move.
#
# That mitigation lived in one YAML file with nothing asserting it. Reverting
# `ref.commit` to `ref.branch` — deliberately, for auto-updates, or as collateral
# in an unrelated refactor — would silently restore the original exposure and no
# check in CI would notice (#3124). This guard removes that possibility rather
# than documenting it.
#
# 🔴 SELECT ON THE DOCUMENT'S OWN `.kind`, NEVER ON THE TEXT.
#
# `kind: GitRepository` also appears inside a Kustomization's `spec.sourceRef`,
# which is a REFERENCE to a source and carries no `ref` of its own. A text match
# therefore reports a Kustomization as an unpinned GitRepository — a false
# refusal on a file that is correct. Only a top-level `.kind` decides.
#
# ⚠️ ANTI-VACUITY: finding NO GitRepository at all is exit 2, not exit 0.
# A selector that matched nothing is indistinguishable from a healthy tree, which
# is the failure mode this guard exists to prevent one level down. "Nothing to
# check" must never render as "checked and clean".
#
# Exit codes:
#   0  every GitRepository found is pinned to a 40-hex commit
#   1  at least one is not — the defect
#   2  cannot check: bad usage, missing root, unparseable YAML, or no
#      GitRepository document found anywhere under the root

set -uo pipefail

die() {
  printf 'guard-gitrepository-commit-pin: %s\n' "$*" >&2
  exit 2
}

[ "$#" -eq 1 ] || die "usage: $0 <root-directory>"
root="$1"
[ -d "$root" ] || die "root '$root' is not a directory"
command -v yq >/dev/null 2>&1 || die "yq is required but not installed"
# 🔴 NO TEXT PREFILTER AT ALL. THE PARSER DECIDES WHICH FILES HOLD A GitRepository.
#
# Every text prefilter tried here has been a fail-open, because the question "does this file
# contain a GitRepository?" is a question about PARSED YAML and a text match only approximates
# it. Three approximations failed in turn:
#
#   `kind: GitRepository`   missed `kind:   GitRepository` and quoted scalars.
#   `GitRepository`         misses `kind: "GitRepository"` -- a legal escape that yq
#                           parses as GitRepository and no raw-text search can see.
#   any grep at all         reported a truncated list as a complete one when it hit an
#                           unreadable path, since it still exits 0/1 for what it did read.
#
# Each one is the same failure: in a MIXED tree the anti-vacuity check below is satisfied by
# some OTHER well-formed resource, so the guard prints "all commit-pinned", exit 0, with an
# unpinned GitRepository sitting beside it. Widening the pattern once more would only move the
# boundary; enumerating every YAML file and letting `yq` apply `.kind` removes it. That costs
# one `yq` invocation per file -- measured at ~6s over this repository's 725 manifests, which
# is noise next to the job it runs in, and correctness here is not worth trading for it.
#
# `find` exits non-zero when it cannot traverse something, which is the same
# "could not read the tree" case handled below, so that check is kept and now keys on find.
candidates="$(find "$root" \( -name '*.yaml' -o -name '*.yml' \) -type f -print 2>/dev/null)"
find_status=$?
[ "$find_status" -eq 0 ] ||
  die "could not enumerate '$root' (find exit $find_status) — refusing to report a tree it could not fully read"

checked=0
violations=0

# A here-string, not a pipe: a piped `while` runs in a subshell, so the counters
# it increments would be discarded and the guard would report 0 violations.
while IFS= read -r file; do
  [ -n "$file" ] || continue

  # `select(tag == "!!map")` FIRST, as its own stage. Now that every YAML file is parsed
  # rather than only those matching a text pattern, sequence documents are routine here: a
  # Kustomize JSON-patch file is a top-level ARRAY, and `.kind` against an array is a yq
  # ERROR ("cannot index array with 'kind'"), not an empty result. Without this the guard
  # exits 2 on this repository's own manifests -- 1 of its 725 files is exactly that shape.
  # Two stages rather than one `and`, so the second never sees a non-mapping.
  # A genuinely unparseable file still fails, and is still cannot-check.
  if ! rows="$(yq eval-all \
    'select(tag == "!!map") | select(.kind == "GitRepository") | [(.metadata.namespace // "-"), (.metadata.name // "-"), (.spec.ref.commit // "")] | @tsv' \
    "$file" 2>/dev/null)"; then
    die "could not parse '$file' — refusing to report a tree it could not read"
  fi

  while IFS=$'\t' read -r ns name commit; do
    [ -n "${name:-}" ] || continue
    checked=$((checked + 1))
    if printf '%s' "$commit" | grep -Eq '^[0-9a-f]{40}$'; then
      continue
    fi
    violations=$((violations + 1))
    printf 'VIOLATION %s: GitRepository %s/%s is not pinned to a commit (spec.ref.commit=%s)\n' \
      "$file" "$ns" "$name" "${commit:-<absent>}" >&2
  done <<<"$rows"
done <<<"$candidates"

if [ "$checked" -eq 0 ]; then
  die "no GitRepository document found under '$root' — selector matched nothing, so nothing was verified"
fi

if [ "$violations" -gt 0 ]; then
  cat >&2 <<'FIX'

Every GitRepository must pin a full 40-hex commit:

  spec:
    ref:
      commit: <40-hex sha>

Replace the mutable ref with the commit you intend to deploy. Moving the pin is a
reviewed Platform change, which is the control (#3124, #2707). If a mutable ref is
genuinely required, this guard is the place to record that decision.
FIX
  printf 'guard-gitrepository-commit-pin: %d of %d GitRepository document(s) not commit-pinned\n' \
    "$violations" "$checked" >&2
  exit 1
fi

printf 'guard-gitrepository-commit-pin: OK — %d GitRepository document(s), all commit-pinned\n' "$checked"
exit 0
