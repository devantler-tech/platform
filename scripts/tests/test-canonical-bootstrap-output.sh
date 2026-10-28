#!/usr/bin/env bash
# Exercise the real admission CLI against output races using private fixtures.
set -euo pipefail
test_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
test_work="$(mktemp -d)"
trap 'rm -rf "$test_work"' EXIT
fixture="$test_work/root"
graph=k8s/bases/infrastructure/resource-graph-definitions/tenant/resource-graph-definition.yaml
tenant=k8s/providers/hetzner/apps/business-site/tenant.yaml
mkdir -p "$fixture/$(dirname "$graph")" "$fixture/$(dirname "$tenant")" "$fixture/scripts" "$test_work/redirect"
cp "$test_root/$graph" "$fixture/$graph"
cp "$test_root/$tenant" "$fixture/$tenant"
cp "$test_root/scripts/publish-workflow-canonical-approvals.tsv" "$fixture/scripts/publish-workflow-canonical-approvals.tsv"
printf 'consumer\tworkflow\tapplied_tag\tapplied_digest\tapplied_publisher_identity\tmain_publisher_identity\tobserved_on\n' >"$fixture/scripts/publish-workflow-approved-revisions.identities.tsv"
jq '.bindings[0] | {schemaVersion:1,kind:"manifest-publication-evidence",consumer,deploymentAuthorized:false,
 gitTag:.publication.gitTag,sourceSHA:.publication.sourceSHA,digest:.sourceIdentity.digest,
 publisherIdentity:.publication.publisherIdentity,callerWorkflow:.publication.callerWorkflow,event:.publication.event,
 workflowRunId:.publication.workflowRunId,workflowRunAttempt:.publication.workflowRunAttempt}' \
 "$test_root/scripts/canonical-tenant-source-bindings.json" >"$test_work/receipt.json"
# shellcheck source=scripts/bootstrap-tenant-source.sh
source "$test_root/scripts/bootstrap-tenant-source.sh"
business_site_manifest_evidence() { cat "$test_work/receipt.json"; }
kubectl() {
  case "${race:-}" in
    directory) mkdir "$fixture_output" ;;
    symlink) ln -s "$test_work/redirect" "$fixture_output" ;;
    conflict) printf '{"unreviewed":true}\n' >"$fixture_output" ;;
    observed) jq '.bindings[0].kind="published-binding"' "$fixture_output" >"$fixture_output.changed"; mv "$fixture_output.changed" "$fixture_output" ;;
  esac
  printf '{"apiVersion":"v1","kind":"List","metadata":{},"items":[]}\n'
}
fixture_output="$fixture/scripts/canonical-tenant-source-bindings.json"
run_admission() {
  PUBLISH_KUBE_CONTEXT=fixture CANONICAL_APPROVALS_FILE="$fixture/scripts/publish-workflow-canonical-approvals.tsv" \
    bootstrap_tenant_source --admit-canonical-source --source-root "$fixture" \
    --tag "$(jq -r .gitTag "$test_work/receipt.json")" --source-sha "$(jq -r .sourceSHA "$test_work/receipt.json")" \
    --digest "$(jq -r .digest "$test_work/receipt.json")"
}
printf '{"schemaVersion":1,"bindings":[]}\n' >"$fixture_output"
if run_admission >"$test_work/result" 2>&1; then echo 'empty registry reported an unchanged admission' >&2; exit 1; fi
test "$(jq '.bindings|length' "$fixture_output")" = 0
rm "$fixture_output"
for race in directory symlink conflict; do
  if run_admission >"$test_work/result" 2>&1; then echo "output $race race reported success" >&2; exit 1; fi
  case "$race" in
    directory) test -z "$(find "$fixture_output" -mindepth 1 -print)"; rmdir "$fixture_output" ;;
    symlink) test -L "$fixture_output"; test -z "$(find "$test_work/redirect" -mindepth 1 -print)"; rm "$fixture_output" ;;
    conflict) grep -qxF '{"unreviewed":true}' "$fixture_output"; rm "$fixture_output" ;;
  esac
done
race=''
run_admission >"$test_work/result"
test "$(jq '.bindings|length' "$fixture_output")" = 1
cp "$fixture_output" "$test_work/first.json"
run_admission >"$test_work/result"
cmp "$test_work/first.json" "$fixture_output"
race=observed
if run_admission >"$test_work/result" 2>&1; then echo 'concurrent observed record was overwritten' >&2; exit 1; fi
test "$(jq -r '.bindings[0].kind' "$fixture_output")" = published-binding
printf 'Canonical bootstrap output: empty registry and four races refused; exclusive creation/idempotence passed.\n'
