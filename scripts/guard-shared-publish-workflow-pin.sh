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
# Pinning a fixed revision is not the same as pinning an APPROVED one. This guard
# accepts either recognisable form — the pattern text `[0-9a-f]{40}`, which admits any
# 40-hex commit, or a concrete 40-hex commit naming exactly one — so it proves the ref
# SHAPE pins a revision. It does not decide WHICH revisions are approved, and it keeps
# the property #2816 established from silently regressing.
#
# Which revisions must stay trusted is computed per consumer by
# scripts/report-publish-workflow-signing-revisions.sh (#3048): it names both the
# revision that signed the artifact currently deployed and the revision that consumer
# pins today, and those routinely differ. A consumer narrowed to a set omitting its
# signing revision would stop verifying an artifact that is running right now, so any
# narrowing is derived from that report rather than from this guard or by hand (#3308).
# Set MEMBERSHIP — that each per-consumer matcher names exactly the generated pair and no
# revision outside it — is guard-publish-workflow-approved-revisions.sh's question (#3551).
#
# TWO PUBLISHER FAMILIES (#4502)
# A manifest matcher may accept the canonical manifest workflow in devantler-tech/.github
# beside the legacy one, as ONE subject holding one group:
#
#   ^https://github\.com/devantler-tech/(actions/\.github/workflows/publish-<w>\.yaml@<ref>|\.github/\.github/workflows/publish-<w>\.yaml@<commit>)$
#
# That subject carries two `@`, which the single-identity rule below exists to refuse, so
# it is read by its own strict parse instead of by loosening that rule: the exact prefix,
# the legacy family first, the canonical family second, each exactly once, and nothing
# else. The legacy ref is judged by the same allow-list as every other subject. The
# canonical ref must be ONE concrete 40-hex commit — the pattern form is not accepted
# there, because the canonical family is approved one reviewed commit at a time. A subject
# naming only the canonical family, or the two in any other arrangement, is refused: an
# arrangement nobody reviewed is not a spelling variant. Which commit is approved is still
# the approved-revisions guard's question, not this one's.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly REPO_ROOT

# The subjects are cosign identity regexes, so the `.` of `github.com` and `.github`
# are escaped in the file. Match both the escaped and bare spellings rather than
# assuming one, and accept all three key names in use (`subject` on Flux
# OCIRepositories, `subjectRegex` on the Talos image-verification config,
# `subjectRegExp` in Kyverno policies) — a fourth spelling appearing later should show
# up as a MISSING match against the floor below, not be silently skipped.
#
# Either publisher family, with an optional group opener (#4502), so a two-family subject
# and a canonical-only one are FOUND and judged rather than left outside the scan. Kept
# textually parallel to FAMILY_SUBJECT_PATTERN in publish-workflow-approved-revisions.lib.sh.
readonly SUBJECT_PATTERN='(subject|subjectRegex|subjectRegExp):[[:space:]]*.?\^?https://github\\?\.com/devantler-tech/[(]?(actions|\\?\.github)/\\?\.github/workflows/publish-(app|manifests)\\?\.yaml@'

# The workflow identity of either family with no key name, no organisation prefix and no
# line structure, for the independent discovery below. The canonical half of a two-family
# subject follows a `|`, not the organisation, so a pattern anchored on the organisation
# would not see a canonical reference written anywhere but first.
readonly DISCOVERY_PATTERN='(actions|\\?\.github)/\\?\.github/workflows/publish-(app|manifests)\\?\.yaml'

# The exact text of a two-family subject around its two refs (#4502). These are compared
# literally, so the backslashes are the ones written in the manifest.
readonly FAMILY_PREFIX='^https://github\.com/devantler-tech/('
readonly LEGACY_FAMILY='actions/\.github/workflows/publish-'
# The exact opening of a single-family subject, up to the workflow name.
readonly LEGACY_SUBJECT_PREFIX='^https://github\.com/devantler-tech/actions/\.github/workflows/publish-'
readonly CANONICAL_FAMILY='\.github/\.github/workflows/publish-'
# Only the manifest workflow has a canonical family today; the application workflow and
# the three generic subjects stay legacy-only.
readonly CANONICAL_WORKFLOW_NAME='manifests'

