#!/usr/bin/env bash
# Exercise real upgrade preparation, discovery and regeneration with provider adapters only.
set -euo pipefail
test_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
upgrade_test_work="$(mktemp -d)"
trap 'rm -rf "$upgrade_test_work"' EXIT
tree="$upgrade_test_work/tree"
mkdir -p "$tree/scripts"
cp -R "$test_root/k8s" "$tree/k8s"
cp "$test_root/ksail.prod.yaml" "$tree/ksail.prod.yaml"
cp "$test_root/scripts/"*.tsv "$tree/scripts/"
cp "$test_root/scripts/canonical-tenant-source-bindings.json" "$tree/scripts/"
cp "$test_root/scripts/publish-workflow-bootstrap-publications.json" "$tree/scripts/"
mkdir -p "$tree/talos/cluster"
cp "$test_root/talos/cluster/verify-first-party-images.yaml" "$tree/talos/cluster/"
binding_file="$tree/scripts/canonical-tenant-source-bindings.json"
tenant_path=k8s/providers/hetzner/apps/business-site/tenant.yaml
jq '.bindings |= map((.previousBinding // .)|.kind="published-bootstrap"|
 .sourceIdentity.digest="sha256:fa22e5796a39872feb8c75b5d56e2c6d68ff289230e2c5762f0bbfa0b30d6744"|
 .publication.gitTag="v1.0.3"|.publication.sourceSHA="498248e86e49024b72f3249b518e4dd1cef0104b"|
 .publication.workflowRunId=39921694859|.publication.workflowRunAttempt=1)' "$binding_file" >"$upgrade_test_work/seed.json"
mv "$upgrade_test_work/seed.json" "$binding_file"
UPGRADE_SEED_DIGEST="$(jq -r '.bindings[0].sourceIdentity.digest' "$binding_file")" \
  yq -i '.spec.source.digest=strenv(UPGRADE_SEED_DIGEST)' "$tree/$tenant_path"
awk -F '\t' '$1!="business-site"' "$tree/scripts/publish-workflow-approved-revisions.identities.tsv" >"$upgrade_test_work/seed-identities.tsv"
mv "$upgrade_test_work/seed-identities.tsv" "$tree/scripts/publish-workflow-approved-revisions.identities.tsv"
cp "$binding_file" "$upgrade_test_work/original-binding.json"
cp "$tree/$tenant_path" "$upgrade_test_work/original-tenant.yaml"
export PUBLISH_CONSUMER_ROOT="$tree" APPROVED_REVISIONS_FILE="$tree/scripts/publish-workflow-approved-revisions.tsv"
export APPROVED_IDENTITIES_FILE="$tree/scripts/publish-workflow-approved-revisions.identities.tsv"
export CANONICAL_APPROVALS_FILE="$tree/scripts/publish-workflow-canonical-approvals.tsv"
export PUBLISH_BOOTSTRAP_FILE="$tree/scripts/publish-workflow-bootstrap-publications.json" PUBLISH_KUBE_CONTEXT=fixture
# shellcheck source=scripts/bootstrap-tenant-source.sh
source "$test_root/scripts/bootstrap-tenant-source.sh"
old_record="$(jq -c '.bindings[0]' "$binding_file")"
next_digest="sha256:$(printf '%064d' 8)"
next_source="$(printf '%040d' 7)"
next_tag=v2.0.0
jq --arg digest "$next_digest" --arg source "$next_source" '
  .bindings[0] | {schemaVersion:1,kind:"manifest-publication-evidence",consumer,
  deploymentAuthorized:false,gitTag:"v2.0.0",sourceSHA:$source,digest:$digest,
  publisherIdentity:.publication.publisherIdentity,callerWorkflow:.publication.callerWorkflow,
  event:.publication.event,workflowRunId:12345,workflowRunAttempt:1}' "$binding_file" >"$upgrade_test_work/receipt.json"
