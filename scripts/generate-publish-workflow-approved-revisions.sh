#!/usr/bin/env bash
# generate-publish-workflow-approved-revisions.sh â derive, per consumer of the shared
# publish workflows, the two revisions its OCIRepository cosign matcher must accept, and
# commit them as a reviewable data file (#3550, first child of #3308).
#
# WHY A GENERATED FILE
# Narrowing a consumer's cosign matcher from `@[0-9a-f]{40}` to a named set is only safe
# when that set contains BOTH the revision that signed the artifact Flux has APPLIED and the
# revision the consumer pins on its default branch TODAY: the first keeps the deployed
# artifact verifying, the second keeps the next artifact verifying. Hand-writing that set is
# what #3308 rules out, so this script derives it from two observed inputs and writes one row
# per consumer. It changes no matcher: guard-publish-workflow-approved-revisions.sh (#3551)
# holds each per-consumer matcher to the pair written here, and the switch that refuses the
# pattern form (APPROVED_REVISIONS_ENFORCE) flips with the narrowing itself.
#
# THE TWO INPUTS
#   applied  â the artifact revision Flux has verified and applied, read from the consumer's
#              OCIRepository `status.artifact.revision` (`<tag>@sha256:<digest>`) on the
#              cluster named by PUBLISH_KUBE_CONTEXT; the revision that SIGNED it is the shared
#              workflow revision recorded on the `ð CD` run that published exactly that tag
#              (`referenced_workflows[].sha`), which is the same fact the OIDC
#              `job_workflow_ref` claim is minted from and needs no package read.
#   pin      â `.jobs[].uses` of the consumer's cd.yaml at its default branch, via the report's
#              `pin_at_ref`.
#
# FAIL CLOSED, WRITE NOTHING. A consumer that cannot be resolved on either half is named on
# stderr and the run exits 1 having written NO file: an approved set missing a consumer, or
# carrying one revision where two are needed, reads exactly like a complete one to whoever
# narrows the matchers from it.
#
# BYTE-IDENTICAL ON UNCHANGED INPUTS. `observed_on` is kept from the existing row whenever a
# consumer's four-tuple has not moved, so regenerating on the same inputs is a no-op and a
# guard can diff the committed file against a fresh run.
#
# SEAMS (tests substitute these; production leaves them unset)
#   APPROVED_REVISION_OBSERVER      <repo> <workflow> â "<applied-tag>\t<sha256:digest>\t<signer-sha>"
#   APPROVED_REVISION_PIN_RESOLVER  <repo> <workflow> â "<pin-sha>"
#   APPROVED_REVISIONS_FILE         output path (default scripts/publish-workflow-approved-revisions.tsv)
#   APPROVED_REVISIONS_OBSERVED_ON  YYYY-MM-DD stamped on rows whose tuple changed (default: today, UTC)
#   PUBLISH_KUBE_CONTEXT            kube context the default observer reads OCIRepositories through
#   PUBLISH_CONSUMER_ROOT           discovery root, passed through to the report
set -euo pipefail

GEN_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly GEN_DIR
readonly REPORT="$GEN_DIR/report-publish-workflow-signing-revisions.sh"
# shellcheck source=scripts/report-publish-workflow-signing-revisions.sh
source "$REPORT"

readonly OUTPUT="${APPROVED_REVISIONS_FILE:-$REPO_ROOT/scripts/publish-workflow-approved-revisions.tsv}"
readonly HEADER=$'consumer\tworkflow\tapplied_tag\tapplied_digest\tapplied_signer_sha\tmain_pin_sha\tobserved_on'

refuse() {
  printf 'generate-publish-workflow-approved-revisions: %s\n' "$*" >&2
}

is_digest() { [[ "$1" =~ ^sha256:[0-9a-f]{64}$ ]]; }
is_date() { [[ "$1" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]]; }
# An OCI tag: no whitespace, no field separator, nothing that could be a JSON error body.
plausible_tag() {
  local tag="$1"
  [ -n "$tag" ] || return 1
  case "$tag" in
    *[[:space:]]* | *'{'* | *'}'* | *'"'* | *'@'*) return 1 ;;
  esac
  return 0
}

