#!/usr/bin/env bash
#
# Fail when a `checkov.io/skipN` annotation carries no readable reason.
#
# `.mega-linter.yml` states the rule this protects: "No suppression lands without
# its stated reason being checked against the manifest it describes." Two shapes
# defeat that rule without failing anything, because checkov reads only the check
# id, which sits before the damage:
#
#   1. TRUNCATION. In a YAML plain (unquoted) scalar, whitespace followed by `#`
#      opens a comment. So
#          checkov.io/skip2: CKV_K8S_40=deferred to #3202 -- <the whole reason>
#      parses as `CKV_K8S_40=deferred to`. The suppression still works and the
#      stated reason is simply gone. Quoting the scalar is the fix.
#   2. AN ABSENT REASON. `CKV_K8S_40=` (or a bare id with no `=`) suppresses just
#      as effectively while asserting nothing.
#
# Both leave a bare suppression that reads as reviewed. Nothing else in CI can see
# either one, which is why this guard exists.
#
# Usage: guard-checkov-skip-reasons.sh [root]        (default: k8s)
# Exit:  0 every annotation carries an intact reason
#        1 at least one does not (each is named)
#        2 the guard could not check — missing tool, unreadable root, parse failure
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

while IFS= read -r file; do
  [ -n "$file" ] || continue

  # --- Parsed pass: does the reason survive YAML parsing at all? --------------
  # Recursive descent, because these annotations sit on several different objects
  # (pod templates, job specs, bare metadata) and a fixed path would miss most.
  # -I=0 keeps one JSON document per line so multi-document files stay parseable.
  if ! parsed=$(yq -o=json -I=0 \
    '[.. | select(kind == "map") | to_entries[]
        | select(.key | test("^checkov\.io/skip[0-9]+$"))]' -- "$file" 2>&1); then
    die "could not parse $file as YAML: $parsed"
  fi

  while IFS= read -r entry; do
    [ -n "$entry" ] || continue
    key=$(printf '%s' "$entry" | jq -r '.key') || die "could not read a key from $file"
    value=$(printf '%s' "$entry" | jq -r '.value') || die "could not read a value from $file"
    checked=$((checked + 1))

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

  # --- Raw pass: did a reason exist in the source but parse away? -------------
  # This cannot be answered from the parsed value alone — truncation is lossless
  # from YAML's point of view, so the parse looks perfectly well-formed. It has to
  # be read off the source text.
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    lineno=${line%%:*}
    text=${line#*:}
    # The scalar is everything after the annotation key's colon.
    scalar=${text#*checkov.io/skip}
    scalar=${scalar#*:}
    # Strip leading blanks without a subshell.
    scalar=${scalar#"${scalar%%[![:space:]]*}"}

    # An empty scalar here means a block scalar or a value on a following line.
    # The parsed pass above already validated its reason; there is no plain-scalar
    # comment to detect, so there is nothing further to check.
    [ -n "$scalar" ] || continue

    # A quoted scalar cannot be truncated by a comment: inside quotes `#` is
    # literal. This is exactly the fix, so flagging it would punish the remedy.
    case $scalar in
      '"'* | "'"*) continue ;;
    esac

    # In a plain scalar, whitespace followed by `#` opens a comment and everything
    # after it is discarded. A `#` NOT preceded by whitespace (issue#3202) is a
    # literal character and must not be reported.
    if printf '%s' "$scalar" | grep -qE '[[:space:]]#'; then
      keyname=$(printf '%s' "$text" | grep -oE 'checkov\.io/skip[0-9]+' | head -1)
      report "$file" "${keyname:-checkov.io/skipN}" \
        'the reason is truncated by an unquoted YAML comment' \
        "line ${lineno}: everything from ' #' onward is discarded — wrap the value in double quotes"
    fi
  done < <(grep -nE 'checkov\.io/skip[0-9]+[[:space:]]*:' -- "$file" || true)
done <<EOF
$files
EOF

if [ "$findings" -ne 0 ]; then
  printf '\ncheckov skip reasons: %d annotation(s) carry no readable reason (%d checked)\n' \
    "$findings" "$checked" >&2
  exit 1
fi

printf 'checkov skip reasons: %d annotation(s) checked, all carry an intact reason\n' "$checked"
