#!/usr/bin/env bash
# Compute, per consumer, WHICH revision of a shared devantler-tech/actions publish
# workflow signed the artifact that is currently deployed, and which revision that
# consumer pins today.
#
# WHY THIS EXISTS (#3048, under #2818)
# `guard-shared-publish-workflow-pin.sh` proves every cosign matcher pins a FIXED
# revision. It says nothing about WHICH, and it says so itself: a superseded actions SHA
# still matches `[0-9a-f]{40}`. Narrowing those matchers to approved revisions is the open
# half, and it needs the one fact nothing computed — the relationship between "what signed
# what is running" and "what we pin now".
#
# Measured on #2818, five artifacts were signed by five different revisions and TWO were
# signed by a revision their consumer no longer pins. An allow-list generated from current
# pins alone would therefore have stopped verifying two artifacts that are deployed right
# now, and the failure mode is an OCIRepository that quietly stops reconciling while every
# check in CI stays green.
#
# So this REPORTS; it does not gate. Divergence is the expected state today, and a check
# that failed on it would fail on the known-good configuration on its first run — which is
# how a control gets switched off, taking its real coverage with it. What it DOES fail on
# is not being able to SEE.
#
# EVERY FAILURE MODE HERE IS "REPORTS CLEAN WHILE HAVING CHECKED NOTHING"
# That is the only way this script can do harm, so each guard below exists against one
# measured instance of it. They are noted where they sit rather than listed here.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly REPO_ROOT

# Matches the subject spelling `guard-shared-publish-workflow-pin.sh` validates. Kept
# textually parallel to that guard: if one moves, the other should be looked at.
readonly SUBJECT_PATTERN='(subject|subjectRegex|subjectRegExp):[[:space:]]*.?\^?https://github\\?\.com/devantler-tech/actions/\\?\.github/workflows/publish-(app|manifests)\\?\.yaml@'

# 🔴 AN IDENTITY FLOOR, NOT A COUNT. A count answers "did I find five things?", which is
# not the question — "did I find THESE five?" is. With a bare count, one consumer moving
# out of the pattern while any other file moves in leaves the total at five, the floor
# passes, and the vanished consumer's revision is never checked. Verified by deleting
# wedding-app's manifest and adding an unrelated matching file: five consumers reported,
# exit 0, wedding-app silently absent. Add a name here when a consumer is genuinely added.
readonly EXPECTED_CONSUMERS=(
  '.github'
  'ascoachingogvaner'
  'aws'
  'doggy-countdown'
  'wedding-app'
)

fail() {
  printf 'report-publish-workflow-signing-revisions: %s\n' "$*" >&2
  exit 1
}

# The OCI artifact name is USUALLY the source repository's name, but it is not the same
# thing, and one consumer proves it: `.github` has a leading dot, which is an invalid OCI
# path component, so that repository publishes its artifact as `github-config` (documented
# in k8s/bases/apps/github-config/oci-repository.yaml). Deriving the repo from the URL
# alone therefore names a repository that does not exist, and every lookup for a perfectly
# healthy consumer fails.
#
# An explicit reviewed table rather than a guess: a name that needs translating is a
# decision someone should make on purpose. An unmapped name passes through unchanged.
oci_name_to_repo() {
  case "$1" in
    github-config) printf '%s\n' '.github' ;;
    *) printf '%s\n' "$1" ;;
  esac
}

# 🔴 `gh api` WRITES ITS ERROR BODY TO STDOUT, so `2>/dev/null || true` does not make a
# failed call empty — it makes it a JSON blob every `[ -n ... ]` test reads as a good
# answer. An earlier revision resolved a healthy consumer against a ref named
# `{"message":"Not Found",...}`: nothing errored, and the report was confidently wrong.
# Exit status is therefore checked directly, and every value is shape-checked before use.
plausible_ref() {
  local ref="$1"
  [ -n "$ref" ] || return 1
  case "$ref" in
    *[[:space:]]* | *'{'* | *'}'* | *'"'*) return 1 ;;
  esac
  return 0
}

