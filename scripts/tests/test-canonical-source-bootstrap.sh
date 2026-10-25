#!/usr/bin/env bash
# Exercise the real raw discovery path with one fully qualified source-only binding.
set -euo pipefail
test_repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
tree="$work/tree"
graph_path=k8s/bases/infrastructure/resource-graph-definitions/tenant/resource-graph-definition.yaml
source_path=k8s/providers/hetzner/apps/business-site/tenant.yaml
mkdir -p "$tree/$(dirname "$graph_path")" "$tree/$(dirname "$source_path")" "$tree/scripts"
cp "$test_repo_root/$graph_path" "$tree/$graph_path"
cat >"$tree/$source_path" <<'YAML'
apiVersion: kro.run/v1alpha1
kind: Tenant
metadata:
  name: business-site
  namespace: flux-system
spec:
  name: business-site
  externalDns: false
  sops: false
  webApp: false
  source:
    digest: sha256:fa22e5796a39872feb8c75b5d56e2c6d68ff289230e2c5762f0bbfa0b30d6744
    publisherRevision: 467f07582436a26b62cf4aa756423512f85ba0c0
    reconcileApplication: false
YAML
graph_hash="$(yq -o=json -I=0 '.spec' "$tree/$graph_path" | jq -cS . | shasum -a 256 | awk '{print $1}')"
jq -n --arg source "$source_path" --arg graph "$graph_hash" '{schemaVersion:1,bindings:[{
 consumer:"business-site",consumerRepository:"devantler-tech/business-site",workflow:"publish-app",
 kind:"published-bootstrap",sourcePath:$source,constructorSpecSha256:$graph,
 tenantIdentity:{apiVersion:"kro.run/v1alpha1",kind:"Tenant",namespace:"flux-system",name:"business-site"},
 sourceIdentity:{apiVersion:"source.toolkit.fluxcd.io/v1",kind:"OCIRepository",namespace:"business-site",name:"business-site",
 artifactURL:"oci://ghcr.io/devantler-tech/business-site/manifests",
 digest:"sha256:fa22e5796a39872feb8c75b5d56e2c6d68ff289230e2c5762f0bbfa0b30d6744"},
 publication:{gitTag:"v1.0.3",sourceSHA:"498248e86e49024b72f3249b518e4dd1cef0104b",
 publisherIdentity:"devantler-tech/.github/.github/workflows/publish-app.yaml@467f07582436a26b62cf4aa756423512f85ba0c0",
 callerWorkflow:".github/workflows/publish-runtime.yaml",event:"release",workflowRunId:39921694859,
 workflowRunAttempt:1,verifiedOn:"2026-10-25"}
}]}' >"$tree/scripts/canonical-tenant-source-bindings.json"
cp "$tree/scripts/canonical-tenant-source-bindings.json" "$work/good.json"
cp "$tree/$source_path" "$work/good-tenant.yaml"
printf 'consumer\tworkflow\trepository\tcommit\nbusiness-site\tpublish-app\tdevantler-tech/.github\t467f07582436a26b62cf4aa756423512f85ba0c0\n' >"$tree/scripts/publish-workflow-canonical-approvals.tsv"
printf 'consumer\tworkflow\tapplied_tag\tapplied_digest\tapplied_publisher_identity\tmain_publisher_identity\tobserved_on\n' >"$tree/scripts/publish-workflow-approved-revisions.identities.tsv"
# Discovery is the public caller: a parser-only success cannot establish adoption.
discover() (
  trap - EXIT
  # shellcheck source=scripts/report-publish-workflow-signing-revisions.sh
  source "$test_repo_root/scripts/report-publish-workflow-signing-revisions.sh"
  discover_consumers "$tree" with-identity
)
if ! discover >"$work/rows" 2>"$work/error"; then
  echo 'qualified source-only Tenant was omitted or refused by actual discovery' >&2
  cat "$work/error" >&2
  exit 1
fi
test "$(wc -l <"$work/rows" | tr -d ' ')" = 1
awk -F '\t' '$1=="business-site" && $2=="publish-app" &&
 $3=="digest:sha256:fa22e5796a39872feb8c75b5d56e2c6d68ff289230e2c5762f0bbfa0b30d6744" &&
 $4=="business-site/manifests" && $6=="business-site" && $7=="business-site" &&
 $8=="oci://ghcr.io/devantler-tech/business-site/manifests" {ok=1} END {exit !ok}' "$work/rows"
