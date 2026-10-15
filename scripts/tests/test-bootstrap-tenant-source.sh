#!/usr/bin/env bash
set -euo pipefail
test_repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
test_work="$(mktemp -d)"
trap 'rm -rf "$test_work"' EXIT
mkdir -p "$test_work/bin" "$test_work/k8s/source"
export BOOT_TEST_WORK="$test_work" PUBLISH_KUBE_CONTEXT=fixture-only
export PATH="$test_work/bin:$PATH"
export APPROVED_REVISIONS_FILE="$test_work/applied.tsv"
printf 'consumer\tworkflow\tapplied_tag\tapplied_digest\tapplied_signer_sha\tmain_pin_sha\trelease_candidate_sha\tobserved_on\n' >"$APPROVED_REVISIONS_FILE"
cat >"$test_work/k8s/source/oci-repository.yaml" <<'EOF'
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: khoist-documentation
  namespace: data-product-controller
spec:
  url: oci://ghcr.io/devantler-tech/data-product-controller/manifests
  ref:
    digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
EOF
cat >"$test_work/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'called\n' >>"$BOOT_TEST_WORK/provider-calls"
case "$BOOT_CASE" in
  cluster-error) exit 42 ;;
  malformed-list) printf '{"items":null}\n' ;;
  trailing-list) printf '{"items":[]}\n{"items":[]}\n' ;;
  partial-list) printf '{"items":[],"metadata":{"continue":"more"}}\n' ;;
  observed|stale-generation|unsigned-source|wrong-observed-source)
    yq -o=json '.' "$BOOT_TEST_WORK/k8s/source/oci-repository.yaml" |
      jq --arg mode "$BOOT_CASE" '{items:[.] } |
        .items[0].metadata.generation=7 |
        .items[0].status={observedGeneration:7,artifact:{revision:"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",digest:"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},conditions:[{type:"Ready",status:"True"},{type:"SourceVerified",status:"True"}]} |
        if $mode=="stale-generation" then .items[0].status.observedGeneration=6
        elif $mode=="unsigned-source" then .items[0].status.conditions[1].status="False"
        elif $mode=="wrong-observed-source" then .items[0].metadata.name="another-source"
        else . end' ;;
  fetched|wrong-source|multiple)
    yq -o=json '.' "$BOOT_TEST_WORK/k8s/source/oci-repository.yaml" |
      jq --arg mode "$BOOT_CASE" '{items:[.] } |
        if $mode=="fetched" then .items[0].status.artifact.revision="sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
        elif $mode=="wrong-source" then .items[0].metadata.name="different-source"
        else .items += .items end' ;;
  *) printf '{"items":[]}\n' ;;
esac
EOF
cat >"$test_work/bin/cosign" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
# Assert the verifier is given immutable bytes AND the caller and publisher identity.
[[ "$*" == *'--certificate-identity https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@2222222222222222222222222222222222222222'* ]]
[[ "$*" == *'--certificate-oidc-issuer https://token.actions.githubusercontent.com'* ]]
[[ "$*" == *'--certificate-github-workflow-repository devantler-tech/data-product-controller'* ]]
[[ "$*" == *'--certificate-github-workflow-ref refs/tags/v5.2.0'* ]]
[[ "$*" == *'--certificate-github-workflow-sha 1111111111111111111111111111111111111111'* ]]
[[ "${!#}" == 'ghcr.io/devantler-tech/data-product-controller/manifests@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' ]]
digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
[ "$BOOT_CASE" != wrong-digest ] || digest=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
jq -n --arg digest "$digest" '[{critical:{image:{"docker-manifest-digest":$digest},identity:{"docker-reference":("ghcr.io/devantler-tech/data-product-controller/manifests@"+$digest)}}}]'
[ "$BOOT_CASE" != verify-error ] || exit 42
EOF
chmod +x "$test_work/bin/kubectl" "$test_work/bin/cosign"
# shellcheck source=scripts/bootstrap-tenant-source.sh
source "$test_repo_root/scripts/bootstrap-tenant-source.sh"
tag_commit() {
  if [ "$BOOT_CASE" = moved-tag ] && [ -f "$BOOT_TEST_WORK/tag-read" ]; then
    printf '%040d\n' 9
  else
    touch "$BOOT_TEST_WORK/tag-read"
    printf '1111111111111111111111111111111111111111\n'
  fi
}
tag_was_published() { [ "$BOOT_CASE" != unpublished ]; }
signer_for_tag() { printf '2222222222222222222222222222222222222222\n'; }
report_publisher_at_ref() {
  if [ "$BOOT_CASE" = wrong-publisher ]; then printf 'unapproved\n'; else
    printf 'devantler-tech/actions/.github/workflows/publish-app.yaml@2222222222222222222222222222222222222222\n'
  fi
}
default_pin_resolver() { printf '3333333333333333333333333333333333333333\n'; }
default_release_resolver() { printf '4444444444444444444444444444444444444444\n'; }