# A repository name flows into an API path, so it is shape-checked too rather than
# trusted: a manifest URL of `oci://ghcr.io/devantler-tech/../foo/manifests` would
# otherwise yield `..`.
plausible_repo() {
  [[ "$1" =~ ^[A-Za-z0-9._-]+$ ]] && [ "$1" != '.' ] && [ "$1" != '..' ]
}

# A revision is reported to a human who will build an allow-list from it, so it must be a
# commit SHA and nothing else.
is_sha() { [[ "$1" =~ ^[0-9a-f]{40}$ ]]; }

# A transient 5xx or secondary rate limit must not make main red on a REPORT: a control
# that goes red for reasons unrelated to what it checks is one people learn to ignore.
# Bounded, so a genuine outage still fails rather than hanging.
gh_retry() {
  local attempt out
  for attempt in 1 2 3; do
    if out="$(gh "$@" 2>/dev/null)"; then
      printf '%s' "$out"
      return 0
    fi
    [ "$attempt" -eq 3 ] || sleep $((attempt * 3))
  done
  return 1
}

# Print "<repo>\t<workflow>\t<pinned-version-or-empty>" per consumer.
#
# 🔴 ITERATES DOCUMENTS, NOT FILES. Reading one value per FILE drops every consumer after
# the first in a multi-document manifest, and — worse — pairs fields across documents: with
# two matching documents `yq` emits `---` separators, so a first document using a semver
# range and a second carrying a tag yielded the literal `---` as the version. Emitting
# url, tag and subject together as one row per document is what keeps them from the same
# document by construction.
#
# The workflow is read from the document's own cosign SUBJECT, never from a whole-file
# grep: an unanchored grep matches prose, so a comment mentioning the other workflow
# reattributed a consumer — and where a consumer's cd.yaml calls both shared workflows
# that returns the wrong revision silently.
discover_consumers() {
  local root="$1" file url version subjects repo workflow workflows
  [ -d "$root" ] || return 0
  while IFS= read -r file; do
    [ -n "$file" ] || continue
    while IFS=$'\t' read -r url subjects version; do
      # 🔴 TAB IS IFS WHITESPACE, so `read` COLLAPSES consecutive tabs and an empty MIDDLE
      # field silently shifts every later field left. Two consumers pin no `spec.ref.tag`,
      # so their row was `url\t\tsubject` and the subject landed in `version` — both
      # vanished from discovery. Every field is emitted with a `-` placeholder instead of
      # empty, so no field is ever blank and no collapse is possible.
      [ "$version" = "-" ] && version=""
      [ "$subjects" = "-" ] && subjects=""
      [ -n "$url" ] || continue
      case "$url" in
        oci://ghcr.io/devantler-tech/*) ;;
        *) continue ;;
      esac
      # Exactly one shared publish workflow per document, or the attribution is ambiguous
      # and a guess would be worse than a failure.
      workflows="$(printf '%s\n' "$subjects" |
        grep -oE 'publish-(app|manifests)\\?\.yaml@' | sed 's/\\\{0,1\}\.yaml@$//' | sort -u || true)"
      [ -n "$workflows" ] || continue
      if [ "$(printf '%s\n' "$workflows" | grep -c .)" -ne 1 ]; then
        printf 'ambiguous: %s names more than one shared publish workflow\n' "$url" >&2
        continue
      fi
      workflow="$workflows"
      repo="${url#oci://ghcr.io/devantler-tech/}"
      repo="${repo%%/*}"
      [ -n "$repo" ] || continue
      repo="$(oci_name_to_repo "$repo")"
      plausible_repo "$repo" || continue
      printf '%s\t%s\t%s\n' "$repo" "$workflow" "$version"
    done < <(yq eval -r '
      select(.kind == "OCIRepository") |
      [(.spec.url // "-"),
       ((.spec.verify.matchOIDCIdentity // []) | map(.subject // "") | join(" ") | select(. != "") // "-"),
       (.spec.ref.tag // ("semver:" + (.spec.ref.semver // "")) // "-")] | @tsv
    ' "$file" 2>/dev/null || true)
  done < <(grep -rlE "$SUBJECT_PATTERN" --include='*.yaml' "$root" 2>/dev/null | sort -u) | sort -u
}

# 🔴 A SEMVER RANGE IS A CONSTRAINT, NOT "WHATEVER IS NEWEST". Discovery carries the
# expression through as `semver:<expr>` instead of discarding it, because the two are
# interchangeable only for an UNBOUNDED lower bound. All three range consumers use
# `>=1.0.0` today, so the newest published tag is exactly what Flux resolves — but a
# bounded selector such as `~1.4` or `<2.0.0` would make the report pick a tag Flux would
# never serve, attribute its workflow revision to the deployed artifact, and omit the real
# signer from the proposed allow-list.
#
# Resolving a bounded range correctly needs Flux-compatible semver selection, which this
# script deliberately does not implement, so it REFUSES rather than guesses.
#
# This is decided from the MANIFEST, before any resolver is consulted — the constraint is a
# property of what is written down, not of anything the network can tell us. Keeping it out
# of `deployed_tag` is also what lets the test suite prove it without network access; when
# this lived inside the resolver, the only way to reach it was to let the real resolver run,
# which made the case non-hermetic and it failed in CI for an unrelated reason.
#
# Prints the effective version (empty means "newest published"), or fails naming the
# constraint.
effective_version() {
  local raw="$1" expr
  case "$raw" in
    '-' | '')
      printf '%s\n' ''
      return 0
      ;;
    semver:*)
      expr="${raw#semver:}"
      case "$expr" in
        '*' | '>='[0-9]*)
          printf '%s\n' ''
          return 0
          ;;
        *)
          printf 'bounded semver constraint "%s" needs Flux-compatible selection, which this script does not implement\n' \
            "$expr" >&2
          return 1
          ;;
      esac
      ;;
    *)
      printf '%s\n' "$raw"
      return 0
      ;;
  esac
}