controls=0
for mutation in \
 '.bindings[0].consumer="different"' \
 '.bindings[0].consumerRepository="other/business-site"' \
 '.bindings[0].workflow="publish-manifests"' \
 '.bindings[0].kind="unknown"' \
 '.bindings[0].extra=true' \
 '.bindings += .bindings' \
 '.bindings[0].sourcePath="../outside.yaml"' \
 '.bindings[0].constructorSpecSha256=("0"*64)' \
 '.bindings[0].tenantIdentity.kind="OtherTenant"' \
 '.bindings[0].tenantIdentity.namespace="other"' \
 '.bindings[0].sourceIdentity.name="other"' \
 '.bindings[0].sourceIdentity.artifactURL="oci://ghcr.io/other/business-site/manifests"' \
 '.bindings[0].sourceIdentity.digest="sha256:invalid"' \
 '.bindings[0].publication.gitTag="not-a-tag"' \
 '.bindings[0].publication.sourceSHA="partial"' \
 '.bindings[0].publication.publisherIdentity="devantler-tech/actions/.github/workflows/publish-app.yaml@467f07582436a26b62cf4aa756423512f85ba0c0"' \
 '.bindings[0].publication.callerWorkflow=".github/workflows/cd.yaml"' \
 '.bindings[0].publication.event="push"' \
 '.bindings[0].publication.workflowRunId=0' \
 '.bindings[0].publication.workflowRunAttempt="1"'; do
  jq "$mutation" "$work/good.json" >"$tree/scripts/canonical-tenant-source-bindings.json"
  if discover >"$work/rows" 2>"$work/error"; then
    echo "invalid canonical binding admitted: $mutation" >&2; exit 1
  fi
  test ! -s "$work/rows"
  controls=$((controls+1))
done
cp "$work/good.json" "$tree/scripts/canonical-tenant-source-bindings.json"
for mutation in '.spec.source.reconcileApplication=true' '.spec.webApp=true' '.spec.externalDns=true' '.spec.sops=true' '.spec.extra="unreviewed"'; do
  yq "$mutation" "$work/good-tenant.yaml" >"$tree/$source_path"
  if discover >"$work/rows" 2>"$work/error"; then
    echo "changed source-only instance admitted: $mutation" >&2; exit 1
  fi
  test ! -s "$work/rows"
  controls=$((controls+1))
done
cp "$work/good-tenant.yaml" "$tree/$source_path"
cp "$tree/$source_path" "$tree/$(dirname "$source_path")/clone.yaml"
if discover >"$work/rows" 2>"$work/error"; then echo 'cloned Tenant admitted' >&2; exit 1; fi
test ! -s "$work/rows"
printf 'Canonical source bootstrap: positive discovery and %d binding/instance refusal controls passed.\n' "$((controls+1))"

# The production guard must independently attribute the real rendered graph.
# A passing raw parser cannot substitute for this actual Kustomize comparison.
production="$work/production"
mkdir -p "$production/scripts"
cp -R "$test_repo_root/k8s" "$production/k8s"
cp "$test_repo_root/ksail.prod.yaml" "$production/ksail.prod.yaml"
mkdir -p "$production/$(dirname "$source_path")"
cp "$work/good-tenant.yaml" "$production/$source_path"
printf 'apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - tenant.yaml\n' >"$production/$(dirname "$source_path")/kustomization.yaml"
yq -i '.resources = ((.resources + ["business-site/"])|unique)' "$production/k8s/providers/hetzner/apps/kustomization.yaml"
cp "$work/good.json" "$production/scripts/canonical-tenant-source-bindings.json"
cp "$tree/scripts/publish-workflow-approved-revisions.identities.tsv" "$production/scripts/publish-workflow-approved-revisions.identities.tsv"
awk -F '\t' '$1!="business-site"' "$test_repo_root/scripts/publish-workflow-canonical-approvals.tsv" >"$production/scripts/publish-workflow-canonical-approvals.tsv"
printf 'business-site\tpublish-app\tdevantler-tech/.github\t467f07582436a26b62cf4aa756423512f85ba0c0\n' >>"$production/scripts/publish-workflow-canonical-approvals.tsv"
if ! bash "$test_repo_root/scripts/guard-consumer-discovery-conservation.sh" "$production" >"$work/conservation" 2>"$work/conservation-error"; then
  echo 'qualified source-only registration failed actual production render conservation' >&2
  cat "$work/conservation-error" >&2
  exit 1
