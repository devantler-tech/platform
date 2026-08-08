#!/usr/bin/env bash
# Assert that every cosign subject matcher naming a SHARED devantler-tech/actions
# publish workflow pins a FIXED revision, and never a moving ref.
#
# WHY THIS EXISTS (#2818)
# #2816 tightened these matchers from `@.+` to `@([0-9a-f]{40}|refs/tags/v.+)`, which
# closed moving refs. Nothing keeps them closed: the property lives in eight separate
# regex strings, hand-written, under three different key spellings, and a single one
# widened back to `@.+` restores "trust any signer that can run this workflow from any
# ref" while every other check in CI stays green. A widened matcher is not a broken
# matcher — it verifies, it just verifies less — so no schema, no kubeconform pass and
# no deploy will notice.
#
# The ref is judged by an ALLOW-LIST — a 40-hex commit, or a tag under refs/tags/ —
# because enumerating forbidden shapes only catches the regressions someone thought
# of, and would wave through `@main` or `@v1`.
#
# 🔴 THE ROOT SOURCE IS DELIBERATELY EXCLUDED, AND THAT IS THE WHOLE DESIGN.
#
# The platform's own root OCIRepository is signed by devantler-tech/PLATFORM workflows
# running from a branch — `cd.yaml@refs/heads/main` and `ci.yaml` from the merge queue
# — because those workflows sign the artifact as they merge it. Its subject therefore
# contains `refs/heads/`, legitimately and permanently.
#
# So a guard that simply demanded "no branch refs in any cosign subject" would fail on
# the correct, deployed configuration. A control that fires at the known-good state is
# not a strict control; it is one that gets switched off the first time it blocks a
# release, taking its real coverage with it. This guard is scoped by SUBJECT to the
# shared `devantler-tech/actions` publish workflows, whose callers do pin by SHA, and
# says nothing about first-party platform workflows.
#
# WHAT IT DOES NOT CLAIM
# Pinning a fixed revision is not the same as pinning an APPROVED one: a superseded
# actions SHA still matches `[0-9a-f]{40}`. Restricting to approved revisions needs a
# generated allow-list and is tracked separately on #2818. This guard keeps the
# property #2816 established from silently regressing.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly REPO_ROOT

# The subjects are cosign identity regexes, so the `.` of `github.com` and `.github`
# are escaped in the file. Match both the escaped and bare spellings rather than
# assuming one, and accept all three key names in use (`subject` on Flux
# OCIRepositories, `subjectRegex` on the Talos image-verification config,
# `subjectRegExp` in Kyverno policies) — a fourth spelling appearing later should show
# up as a MISSING match against the floor below, not be silently skipped.
readonly SUBJECT_PATTERN='(subject|subjectRegex|subjectRegExp):[[:space:]]*.?\^?https://github\\?\.com/devantler-tech/actions/\\?\.github/workflows/publish-(app|manifests)\\?\.yaml@'

# A floor, because an empty result from a filtered read is a claim about the FILTER.
# If a refactor moves these subjects into a generator, a template, or a different key,
# the grep below returns nothing and — without this — the guard would exit 0 and
# report a clean repository while checking absolutely nothing. Failing closed on an
# unexpectedly small match set is what makes a passing run mean something. Raise this
# when a new consumer is genuinely added.
readonly EXPECTED_MIN_SUBJECTS=8