# THE FLOOR NAMES WHAT MUST BE FOUND (#4558).
# An empty result from a filtered read is a claim about the FILTER. If a refactor moves
# these subjects into a generator, a template, or a different key, the grep below returns
# less and — without a floor — the guard would exit 0 and report a clean repository while
# checking nothing.
#
# The floor used to be a COUNT, and a count cannot tell which matcher went missing: one
# rewritten in a spelling the pattern does not select was simply not counted, and any
# other matcher made the number up. So the floor is the list of files that hold a matcher
# and how many each holds, one `<count><TAB><path>` per line. Each named file must yield
# exactly that many, and a matcher in a file the list does not name fails too. Add a line
# when a consumer is genuinely added, and remove one only with the file it names.
readonly EXPECTED_MATCHERS_FILE='scripts/shared-publish-workflow-matchers.tsv'

# EVERY SUBJECT IS ACCOUNTED FOR, HOWEVER ITS VALUE IS SPELLED (#4558).
# SUBJECT_PATTERN and DISCOVERY_PATTERN both recognise the workflow identity by its text,
# so a matcher whose regex means the same identity in another spelling is selected by
# neither. This pattern selects by KEY instead: any mapping entry keyed by one of the three
# subject spellings. Whatever it selects is either judged as a shared-workflow matcher or
# must be proved to name a different repository (prove_other_identity).
readonly SUBJECT_KEY_PATTERN='^[[:space:]]*(-[[:space:]]+)*["'\'']?(subject|subjectRegex|subjectRegExp)["'\'']?[[:space:]]*:'
# The same keys inside a flow mapping, `{issuer: x, subject: y}`, which the line-anchored
# pattern above cannot see. No manifest here writes one, so the form is refused.
readonly FLOW_SUBJECT_KEY_PATTERN='[{,][[:space:]]*["'\'']?(subject|subjectRegex|subjectRegExp)["'\'']?[[:space:]]*:[[:space:]]'
readonly GITHUB_IDENTITY_PREFIX='^https://github\.com/'

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

# judge_ref <location> <ref> — the allow-list over one ref constraint. Returns non-zero,
# having said why, unless every alternative of the ref pins a fixed revision.
judge_ref() {
  local location="$1" ref="$2" status=0
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
      return 1
      ;;
    *'|'*)
      # An alternation with no group around it is not an alternation over REFS. `@A|B$`
      # reads as `^…@A` OR `B$`, and the second half is anchored to nothing before it —
      # so `@<commit>|[0-9a-f]{40}$` accepts any identity at all that ends in a commit.
      # Splitting it on `|` and finding two well-formed alternatives is what waved it
      # through. Several refs must be written `(A|B)`.
      printf '%s: ref %s is not a fully grouped alternation; this guard cannot prove it pins a revision\n' \
        "$location" "$ref" >&2
      return 1
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
    # TWO recognisable forms, both of which pin (#3308).
    #
    # 1. The literal PATTERN text `[0-9a-f]{40}`. These subjects are cosign identity
    #    regexes, so this fragment accepts any 40-hex commit: a FIXED revision, but
    #    not an APPROVED one.
    # 2. A CONCRETE 40-hex commit, which is what a generated approved-revision
    #    allow-list emits. It is strictly NARROWER than form 1 — it names one
    #    revision rather than the whole 40-hex space.
    #
    # Only form 1 is a regex fragment; form 2 is a literal identity. Both are judged
    # by the same allow-list below, so adding a third recognisable shape is likewise a
    # deliberate edit here rather than a silent widening (#3308).
    #
    # It is a WIDENING OF THE ALLOW-LIST, not a loosening of the property. Both forms
    # keep the whole-line anchor (`-x`), so nothing may be appended to either: a short
    # SHA, a tag, a branch, a bare ref and a partial group are all still rejected, and
    # each keeps its own RED case in the test. Uppercase hex is deliberately NOT
    # accepted — git emits lowercase, so an uppercase subject is an unreviewed shape
    # rather than a spelling variant.
    if printf '%s' "$alternative" | grep -qxE '\[0-9a-f\]\{40\}'; then
      continue
    fi
    if printf '%s' "$alternative" | grep -qxE '[0-9a-f]{40}'; then
      continue
    fi
    printf '%s: ref alternative %s does not pin a fixed revision (subject: %s)\n' \
      "$location" "$alternative" "$ref" >&2
    status=1
  done < <(printf '%s\n' "$alternatives" | tr '|' '\n')
  return "$status"
}