fi
grep -q '6 consumer(s)' "$work/conservation"
printf 'Canonical source bootstrap: actual six-consumer production render conserved.\n'
for table in publish-workflow-approved-revisions.tsv publish-workflow-approved-revisions.identities.tsv publish-workflow-bootstrap-publications.json; do
  cp "$test_repo_root/scripts/$table" "$production/scripts/$table"
done
awk -F '\t' '$1!="business-site"' "$production/scripts/publish-workflow-approved-revisions.identities.tsv" >"$work/legacy-identities.tsv"
mv "$work/legacy-identities.tsv" "$production/scripts/publish-workflow-approved-revisions.identities.tsv"
mkdir -p "$production/talos/cluster"
cp "$test_repo_root/talos/cluster/verify-first-party-images.yaml" "$production/talos/cluster/verify-first-party-images.yaml"
cp "$production/$source_path" "$work/tenant-before-writer.yaml"
cp "$production/$graph_path" "$work/constructor-before-writer.yaml"
APPROVED_REVISIONS_FILE="$production/scripts/publish-workflow-approved-revisions.tsv" \
  CANONICAL_APPROVALS_FILE="$production/scripts/publish-workflow-canonical-approvals.tsv" \
  PUBLISH_BOOTSTRAP_FILE="$production/scripts/publish-workflow-bootstrap-publications.json" \
  PUBLISH_CONSUMER_ROOT="$production" APPROVED_REVISIONS_ENFORCE=1 \
  bash "$test_repo_root/scripts/guard-publish-workflow-approved-revisions.sh" >"$work/approval-guard"
grep -q 'business-site.*canonical-source-only' "$work/approval-guard"
APPROVED_REVISIONS_FILE="$production/scripts/publish-workflow-approved-revisions.tsv" \
  CANONICAL_APPROVALS_FILE="$production/scripts/publish-workflow-canonical-approvals.tsv" \
  PUBLISH_BOOTSTRAP_FILE="$production/scripts/publish-workflow-bootstrap-publications.json" \
  PUBLISH_CONSUMER_ROOT="$production" APPROVED_REVISIONS_ENFORCE=1 \
  bash "$test_repo_root/scripts/write-publish-workflow-matchers.sh" >"$work/approval-writer"
cmp "$work/tenant-before-writer.yaml" "$production/$source_path"
cmp "$work/constructor-before-writer.yaml" "$production/$graph_path"
test "$(awk -F '\t' '$1=="business-site" {n++} END {print n+0}' "$production/scripts/publish-workflow-approved-revisions.tsv")" = 0
printf 'Canonical source bootstrap: approval and staged writer preserve the pinned Tenant without a legacy row.\n'