# The OCIRepository Flux applies for one consumer, read from the cluster. Prints
# "<registry-tag>\t<sha256:digest>". Exactly one OCIRepository may carry the consumer's URL.
applied_revision() {
  local repo="$1" context="${PUBLISH_KUBE_CONTEXT:-}" url json revision
  if [ -z "$context" ]; then
    refuse "$repo: the applied revision is a cluster observation and PUBLISH_KUBE_CONTEXT is unset"
    return 1
  fi
  url="oci://ghcr.io/devantler-tech/$(ghcr_package_for_repo "$repo")"
  json="$(kubectl --context "$context" get ocirepositories.source.toolkit.fluxcd.io -A -o json 2>/dev/null)" || {
    refuse "$repo: could not list OCIRepositories through context $context"
    return 1
  }
  # COUNT the objects before reading a revision: joining revisions and testing the join for a
  # newline is order-dependent, because a second object with NO artifact yet contributes an
  # empty string that `$(...)` strips when it is last - the same cluster state is refused in
  # one listing order and silently accepted in the other.
  local count
  count="$(printf '%s' "$json" | jq -r --arg u "$url" '[.items[] | select(.spec.url == $u)] | length' 2>/dev/null)" || {
    refuse "$repo: OCIRepository listing was not parseable"
    return 1
  }
  case "$count" in
    1) ;;
    0) refuse "$repo: no OCIRepository on $context has url $url"; return 1 ;;
    *) refuse "$repo: $count OCIRepositories on $context have url $url; the applied revision is ambiguous"; return 1 ;;
  esac
  revision="$(printf '%s' "$json" | jq -r --arg u "$url" \
    'first(.items[] | select(.spec.url == $u)) | .status.artifact.revision // ""' 2>/dev/null)" || {
    refuse "$repo: OCIRepository listing was not parseable"
    return 1
  }
  local tag="${revision%%@*}" digest="${revision#*@}"
  if [ "$tag" = "$revision" ] || ! plausible_tag "$tag" || ! is_digest "$digest"; then
    refuse "$repo: applied revision '$revision' is not <tag>@sha256:<digest>; the OCIRepository has not applied an artifact"
    return 1
  fi
  printf '%s\t%s\n' "$tag" "$digest"
}

# The git tag whose publish produced one registry tag. The shared workflows strip a leading
# `v` and spell `+` as `_`, so the translation is not invertible: BOTH `v1.2.3` and `1.2.3`
# would publish as `1.2.3`. Exactly one existing git tag may translate to the applied tag,
# or the signer cannot be attributed to one commit â the same refusal the report makes.
git_tag_for_registry_tag() {
  local repo="$1" registry_tag="$2" tags matches count
  tags="$(gh_retry api --paginate "repos/devantler-tech/${repo}/tags?per_page=100" --jq '.[].name')" || return 1
  matches="$(printf '%s\n' "$tags" | while IFS= read -r t; do
    [ -n "$t" ] || continue
    [ "$(registry_tag_for_git_tag "$t")" = "$registry_tag" ] && printf '%s\n' "$t"
  done || true)"  # the loop's status is the LAST tag's comparison, never a verdict
  count="$(printf '%s' "$matches" | grep -c . || true)"
  [ "$count" -eq 1 ] || return 1
  plausible_ref "$matches" || return 1
  printf '%s\n' "$matches"
}

# The shared-workflow revision the consumer's CD run referenced when it published exactly
# this tag, at the commit the tag resolves to TODAY. A run at another commit is a moved tag
# and is refused rather than attributed, exactly as `tag_was_published` refuses it.
signer_for_tag() {
  local repo="$1" workflow="$2" tag="$3" sha runs shas count
  sha="$(tag_commit "$repo" "$tag")" || return 1
  runs="$(gh_retry api --method GET "repos/devantler-tech/${repo}/actions/runs" \
    --raw-field "branch=${tag}" --raw-field event=push --raw-field per_page=100)" || return 1
  shas="$(printf '%s' "$runs" | jq -r --arg t "$tag" --arg s "$sha" --arg w "$workflow" '
    [.workflow_runs[]
     | select(.head_branch == $t and .path == ".github/workflows/cd.yaml"
              and .head_sha == $s and .conclusion == "success")
     | .referenced_workflows[]?
     | select(.path | startswith("devantler-tech/actions/.github/workflows/" + $w + ".yaml@"))
     | .sha] | unique | .[]' 2>/dev/null)" || return 1
  count="$(printf '%s' "$shas" | grep -c . || true)"
  [ "$count" -eq 1 ] || return 1
  is_sha "$shas" || return 1
  printf '%s\n' "$shas"
}

default_observer() {
  local repo="$1" workflow="$2" applied tag digest git_tag signer
  applied="$(applied_revision "$repo")" || return 1
  IFS=$'\t' read -r tag digest <<<"$applied"
  git_tag="$(git_tag_for_registry_tag "$repo" "$tag")" || {
    refuse "$repo: applied tag $tag does not translate to exactly one git tag on devantler-tech/$repo"
    return 1
  }
  signer="$(signer_for_tag "$repo" "$workflow" "$git_tag")" || {
    refuse "$repo: no single successful CD run published $git_tag at its current commit referencing $workflow.yaml"
    return 1
  }
  printf '%s\t%s\t%s\n' "$tag" "$digest" "$signer"
}

default_pin_resolver() {
  local repo="$1" workflow="$2" branch
  branch="$(gh_retry api "repos/devantler-tech/${repo}" --jq .default_branch)" || return 1
  plausible_ref "$branch" || return 1
  pin_at_ref "$repo" "$workflow" "$branch"
}