# The `uses:` pin for one shared workflow, as it stands at one ref of one consumer.
pin_at_ref() {
  local repo="$1" workflow="$2" ref="$3" body sha
  body="$(gh_retry api "repos/devantler-tech/${repo}/contents/.github/workflows/cd.yaml?ref=${ref}" \
    -H "Accept: application/vnd.github.raw")" || return 1
  [ -n "$body" ] || return 1
  # 🔴 PARSE THE YAML, NOT THE TEXT. An unanchored `grep … | head -1` selects a COMMENTED-OUT
  # old call if it sits above the active one, and the SHA it yields still passes `is_sha` — so
  # both revisions get confidently misreported and the real signer is omitted from the
  # allow-list. Reading `.jobs[].uses` sees only calls that actually run.
  #
  # Exactly one DISTINCT revision, or the attribution is ambiguous. Deduplicate first: two jobs
  # calling the same shared workflow at the same revision is not ambiguity, and counting call
  # SITES rather than revisions would report a perfectly unambiguous consumer as UNRESOLVED and
  # fail the run. Zero means this consumer does not call that workflow at that ref.
  local matches count
  matches="$(printf '%s\n' "$body" | yq eval -r '.jobs[].uses // ""' - 2>/dev/null |
    grep -E "^devantler-tech/actions/\\.github/workflows/${workflow}\\.yaml@[0-9a-f]{40}$" |
    sort -u || true)"
  count="$(printf '%s' "$matches" | grep -c . || true)"
  [ "$count" -eq 1 ] || return 1
  sha="${matches##*@}"
  is_sha "$sha" || return 1
  printf '%s\n' "$sha"
}