# Test the public generator's first-publication branch, not a replacement parser.
export CANONICAL_FIXTURE_ROOT="$production" CANONICAL_FIXTURE_REPO="$test_repo_root" CANONICAL_FIXTURE_WORK="$work"
printf '{"apiVersion":"v1","kind":"List","metadata":{},"items":[]}\n' >"$work/source-inventory.json"
cat >"$work/generate-fixture" <<'BASH'
#!/usr/bin/env bash
set -euo pipefail
export PUBLISH_CONSUMER_ROOT="$CANONICAL_FIXTURE_ROOT"
export APPROVED_REVISIONS_FILE="$CANONICAL_FIXTURE_ROOT/scripts/publish-workflow-approved-revisions.tsv"
export CANONICAL_APPROVALS_FILE="$CANONICAL_FIXTURE_ROOT/scripts/publish-workflow-canonical-approvals.tsv"
export PUBLISH_BOOTSTRAP_FILE="$CANONICAL_FIXTURE_ROOT/scripts/publish-workflow-bootstrap-publications.json"
export APPROVED_REVISIONS_ENFORCE=1 PUBLISH_KUBE_CONTEXT=fixture
source "$CANONICAL_FIXTURE_REPO/scripts/generate-publish-workflow-approved-revisions.sh"
# Provider adapters only: the production generator and validation remain real.
default_observer() {
  if [ "$1" = data-product-controller ]; then
    jq -r '.publications[]|select(.consumer=="data-product-controller")|[(.gitTag|ltrimstr("v")),.digest,.signerSHA]|@tsv' "$PUBLISH_BOOTSTRAP_FILE"
  else awk -F '\t' -v c="$1" '$1==c {print $3 "\t" $4 "\t" $5}' "$APPROVED_REVISIONS_FILE"; fi
}
default_pin_resolver() {
  if [ "$1" = business-site ]; then jq -r '.bindings[0].publication.publisherIdentity' "$CANONICAL_FIXTURE_ROOT/scripts/canonical-tenant-source-bindings.json"
  elif [ "$1" = data-product-controller ]; then jq -r '.publications[]|select(.consumer=="data-product-controller")|.mainPinSHA' "$PUBLISH_BOOTSTRAP_FILE"
  else awk -F '\t' -v c="$1" '$1==c {print $6}' "$APPROVED_REVISIONS_FILE"; fi
}
canonical_publication_verify() { [ "${CANONICAL_VERIFY_FAILURE:-0}" = 0 ]; }
kubectl() { [ "${CANONICAL_INVENTORY_FAILURE:-0}" = 0 ] && cat "$CANONICAL_FIXTURE_WORK/source-inventory.json"; }
main
BASH
chmod 755 "$work/generate-fixture"
cp "$production/scripts/publish-workflow-approved-revisions.tsv" "$work/legacy-before.tsv"
"$work/generate-fixture" >"$work/generation"
cmp "$work/legacy-before.tsv" "$production/scripts/publish-workflow-approved-revisions.tsv"
test "$(jq -r '.bindings[0].kind' "$production/scripts/canonical-tenant-source-bindings.json")" = published-bootstrap
test "$(awk -F '\t' '$1=="business-site" {n++} END {print n+0}' "$production/scripts/publish-workflow-approved-revisions.identities.tsv")" = 0
printf 'Canonical source bootstrap: absent source retains verified bootstrap without invented observations.\n'

# The supported admission CLI regenerates this record from independently
# verified publication evidence; its provider adapters are fixture-only.
jq '.bindings[0] | {schemaVersion:1,kind:"manifest-publication-evidence",consumer, deploymentAuthorized:false,
 gitTag:.publication.gitTag,sourceSHA:.publication.sourceSHA,digest:.sourceIdentity.digest,
 publisherIdentity:.publication.publisherIdentity,callerWorkflow:.publication.callerWorkflow,event:.publication.event,
 workflowRunId:.publication.workflowRunId,workflowRunAttempt:.publication.workflowRunAttempt}' "$work/good.json" >"$work/publication-receipt.json"
cat >"$work/bootstrap-fixture" <<'BASH'
#!/usr/bin/env bash
set -euo pipefail
export PUBLISH_CONSUMER_ROOT="$CANONICAL_FIXTURE_ROOT" PUBLISH_KUBE_CONTEXT=fixture
export CANONICAL_APPROVALS_FILE="$CANONICAL_FIXTURE_ROOT/scripts/publish-workflow-canonical-approvals.tsv"
source "$CANONICAL_FIXTURE_REPO/scripts/bootstrap-tenant-source.sh"
business_site_manifest_evidence() { [ "${CANONICAL_VERIFY_FAILURE:-0}" = 0 ] && cat "$CANONICAL_FIXTURE_WORK/publication-receipt.json"; }
kubectl() {
  [ "${CANONICAL_INVENTORY_FAILURE:-0}" = 0 ] || return 1
  local destination="$CANONICAL_FIXTURE_ROOT/scripts/canonical-tenant-source-bindings.json"
  case "${CANONICAL_BOOT_RACE:-}" in
    directory) mkdir "$destination" ;;
    symlink) mkdir -p "$CANONICAL_FIXTURE_WORK/redirect"; ln -s "$CANONICAL_FIXTURE_WORK/redirect" "$destination" ;;
    conflict) printf '{"unreviewed":true}\n' >"$destination" ;;
    observed) jq '.bindings[0].kind="published-binding"' "$destination" >"$destination.changed"; mv "$destination.changed" "$destination" ;;
  esac
  cat "$CANONICAL_FIXTURE_WORK/source-inventory.json"
}
bootstrap_tenant_source --admit-canonical-source --source-root "$CANONICAL_FIXTURE_ROOT" --tag v1.0.3 \
 --source-sha 498248e86e49024b72f3249b518e4dd1cef0104b \
 --digest sha256:fa22e5796a39872feb8c75b5d56e2c6d68ff289230e2c5762f0bbfa0b30d6744