# judge_family_subject <location> <subject> — the strict parse of a two-family subject
# (#4502). Returns non-zero, having said why, unless the subject is exactly the legacy
# family followed by the canonical family, the legacy ref passes the allow-list and the
# canonical ref is one concrete commit.
#
# The split is on the literal text that opens the canonical family, never on `|` or `@`
# alone: the legacy ref is itself an alternation, so both characters occur inside it.
judge_family_subject() {
  local location="$1" subject="$2" inner boundary legacy_part canonical_part legacy_ref canonical_ref at_count

  case "$subject" in
    "$FAMILY_PREFIX"*')$') ;;
    *)
      printf '%s: subject opens a publisher group but is not exactly %s<legacy family>|<canonical family>)$; this guard cannot prove an arrangement it does not recognise pins a revision (subject: %s)\n' \
        "$location" "$FAMILY_PREFIX" "$subject" >&2
      return 1
      ;;
  esac

  at_count="$(printf '%s' "$subject" | tr -cd '@' | wc -c | tr -d ' ')"
  if [ "$at_count" -ne 2 ]; then
    printf '%s: two-family subject carries %s "@" separators where exactly two are expected, one per publisher family (subject: %s)\n' \
      "$location" "$at_count" "$subject" >&2
    return 1
  fi

  inner="${subject#"$FAMILY_PREFIX"}"
  inner="${inner%')$'}"
  boundary='|'"$CANONICAL_FAMILY"
  case "$inner" in
    "$LEGACY_FAMILY"*"$boundary"*) ;;
    *)
      printf '%s: two-family subject is not the legacy family followed by the canonical family (subject: %s)\n' \
        "$location" "$subject" >&2
      return 1
      ;;
  esac
  canonical_part="${inner#*"$boundary"}"                # <workflow>\.yaml@<commit>
  legacy_part="${inner%"$boundary$canonical_part"}"
  legacy_part="${legacy_part#"$LEGACY_FAMILY"}"         # <workflow>\.yaml@<ref>
  case "$legacy_part$canonical_part" in
    *"$LEGACY_FAMILY"* | *"$CANONICAL_FAMILY"*)
      printf '%s: two-family subject names a publisher family more than once (subject: %s)\n' \
        "$location" "$subject" >&2
      return 1
      ;;
  esac

  # Each side of the boundary ends up holding exactly one `@`, so neither ref can hide a
  # further identity, without a check of its own: the subject carries two in all, the
  # canonical side must be `<workflow>\.yaml@` followed by 40 hex characters and nothing
  # else, and the legacy side must open with `<workflow>\.yaml@`.
  canonical_ref="${canonical_part#"$CANONICAL_WORKFLOW_NAME"'\.yaml@'}"
  if [ "$canonical_ref" = "$canonical_part" ]; then
    printf '%s: the canonical family names a workflow other than publish-%s, the only one with a canonical publisher (subject: %s)\n' \
      "$location" "$CANONICAL_WORKFLOW_NAME" "$subject" >&2
    return 1
  fi
  case "$legacy_part" in
    "$CANONICAL_WORKFLOW_NAME"'\.yaml@'*) ;;
    *)
      printf '%s: the two publisher families name different workflows (subject: %s)\n' \
        "$location" "$subject" >&2
      return 1
      ;;
  esac

  # ONE concrete commit, matched whole. The pattern text `[0-9a-f]{40}` is deliberately
  # not accepted here: it would admit every commit of the canonical repository, and the
  # canonical family is approved one reviewed commit at a time.
  if ! printf '%s' "$canonical_ref" | grep -qxE '[0-9a-f]{40}'; then
    printf '%s: canonical ref %s does not pin one concrete 40-hex commit (subject: %s)\n' \
      "$location" "$canonical_ref" "$subject" >&2
    return 1
  fi

  legacy_ref="${legacy_part#*@}"
  judge_ref "$location" "$legacy_ref"
}

