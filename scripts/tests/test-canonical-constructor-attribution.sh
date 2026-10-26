#!/usr/bin/env bash
# A counted producer and a separately hashed graph must never borrow authority.
set -euo pipefail
attribution_repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
tree="$work/tree"
graph=k8s/bases/infrastructure/resource-graph-definitions/tenant/resource-graph-definition.yaml
tenant=k8s/providers/hetzner/apps/business-site/tenant.yaml
mkdir -p "$tree/$(dirname "$graph")" "$tree/$(dirname "$tenant")" "$tree/scripts"
cp "$attribution_repo_root/$graph" "$tree/$graph"
cp "$attribution_repo_root/$tenant" "$tree/$tenant"
jq '.bindings |= map(del(.previousBinding,.previousObservation)|.kind="published-bootstrap")' "$attribution_repo_root/scripts/canonical-tenant-source-bindings.json" >"$tree/scripts/canonical-tenant-source-bindings.json"
cp "$attribution_repo_root/scripts/publish-workflow-canonical-approvals.tsv" "$tree/scripts/"
awk -F '\t' '$1!="business-site"' "$attribution_repo_root/scripts/publish-workflow-approved-revisions.identities.tsv" >"$tree/scripts/publish-workflow-approved-revisions.identities.tsv"
discover() (
  trap - EXIT
  # shellcheck source=scripts/report-publish-workflow-signing-revisions.sh
  source "$attribution_repo_root/scripts/report-publish-workflow-signing-revisions.sh"
  discover_consumers "$tree" with-identity
)
discover >"$work/positive"
test "$(wc -l <"$work/positive" | tr -d ' ')" = 1
cp "$tree/$graph" "$work/graph.yaml"
# The named/hash-bound graph now declares another group. A different graph
# creates the actual Tenant kind using an unadmitted source constructor.
yq -i '.spec.schema.group="other.example"' "$tree/$graph"
hash="$(yq -o=json -I=0 '.spec' "$tree/$graph" | jq -cS . | shasum -a 256 | awk '{print $1}')"
jq --arg hash "$hash" '.bindings[0].constructorSpecSha256=$hash' "$tree/scripts/canonical-tenant-source-bindings.json" >"$work/binding.json"
mv "$work/binding.json" "$tree/scripts/canonical-tenant-source-bindings.json"
yq '.metadata.name="unadmitted-tenant.kro.run" | (.spec.resources[] | select(.id=="ociRepository") | .template.spec.url)="oci://ghcr.io/unadmitted/manifests"' "$work/graph.yaml" >"$tree/$(dirname "$graph")/unadmitted.yaml"
accepted=0
if discover >"$work/raw" 2>"$work/raw-error"; then
  printf 'raw discovery attributed a Tenant to an unrelated-group graph\n' >&2
  accepted=$((accepted+1))
fi
test ! -s "$work/raw" || test "$accepted" -gt 0
# Exercise the rendered reader independently with its complete parser envelope.
yq -N -o=json -I=0 '{"file":filename,"document":explode(.)}' "$tree/$graph" "$tree/$(dirname "$graph")/unadmitted.yaml" "$tree/$tenant" >"$work/inventory.jsonl"
(
  trap - EXIT
  # shellcheck source=scripts/canonical-tenant-source.lib.sh
  source "$attribution_repo_root/scripts/canonical-tenant-source.lib.sh"
  canonical_typed_source_hits "$tree" "$work/inventory.jsonl" >"$work/hits.json"
  canonical_tenant_inventory_projection "$tree" "$work/inventory.jsonl" "$work/hits.json" rendered
) >"$work/rendered" 2>"$work/rendered-error" && accepted=$((accepted+1))
if [ "$accepted" -ne 0 ]; then
  printf 'constructor attribution accepted %s invalid raw/rendered projections\n' "$accepted" >&2
  exit 1
fi
test ! -s "$work/rendered"
printf 'Canonical constructor attribution: the hashed graph must produce the admitted full GVK in both readers.\n'