BASH
chmod 755 "$work/bootstrap-fixture"
rm "$production/scripts/canonical-tenant-source-bindings.json"
for race in directory symlink conflict; do
  if env CANONICAL_BOOT_RACE="$race" "$work/bootstrap-fixture" >"$work/error" 2>&1; then echo "bootstrap accepted destination $race race" >&2; exit 1; fi
  case "$race" in
    directory) test -z "$(find "$production/scripts/canonical-tenant-source-bindings.json" -mindepth 1 -print)"; rmdir "$production/scripts/canonical-tenant-source-bindings.json" ;;
    symlink) test -L "$production/scripts/canonical-tenant-source-bindings.json"; test -z "$(find "$work/redirect" -mindepth 1 -print)"; rm "$production/scripts/canonical-tenant-source-bindings.json" ;;
    conflict) grep -qxF '{"unreviewed":true}' "$production/scripts/canonical-tenant-source-bindings.json"; rm "$production/scripts/canonical-tenant-source-bindings.json" ;;
  esac
done
"$work/bootstrap-fixture" >"$work/bootstrap-generation"
cp "$production/scripts/canonical-tenant-source-bindings.json" "$work/bootstrap-generated.json"
if env CANONICAL_BOOT_RACE=observed "$work/bootstrap-fixture" >"$work/error" 2>&1; then echo 'bootstrap overwrote concurrent observed binding' >&2; exit 1; fi
test "$(jq -r '.bindings[0].kind' "$production/scripts/canonical-tenant-source-bindings.json")" = published-binding
cp "$work/bootstrap-generated.json" "$production/scripts/canonical-tenant-source-bindings.json"
"$work/bootstrap-fixture" >"$work/bootstrap-generation"
cmp "$work/bootstrap-generated.json" "$production/scripts/canonical-tenant-source-bindings.json"
for failure in CANONICAL_VERIFY_FAILURE CANONICAL_INVENTORY_FAILURE; do
  if env "$failure=1" "$work/bootstrap-fixture" >"$work/error" 2>&1; then echo "bootstrap admitted failed $failure" >&2; exit 1; fi
  cmp "$work/bootstrap-generated.json" "$production/scripts/canonical-tenant-source-bindings.json"
done

