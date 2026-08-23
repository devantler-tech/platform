#!/usr/bin/env bash
# Assert that every cosign subject matcher naming a SHARED devantler-tech/actions
# publish workflow pins a FIXED revision, and never a moving ref.
#
# WHY THIS EXISTS (#2818)
# The accepted ref shape is a 40-hex commit and nothing else. The property lives in
# eight separate regex strings, hand-written, under three different key spellings, and
# a single one widened back to `@.+` restores "trust any signer that can run this
# workflow from any ref" while every other check in CI stays green. A widened matcher
# is not a broken matcher — it verifies, it just verifies less — so no schema, no
# kubeconform pass and no deploy will notice.
#
# The ref is judged by an ALLOW-LIST — a 40-hex commit — because enumerating forbidden
# shapes only catches the regressions someone thought of, and would wave through
# `@main` or `@v1`.
#
# THE TAG ALTERNATIVE IS GONE, AND THIS GUARD IS WHAT KEEPS IT GONE (#3022).
# A `refs/tags/v.+` alternative used to sit beside the commit form. It was removed once
# it was confirmed unexercised: every artifact these subjects verify is signed by a
# caller that pins the shared workflow by commit — measured across the complete history
# of all six consumers and against the signing certificates of every readable artifact,
# with zero tag-form signatures. Re-adding it would widen the trusted signer set for no
# artifact that exists, so the allow-list below rejects it rather than merely
# tolerating its absence.
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
#
# Note for whoever implements that allow-list: the check below matches the literal
# PATTERN text `[0-9a-f]{40}`, because these subjects are regexes. A subject naming one
# concrete commit — which is what a generated approved-revision list would emit — is
# therefore REJECTED here today, despite being strictly narrower than what is accepted.
# That is a limit of this check, not a judgement about the shape; extend the allow-list
# deliberately when the generator lands, rather than reading the failure as a defect in
# the generated output.

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