run_bootstrap() {
  bootstrap_tenant_source --source-root "$test_work" --source-file "${BOOT_SOURCE:-k8s/source/oci-repository.yaml}" \
    --tag v5.2.0 --output "$test_work/result.json"
}
export BOOT_CASE=valid
run_bootstrap
jq -e '.publications[0].kind=="published-bootstrap" and .publications[0].sourceSHA=="1111111111111111111111111111111111111111"' "$test_work/result.json" >/dev/null
cp "$test_work/result.json" "$test_work/expected.json"
verify_bound_publication "$(jq -c '.publications[0]' "$test_work/result.json")"
export BOOT_CASE=verify-error
if verify_bound_publication "$(jq -c '.publications[0]' "$test_work/result.json")"; then
  echo 'shared verifier accepted a failed command' >&2
  exit 1
fi
export BOOT_CASE=valid
run_bootstrap
cmp "$test_work/result.json" "$test_work/expected.json"
rm "$test_work/result.json"
for BOOT_CASE in cluster-error malformed-list trailing-list partial-list fetched wrong-source multiple verify-error wrong-digest unpublished wrong-publisher moved-tag; do
  export BOOT_CASE
  rm -f "$test_work/tag-read"
  if run_bootstrap >"$test_work/stdout" 2>"$test_work/stderr"; then
    echo "bootstrap accepted $BOOT_CASE" >&2
    exit 1
  fi
  test ! -e "$test_work/result.json"
done
export BOOT_CASE=valid BOOT_SOURCE=../outside.yaml
rm -f "$test_work/provider-calls"
if run_bootstrap >/dev/null 2>&1; then
  echo 'bootstrap accepted traversal' >&2
  exit 1
fi
test ! -e "$test_work/provider-calls"
unset BOOT_SOURCE
printf 'data-product-controller\tpublish-app\t5.2.0\tsha256:a\tsigner\tpin\tcandidate\t2026-10-15\n' >>"$APPROVED_REVISIONS_FILE"
if run_bootstrap >/dev/null 2>&1; then
  echo 'bootstrap replaced ordinary observation' >&2
  exit 1
fi
test ! -e "$test_work/result.json"
# The ordinary observer must bind the exact verified Flux source and its current
# generation. Flux's local archive hash deliberately differs from the OCI digest.
export PUBLISH_BOOTSTRAP_FILE="$test_work/expected.json" PUBLISH_CONSUMER_ROOT="$test_work"
export BOOT_CASE=observed
test "$(applied_revision data-product-controller)" = $'5.2.0\tsha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
for BOOT_CASE in stale-generation unsigned-source wrong-observed-source; do
  export BOOT_CASE
  if applied_revision data-product-controller >/dev/null 2>&1; then
    echo "ordinary observation accepted $BOOT_CASE" >&2
    exit 1
  fi
done
export BOOT_CASE=valid
gh_retry() { printf 'main\n'; }
report_publisher_at_ref() { printf 'devantler-tech/actions/.github/workflows/publish-app.yaml@3333333333333333333333333333333333333333\n'; }
bound_answer="$(bound_publication_resolver data-product-controller publish-app sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa "$test_work")"
test "$bound_answer" = $'2222222222222222222222222222222222222222\tdevantler-tech/actions/.github/workflows/publish-app.yaml@3333333333333333333333333333333333333333\tpublished-binding'
export BOOT_CASE=verify-error
if bound_publication_resolver data-product-controller publish-app sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa "$test_work" >/dev/null; then
  echo 'report accepted an unverified immutable publication' >&2
  exit 1
fi
echo 'bootstrap requires complete inventory and a verified immutable publication; failures write nothing'