# The old live source is independently populated from the committed admission.
jq -n --argjson binding "$old_record" '{apiVersion:"v1",kind:"List",metadata:{},items:[{
 apiVersion:$binding.sourceIdentity.apiVersion,kind:$binding.sourceIdentity.kind,
 metadata:{namespace:$binding.sourceIdentity.namespace,name:$binding.sourceIdentity.name,
 uid:"fictional-source-uid",resourceVersion:"1234",generation:2},
 spec:{url:$binding.sourceIdentity.artifactURL,ref:{digest:$binding.sourceIdentity.digest},
 verify:{provider:"cosign",matchOIDCIdentity:[{issuer:"^https://token\\.actions\\.githubusercontent\\.com$",
 subject:"^https://github\\.com/devantler-tech/\\.github/\\.github/workflows/publish-app\\.yaml@467f07582436a26b62cf4aa756423512f85ba0c0$"}]}},
 status:{observedGeneration:2,artifact:{revision:$binding.sourceIdentity.digest},conditions:[
 {type:"Ready",status:"True",observedGeneration:2},{type:"SourceVerified",status:"True",observedGeneration:2}]}}]}' >"$upgrade_test_work/inventory.json"
cp "$upgrade_test_work/inventory.json" "$upgrade_test_work/original-inventory.json"
business_site_manifest_evidence() { cat "$upgrade_test_work/receipt.json"; }
kubectl() {
  if [ -n "${input_race:-}" ]; then
    if [ -e "$upgrade_test_work/read-once" ]; then
      case "$input_race" in
        registry)
          jq '.bindings[0].publication.workflowRunAttempt=2' "$binding_file" >"$upgrade_test_work/raced.json"
          mv "$upgrade_test_work/raced.json" "$binding_file" ;;
        tenant) yq -i '.spec.webApp=true' "$tree/$tenant_path" ;;
        constructor) yq -i '.spec.schema.group="unapproved.example"' "$tree/k8s/bases/infrastructure/resource-graph-definitions/tenant/resource-graph-definition.yaml" ;;
        output) mkdir "$upgrade_test_work/candidate"; printf 'keep\n' >"$upgrade_test_work/candidate/keep" ;;
      esac
    else touch "$upgrade_test_work/read-once"; fi
  fi
  if [ "${observation_race:-0}" = 1 ]; then
    # A changed resourceVersion is enough to invalidate the preparation snapshot.
    if [ -e "$upgrade_test_work/read-once" ]; then jq '.items[0].metadata.resourceVersion="changed"' "$upgrade_test_work/inventory.json"; return; fi
    touch "$upgrade_test_work/read-once"
  fi
  cat "$upgrade_test_work/inventory.json"
}
prepare() {
  PUBLISH_KUBE_CONTEXT=fixture CANONICAL_APPROVALS_FILE="$tree/scripts/publish-workflow-canonical-approvals.tsv" \
    bootstrap_tenant_source --prepare-canonical-upgrade --source-root "$tree" --output "$upgrade_test_work/candidate" \
    --tag "$next_tag" --source-sha "$next_source" --digest "$next_digest"
}
prepare >"$upgrade_test_work/result" 2>"$upgrade_test_work/error" || { cat "$upgrade_test_work/error" >&2; echo 'real upgrade preparation failed' >&2; exit 1; }
candidate_record="$upgrade_test_work/candidate/scripts/canonical-tenant-source-bindings.json"
test -f "$candidate_record"
test -f "$upgrade_test_work/candidate/$tenant_path"
cmp "$binding_file" "$upgrade_test_work/original-binding.json"
cmp "$tree/$tenant_path" "$upgrade_test_work/original-tenant.yaml"
jq -e --arg digest "$next_digest" --argjson old "$old_record" '
 .bindings[0] | .kind=="published-upgrade" and .sourceIdentity.digest==$digest and
 .previousBinding==$old and .previousObservation.generation==2 and
 (.previousObservation.uidSha256|test("^[0-9a-f]{64}$"))' "$candidate_record" >/dev/null
