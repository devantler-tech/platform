#!/usr/bin/env bash
#
# Fail when a `checkov.io/skipN` annotation carries no readable reason.
#
# `.mega-linter.yml` states the rule this protects: "No suppression lands without
# its stated reason being checked against the manifest it describes." checkov reads
# only the check id, which sits before the reason, so a reason can be lost while the
# suppression keeps working — leaving a bare exception that still reads as reviewed.
#
# Two shapes lose it:
#
#   1. TRUNCATION. In a YAML plain (unquoted) scalar, whitespace followed by `#`
#      opens a comment. So
#          checkov.io/skip2: CKV_K8S_40=deferred to #3202 -- <the whole reason>
#      parses as `CKV_K8S_40=deferred to`, losslessly as far as YAML is concerned.
#   2. AN ABSENT REASON. `CKV_K8S_40=` (or a bare id with no `=`) suppresses just as
#      effectively while asserting nothing.
#
# THE RULE THIS ENFORCES: every `checkov.io/skipN` VALUE MUST BE AN EXPLICITLY
# QUOTED SCALAR.
#
# That is a deliberate inversion. Detecting truncation after the fact is not merely
# hard, it is impossible from the source text: `reason  # trailing note` (a complete
# reason plus an ordinary comment) and `reason  # rest of the reason` (a truncated
# one) are LEXICALLY IDENTICAL, and only the author's intent separates them. Any
# guard that pattern-matches source text must therefore mis-handle one of them.
#
# Requiring the quote removes the failure instead of detecting it: inside quotes `#`
# is a literal character, so a quoted reason CANNOT be comment-truncated. That makes
# this check purely structural — it asks yq for each value node's STYLE and never
# lexes YAML itself.
#
# Usage: guard-checkov-skip-reasons.sh [root]        (default: k8s)
# Exit:  0 every annotation is quoted and carries an intact reason
#        1 at least one does not (each is named, with the edit that fixes it)
#        2 the guard could not check — missing tool, unreadable root, parse failure,
#          or an annotation the parser cannot see (see the reconciliation below)
#
# Exit 2 is deliberately distinct from both. A guard that cannot run must never be
# indistinguishable from a clean tree: that is the failure mode where CI goes green
# having checked nothing.

set -uo pipefail

root=${1:-k8s}

die() {
  printf 'guard-checkov-skip-reasons: %s\n' "$1" >&2
  exit 2
}

command -v yq >/dev/null 2>&1 || die 'yq is required but not on PATH'
command -v jq >/dev/null 2>&1 || die 'jq is required but not on PATH'
[ -d "$root" ] || die "not a directory: $root"

# Only files that actually carry the annotation are parsed. The vendored operator
# bundles are thousands of lines each, and every other YAML file in the tree would
# be walked for nothing.
files=$(grep -rlE 'checkov\.io/skip[0-9]+' --include='*.yaml' --include='*.yml' -- "$root" 2>/dev/null)
grep_rc=$?
# grep exits 1 for "no matches", which is a legitimately clean tree, and >1 for a
# real error. Only the latter is an infrastructure failure.
if [ "$grep_rc" -gt 1 ]; then
  die "could not search $root for annotations (grep exit $grep_rc)"
fi
[ -n "$files" ] || {
  printf 'checkov skip reasons: no annotations under %s\n' "$root"
  exit 0
}

findings=0
checked=0

report() { # <file> <key> <problem> <detail>
  printf '  %s\n    %s: %s\n      %s\n' "$1" "$2" "$3" "$4" >&2
  findings=$((findings + 1))
}

# A key-shaped occurrence: the annotation name in KEY position, i.e. followed by an
# optional closing quote and an optional-blank colon. The optional quote matters —
# `"checkov.io/skip1": value` is a legitimate shape, and omitting it here would make
# the reconciliation below reject that file as uncheckable instead of judging it.
#
# This deliberately does NOT match prose that merely mentions the annotation ("see
# the checkov.io/skip2 rationale"), which is why the tree's own cross-references do
# not inflate the count.
sq="'"
key_shape="(^|[^A-Za-z0-9_./-])checkov\.io/skip[0-9]+[\"$sq]?[[:space:]]*:"

