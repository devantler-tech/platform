#!/usr/bin/env bash
# Compute, per consumer, WHICH revision of a shared devantler-tech/actions publish
# workflow signed the artifact that is currently deployed, and which revision that
# consumer pins today.
#
# WHY THIS EXISTS (#3048, under #2818)
# `guard-shared-publish-workflow-pin.sh` proves every cosign matcher pins a FIXED
# revision. It says nothing about WHICH revision, and it says so itself: a superseded
# actions SHA still matches `[0-9a-f]{40}`. Narrowing those matchers to approved
# revisions is the open half, and it needs one fact nothing computes today — the
# relationship between "what signed what is running" and "what we pin now".
#
# That relationship is not decorative. Measured on #2818, five artifacts were signed by
# five different revisions, and TWO of them were signed by a revision their consumer no
# longer pins. An allow-list generated from current pins alone would therefore have
# stopped verifying two artifacts that are deployed right now — and the failure mode is
# an OCIRepository that quietly stops reconciling while every check in CI stays green.
#
# So this REPORTS; it does not gate. Divergence is the expected state today, and a check
# that failed on it would fail on the known-good configuration on its first run — which
# is how a control gets switched off, taking its real coverage with it. What it DOES
# fail on is not being able to see: a consumer that went missing from discovery, or a
# revision it could not resolve. Those are the states in which a silent "no divergence"
# would be a lie.
#
# WHAT IT DOES NOT DO
# Narrowing any matcher, generating one, and deciding how long a superseded revision
# stays accepted all stay on #2818, and should be designed against this output.
#
# THE RESOLVER IS A SEAM, ON PURPOSE
# Resolving a revision needs the network. Tests inject PUBLISH_REVISION_RESOLVER so the
# comparison logic is provable without it. Discovery is deliberately NOT injectable in
# the same way: it reads this repository's real manifests, because a stub there could
# hide exactly the regression the floor below exists to catch.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly REPO_ROOT

# Matches the subject spelling `guard-shared-publish-workflow-pin.sh` validates. Kept
# textually parallel to that guard: if one moves, the other should be looked at.
readonly SUBJECT_PATTERN='(subject|subjectRegex|subjectRegExp):[[:space:]]*.?\^?https://github\\?\.com/devantler-tech/actions/\\?\.github/workflows/publish-(app|manifests)\\?\.yaml@'

# A floor, because an empty result from a filtered read is a claim about the FILTER.
# Five consumer OCIRepositories carry these matchers (wedding-app, ascoachingogvaner,
# doggy-countdown, github-config/.github, aws). The other three matches are generic —
# the tenant ResourceGraphDefinition template, the Kyverno image policy and the Talos
# image config — and name no single consumer, so they are not counted here.
# Raise this when a consumer is genuinely added.
readonly EXPECTED_MIN_CONSUMERS=5

fail() {
  printf 'report-publish-workflow-signing-revisions: %s\n' "$*" >&2
  exit 1
}

# The OCI artifact name is USUALLY the source repository's name, but it is not the same
# thing, and one consumer proves it: `.github` has a leading dot, which is an invalid OCI
# path component, so that repository publishes its artifact as `github-config`
# (documented in k8s/bases/apps/github-config/oci-repository.yaml). Deriving the repo
# from the URL alone therefore resolves a repository that does not exist, and the run
# would report "could not resolve" for a consumer that is perfectly healthy.
#
# This mapping is deliberately an explicit, reviewed table rather than a guess: a name
# that needs translating is a decision someone should make on purpose, and an entry
# added here is visible in review. An unmapped name is passed through unchanged, so
# adding a consumer needs no edit unless its artifact name genuinely differs.
oci_name_to_repo() {
  case "$1" in
    github-config) printf '%s\n' '.github' ;;
    *) printf '%s\n' "$1" ;;
  esac
}

# 🔴 `gh api` WRITES ITS ERROR BODY TO STDOUT, so `2>/dev/null || true` does not make a
# failed call empty — it makes it a JSON blob that every downstream `[ -n ... ]` test
# reads as a perfectly good answer. That is how an earlier revision of this script
# reported healthy consumers as UNRESOLVED: a 404 body became the "tag", and the pin
# lookup then ran against a ref named `{"message":"Not Found",...}`.
#
# The failure was silent and self-consistent, which is the dangerous shape: nothing
# errored, and the report simply attributed the wrong state to a repository that was
# fine. So exit status is checked directly, and every ref is shape-checked before use —
# a ref cannot contain whitespace or a brace, which is enough to reject an error body
# without pretending to validate git's full ref grammar.
plausible_ref() {
  local ref="$1"
  [ -n "$ref" ] || return 1
  case "$ref" in
    *[[:space:]]* | *'{'* | *'}'* | *'"'*) return 1 ;;
  esac
  return 0
}