test "$(yq '.spec.source.digest' "$upgrade_test_work/candidate/$tenant_path")" = "$next_digest"
test "$(yq '.spec.webApp or .spec.externalDns or .spec.sops or .spec.source.reconcileApplication' "$upgrade_test_work/candidate/$tenant_path")" = false
if prepare >"$upgrade_test_work/result" 2>&1; then echo 'existing candidate directory overwritten' >&2; exit 1; fi
cp -R "$upgrade_test_work/candidate" "$upgrade_test_work/good-candidate"
rm -rf "$upgrade_test_work/candidate"
prepare >"$upgrade_test_work/result"
diff -r "$upgrade_test_work/good-candidate" "$upgrade_test_work/candidate"
rm -rf "$upgrade_test_work/candidate"
mkdir "$upgrade_test_work/redirect"
ln -s "$upgrade_test_work/redirect" "$upgrade_test_work/candidate"
if prepare >"$upgrade_test_work/result" 2>&1; then echo 'candidate symlink followed' >&2; exit 1; fi
test -z "$(find "$upgrade_test_work/redirect" -mindepth 1 -print)"
rm "$upgrade_test_work/candidate"
observation_race=1
if prepare >"$upgrade_test_work/result" 2>&1; then echo 'stale observation accepted' >&2; exit 1; fi
test ! -e "$upgrade_test_work/candidate"
observation_race=0
graph_path=k8s/bases/infrastructure/resource-graph-definitions/tenant/resource-graph-definition.yaml
cp "$tree/$graph_path" "$upgrade_test_work/original-graph.yaml"
for input_race in registry tenant constructor output; do
  rm -f "$upgrade_test_work/read-once"
  if prepare >"$upgrade_test_work/result" 2>&1; then echo "changed $input_race input accepted" >&2; exit 1; fi
  if [ "$input_race" = output ]; then
    test "$(find "$upgrade_test_work/candidate" -type f | wc -l | tr -d ' ')" = 1
    grep -qx keep "$upgrade_test_work/candidate/keep"
    rm -rf "$upgrade_test_work/candidate"
  else test ! -e "$upgrade_test_work/candidate"; fi
  cp "$upgrade_test_work/original-binding.json" "$binding_file"
  cp "$upgrade_test_work/original-tenant.yaml" "$tree/$tenant_path"
  cp "$upgrade_test_work/original-graph.yaml" "$tree/$graph_path"
done
input_race=''
for mutation in \
 '.items += .items' \
 '.metadata.continue="next-page"' \
 '.items[0].metadata.uid=""' \
 '.items[0].status.conditions[1].status="False"' \
 '.items[0].status.observedGeneration=1' \
 '.items[0].spec.verify.matchOIDCIdentity[0].subject=".*"' \
 '.items[0].spec.url="oci://ghcr.io/other/repository"' \
 '.items[0].spec.ref.digest="sha256:bad"'; do
  jq "$mutation" "$upgrade_test_work/original-inventory.json" >"$upgrade_test_work/inventory.json"
  if prepare >"$upgrade_test_work/result" 2>&1; then echo "unsafe observed source accepted: $mutation" >&2; exit 1; fi
  test ! -e "$upgrade_test_work/candidate"
done
cp "$upgrade_test_work/original-inventory.json" "$upgrade_test_work/inventory.json"
cp "$upgrade_test_work/receipt.json" "$upgrade_test_work/good-receipt.json"
for mutation in '.digest="sha256:bad"' '.sourceSHA=("9"*40)' '.gitTag="v3.0.0"' '.publisherIdentity="unapproved"' '.deploymentAuthorized=true' '.consumer="other"' '.event="push"' '.callerWorkflow=".github/workflows/other.yaml"'; do
  jq "$mutation" "$upgrade_test_work/good-receipt.json" >"$upgrade_test_work/receipt.json"
  if prepare >"$upgrade_test_work/result" 2>&1; then echo "wrong candidate publication accepted: $mutation" >&2; exit 1; fi
  test ! -e "$upgrade_test_work/candidate"
done
cp "$upgrade_test_work/good-receipt.json" "$upgrade_test_work/receipt.json"
prepare >"$upgrade_test_work/result"
cp "$candidate_record" "$binding_file"
cp "$upgrade_test_work/candidate/$tenant_path" "$tree/$tenant_path"
discover_consumers "$tree" with-identity >"$upgrade_test_work/consumers.tsv"
grep -q "digest:$next_digest" "$upgrade_test_work/consumers.tsv"
APPROVED_REVISIONS_FILE="$tree/scripts/publish-workflow-approved-revisions.tsv" \
  CANONICAL_APPROVALS_FILE="$tree/scripts/publish-workflow-canonical-approvals.tsv" \
  PUBLISH_BOOTSTRAP_FILE="$tree/scripts/publish-workflow-bootstrap-publications.json" \
  PUBLISH_CONSUMER_ROOT="$tree" APPROVED_REVISIONS_ENFORCE=1 \
  bash "$test_root/scripts/guard-consumer-discovery-conservation.sh" "$tree" >"$upgrade_test_work/conservation"