# Split "<a>\t<b>\t<c>" into three variables WITHOUT `read`, which collapses an empty middle
# field (the report documents the measured failure). Sets OBS_TAG OBS_DIGEST OBS_SIGNER and
# OBS_COUNT.
split_observation() {
  local answer="$1" field n=0
  OBS_TAG=''; OBS_DIGEST=''; OBS_SIGNER=''
  while IFS= read -r field; do
    case "$n" in
      0) OBS_TAG="$field" ;;
      1) OBS_DIGEST="$field" ;;
      2) OBS_SIGNER="$field" ;;
    esac
    n=$((n + 1))
  done < <(answer_fields "$answer")
  case "$answer" in
    *$'\n'*) n=0 ;;
  esac
  OBS_COUNT="$n"
}

main() {
  local observer="${APPROVED_REVISION_OBSERVER:-}" pin_resolver="${APPROVED_REVISION_PIN_RESOLVER:-}"
  local today="${APPROVED_REVISIONS_OBSERVED_ON:-$(date -u +%Y-%m-%d)}"
  is_date "$today" || { refuse "APPROVED_REVISIONS_OBSERVED_ON must be YYYY-MM-DD, got '$today'"; exit 1; }

  local consumers
  consumers="$("$REPORT" --list-consumers)" || { refuse 'consumer discovery failed; see the report above'; exit 1; }
  [ -n "$consumers" ] || { refuse 'consumer discovery returned nothing'; exit 1; }

  # The existing row for one consumer, "<tag>\t<digest>\t<signer>\t<pin>\t<date>" or
  # nothing, so an unchanged tuple keeps its date. A lookup over the file rather than an
  # associative array: this runs under macOS's bash 3.2 as well as CI's bash 5.
  prev_row() { # <consumer> <workflow>
    [ -f "$OUTPUT" ] || return 0
    awk -F'\t' -v c="$1" -v w="$2" '$1 == c && $2 == w { print $3 "\t" $4 "\t" $5 "\t" $6 "\t" $7; exit }' "$OUTPUT"
  }

  local repo workflow _version answer pin rows='' unresolved=0 examined=0 changed=0
  while IFS=$'\t' read -r repo workflow _version; do
    [ -n "$repo" ] || continue
    examined=$((examined + 1))
    if [ -n "$observer" ]; then
      answer="$("$observer" "$repo" "$workflow")" || answer=''
    else
      answer="$(default_observer "$repo" "$workflow")" || answer=''
    fi
    split_observation "$answer"
    if [ "$OBS_COUNT" -ne 3 ] || ! plausible_tag "$OBS_TAG" || ! is_digest "$OBS_DIGEST" || ! is_sha "$OBS_SIGNER"; then
      refuse "$repo ($workflow): applied revision not resolved to <tag>, <sha256:digest> and one signer SHA; refusing to write a set with one revision"
      unresolved=$((unresolved + 1))
      continue
    fi
    if [ -n "$pin_resolver" ]; then
      pin="$("$pin_resolver" "$repo" "$workflow")" || pin=''
    else
      pin="$(default_pin_resolver "$repo" "$workflow")" || pin=''
    fi
    case "$pin" in *$'\n'*) pin='' ;; esac
    if ! is_sha "${pin:-}"; then
      refuse "$repo ($workflow): default-branch pin not resolved to one commit SHA; refusing to write a set with one revision"
      unresolved=$((unresolved + 1))
      continue
    fi
    local key="$repo	$workflow" tuple="$OBS_TAG	$OBS_DIGEST	$OBS_SIGNER	$pin" observed prev prev_date
    prev="$(prev_row "$repo" "$workflow")"
    prev_date="${prev##*	}"
    if [ -n "$prev" ] && [ "${prev%	*}" = "$tuple" ] && is_date "$prev_date"; then
      observed="$prev_date"
    else
      observed="$today"
      changed=$((changed + 1))
    fi
    rows="${rows}${key}	${tuple}	${observed}"$'\n'
  done <<<"$consumers"

  if [ "$unresolved" -gt 0 ]; then
    refuse "$unresolved of $examined consumer(s) unresolved; no file written (an incomplete approved set reads like a complete one)"
    exit 1
  fi
  [ "$examined" -gt 0 ] || { refuse 'no consumers examined; no file written'; exit 1; }

  local tmp
  tmp="$(mktemp "${OUTPUT}.XXXXXX")"
  trap 'rm -f "$tmp"' EXIT
  {
    printf '%s\n' "$HEADER"
    printf '%s' "$rows" | LC_ALL=C sort
  } >"$tmp"
  # mktemp creates 0600; the committed data file is an ordinary 0644 file and must stay one.
  chmod 0644 "$tmp"
  mv "$tmp" "$OUTPUT"
  trap - EXIT
  printf '%d consumer(s) examined, %d row(s) re-stamped, written to %s\n' "$examined" "$changed" "$OUTPUT"
}

main "$@"