# Return the YAML scalar of a `key: value` line, with any inline comment removed.
#
# `#` opens a comment only OUTSIDE a quoted scalar. Stripping at the first
# whitespace-`#` unconditionally truncates a QUOTED value that legitimately contains
# one — and these values are cosign identity regexes, so the surviving half can pin a
# tag while the half YAML actually hands to cosign carries a second `|` alternative
# permitting `refs/heads/`. Reproduced before this fix: the single-quoted subject
# `…@refs/tags/v.+ # x|^https://…@refs/heads/.+$` was accepted and the guard reported
# all eight subjects pinned.
#
# FAILS CLOSED. Returning non-zero means "this line is not something I can read the
# way YAML reads it", and the caller rejects it rather than validating a guess. That
# covers an unterminated quote and a double-quoted scalar, whose backslash escapes
# would have to be unescaped before the ref could be judged; every subject in this
# repository is single-quoted or plain, so a double-quoted one is a new shape that
# gets reviewed here deliberately instead of being parsed on a guess.
yaml_scalar() {
  local raw="$1" value body scalar rest
  value="${raw#"${raw%%[![:space:]]*}"}"     # indentation
  value="${value#- }"                        # optional block-sequence entry
  value="${value#*:}"                        # the key
  value="${value#"${value%%[![:space:]]*}"}" # whitespace after the colon

  case "$value" in
    # A double-quoted scalar would need its backslash escapes resolved before the ref
    # could be judged, and every subject here is single-quoted or plain. A new one is
    # reviewed deliberately rather than parsed on a guess.
    '"'*) return 1 ;;
    "'"*) ;;
    *)
      # A plain scalar, where a whitespace-`#` genuinely does open a comment. YAML also excludes
      # TRAILING whitespace from a plain scalar, and this has to be removed separately: with no
      # comment present the strip above matches nothing and every trailing space survives, and with
      # one present it removes only the single space adjacent to the `#`. Either way the leftover
      # whitespace rides into the ref, stops the trailing `$` being stripped, and fails the fixed
      # `[0-9a-f]{40}` alternative against the whole-line allow-list — so the guard blocks a VALID
      # pinned subject. Fail-closed, but a false refusal is still a defect.
      scalar="${value%%[[:space:]]#*}"
      scalar="${scalar%"${scalar##*[![:space:]]}"}"
      printf '%s' "$scalar"
      return 0
      ;;
  esac

  body="${value#\'}"
  # No closing quote on this line: a multi-line or malformed scalar.
  case "$body" in
    *"'"*) ;;
    *) return 1 ;;
  esac
  scalar="${body%%\'*}"
  body="${body#*\'}"

  # `''` is an escaped quote rather than the end of the scalar. No subject can reach
  # here carrying one: everything before the last @ must match SUBJECT_PATTERN, which
  # admits no quote, and everything after it is judged by an allow-list that admits
  # none either. Refuse the shape rather than carry an unreachable — and therefore
  # untested — branch through a security guard.
  case "$body" in
    "'"*) return 1 ;;
  esac

  # Past the closing quote only a comment may follow. The whitespace-before-`#` rule
  # belongs to PLAIN scalars, where it is what separates the comment from the value;
  # a quoted scalar has already ended at its closing quote, so `gopkg.in/yaml.v3`
  # opens a comment on a `#` that follows immediately. Measured against yaml.v3
  # directly: `subject: 'abc'# c` parses to `abc`, while `subject: 'abc'x` is a parse
  # error. Applying the plain-scalar rule here rejected a subject the platform's own
  # parser accepts — fail-closed, but a false refusal is still a defect, and one that
  # blocks every workflow invoking this guard.
  #
  # Non-comment trailing content stays REJECTED in both shapes, which is what keeps
  # this a narrowing of the rule rather than a hole: yaml.v3 errors on it too, so a
  # line carrying it was not read the way YAML reads it.
  case "$body" in
    '') ;;
    '#'*) ;;
    [[:space:]]*)
      rest="${body#"${body%%[![:space:]]*}"}"
      case "$rest" in
        '' | '#'*) ;;
        *) return 1 ;;
      esac
      ;;
    *) return 1 ;;
  esac

  printf '%s' "$scalar"
}

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

    # Work on the YAML scalar, not the entire source line, and read the scalar the
    # way YAML reads it. An inline comment may contain another `@refs/tags/...`;
    # taking the last @ before removing that comment would validate the comment
    # instead of the value consumed by YAML. A `#` INSIDE a quoted scalar is not a
    # comment at all, so removing it would validate a truncation of the value.
    if ! subject="$(yaml_scalar "$subject")"; then
      printf '%s: could not read the YAML scalar on this line; this guard will not\n' "$location" >&2
      printf 'validate a value it cannot parse the way YAML parses it (a double-quoted or\n' >&2
      printf 'unterminated scalar). Rewrite it as a single-quoted or plain scalar, or extend\n' >&2
      printf 'yaml_scalar to understand this form deliberately.\n' >&2
      status=1
      continue
    fi

    # EXACTLY ONE `@`, checked BEFORE the ref is read. The ref constraint is everything
    # after the LAST @, so any alternative carrying its own `...@...` earlier in the scalar
    # is never examined. Writing the fixed-SHA alternative LAST makes the last-@ read land
    # on that decoy: the guard validates it, reports the subject pinned, and an earlier
    # alternative still permits any branch — cosign honours both. This is the same hiding
    # trick as a `#` inside a quoted scalar, but sensitive to ORDER rather than shape, so
    # rejecting one spelling of it leaves the other open.
    #
    # A legitimate subject never needs a second identity: alternation over refs belongs
    # INSIDE the group after the single @, as `@(sha1|sha2)`, which the allow-list below
    # already parses per alternative.
    local at_count
    at_count="$(printf '%s' "$subject" | tr -cd '@' | wc -c | tr -d ' ')"
    if [ "$at_count" -ne 1 ]; then
      printf '%s: subject carries %s "@" separators, so it names more than one workflow identity; only the ref after the last @ is validated, so an earlier alternative does not pin a fixed revision (subject: %s)\n' \
        "$location" "$at_count" "$subject" >&2
      status=1
      continue
    fi

    # Everything after the LAST @ in the scalar is the ref constraint; a trailing
    # $ anchor and any closing quote are not part of it.
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
    # Exactly one form qualifies: a 40-hex commit (#3022).
    #
    # An alternation is therefore always a widening now, but it is still parsed
    # per alternative rather than rejected wholesale on sight — that way the error
    # names the offending alternative instead of the whole string, and a future
    # legitimately-added form is a deliberate edit to the allow-list below rather
    # than a change to this parsing.
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
      # Match the alternative WHOLE (`-x`), never by prefix.
      #
      # A prefix match is what let `(refs/tags/v.+)?refs/heads/.+$` through when the
      # tag form was still accepted: the group was optional and a branch ref followed
      # it, yet the guard reported all eight subjects pinned. The commit form has no
      # prefix hazard of its own, but the whole-line anchor is what guarantees that —
      # `[0-9a-f]{40}` must be the entire alternative, so nothing can be appended to
      # it.
      if printf '%s' "$alternative" | grep -qxE '\[0-9a-f\]\{40\}'; then
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