record="$(jq -c '.bindings[0]' "$binding_file")"
test "$(canonical_source_inventory_state "$record" "$upgrade_test_work/inventory.json")" = previous
canonical_observation_consistent "$record" "$tree/scripts/publish-workflow-approved-revisions.identities.tsv"
# The actual generator must preserve old observations while the candidate is unapplied.
default_observer() {
  if [ "$1" = data-product-controller ]; then
    jq -r '.publications[]|select(.consumer=="data-product-controller")|[(.gitTag|ltrimstr("v")),.digest,.signerSHA]|@tsv' "$PUBLISH_BOOTSTRAP_FILE"
  else awk -F '\t' -v c="$1" '$1==c {print $3 "\t" $4 "\t" $5}' "$APPROVED_REVISIONS_FILE"; fi
}
default_pin_resolver() {
  if [ "$1" = business-site ]; then printf '%s\n' "$record" | jq -r .publication.publisherIdentity
  elif [ "$1" = data-product-controller ]; then jq -r '.publications[]|select(.consumer=="data-product-controller")|.mainPinSHA' "$PUBLISH_BOOTSTRAP_FILE"
  else awk -F '\t' -v c="$1" '$1==c {print $6}' "$APPROVED_REVISIONS_FILE"; fi
}
canonical_publication_verify() { :; }
main >"$upgrade_test_work/generator-pending"
test "$(jq -r '.bindings[0].kind' "$binding_file")" = published-upgrade
test "$(awk -F '\t' '$1=="business-site" {n++} END {print n+0}' "$APPROVED_IDENTITIES_FILE")" = 0
jq --arg digest "$next_digest" '.items[0].spec.ref.digest=$digest | .items[0].status.artifact.revision=$digest |
 .items[0].metadata.generation=3 | .items[0].status.observedGeneration=3 | .items[0].status.conditions[].observedGeneration=3' \
 "$upgrade_test_work/original-inventory.json" >"$upgrade_test_work/inventory.json"
main >"$upgrade_test_work/generator-observed"
test "$(jq -r '.bindings[0].kind' "$binding_file")" = published-binding
jq -e '.bindings[0]|has("previousBinding")|not' "$binding_file" >/dev/null
awk -F '\t' -v digest="$next_digest" '$1=="business-site" {n++; if($3!="2.0.0" || $4!=digest) exit 1} END {exit n!=1}' "$APPROVED_IDENTITIES_FILE"
cp "$binding_file" "$upgrade_test_work/applied.json"
cp "$APPROVED_IDENTITIES_FILE" "$upgrade_test_work/applied-identities.tsv"
main >"$upgrade_test_work/generator-repeat"
cmp "$upgrade_test_work/applied.json" "$binding_file"
cmp "$upgrade_test_work/applied-identities.tsv" "$APPROVED_IDENTITIES_FILE"
# A subsequent transition must also preserve a real existing applied row.
next_tag=v2.1.0
next_digest="sha256:$(printf '%064d' 9)"
next_source="$(printf '%040d' 8)"
jq --arg tag "$next_tag" --arg digest "$next_digest" --arg source "$next_source" \
  '.gitTag=$tag|.digest=$digest|.sourceSHA=$source' "$upgrade_test_work/good-receipt.json" >"$upgrade_test_work/receipt.json"
rm -rf "$upgrade_test_work/candidate"
prepare >"$upgrade_test_work/result"
jq -e '.bindings[0].previousBinding.kind=="published-binding"' "$candidate_record" >/dev/null
cp "$candidate_record" "$binding_file"
cp "$upgrade_test_work/candidate/$tenant_path" "$tree/$tenant_path"
record="$(jq -c '.bindings[0]' "$binding_file")"
main >"$upgrade_test_work/generator-second-pending"
cmp "$upgrade_test_work/applied-identities.tsv" "$APPROVED_IDENTITIES_FILE"
printf 'Canonical upgrade: distinct proposed/observed states, portable candidate, real discovery/regeneration and refusal controls passed.\n'