# Did this tag's release actually PUBLISH an artifact?
#
# 🔴 A GIT TAG IS NOT A PUBLISHED ARTIFACT. For the three consumers pinning a semver RANGE
# there is no version in the manifest, so the newest tag stands in for "what is deployed" —
# and if that tag's CD run failed or was skipped, no artifact was pushed, Flux is still
# serving the PREVIOUS one, and this script would read a workflow pin from a release that
# never signed anything and print it as a SHA the allow-list "must accept". That omits the
# revision which actually signed the running artifact — the exact outage #3048 exists to
# prevent, one level in.
#
# The publish outcome is public Actions data on the consumer's own repository, so this
# needs no package read and no cluster access, which keeps the check inside the scope
# #3048 set for it.
tag_was_published() {
  local repo="$1" tag="$2" runs
  # Scoped to this tag rather than fetching the newest 100 runs of the whole repository: on a
  # repository with frequent pushes a candidate release's run falls off that first page, every
  # retry re-fetches the same incomplete page, and a healthy consumer stays UNRESOLVED until the
  # next release. Filtering by ref makes the result set small enough that one page is complete.
  runs="$(gh_retry api "repos/devantler-tech/${repo}/actions/runs?branch=${tag}&event=push&per_page=100")" || return 1
  printf '%s' "$runs" |
    jq -r --arg t "$tag" '
      [.workflow_runs[]
       | select(.head_branch == $t and .path == ".github/workflows/cd.yaml")
       | .conclusion] | .[]' 2>/dev/null |
    grep -qx 'success'
}

# The tag that produced the DEPLOYED artifact.
#
# 🔴 A GitHub RELEASE is the obvious source and the WRONG one. Releases and the artifact
# stream drift apart without anything breaking: `.github` last cut a Release (`v1.4.2`) in
# June 2026 while the artifact its OCIRepository runs today is `1.23.0`, a tag with no
# Release object; `aws` publishes from tags and cuts no Releases at all. Releases are
# therefore not consulted.
#
# Prints "<tag>\t<exact|inferred>". `exact` means the OCIRepository pins that version, so
# it IS what is deployed. `inferred` means the consumer uses a semver RANGE, so the newest
# published tag is the closest honest answer — and the caller marks the row, because an
# inferred revision must not be mistaken for a measured one when an allow-list is built
# from this output.
deployed_tag() {
  local repo="$1" version="$2" tags candidate bare
  # --paginate: the endpoint caps at 100 per page and these repos already carry 50+ tags.
  # Past the cap an un-paginated read returns an arbitrary subset, which either misses a
  # real tag (false UNRESOLVED) or picks the newest of a truncated page (silently wrong).
  tags="$(gh_retry api --paginate "repos/devantler-tech/${repo}/tags?per_page=100" --jq '.[].name')" || return 1
  [ -n "$tags" ] || return 1
  if [ -n "$version" ]; then
    for candidate in "$version" "v${version}"; do
      if printf '%s\n' "$tags" | grep -qxF -- "$candidate"; then
        # 🔴 A PINNED TAG EXISTING IS NOT THE SAME AS ITS ARTIFACT HAVING BEEN PUBLISHED. The
        # publication check was reachable only from the semver branch, so the two exact-tag
        # consumers returned as soon as the git tag existed. If that tag's cd.yaml failed, the
        # previous artifact is still applied and this would name the unpublishing tag's pin as
        # the signer of what is deployed. A pinned-but-unpublished version is an anomaly worth
        # surfacing, so it is UNRESOLVED rather than silently walked back to an older tag.
        tag_was_published "$repo" "$candidate" || return 1
        printf '%s\texact\n' "$candidate"
        return 0
      fi
    done
    return 1
  fi
  # Strip the optional `v` before ordering: `sort -V` compares it as text, so a mixed list
  # orders `v1.23.0` above `1.24.0`. Sort on the bare version, then recover the real tag.
  #
  # Walk NEWEST-FIRST to the newest tag that actually published. Stopping at the newest tag
  # regardless of its release outcome is what would attribute a signature to a revision no
  # deployed artifact was ever signed by. Bounded, because a consumer whose last several
  # releases all failed is a different problem and should surface as UNRESOLVED rather than
  # send this walking back through its whole history.
  local candidates checked=0
  candidates="$(printf '%s\n' "$tags" | grep -E '^v?[0-9]+\.[0-9]+\.[0-9]+$' |
    sed 's/^v//' | sort -V -r || true)"
  [ -n "$candidates" ] || return 1
  while IFS= read -r bare; do
    [ -n "$bare" ] || continue
    checked=$((checked + 1))
    [ "$checked" -le 5 ] || break
    for candidate in "v${bare}" "$bare"; do
      printf '%s\n' "$tags" | grep -qxF -- "$candidate" || continue
      plausible_ref "$candidate" || continue
      if tag_was_published "$repo" "$candidate"; then
        printf '%s\tinferred\n' "$candidate"
        return 0
      fi
      break
    done
  done <<<"$candidates"
  return 1
}

