#!/usr/bin/env bash
# The public generator cannot publish a binding inside a raced directory.
set -euo pipefail
generator_test_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
generator_test_work="$(mktemp -d)"
trap 'rm -rf "$generator_test_work"' EXIT
fixture="$generator_test_work/root"
mkdir -p "$fixture/scripts" "$fixture/talos/cluster"
cp -R "$generator_test_root/k8s" "$fixture/k8s"
cp "$generator_test_root/talos/cluster/verify-first-party-images.yaml" "$fixture/talos/cluster/"
cp "$generator_test_root/scripts/publish-workflow-canonical-approvals.tsv" "$fixture/scripts/"
jq '.bindings[0].kind="published-bootstrap"' "$generator_test_root/scripts/canonical-tenant-source-bindings.json" >"$fixture/scripts/canonical-tenant-source-bindings.json"
cp "$generator_test_root/scripts/publish-workflow-approved-revisions.tsv" "$fixture/scripts/"
awk -F '\t' '$1!="business-site"' "$generator_test_root/scripts/publish-workflow-approved-revisions.identities.tsv" >"$fixture/scripts/publish-workflow-approved-revisions.identities.tsv"
cp "$generator_test_root/scripts/publish-workflow-bootstrap-publications.json" "$fixture/scripts/"
export CANONICAL_GENERATOR_TEST_ROOT="$generator_test_root" CANONICAL_GENERATOR_FIXTURE="$fixture"
cat >"$generator_test_work/run" <<'BASH'
#!/usr/bin/env bash
set -euo pipefail
export PUBLISH_CONSUMER_ROOT="$CANONICAL_GENERATOR_FIXTURE" PUBLISH_KUBE_CONTEXT=fixture APPROVED_REVISIONS_ENFORCE=1
export APPROVED_REVISIONS_FILE="$CANONICAL_GENERATOR_FIXTURE/scripts/publish-workflow-approved-revisions.tsv"
export CANONICAL_APPROVALS_FILE="$CANONICAL_GENERATOR_FIXTURE/scripts/publish-workflow-canonical-approvals.tsv"
export PUBLISH_BOOTSTRAP_FILE="$CANONICAL_GENERATOR_FIXTURE/scripts/publish-workflow-bootstrap-publications.json"
source "$CANONICAL_GENERATOR_TEST_ROOT/scripts/generate-publish-workflow-approved-revisions.sh"
canonical_publication_verify() { return 0; }
kubectl() { printf '{"apiVersion":"v1","kind":"List","metadata":{},"items":[]}\n'; }
default_observer() {
  if [ "$1" = data-product-controller ]; then
    jq -r '.publications[]|select(.consumer=="data-product-controller")|[(.gitTag|ltrimstr("v")),.digest,.signerSHA]|@tsv' "$PUBLISH_BOOTSTRAP_FILE"
  else awk -F '\t' -v c="$1" '$1==c {print $3 "\t" $4 "\t" $5}' "$APPROVED_REVISIONS_FILE"; fi
}
default_pin_resolver() {
  if [ "$1" = business-site ]; then
    if [ "${CANONICAL_GENERATOR_DIRECTORY_RACE:-0}" = 1 ]; then
      rm "$CANONICAL_GENERATOR_FIXTURE/scripts/canonical-tenant-source-bindings.json"
      mkdir "$CANONICAL_GENERATOR_FIXTURE/scripts/canonical-tenant-source-bindings.json"
    fi
    printf '%s\n' devantler-tech/.github/.github/workflows/publish-app.yaml@467f07582436a26b62cf4aa756423512f85ba0c0
  elif [ "$1" = data-product-controller ]; then
    jq -r '.publications[]|select(.consumer=="data-product-controller")|.mainPinSHA' "$PUBLISH_BOOTSTRAP_FILE"
  else
    awk -F '\t' -v c="$1" '$1==c {print $6}' "$APPROVED_REVISIONS_FILE"
  fi
}
if [ "${1:-}" = replace ]; then
  replace_generated_record "$2" "$3"
else
  main
fi
BASH
chmod 755 "$generator_test_work/run"
cp "$fixture/scripts/canonical-tenant-source-bindings.json" "$generator_test_work/canonical-before.json"
"$generator_test_work/run" >"$generator_test_work/positive"
grep -q '6 consumer(s) examined' "$generator_test_work/positive"
test "$(jq '.bindings|length' "$fixture/scripts/canonical-tenant-source-bindings.json")" = 1
# An absent application produces no new canonical observation. Preserve the
# existing ledger's exact formatting, not merely its parsed JSON value.
cmp "$generator_test_work/canonical-before.json" "$fixture/scripts/canonical-tenant-source-bindings.json"
"$generator_test_work/run" >"$generator_test_work/unchanged"
cmp "$generator_test_work/canonical-before.json" "$fixture/scripts/canonical-tenant-source-bindings.json"
cp "$fixture/scripts/publish-workflow-approved-revisions.tsv" "$generator_test_work/legacy-before"
cp "$fixture/scripts/publish-workflow-approved-revisions.identities.tsv" "$generator_test_work/identities-before"
if CANONICAL_GENERATOR_DIRECTORY_RACE=1 "$generator_test_work/run" >"$generator_test_work/race" 2>&1; then
  printf 'generator reported success after the canonical destination became a directory\n' >&2
  exit 1
fi
test -d "$fixture/scripts/canonical-tenant-source-bindings.json"
test -z "$(find "$fixture/scripts/canonical-tenant-source-bindings.json" -mindepth 1 -print)"
cmp "$generator_test_work/legacy-before" "$fixture/scripts/publish-workflow-approved-revisions.tsv"
cmp "$generator_test_work/identities-before" "$fixture/scripts/publish-workflow-approved-revisions.identities.tsv"
# Even a directory appearing after the preflight cannot reinterpret the final
# rename. Exercise the same replacement function against the real host tool.
printf 'new record\n' >"$generator_test_work/pending"
printf 'old record\n' >"$generator_test_work/target"
"$generator_test_work/run" replace "$generator_test_work/pending" "$generator_test_work/target"
test ! -e "$generator_test_work/pending"
grep -qxF 'new record' "$generator_test_work/target"
printf 'still pending\n' >"$generator_test_work/pending"
mkdir "$generator_test_work/directory"
if "$generator_test_work/run" replace "$generator_test_work/pending" "$generator_test_work/directory" >"$generator_test_work/replace-error" 2>&1; then
  printf 'replacement interpreted a directory as an output container\n' >&2; exit 1
fi
test -z "$(find "$generator_test_work/directory" -mindepth 1 -print)"
grep -qxF 'still pending' "$generator_test_work/pending"
printf 'Canonical generator output: real generation succeeds; a raced directory refuses without nested output.\n'