while IFS= read -r file; do
  [ -n "$file" ] || continue

  # Recursive descent, because these annotations sit on several different objects
  # (pod templates, job specs, bare metadata) and a fixed path would miss most.
  # -I=0 keeps one JSON document per line so multi-document files stay parseable.
  #
  # `.key | type == "!!str"` is load-bearing: a YAML merge key (`<<: *anchor`) has
  # tag !!merge, and calling test() on it aborts the whole run with a parse error —
  # so a single merge key anywhere in an annotated file would take the guard down
  # before it checked anything.
  if ! parsed=$(yq -o=json -I=0 \
    '[.. | select(kind == "map") | to_entries[]
        | select((.key | type == "!!str")
             and (.key | test("^checkov\.io/skip[0-9]+$")))
        | {"key": .key, "value": .value, "style": (.value | style)}]' -- "$file" 2>&1); then
    die "could not parse $file as YAML: $parsed"
  fi

  file_checked=0
  while IFS= read -r entry; do
    [ -n "$entry" ] || continue
    key=$(printf '%s' "$entry" | jq -r '.key') || die "could not read a key from $file"
    value=$(printf '%s' "$entry" | jq -r '.value') || die "could not read a value from $file"
    style=$(printf '%s' "$entry" | jq -r '.style') || die "could not read a style from $file"
    checked=$((checked + 1))
    file_checked=$((file_checked + 1))

    # --- The structural rule -------------------------------------------------
    # yq reports the VALUE node's style, so this is correct even when the KEY is
    # quoted and the value is not — a shape that defeats every source-text match.
    case $style in
      double | single) ;;
      *)
        report "$file" "$key" 'the value is an unquoted (plain) scalar' \
          "a ' #' anywhere in it would silently truncate the reason — wrap the value in double quotes"
        ;;
    esac

    # --- The reason is present at all ----------------------------------------
    case $value in
      *=*)
        reason=${value#*=}
        # A reason of only whitespace asserts nothing, so it is treated as absent
        # rather than present-but-short.
        if [ -z "$(printf '%s' "$reason" | tr -d '[:space:]')" ]; then
          report "$file" "$key" 'the reason is empty' \
            "parsed value was '${value}' — everything after '=' is blank"
        fi
        ;;
      *)
        report "$file" "$key" 'no "=" separator, so there is no reason at all' \
          "parsed value was '${value}'"
        ;;
    esac
  done < <(printf '%s' "$parsed" | jq -c '.[]')

  # --- Reconciliation: did the parser SEE every annotation in this file? ------
  # An annotation nested inside a block scalar (a kustomize `patch: |`) is opaque
  # string content, so yq reports nothing for it and the loop above would report a
  # contented "0 checked" while a real suppression went unread. Comparing the
  # key-shaped source count against what was actually checked is what turns that
  # silent gap into an explicit "cannot check".
  #
  # Full-line comments are dropped first: a `#`-leading line is never an annotation
  # key, in a comment or inside a block scalar, and this tree does carry prose
  # cross-references shaped like one.
  # `grep -o | wc -l` counts each KEY, not each matching line: a flow map can carry
  # several annotations on one line, and counting lines there would under-count and
  # reject valid YAML as uncheckable.
  raw_keys=$(grep -vE '^[[:space:]]*#' -- "$file" | grep -oE "$key_shape" | wc -l | tr -d ' ')
  raw_rc=$?
  if [ "$raw_rc" -gt 1 ]; then
    die "could not count annotations in $file (grep exit $raw_rc)"
  fi
  if [ "$raw_keys" -ne "$file_checked" ]; then
    die "$file: $raw_keys annotation(s) in the source but $file_checked reached the parser — one is not a plain map entry (a block scalar such as a kustomize 'patch: |' hides it), so this file cannot be checked"
  fi
done <<EOF
$files
EOF

if [ "$findings" -ne 0 ]; then
  printf '\ncheckov skip reasons: %d annotation(s) carry no readable reason (%d checked)\n' \
    "$findings" "$checked" >&2
  exit 1
fi

printf 'checkov skip reasons: %d annotation(s) checked, all quoted and carrying an intact reason\n' "$checked"