# Default resolver: "<signing revision>\t<current pin>\t<exact|inferred>".
default_resolver() {
  local repo="$1" workflow="$2" version="${3:-}" branch tagline tag origin signing current
  branch="$(gh_retry api "repos/devantler-tech/${repo}" --jq .default_branch)" || return 1
  plausible_ref "$branch" || return 1
  tagline="$(deployed_tag "$repo" "$version")" || return 1
  IFS=$'\t' read -r tag origin <<<"$tagline"
  current="$(pin_at_ref "$repo" "$workflow" "$branch")" || return 1
  signing="$(pin_at_ref "$repo" "$workflow" "$tag")" || return 1
  printf '%s\t%s\t%s\n' "$signing" "$current" "$origin"
}

main() {
  local root="${PUBLISH_CONSUMER_ROOT:-$REPO_ROOT}"
  local consumers found missing=""
  consumers="$(discover_consumers "$root")"
  found="$(printf '%s' "$consumers" | cut -f1 | sort -u)"

  local expected
  for expected in "${EXPECTED_CONSUMERS[@]}"; do
    printf '%s\n' "$found" | grep -qxF -- "$expected" || missing="${missing} ${expected}"
  done

  # Both directions. Checking only that every EXPECTED name was found accepts an unregistered
  # sixth consumer — and that one can later move or change its subject spelling, disappear, and
  # leave all five registered names present so the report exits clean. That is exactly the silent
  # disappearance this floor exists to prevent, just one consumer along. Requiring registration
  # makes adding a consumer a deliberate, reviewed act.
  local discovered unregistered=""
  while IFS= read -r discovered; do
    [ -n "$discovered" ] || continue
    local known=0 e
    for e in "${EXPECTED_CONSUMERS[@]}"; do
      [ "$e" = "$discovered" ] && known=1 && break
    done
    [ "$known" -eq 1 ] || unregistered="${unregistered} ${discovered}"
  done <<<"$found"

  if [ -n "$unregistered" ]; then
    printf 'discovered consumer(s) not registered in EXPECTED_CONSUMERS:%s\n' "$unregistered" >&2
    printf 'A consumer this script does not know about is one it cannot notice the LOSS of later.\n' >&2
    printf 'Add it to EXPECTED_CONSUMERS so its disappearance would fail this run.\n' >&2
    exit 1
  fi

  if [ -n "$missing" ]; then
    printf 'expected consumer(s) not discovered:%s\n' "$missing" >&2
    printf 'The scan, not the repository, is the likely cause: those OCIRepositories may have\n' >&2
    printf 'moved, been renamed, or adopted a subject spelling this pattern does not match.\n' >&2
    printf 'Verify by hand, then fix the pattern or amend EXPECTED_CONSUMERS with the reason.\n' >&2
    exit 1
  fi

  if [ "${1:-}" = "--list-consumers" ]; then
    printf '%s\n' "$consumers"
    return 0
  fi

  local resolver="${PUBLISH_REVISION_RESOLVER:-}"
  local repo workflow version answer signing current origin
  local diverged=0 unresolved=0 examined=0

  printf 'Shared publish-workflow revisions, per consumer (#3048)\n'
  printf '%s\n' '-------------------------------------------------------'

  while IFS=$'\t' read -r repo workflow version; do
    [ -n "$repo" ] || continue
    examined=$((examined + 1))
    # Classify from the manifest first: a bounded range is refused before any resolver is
    # consulted, because the constraint is written down rather than discovered remotely.
    if ! version="$(effective_version "$version")"; then
      unresolved=$((unresolved + 1))
      printf 'UNRESOLVED %-22s %-18s bounded semver constraint — not resolvable here\n' \
        "$repo" "$workflow"
      continue
    fi
    if [ -n "$resolver" ]; then
      answer="$("$resolver" "$repo" "$workflow" "$version")" || answer=""
    else
      answer="$(default_resolver "$repo" "$workflow" "$version")" || answer=""
    fi
    # 🔴 `cut -f2` WITHOUT `-s` ECHOES THE WHOLE LINE when the delimiter is absent, so a
    # resolver emitting one tab-free line (an error body — the very shape warned about
    # above) set signing == current and reported IN-SYNC for every consumer with exit 0.
    # `read` splits on tabs only, and both values must then be commit SHAs: an
    # unparseable answer is UNRESOLVED, never agreement.
    IFS=$'\t' read -r signing current origin <<<"$answer"
    if ! is_sha "${signing:-}" || ! is_sha "${current:-}"; then
      unresolved=$((unresolved + 1))
      printf 'UNRESOLVED %-22s %-18s could not resolve both revisions to a commit SHA\n' \
        "$repo" "$workflow"
      continue
    fi
    local mark=''
    # An inferred revision is the newest tag whose release actually published; it is still
    # not a measurement of what Flux has APPLIED. Saying so is the difference between an
    # allow-list built on evidence and one built on a guess that reads identically.
    [ "${origin:-}" = 'inferred' ] && mark=' (deployed version inferred from newest PUBLISHED tag)'
    if [ "$signing" = "$current" ]; then
      printf 'IN-SYNC    %-22s %-18s signed=%s pinned=%s%s\n' \
        "$repo" "$workflow" "$signing" "$current" "$mark"
      printf '           allow-list must accept: %s\n' "$signing"
    else
      diverged=$((diverged + 1))
      printf 'DIVERGED   %-22s %-18s signed=%s pinned=%s%s\n' \
        "$repo" "$workflow" "$signing" "$current" "$mark"
      # AC4: an allow-list narrowed to the current pin alone would stop verifying the
      # deployed artifact. Naming BOTH is the actionable output of this whole script.
      printf '           allow-list must accept: %s AND %s\n' "$signing" "$current"
    fi
  done <<<"$consumers"

  printf '%s\n' '-------------------------------------------------------'
  # `unresolved` is in the tally deliberately. The failure detail below also goes to
  # stdout: `tee` into the step summary captures stdout only, so a stderr-only diagnostic
  # left a human reading `0 diverged` as the last line of a run that had FAILED.
  printf '%d consumer(s) examined, %d diverged, %d unresolved.\n' \
    "$examined" "$diverged" "$unresolved"

  if [ "$unresolved" -gt 0 ]; then
    printf '\nFAILED: %d consumer(s) could not be resolved. A consumer whose revisions cannot\n' "$unresolved"
    printf 'be read is not evidence of agreement, so this run fails rather than reporting a\n'
    printf 'clean portfolio it could not see.\n'
    return 1
  fi

  if [ "$diverged" -gt 0 ]; then
    printf '\nDivergence is REPORTED, not failed: an allow-list on #2818 must accept both\n'
    printf 'revisions for each diverged consumer, or it will stop verifying a deployed artifact.\n'
  fi
}

main "$@"