# Print "<repo>\t<workflow>\t<deployed-version-or-empty>" for every consumer whose
# artifact is verified by a shared publish-workflow matcher.
#
# The consumer repository is derived from the OCIRepository's own `spec.url`, never from
# the file path — a path is a naming convention, and a renamed directory would silently
# change which repository a revision is attributed to.
#
# The deployed version comes from `spec.ref.tag` where the OCIRepository pins one. That
# is the single most load-bearing field here: it names the artifact the cluster is
# ACTUALLY running, which is the whole question. Where the consumer uses a semver RANGE
# instead, no fixed version exists in the manifest and the resolver falls back to the
# newest published version tag.
discover_consumers() {
  local root="$1" file url repo workflow version
  [ -d "$root" ] || return 0
  while IFS= read -r file; do
    [ -n "$file" ] || continue
    # Only a real OCIRepository names a consumer. The RGD template, the Kyverno policy
    # and the Talos config match the subject pattern too but describe no single repo.
    url="$(yq eval -r 'select(.kind == "OCIRepository") | .spec.url // ""' "$file" 2>/dev/null | grep -m1 . || true)"
    [ -n "$url" ] || continue
    case "$url" in
      oci://ghcr.io/devantler-tech/*) ;;
      *) continue ;;
    esac
    repo="${url#oci://ghcr.io/devantler-tech/}"
    repo="${repo%%/*}"
    [ -n "$repo" ] || continue
    repo="$(oci_name_to_repo "$repo")"
    workflow="$(grep -oE 'publish-(app|manifests)' "$file" | head -1 || true)"
    [ -n "$workflow" ] || continue
    version="$(yq eval -r 'select(.kind == "OCIRepository") | .spec.ref.tag // ""' "$file" 2>/dev/null | grep -m1 . || true)"
    printf '%s\t%s\t%s\n' "$repo" "$workflow" "$version"
  done < <(grep -rlE "$SUBJECT_PATTERN" --include='*.yaml' "$root" 2>/dev/null | sort -u) | sort -u
}

# The `uses:` pin for one shared workflow, as it stands at one ref of one consumer.
pin_at_ref() {
  local repo="$1" workflow="$2" ref="$3" body sha
  body="$(gh api "repos/devantler-tech/${repo}/contents/.github/workflows/cd.yaml?ref=${ref}" \
    -H "Accept: application/vnd.github.raw" 2>/dev/null)" || return 1
  [ -n "$body" ] || return 1
  sha="$(printf '%s\n' "$body" |
    grep -oE "devantler-tech/actions/\\.github/workflows/${workflow}\\.yaml@[0-9a-f]{40}" |
    head -1 || true)"
  [ -n "$sha" ] || return 1
  printf '%s\n' "${sha##*@}"
}

# The tag that produced the DEPLOYED artifact.
#
# 🔴 A GitHub RELEASE is the obvious source for this and it is the WRONG one. Releases
# and the artifact stream are different surfaces that can drift apart without anything
# breaking: `.github` last cut a Release (`v1.4.2`) in June 2026, while the artifact its
# OCIRepository is running today is `1.23.0` — a tag with no Release object at all. An
# earlier revision of this script resolved that consumer at `v1.4.2` and reported it
# against a `cd.yaml` that predated an entire workflow migration, producing a confident
# answer about a revision nobody is running. `aws` fails the same way from the other
# direction: it publishes from tags and cuts no Releases, so `releases/latest` 404s.
#
# So the tag stream is authoritative and Releases are not consulted at all. Where the
# OCIRepository pins an exact version, that version IS the deployed artifact and is used
# directly; a semver range names no single version, so the newest published tag is the
# closest honest answer. `sort -V` rather than the API's order, because the tags endpoint
# promises no version ordering and the wrong tag here attributes a signature to the wrong
# revision — a quietly incorrect answer, which is worse than none.
deployed_tag() {
  local repo="$1" version="$2" tags
  tags="$(gh api "repos/devantler-tech/${repo}/tags?per_page=100" --jq '.[].name' 2>/dev/null)" || return 1
  if [ -n "$version" ]; then
    # The manifest's version may or may not carry the `v` the tag uses.
    local candidate
    for candidate in "$version" "v${version}"; do
      if printf '%s\n' "$tags" | grep -qxF "$candidate"; then
        printf '%s\n' "$candidate"
        return 0
      fi
    done
    return 1
  fi
  local newest
  newest="$(printf '%s\n' "$tags" | grep -E '^v?[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -1 || true)"
  plausible_ref "$newest" || return 1
  printf '%s\n' "$newest"
}

