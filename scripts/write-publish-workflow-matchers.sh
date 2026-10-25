#!/usr/bin/env bash
# Derive every registered consumer's cosign subject from the generated approved revision set.
# Validate every input and staged result before replacing any consumer manifest.
# Generic multi-consumer subjects remain unchanged. Unchanged output preserves bytes.
set -euo pipefail

WRITER_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/publish-workflow-approved-revisions.lib.sh
source "$WRITER_DIR/publish-workflow-approved-revisions.lib.sh"

readonly FIXED_REF_RE='^([0-9a-f]{40}|\([0-9a-f]{40}(\|[0-9a-f]{40}){0,2}\))$'
STAGED="$(mktemp -d "${TMPDIR:-/tmp}/write-publish-workflow-matchers.XXXXXX")"
trap 'rm -rf "$STAGED"' EXIT

# Include the generic files so the ordinary guard validates the complete scope.
# The shared discovery has already rejected every unattributed source-tree subject.
for generic in "${GENERIC_SUBJECT_FILES[@]}"; do
  mkdir -p "$STAGED/$(dirname "$generic")"
  cp "$SCAN_ROOT/$generic" "$STAGED/$generic"
done

for consumer in "${EXPECTED_CONSUMERS[@]}"; do
  record="$(lookup "$observed" "$consumer")"
  [ -n "$record" ] || refuse "no OCIRepository is attributed to registered consumer $consumer"
  IFS=$'\t' read -r file workflow ref _canonical_ref <<<"$record"
  IFS=$'\t' read -r set_workflow signer pin candidate <<<"$(lookup "$approved" "$consumer")"
  [ "$workflow" = "$set_workflow" ] || refuse "$consumer: $file names $workflow but the approved set names $set_workflow"
  [ "$ref" = "$PATTERN_REF" ] || [[ "$ref" =~ $FIXED_REF_RE ]] ||
    refuse "$consumer: unrecognised existing revision '$ref'; review the source identity before rewriting"
  if [ -L "$SCAN_ROOT/$file" ] || [ ! -f "$SCAN_ROOT/$file" ] || [ ! -w "$SCAN_ROOT/$file" ]; then
    refuse "$consumer: source manifest must be a writable regular file, not a symlink"
  fi

  desired_ref="$(approved_ref "$signer" "$pin" "$candidate")"
  subject="$(approved_subject "$workflow" "$desired_ref" "$(lookup "$canonical" "$consumer")")"
  mkdir -p "$STAGED/$(dirname "$file")"
  # Select the actual OCIRepository document; unrelated documents remain unchanged.
  # shellcheck disable=SC2016
  MATCHER_SUBJECT="$subject" yq eval \
    '(. | select(.kind == "OCIRepository") | .spec.verify.matchOIDCIdentity[0].subject) = strenv(MATCHER_SUBJECT)' \
    "$SCAN_ROOT/$file" >"$STAGED/$file"
done

# The canonical-only instance is already pinned. Stage it byte-for-byte with
# its complete constructor/binding context; never rewrite the shared graph.
while IFS= read -r canonical_file; do
  [ -n "$canonical_file" ] || continue
  mkdir -p "$STAGED/$(dirname "$canonical_file")"
  cp "$SCAN_ROOT/$canonical_file" "$STAGED/$canonical_file"
done < <(printf '%s\n' "$canonical_only" | jq -r '.bindings[].sourcePath')
mkdir -p "$STAGED/scripts"
printf '%s\n' "$canonical_only" >"$STAGED/scripts/canonical-tenant-source-bindings.json"
if [ "$(printf '%s\n' "$canonical_only" | jq -r '.bindings|length')" -gt 0 ]; then
  cp "${APPROVED_IDENTITIES_FILE:-${APPROVED_SET%.tsv}.identities.tsv}" "$STAGED/scripts/canonical-identities.tsv"
else
  printf 'consumer\tworkflow\tapplied_tag\tapplied_digest\tapplied_publisher_identity\tmain_publisher_identity\tobserved_on\n' >"$STAGED/scripts/canonical-identities.tsv"
fi

APPROVED_REVISIONS_FILE="$APPROVED_SET" CANONICAL_APPROVALS_FILE="$CANONICAL_SET" PUBLISH_CONSUMER_ROOT="$STAGED" \
  CANONICAL_TENANT_BINDINGS_FILE="$STAGED/scripts/canonical-tenant-source-bindings.json" \
  APPROVED_IDENTITIES_FILE="$STAGED/scripts/canonical-identities.tsv" \
  APPROVED_REVISIONS_ENFORCE=1 bash "$WRITER_DIR/guard-publish-workflow-approved-revisions.sh"

changed=0
for consumer in "${EXPECTED_CONSUMERS[@]}"; do
  IFS=$'\t' read -r file _workflow _ref _canonical_ref <<<"$(lookup "$observed" "$consumer")"
  if ! cmp -s "$STAGED/$file" "$SCAN_ROOT/$file"; then
    cat "$STAGED/$file" >"$SCAN_ROOT/$file"
    changed=$((changed + 1))
    printf 'updated %s\n' "$file"
  fi
done
printf 'write-publish-workflow-matchers: %d consumer matcher(s) changed\n' "$changed"
