#!/usr/bin/env bash
# guard-publish-workflow-approved-revisions.sh — assert that each per-consumer cosign matcher
# for the shared devantler-tech/actions publish workflows names exactly its generated approved
# revision set.
#
# WHY A SECOND GUARD
# guard-shared-publish-workflow-pin.sh proves every matcher pins a FIXED revision — the pattern
# text `[0-9a-f]{40}`, one concrete commit, or an alternation of two. It judges ref SHAPE and
# says nothing about set MEMBERSHIP: a matcher narrowed to a revision nobody approved, or a
# regeneration that moved the set while a matcher stayed behind, passes it. This guard reads
# scripts/publish-workflow-approved-revisions.tsv (written by
# generate-publish-workflow-approved-revisions.sh, #3550). Every consumer, application tenants
# included, must name its approved set and nothing else: the applied signer, the default-branch
# pin and, for publish-app consumers, the latest released actions revision (#3917).
#
# ENFORCEMENT
# CI and scheduled regeneration set APPROVED_REVISIONS_ENFORCE=1, which refuses the pattern
# form for every consumer.
#
# SCOPE — GENERIC SUBJECTS ARE EXCLUDED BY NAME
# Three subjects name the shared workflows without belonging to one consumer: the Kyverno
# app-image policy, the tenant ResourceGraphDefinition template, and the Talos first-party
# image-verification rule. Each verifies artifacts from MANY consumers, so no single generated
# pair could be correct for it; they stay on the pattern form and are listed in
# GENERIC_SUBJECT_FILES in the shared library so that the boundary is recorded beside the check. A shared-
# workflow subject that is neither attributed to a registered consumer nor on that list fails
# the run: an unattributed subject is one whose revision nobody is checking.
#
# FAIL CLOSED. A missing, malformed, or incomplete approved set, a consumer whose manifest
# cannot be found or read the way Flux reads it, or a subject this guard cannot attribute all
# exit 1 — a guard that skips what it cannot read reports a clean tree while checking nothing.
#
# SEAMS (tests substitute these; production leaves them unset)
#   APPROVED_REVISIONS_FILE      the generated set (default scripts/publish-workflow-approved-revisions.tsv)
#   PUBLISH_CONSUMER_ROOT        scan root for consumer manifests (default: the repository root)
#   APPROVED_REVISIONS_ENFORCE   `1` refuses the pattern form (default: off)
set -euo pipefail

GUARD_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/publish-workflow-approved-revisions.lib.sh
source "$GUARD_DIR/publish-workflow-approved-revisions.lib.sh"

# ── 3. Each registered consumer: its matcher equals its declared boundary ────────────────
for consumer in "${EXPECTED_CONSUMERS[@]}"; do
  record="$(lookup "$observed" "$consumer")"
  [ -n "$record" ] || refuse "no OCIRepository under $SCAN_ROOT is attributed to registered consumer $consumer; the scan, not the tree, is the likely cause"
  IFS=$'\t' read -r file workflow ref <<<"$record"
  IFS=$'\t' read -r set_workflow signer pin candidate <<<"$(lookup "$approved" "$consumer")"
  [ "$workflow" = "$set_workflow" ] || refuse "$consumer: $file names $workflow but the approved set names $set_workflow"

  accepted="$(approved_ref "$signer" "$pin" "$candidate") (in any order)"

  form=""
  if [ "$ref" = "$PATTERN_REF" ]; then
    if [ "$ENFORCE" = "1" ]; then
      refuse "$consumer: $file still carries the pattern form '@$PATTERN_REF' while APPROVED_REVISIONS_ENFORCE=1; narrow it to @$accepted"
    fi
    form="pattern (accepted while the switch is off)"
  elif ref_matches_approved "$ref" "$signer" "$pin" "$candidate"; then
    form="set"
  else
    refuse "$consumer: $file pins '@$ref', which is not the generated set — expected @$accepted (or the pattern form while the switch is off); run scripts/write-publish-workflow-matchers.sh"
  fi
  printf 'ok %s %s %s form=%s\n' "$consumer" "$workflow" "$file" "$form"
done

printf 'guard-publish-workflow-approved-revisions: %d consumer matcher(s) agree with %s (enforce=%s)\n' \
  "${#EXPECTED_CONSUMERS[@]}" "${APPROVED_SET#"$REPO_ROOT"/}" "$ENFORCE"
