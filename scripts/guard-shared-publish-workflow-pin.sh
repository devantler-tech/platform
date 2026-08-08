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
    local alternatives alternative
    alternatives="${ref#\(}"
    alternatives="${alternatives%\)}"

    while IFS= read -r alternative; do
      case "$alternative" in
        '[0-9a-f]{40}') ;;
        'refs/tags/'*) ;;
        *)
          printf '%s: ref alternative %s does not pin a fixed revision (subject: %s)\n' \
            "$location" "$alternative" "$ref" >&2
          status=1
          ;;
      esac
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