# load_expected_matchers — print the floor as `<count><TAB><path>` lines. Returns non-zero,
# having said why, when the list is missing, empty or holds a line it cannot read: a floor
# that could not be read is not a floor of zero.
load_expected_matchers() {
  local line count path seen=$'\n' listed=0
  if [ ! -r "$EXPECTED_MATCHERS_FILE" ]; then
    printf 'guard: %s is missing or unreadable, so there is no list of the files that must hold a matcher.\n' \
      "$EXPECTED_MATCHERS_FILE" >&2
    return 1
  fi
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      '' | '#'*) continue ;;
    esac
    count="${line%%$'\t'*}"
    path="${line#*$'\t'}"
    case "$path" in
      '' | "$line" | *$'\t'* | ./* | /*)
        printf 'guard: %s holds a line that is not <count><TAB><repository-relative path>: %s\n' \
          "$EXPECTED_MATCHERS_FILE" "$line" >&2
        return 1
        ;;
    esac
    if ! printf '%s' "$count" | grep -qxE '[1-9][0-9]*'; then
      printf 'guard: %s gives %s a count that is not a positive number: %s\n' \
        "$EXPECTED_MATCHERS_FILE" "$path" "$count" >&2
      return 1
    fi
    case "$seen" in
      *$'\n'"$path"$'\n'*)
        printf 'guard: %s names %s more than once.\n' "$EXPECTED_MATCHERS_FILE" "$path" >&2
        return 1
        ;;
    esac
    seen="$seen$path"$'\n'
    listed=$((listed + 1))
    printf '%s\t%s\n' "$count" "$path"
  done <"$EXPECTED_MATCHERS_FILE"
  if [ "$listed" -eq 0 ]; then
    printf 'guard: %s names no file, so nothing is required to hold a matcher.\n' \
      "$EXPECTED_MATCHERS_FILE" >&2
    return 1
  fi
}

# check_named_matchers <expected> <matches> — every named file yields exactly its count,
# and no matcher sits in a file the list does not name.
check_named_matchers() {
  local expected="$1" matches="$2" status=0 count path actual file
  while IFS=$'\t' read -r count path; do
    [ -n "$path" ] || continue
    actual="$(printf '%s\n' "$matches" | grep -cF -- "./$path:" || true)"
    if [ "$actual" -ne "$count" ]; then
      printf 'guard: %s holds %d shared-publish-workflow matcher(s) this guard can judge, expected %d.\n' \
        "$path" "$actual" "$count" >&2
      status=1
    fi
  done <<EOF
$expected
EOF
  while IFS= read -r file; do
    [ -n "$file" ] || continue
    if ! printf '%s\n' "$expected" | cut -f2- | grep -qxF -- "${file#./}"; then
      printf 'guard: %s holds a shared-publish-workflow matcher but is not named in %s.\n' \
        "${file#./}" "$EXPECTED_MATCHERS_FILE" >&2
      status=1
    fi
  done <<EOF
$(printf '%s\n' "$matches" | cut -d: -f1 | sort -u)
EOF
  if [ "$status" -ne 0 ]; then
    printf 'The scan, not the repository, is the likely cause when a count is short: the matcher may\n' >&2
    printf 'have moved, or adopted a key or a spelling this guard does not select. Verify by hand,\n' >&2
    printf 'then fix the matcher or the pattern. Change %s only when a consumer was\n' "$EXPECTED_MATCHERS_FILE" >&2
    printf 'genuinely added or removed, in the change that adds or removes its file.\n' >&2
  fi
  return "$status"
}

# has_top_level_alternation <regex> — true when a `|` sits outside every group and
# character class, which splits the whole expression: `^A|B` is `^A` OR `B`, and the
# second half is anchored to nothing.
has_top_level_alternation() {
  local text="$1" depth=0 in_class=0 index char
  for ((index = 0; index < ${#text}; index++)); do
    char="${text:index:1}"
    if [ "$char" = "\\" ]; then
      index=$((index + 1))
      continue
    fi
    if [ "$in_class" -eq 1 ]; then
      [ "$char" = ']' ] && in_class=0
      continue
    fi
    case "$char" in
      '[') in_class=1 ;;
      '(') depth=$((depth + 1)) ;;
      ')') depth=$((depth - 1)) ;;
      '|') [ "$depth" -le 0 ] && return 0 ;;
    esac
  done
  return 1
}

# prove_other_identity <location> <subject> — the answer for a subject this guard does not
# judge. Returns non-zero, having said why, unless the subject provably names a repository
# other than the two that host the shared publish workflows.
#
# The proof is a literal prefix. A regex that opens with the anchored literal
# `^https://github\.com/<owner>/<repository>` and has no alternation outside a group can
# only match an identity beginning with that text, so one whose owner is not
# devantler-tech, or whose repository cannot begin `actions`, cannot match a shared
# workflow however the rest of it is written. Anything else is refused, not skipped: an
# identity this guard cannot place may be a shared-workflow matcher in a spelling it does
# not select, which is the gap #4558 closes. Extend this deliberately when a new kind of
# signer is added.
prove_other_identity() {
  local location="$1" subject="$2" rest owner repository hosting='actions'
  case "$subject" in
    "$GITHUB_IDENTITY_PREFIX"*) ;;
    *)
      printf '%s: subject does not open with the anchored literal %s, so this guard cannot prove it is not a shared-publish-workflow matcher in a spelling it does not judge (subject: %s)\n' \
        "$location" "$GITHUB_IDENTITY_PREFIX" "$subject" >&2
      return 1
      ;;
  esac
  # A class that opens with `]` holds it as a literal, which the scan below would read as
  # the end of the class. No subject here needs one.
  case "$subject" in
    *'[]'* | *'[^]'*)
      printf '%s: subject holds a character class opening with "]", which this guard does not parse (subject: %s)\n' \
        "$location" "$subject" >&2
      return 1
      ;;
  esac
  if has_top_level_alternation "$subject"; then
    printf '%s: subject holds an alternation outside a group, so its opening literal does not bound what it matches (subject: %s)\n' \
      "$location" "$subject" >&2
    return 1
  fi

  rest="${subject#"$GITHUB_IDENTITY_PREFIX"}"
  owner="${rest%%[!A-Za-z0-9-]*}"
  rest="${rest#"$owner"}"
  if [ -z "$owner" ] || [ "${rest:0:1}" != '/' ]; then
    printf '%s: subject does not name one literal owner after %s (subject: %s)\n' \
      "$location" "$GITHUB_IDENTITY_PREFIX" "$subject" >&2
    return 1
  fi
  [ "$owner" = 'devantler-tech' ] || return 0

  rest="${rest#/}"
  repository="${rest%%[!a-z0-9-]*}"
  rest="${rest#"$repository"}"
  # The literal must END at a `/` or a class: a quantifier there would make its last
  # character optional or repeated, so the text before it would no longer be a prefix.
  case "${rest:0:1}" in
    '/' | '[') ;;
    *) repository='' ;;
  esac
  case "$repository" in
    '')
      printf '%s: subject does not open its repository with a literal name, so this guard cannot prove which repository it accepts (subject: %s)\n' \
        "$location" "$subject" >&2
      return 1
      ;;
  esac
  case "$hosting" in
    "$repository"*)
      printf '%s: subject can name the repository that hosts the shared publish workflows but is not written as %s(app|manifests)\\.yaml@<ref>$, so it was not judged (subject: %s)\n' \
        "$location" "$LEGACY_SUBJECT_PREFIX" "$subject" >&2
      return 1
      ;;
  esac
}

main() {
  cd "$REPO_ROOT"

  local expected
  expected="$(load_expected_matchers)" || return 1

  local matches
  matches="$(grep -rnE "$SUBJECT_PATTERN" --include='*.yaml' . || true)"

  local found=0
  if [ -n "$matches" ]; then
    found="$(printf '%s\n' "$matches" | wc -l | tr -d ' ')"
  fi

  check_named_matchers "$expected" "$matches" || return 1

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
  discovered_lines="$(grep -rlE "$DISCOVERY_PATTERN" --include='*.yaml' . || true)"

  unvalidated=""
  local file discovered_count validated_count
  while IFS= read -r file; do
    [ -n "$file" ] || continue
    discovered_count="$(grep -cE "$DISCOVERY_PATTERN" "$file" || true)"
    validated_count="$(grep -cE "$SUBJECT_PATTERN" "$file" || true)"
    if [ "$discovered_count" -gt "$validated_count" ]; then
      unvalidated="$unvalidated  $file (references: $discovered_count, validated as subjects: $validated_count)
"
    fi
  done <<EOF
$discovered_lines
EOF

  # A BLOCK SCALAR carries ONE value across several lines, and everything in this guard
  # reads LINES. YAML folds the block into a single value, so an indented content line
  # that merely LOOKS like `subject: <pinned>` is not a key at all -- the strict pattern
  # matches that content line, validates it, and reports the subject pinned, while the
  # value cosign actually receives is something like `.*| subject: ...@[0-9a-f]{40}$`
  # whose FIRST alternative accepts every identity.
  #
  # The coverage rule above does NOT catch this. It compares reference counts against
  # validated counts, and a decoy carrying the workflow URL only once keeps them aligned
  # -- measured. A decoy that repeats the URL in both alternatives IS caught there, so
  # pinning only that shape would leave the reachable one open.
  #
  # No legitimate subject needs a block scalar: a cosign identity regex is one line. So
  # the form is refused outright rather than assembled, exactly as the double-quoted
  # scalar is refused below. Scoped to files that reference the shared workflow, so an
  # unrelated `subject:` block scalar elsewhere in the repository is not this guard's
  # business.
  local block_subjects=""
  while IFS= read -r file; do
    [ -n "$file" ] || continue
    # ANY suffix after the indicator, not an enumerated one. A block-scalar header may
    # carry an indentation digit and a chomping sign IN EITHER ORDER -- `>-2` and `>2-`
    # are both valid, and so are `|+2` and `|2+`; all four were measured folding an
    # indented decoy line into the value. A pattern matching only digits-then-sign missed
    # the sign-first spellings, which restored the bypass this check exists to close.
    #
    # Nothing legitimate is lost by being broad: in YAML a value beginning with `|` or `>`
    # IS a block scalar, so there is no plain scalar for this to catch by mistake.
    if grep -qE '(subject|subjectRegex|subjectRegExp):[[:space:]]*[|>][^[:space:]]*[[:space:]]*(#.*)?$' "$file"; then
      block_subjects="$block_subjects  $file
"
    fi
  done <<EOF
$discovered_lines
EOF

  if [ -n "$block_subjects" ]; then
    printf 'guard: a cosign subject is written as a YAML BLOCK SCALAR in:\n' >&2
    printf '%s' "$block_subjects" >&2
    printf 'This guard reads lines, so it cannot assemble the folded value, and an indented\n' >&2
    printf 'content line that looks like a pinned subject key would be validated in its place\n' >&2
    printf 'while the value cosign receives carries an alternative that pins nothing.\n' >&2
    printf 'Write the subject as a single-line plain or single-quoted scalar.\n' >&2
    return 1
  fi

  if [ -n "$unvalidated" ]; then
    printf 'guard: found reference(s) to the shared publish workflows that this guard did not validate:\n' >&2
    printf '%s' "$unvalidated" >&2
    printf 'A consumer written in a form the subject pattern does not match is NOT checked, so it could\n' >&2
    printf 'pin nothing while this guard reports success. Either extend SUBJECT_PATTERN to cover the new\n' >&2
    printf 'form and name its file in %s, or confirm the reference is not a cosign subject.\n' "$EXPECTED_MATCHERS_FILE" >&2
    return 1
  fi

  local flow_subjects
  flow_subjects="$(grep -rnE "$FLOW_SUBJECT_KEY_PATTERN" --include='*.yaml' . | grep -vE '^[^:]+:[0-9]+:[[:space:]]*#' || true)"
  if [ -n "$flow_subjects" ]; then
    printf 'guard: a subject is written inside a YAML flow mapping:\n%s\n' "$flow_subjects" >&2
    printf 'This guard reads one mapping entry per line, so it would never judge that value.\n' >&2
    printf 'Write the subject as a mapping entry on a line of its own.\n' >&2
    return 1
  fi

  local status=0
  local line location subject ref

  # ACCOUNT FOR EVERY SUBJECT THE STRICT PATTERN DID NOT SELECT (#4558). Each is read the
  # way YAML reads it and must be proved to name another repository. One that cannot be is
  # refused: it may be a shared-workflow matcher in a spelling nothing above selects.
  local keyed
  keyed="$(grep -rnE "$SUBJECT_KEY_PATTERN" --include='*.yaml' . || true)"
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    if printf '%s\n' "$line" | grep -qE "$SUBJECT_PATTERN"; then
      continue # judged below
    fi
    location="${line%%:*}"
    line="${line#*:}"
    location="$location:${line%%:*}"
    subject="${line#*:}"
    if ! subject="$(yaml_scalar "$subject")"; then
      printf '%s: could not read the YAML scalar of this subject, so this guard cannot tell which identity it accepts. Write it as a single-quoted or plain scalar.\n' \
        "$location" >&2
      status=1
      continue
    fi
    prove_other_identity "$location" "$subject" || status=1
  done <<EOF
$keyed
EOF
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

    # A TWO-FAMILY SUBJECT IS READ BY ITS OWN STRICT PARSE (#4502). It carries two `@` by
    # design, so it is dispatched before the single-identity rule below rather than by
    # relaxing that rule: every other subject is still held to exactly one.
    case "$subject" in
      *'devantler-tech/('*)
        judge_family_subject "$location" "$subject" || status=1
        continue
        ;;
      *'devantler-tech/\.github/'* | *'devantler-tech/.github/'*)
        printf '%s: subject names only the canonical publisher; the recognised forms are the legacy family alone or the legacy family followed by the canonical one (subject: %s)\n' \
          "$location" "$subject" >&2
        status=1
        continue
        ;;
    esac

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

    # THE WHOLE SCALAR, NOT JUST ITS REF. SUBJECT_PATTERN selects a LINE, and it matches
    # anywhere on it — inside a comment, or after a `|` in the value. Judging only the text
    # after the single `@` therefore validated subjects whose identity half was never the
    # shared workflow at all: a value of `.*@[0-9a-f]{40}` followed by a comment naming the
    # workflow was selected by the comment and passed on its ref. So the scalar itself must
    # be exactly the anchored legacy identity, a ref, and the closing anchor. Compared
    # literally: an unescaped dot or a character class in place of `\.` is a wider regex,
    # not a spelling of this one.
    case "$subject" in
      "$LEGACY_SUBJECT_PREFIX"'app\.yaml@'*'$' | "$LEGACY_SUBJECT_PREFIX"'manifests\.yaml@'*'$') ;;
      *)
        printf '%s: subject is not exactly %s(app|manifests)\\.yaml@<ref>$, so the identity it accepts is not the one this line was selected for (subject: %s)\n' \
          "$location" "$LEGACY_SUBJECT_PREFIX" "$subject" >&2
        status=1
        continue
        ;;
    esac

    # Everything after the single @ is the ref constraint, less the closing anchor.
    ref="${subject##*@}"
    ref="${ref%$}"

    judge_ref "$location" "$ref" || status=1
  done <<EOF
$matches
EOF

  if [ "$status" -eq 0 ]; then
    printf 'guard: %d shared-publish-workflow subject(s) all pin a fixed revision.\n' "$found"
  fi

  return "$status"
}

main "$@"