main() {
  cd "$REPO_ROOT"

  local matches
  matches="$(grep -rnE "$SUBJECT_PATTERN" --include='*.yaml' . || true)"

  local found=0
  if [ -n "$matches" ]; then
    found="$(printf '%s\n' "$matches" | wc -l | tr -d ' ')"
  fi

  if [ "$found" -lt "$EXPECTED_MIN_SUBJECTS" ]; then
    printf 'guard: found %d shared-publish-workflow subject(s), expected at least %d.\n' \
      "$found" "$EXPECTED_MIN_SUBJECTS" >&2
    printf 'The scan, not the repository, is the likely cause: these subjects may have moved,\n' >&2
    printf 'been renamed, or adopted a key spelling this guard does not match. Verify by hand,\n' >&2
    printf 'then either fix the pattern or lower EXPECTED_MIN_SUBJECTS with the reason.\n' >&2
    return 1
  fi

  # DISCOVER independently of formatting, then require discovery and validation to
  # agree. The floor above only proves that the eight KNOWN subjects are still
  # found; it says nothing about a NINTH consumer written differently. A valid
  # multiline form (`subject: >-` with the identity on the next line) or a fourth
  # key spelling is invisible to SUBJECT_PATTERN while the eight existing matches
  # still satisfy the floor — so a new consumer could pin nothing and the guard
  # would report a clean repository.
  #
  # This pattern keys on the shared workflow IDENTITY alone, with no key name and
  # no line structure, so it finds a reference however it is written. Anything it
  # finds that the strict pattern did not validate is reported rather than skipped.
  local discovered_lines unvalidated
  discovered_lines="$(grep -rlE 'devantler-tech/actions/\\?\.github/workflows/publish-(app|manifests)\\?\.yaml' \
    --include='*.yaml' . || true)"

  unvalidated=""
  local file discovered_count validated_count
  while IFS= read -r file; do
    [ -n "$file" ] || continue
    discovered_count="$(grep -cE 'devantler-tech/actions/\\?\.github/workflows/publish-(app|manifests)\\?\.yaml' "$file" || true)"
    validated_count="$(grep -cE "$SUBJECT_PATTERN" "$file" || true)"
    if [ "$discovered_count" -gt "$validated_count" ]; then
      unvalidated="$unvalidated  $file (references: $discovered_count, validated as subjects: $validated_count)
"
    fi
  done <<EOF
$discovered_lines
EOF

  if [ -n "$unvalidated" ]; then
    printf 'guard: found reference(s) to the shared publish workflows that this guard did not validate:\n' >&2
    printf '%s' "$unvalidated" >&2
    printf 'A consumer written in a form the subject pattern does not match is NOT checked, so it could\n' >&2
    printf 'pin nothing while this guard reports success. Either extend SUBJECT_PATTERN to cover the new\n' >&2
    printf 'form and raise EXPECTED_MIN_SUBJECTS, or confirm the reference is not a cosign subject.\n' >&2
    return 1
  fi

  local status=0
  local line location subject ref
  while IFS= read -r line; do
    location="${line%%:*}"
    line="${line#*:}"
    location="$location:${line%%:*}"
    subject="${line#*:}"

    # Everything after the LAST @ is the ref constraint; a trailing $ anchor and any
    # closing quote are not part of it.
    ref="${subject##*@}"
    ref="${ref%\'}"
    ref="${ref%\"}"
    ref="${ref%$}"

    # An ALLOW-LIST over each alternative, not a list of bad shapes to reject.
    #
    # Enumerating what is forbidden only ever catches the regressions someone thought
    # of: rejecting `refs/heads/` and a bare `.+` still waves through `@main`, `@v1`
    # or `@my-branch`, none of which pins a revision. Requiring each alternative to be
    # positively recognisable inverts that — an unfamiliar shape fails, and adding a
    # legitimately new one is a deliberate edit here rather than a silent widening.
    #
    # Exactly two forms qualify: a 40-hex commit, and a tag under refs/tags/. The
    # wildcard inside `refs/tags/v.+` widens the tag NAME, never the ref KIND, so it
    # stays valid — which is why this is judged per alternative rather than on the
    # whole string.
    # A grouped ref must be FULLY grouped. `(A|B)` is the shape this understands;
    # `(A)?B` is not, and stripping a leading paren from it would silently hand the
    # trailing `B` to the per-alternative check as part of A's text. Reject the
    # shape here so an unparsed construct can never reach the allow-list below.
    local alternatives alternative
    case "$ref" in
      '('*')')
        alternatives="${ref#\(}"
        alternatives="${alternatives%\)}"
        ;;
      '('* | *')')
        printf '%s: ref %s is not a fully grouped alternation; this guard cannot prove it pins a revision\n' \
          "$location" "$ref" >&2
        status=1
        continue
        ;;
      *) alternatives="$ref" ;;
    esac

    while IFS= read -r alternative; do
      # Match each alternative WHOLE, never by prefix.
      #
      # `refs/tags/*` as a shell glob accepts anything that merely STARTS with the
      # tag prefix, so `(refs/tags/v.+)?refs/heads/.+$` — where the tag group is
      # optional and a branch ref follows it — was accepted and the guard reported
      # all eight subjects pinned. Reproduced against the real repository before
      # this fix: exit 0 with a subject that permits `refs/heads/`.
      #
      # The tag form therefore allows only characters that widen the tag NAME. `/`
      # is excluded because it is what lets a second ref kind be appended, and the
      # grouping and alternation metacharacters are excluded because a matcher that
      # needs them is not a plain tag pattern and must be reviewed here deliberately.
      if printf '%s' "$alternative" | grep -qxE '\[0-9a-f\]\{40\}'; then
        continue
      fi
      # NOTE the bracket expression's ordering: a backslash is LITERAL inside a
      # POSIX bracket, so `\]` does not escape anything — it ends the class early.
      # `]` must therefore come first and `-` last, which is what makes `[` and `]`
      # members rather than delimiters.
      if printf '%s' "$alternative" | grep -qxE 'refs/tags/[]A-Za-z0-9._*+{}[-]+'; then
        continue
      fi
      printf '%s: ref alternative %s does not pin a fixed revision (subject: %s)\n' \
        "$location" "$alternative" "$ref" >&2
      status=1
    done < <(printf '%s\n' "$alternatives" | tr '|' '\n')
  done <<EOF
$matches
EOF

  if [ "$status" -eq 0 ]; then
    printf 'guard: %d shared-publish-workflow subject(s) all pin a fixed revision.\n' "$found"
  fi

  return "$status"
}

main "$@"