# Default resolver: "<signing revision>\t<current pin>".
default_resolver() {
  local repo="$1" workflow="$2" version="${3:-}" branch tag signing current
  branch="$(gh api "repos/devantler-tech/${repo}" --jq .default_branch 2>/dev/null)" || return 1
  plausible_ref "$branch" || return 1
  tag="$(deployed_tag "$repo" "$version")" || return 1
  current="$(pin_at_ref "$repo" "$workflow" "$branch")" || return 1
  signing="$(pin_at_ref "$repo" "$workflow" "$tag")" || return 1
  printf '%s\t%s\n' "$signing" "$current"
}

main() {
  local root="${PUBLISH_CONSUMER_ROOT:-$REPO_ROOT}"
  local consumers count
  consumers="$(discover_consumers "$root")"
  count="$(printf '%s' "$consumers" | grep -c . || true)"

  if [ "${1:-}" = "--list-consumers" ]; then
    [ "$count" -ge "$EXPECTED_MIN_CONSUMERS" ] ||
      fail "discovered ${count} consumer(s), expected at least ${EXPECTED_MIN_CONSUMERS}"
    printf '%s\n' "$consumers"
    return 0
  fi

  # Fail closed on a shrunken discovery. Without this the loop below would iterate zero
  # times and the report would read "no divergence" over a repository it never looked at.
  if [ "$count" -lt "$EXPECTED_MIN_CONSUMERS" ]; then
    printf 'discovered %d consumer(s), expected at least %d.\n' \
      "$count" "$EXPECTED_MIN_CONSUMERS" >&2
    printf 'The scan, not the repository, is the likely cause: these OCIRepositories may have\n' >&2
    printf 'moved, been renamed, or adopted a subject spelling this pattern does not match.\n' >&2
    printf 'Verify by hand, then fix the pattern or lower EXPECTED_MIN_CONSUMERS with the reason.\n' >&2
    exit 1
  fi

  local resolver="${PUBLISH_REVISION_RESOLVER:-}"
  local repo workflow version answer signing current diverged=0 unresolved=""

  printf 'Shared publish-workflow revisions, per consumer (#3048)\n'
  printf '%s\n' '-------------------------------------------------------'

  while IFS=$'\t' read -r repo workflow version; do
    [ -n "$repo" ] || continue
    if [ -n "$resolver" ]; then
      answer="$("$resolver" "$repo" "$workflow" "$version" 2>/dev/null || true)"
    else
      answer="$(default_resolver "$repo" "$workflow" "$version" || true)"
    fi
    signing="$(printf '%s' "$answer" | cut -f1)"
    current="$(printf '%s' "$answer" | cut -f2)"
    if [ -z "$signing" ] || [ -z "$current" ]; then
      # An unresolvable consumer is NOT "no divergence". Collect it and fail at the end,
      # so one lookup failure does not hide the consumers that did resolve.
      unresolved="${unresolved}  ${repo} (${workflow})
"
      printf 'UNRESOLVED %-22s %-18s could not resolve both revisions\n' "$repo" "$workflow"
      continue
    fi
    if [ "$signing" = "$current" ]; then
      printf 'IN-SYNC    %-22s %-18s signed=%s pinned=%s\n' \
        "$repo" "$workflow" "$signing" "$current"
      printf '           allow-list must accept: %s\n' "$signing"
    else
      diverged=$((diverged + 1))
      printf 'DIVERGED   %-22s %-18s signed=%s pinned=%s\n' \
        "$repo" "$workflow" "$signing" "$current"
      # AC4: an allow-list narrowed to the current pin alone would stop verifying the
      # deployed artifact. Naming BOTH is the actionable output of this whole script.
      printf '           allow-list must accept: %s AND %s\n' "$signing" "$current"
    fi
  done <<<"$consumers"

  printf '%s\n' '-------------------------------------------------------'
  printf '%d consumer(s) examined, %d diverged.\n' "$count" "$diverged"

  if [ -n "$unresolved" ]; then
    printf '\nCould not resolve both revisions for:\n%s' "$unresolved" >&2
    printf 'A consumer whose revisions cannot be read is not evidence of agreement, so this\n' >&2
    printf 'run fails rather than reporting a clean portfolio it could not see.\n' >&2
    exit 1
  fi

  if [ "$diverged" -gt 0 ]; then
    printf '\nDivergence is REPORTED, not failed: an allow-list on #2818 must accept both\n'
    printf 'revisions for each diverged consumer, or it will stop verifying a deployed artifact.\n'
  fi
}

main "$@"