jq '.bindings[0] as $b | {apiVersion:"v1",kind:"List",metadata:{},items:[{
 apiVersion:$b.sourceIdentity.apiVersion,kind:$b.sourceIdentity.kind,
 metadata:{namespace:$b.sourceIdentity.namespace,name:$b.sourceIdentity.name,generation:2},
 spec:{url:$b.sourceIdentity.artifactURL,ref:{digest:$b.sourceIdentity.digest}},
 status:{observedGeneration:2,artifact:{revision:$b.sourceIdentity.digest},conditions:[
 {type:"Ready",status:"True",observedGeneration:2},{type:"SourceVerified",status:"True",observedGeneration:2}]}
}]}' "$work/good.json" >"$work/observed-inventory.json"
jq '.items[0].status={}' "$work/observed-inventory.json" >"$work/source-inventory.json"
"$work/generate-fixture" >"$work/generation"
test "$(jq -r '.bindings[0].kind' "$production/scripts/canonical-tenant-source-bindings.json")" = published-bootstrap
cp "$work/observed-inventory.json" "$work/source-inventory.json"
if "$work/bootstrap-fixture" >"$work/error" 2>&1; then echo 'bootstrap replaced real observation' >&2; exit 1; fi
"$work/generate-fixture" >"$work/generation"
test "$(jq -r '.bindings[0].kind' "$production/scripts/canonical-tenant-source-bindings.json")" = published-binding
test "$(awk -F '\t' '$1=="business-site" && NF==7 {n++} END {print n+0}' "$production/scripts/publish-workflow-approved-revisions.identities.tsv")" = 1
cmp "$work/legacy-before.tsv" "$production/scripts/publish-workflow-approved-revisions.tsv"
cp "$production/scripts/publish-workflow-approved-revisions.identities.tsv" "$work/observed-identities.tsv"
cp "$production/scripts/canonical-tenant-source-bindings.json" "$work/observed-binding.json"
APPROVED_REVISIONS_OBSERVED_ON=2030-01-01 "$work/generate-fixture" >"$work/generation"
cmp "$work/observed-identities.tsv" "$production/scripts/publish-workflow-approved-revisions.identities.tsv"
cmp "$work/observed-binding.json" "$production/scripts/canonical-tenant-source-bindings.json"
for mutation in '.items=[]' '.metadata.continue="next"' '.items += .items' \
 '.items=[{}]' '.metadata.continue=123' '.items[0].status.conditions="malformed"' \
 '.items[0].status={}' '.items[0].status.observedGeneration=1' \
 '.items[0].status.conditions[1].status="False"' '.items[0].status.conditions[0].observedGeneration=1' \
 '.items[0].spec.url="oci://ghcr.io/other/manifests"' '.items[0].status.artifact.revision="sha256:wrong"'; do
  jq "$mutation" "$work/observed-inventory.json" >"$work/source-inventory.json"
  if "$work/generate-fixture" >"$work/error" 2>&1; then echo "observed source regressed: $mutation" >&2; exit 1; fi
  cmp "$work/observed-identities.tsv" "$production/scripts/publish-workflow-approved-revisions.identities.tsv"
  cmp "$work/observed-binding.json" "$production/scripts/canonical-tenant-source-bindings.json"
  cmp "$work/legacy-before.tsv" "$production/scripts/publish-workflow-approved-revisions.tsv"
done
cp "$work/observed-inventory.json" "$work/source-inventory.json"
for failure in CANONICAL_VERIFY_FAILURE CANONICAL_INVENTORY_FAILURE; do
  if env "$failure=1" "$work/generate-fixture" >"$work/error" 2>&1; then echo "generator admitted failed $failure" >&2; exit 1; fi
  cmp "$work/observed-binding.json" "$production/scripts/canonical-tenant-source-bindings.json"
done
# Inconsistent direction one: bootstrap with an observed row.
jq '.bindings[0].kind="published-bootstrap"' "$work/observed-binding.json" >"$production/scripts/canonical-tenant-source-bindings.json"
if "$work/generate-fixture" >"$work/error" 2>&1; then echo 'bootstrap borrowed observed authority' >&2; exit 1; fi
cp "$work/observed-binding.json" "$production/scripts/canonical-tenant-source-bindings.json"
# Inconsistent direction two: binding without its real observed row.
awk -F '\t' '$1!="business-site"' "$work/observed-identities.tsv" >"$production/scripts/publish-workflow-approved-revisions.identities.tsv"
if "$work/generate-fixture" >"$work/error" 2>&1; then echo 'binding invented missing observation' >&2; exit 1; fi
cp "$work/observed-identities.tsv" "$production/scripts/publish-workflow-approved-revisions.identities.tsv"
printf 'Canonical source bootstrap: atomic/idempotent admission, monotonic observed transition and refusal controls passed.\n'

# A production Kustomize patch changes only the effective constructor. Raw
# source validation still passes; independent rendered validation must refuse.
yq -i '.patches += [{"target":{"kind":"ResourceGraphDefinition","name":"tenant.kro.run"},"patch":"- op: replace\n  path: /spec/resources/0/template/metadata/name\n  value: unreviewed"}]' "$production/k8s/providers/hetzner/infrastructure/kustomization.yaml"
if bash "$test_repo_root/scripts/guard-consumer-discovery-conservation.sh" "$production" >"$work/error" 2>&1; then
  echo 'effective constructor changed behind admitted raw graph' >&2; exit 1
fi
printf 'Canonical source bootstrap: effective rendered constructor changes are refused.\n'
