#!/usr/bin/env bash
set -euo pipefail
test_repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
test_work="$(mktemp -d)"
trap 'rm -rf "$test_work"' EXIT
mkdir -p "$test_work/bin" "$test_work/k8s/source"
export BOOT_TEST_WORK="$test_work" PUBLISH_KUBE_CONTEXT=fixture-only
export PATH="$test_work/bin:$PATH"
export APPROVED_REVISIONS_FILE="$test_work/applied.tsv"
BOOT_REAL_LINK="$(command -v link)"
BOOT_REAL_LN="$(command -v ln)"
export BOOT_REAL_LINK BOOT_REAL_LN
cat >"$test_work/bin/link-operation" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$BOOT_CASE" in
  output-directory-race) mkdir -p "$2" ;;
  output-symlink-race)
    mkdir -p "$BOOT_TEST_WORK/redirect"
    "$BOOT_REAL_LN" -s "$BOOT_TEST_WORK/redirect" "$2" ;;
esac
case "${0##*/}" in
  link) exec "$BOOT_REAL_LINK" "$@" ;;
  ln) exec "$BOOT_REAL_LN" "$@" ;;
esac
EOF
cp "$test_work/bin/link-operation" "$test_work/bin/link"
cp "$test_work/bin/link-operation" "$test_work/bin/ln"
chmod +x "$test_work/bin/link" "$test_work/bin/ln"
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
reference="ghcr.io/devantler-tech/data-product-controller/manifests@$digest"
case "$BOOT_CASE" in
  repository-reference) reference=ghcr.io/devantler-tech/data-product-controller/manifests ;;
  foreign-reference) reference=ghcr.io/untrusted/manifests ;;
  wrong-reference-digest) reference=ghcr.io/devantler-tech/data-product-controller/manifests@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb ;;
esac
jq -n --arg digest "$digest" --arg reference "$reference" '[{critical:{image:{"docker-manifest-digest":$digest},identity:{"docker-reference":$reference}}}]'
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
export BOOT_CASE=repository-reference
if ! verify_bound_publication "$(jq -c '.publications[0]' "$test_work/result.json")"; then
  echo 'shared verifier refused the valid repository-only reference' >&2
  exit 1
fi
for BOOT_CASE in foreign-reference wrong-reference-digest; do
  export BOOT_CASE
  if verify_bound_publication "$(jq -c '.publications[0]' "$test_work/result.json")"; then
    echo "shared verifier accepted $BOOT_CASE" >&2
    exit 1
  fi
done
export BOOT_CASE=verify-error
if verify_bound_publication "$(jq -c '.publications[0]' "$test_work/result.json")"; then
  echo 'shared verifier accepted a failed command' >&2
  exit 1
fi
export BOOT_CASE=valid
run_bootstrap
cmp "$test_work/result.json" "$test_work/expected.json"
rm "$test_work/result.json"
export BOOT_CASE=output-directory-race
if run_bootstrap >/dev/null 2>&1; then
  echo 'bootstrap reported success after the output became a directory' >&2
  exit 1
fi
test -d "$test_work/result.json"
test -z "$(find "$test_work/result.json" -mindepth 1 -maxdepth 1 -print)"
rmdir "$test_work/result.json"
export BOOT_CASE=output-symlink-race
if run_bootstrap >/dev/null 2>&1; then
  echo 'bootstrap reported success after the output became a symlink' >&2
  exit 1
fi
test -L "$test_work/result.json"
test -z "$(find "$test_work/redirect" -mindepth 1 -maxdepth 1 -print)"
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

# The business profile verifies evidence only. It must not route a maintained
# publisher through the legacy approval format or touch an ambient cluster.
biz_work="$test_work/business"
mkdir -p "$biz_work/bin"
export BIZ_WORK="$biz_work"
for approval in publish-workflow-approved-revisions.tsv publish-workflow-approved-revisions.identities.tsv \
  publish-workflow-canonical-approvals.tsv publish-workflow-bootstrap-publications.json; do
  cp "$test_repo_root/scripts/$approval" "$biz_work/before-$approval"
done
cat >"$biz_work/tag.json" <<'EOF'
{"ref":"refs/tags/v1.0.2","object":{"type":"commit","sha":"e5eb256dcb5dbf3857257e9072937ab95f1ea6c3"}}
EOF
cat >"$biz_work/release.json" <<'EOF'
{"id":123,"tag_name":"v1.0.2","draft":false,"prerelease":false,"published_at":"2026-10-22T23:10:00Z"}
EOF
cat >"$biz_work/run.json" <<'EOF'
{"id":39869859801,"run_attempt":1,"status":"completed","conclusion":"success","event":"release","path":".github/workflows/publish-runtime.yaml","head_branch":"v1.0.2","head_sha":"e5eb256dcb5dbf3857257e9072937ab95f1ea6c3","repository":{"full_name":"devantler-tech/business-site"},"referenced_workflows":[{"path":"devantler-tech/.github/.github/workflows/publish-app.yaml@467f07582436a26b62cf4aa756423512f85ba0c0","sha":"467f07582436a26b62cf4aa756423512f85ba0c0"}]}
EOF
cat >"$biz_work/caller.yaml" <<'EOF'
on:
  release:
    types: [published]
jobs:
  publish:
    uses: devantler-tech/.github/.github/workflows/publish-app.yaml@467f07582436a26b62cf4aa756423512f85ba0c0
EOF
cat >"$biz_work/bin/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  'api repos/devantler-tech/business-site/git/ref/tags/v1.0.2')
    if [ "$BIZ_CASE" = moved-tag ] && [ -f "$BIZ_WORK/tag-read" ]; then
      jq '.object.sha="1111111111111111111111111111111111111111"' "$BIZ_WORK/tag.json"
    else cat "$BIZ_WORK/tag.json"; fi
    touch "$BIZ_WORK/tag-read"
    [ "$BIZ_CASE" != tag-read-failure ] || exit 42 ;;
  'api repos/devantler-tech/business-site/releases/tags/v1.0.2')
    if [ "$BIZ_CASE" = withdrawn-release ] && [ -f "$BIZ_WORK/release-read" ]; then
      jq '.draft=true' "$BIZ_WORK/release.json"
    else
      case "$BIZ_CASE" in
        draft-release) jq '.draft=true' "$BIZ_WORK/release.json" ;;
        prerelease-release) jq '.prerelease=true' "$BIZ_WORK/release.json" ;;
        unpublished-release) jq '.published_at=null' "$BIZ_WORK/release.json" ;;
        invalid-release-id) jq '.id=0' "$BIZ_WORK/release.json" ;;
        *) cat "$BIZ_WORK/release.json" ;;
      esac
    fi
    touch "$BIZ_WORK/release-read"
    [ "$BIZ_CASE" != release-read-failure ] || exit 42 ;;
  'api --method GET repos/devantler-tech/business-site/actions/runs --raw-field branch=v1.0.2 --raw-field event=release --raw-field per_page=100')
    cat "$BIZ_WORK/list.json"
    [ "$BIZ_CASE" != run-read-failure ] || exit 42 ;;
  'api repos/devantler-tech/business-site/actions/runs/39869859801')
    if [ "$BIZ_CASE" = changed-attempt ]; then jq '.run_attempt=2' "$BIZ_WORK/current-run.json"
    else cat "$BIZ_WORK/current-run.json"; fi ;;
  'api --method GET repos/devantler-tech/business-site/contents/.github/workflows/publish-runtime.yaml --raw-field ref=e5eb256dcb5dbf3857257e9072937ab95f1ea6c3 -H Accept: application/vnd.github.raw')
    cat "$BIZ_WORK/current-caller.yaml" ;;
  *) echo 'unexpected provider request' >&2; exit 42 ;;
esac
EOF
cat >"$biz_work/bin/cosign" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
# Model the certificate check only when the caller actually requests it. A
# same-identity dispatch certificate otherwise passes, as it does in Cosign.
verifier_args="$*"
case "$verifier_args" in
  *'--certificate-github-workflow-trigger release '*)
    [ "$(cat "$BIZ_WORK/certificate-event")" = release ] || exit 42
    verifier_args="${verifier_args/--certificate-github-workflow-trigger release /}" ;;
esac
[[ "$verifier_args" == 'verify --certificate-identity https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@467f07582436a26b62cf4aa756423512f85ba0c0 --certificate-oidc-issuer https://token.actions.githubusercontent.com --certificate-github-workflow-repository devantler-tech/business-site --certificate-github-workflow-ref refs/tags/v1.0.2 --certificate-github-workflow-sha e5eb256dcb5dbf3857257e9072937ab95f1ea6c3 ghcr.io/devantler-tech/business-site/manifests@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' ]]
cat "$BIZ_WORK/signature.json"
[ "$BIZ_CASE" != signature-failure ] || exit 42
EOF
cat >"$biz_work/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
touch "$BIZ_WORK/cluster-was-called"
exit 42
EOF
chmod +x "$biz_work/bin/gh" "$biz_work/bin/cosign" "$biz_work/bin/kubectl"
business_evidence() {
  env -u PUBLISH_KUBE_CONTEXT PATH="$biz_work/bin:$PATH" bash "$test_repo_root/scripts/bootstrap-tenant-source.sh" \
    --consumer business-site --verify-only --tag v1.0.2 \
    --source-sha e5eb256dcb5dbf3857257e9072937ab95f1ea6c3 \
    --digest sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa "$@"
}
business_fixture() {
  rm -f "$biz_work/tag-read" "$biz_work/release-read"
  printf 'release\n' >"$biz_work/certificate-event"
  cp "$biz_work/run.json" "$biz_work/current-run.json"
  cp "$biz_work/caller.yaml" "$biz_work/current-caller.yaml"
  jq -n --slurpfile run "$biz_work/run.json" '{total_count:1,workflow_runs:$run}' >"$biz_work/list.json"
  jq -n '[{critical:{image:{"docker-manifest-digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},identity:{"docker-reference":"ghcr.io/devantler-tech/business-site/manifests"}},optional:{
    "devantler.ci-run-attempt":"1","devantler.ci-run-id":"39867770434",
    "devantler.image-digest":"sha256:d71c867ac38472643dc38669b3b71d9ee3db4062ac46654314fde864846a9471",
    "devantler.image-producer":"https://github.com/devantler-tech/business-site/.github/workflows/ci.yaml@refs/heads/main",
    "devantler.manifests-digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "devantler.public-digest":"3b0947c7936b1003898d87d7e6d21b680cb1b0be087ce6ee40890b872bfbd7ac",
    "devantler.public-profile":"production-template","devantler.repository":"devantler-tech/business-site",
    "devantler.run-attempt":"1","devantler.run-id":"39869859801","devantler.source-ref":"v1.0.2",
    "devantler.source-sha":"e5eb256dcb5dbf3857257e9072937ab95f1ea6c3","devantler.version":"1.0.2",
    "devantler.workflow-ref":"devantler-tech/.github/.github/workflows/publish-app.yaml@467f07582436a26b62cf4aa756423512f85ba0c0"
  }}]' >"$biz_work/signature.json"
}
export BIZ_CASE=valid
business_fixture
if ! business_evidence >"$biz_work/receipt.json" 2>"$biz_work/error"; then
  echo 'business verify-only mode rejected the complete maintained release publication' >&2
  exit 1
fi
jq -e 'keys == ["callerWorkflow","consumer","deploymentAuthorized","digest","event","gitTag","kind","publisherIdentity","schemaVersion","sourceSHA","workflowRunAttempt","workflowRunId"] and
  .schemaVersion==1 and .kind=="manifest-publication-evidence" and .consumer=="business-site" and
  .deploymentAuthorized==false and .gitTag=="v1.0.2" and
  .sourceSHA=="e5eb256dcb5dbf3857257e9072937ab95f1ea6c3" and
  .digest=="sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" and
  .publisherIdentity=="devantler-tech/.github/.github/workflows/publish-app.yaml@467f07582436a26b62cf4aa756423512f85ba0c0" and
  .callerWorkflow==".github/workflows/publish-runtime.yaml" and .event=="release" and
  .workflowRunId==39869859801 and .workflowRunAttempt==1' "$biz_work/receipt.json" >/dev/null
if read_bound_publications "$test_repo_root" "$biz_work/receipt.json" >/dev/null 2>&1; then
  echo 'manifest-only evidence was accepted as deployment approval' >&2
  exit 1
fi
for BIZ_CASE in wrong-event wrong-caller wrong-source wrong-tag wrong-product wrong-publisher duplicate-publisher \
  malformed-list trailing-list incomplete-list run-read-failure tag-read-failure wrong-caller-pin multi-document-caller \
  signature-failure malformed-signature trailing-signature wrong-digest wrong-reference changed-attempt moved-tag withdrawn-release \
  draft-release prerelease-release unpublished-release invalid-release-id release-read-failure signature-wrong-event \
  signature-wrong-run signature-wrong-attempt signature-wrong-source signature-wrong-workflow \
  signature-missing-claims signature-wrong-ref signature-wrong-version signature-wrong-repository signature-wrong-manifest-claim; do
  export BIZ_CASE
  business_fixture
  filter='.'
  signature_filter='.'
  case "$BIZ_CASE" in
    wrong-event) filter='.workflow_runs[0].event="push"' ;;
    wrong-caller) filter='.workflow_runs[0].path=".github/workflows/cd.yaml"' ;;
    wrong-source) filter='.workflow_runs[0].head_sha="1111111111111111111111111111111111111111"' ;;
    wrong-tag) filter='.workflow_runs[0].head_branch="v1.0.1"' ;;
    wrong-product) filter='.workflow_runs[0].repository.full_name="devantler-tech/another-product"' ;;
    wrong-publisher) filter='.workflow_runs[0].referenced_workflows[0].path="devantler-tech/actions/.github/workflows/publish-app.yaml@467f07582436a26b62cf4aa756423512f85ba0c0"' ;;
    duplicate-publisher) filter='.workflow_runs[0].referenced_workflows += .workflow_runs[0].referenced_workflows' ;;
    incomplete-list) filter='.total_count=2' ;;
    malformed-list) printf '{broken' >"$biz_work/list.json" ;;
    trailing-list) printf '\n{}\n' >>"$biz_work/list.json" ;;
    wrong-caller-pin) sed 's/467f07582436a26b62cf4aa756423512f85ba0c0/1111111111111111111111111111111111111111/' "$biz_work/caller.yaml" >"$biz_work/current-caller.yaml" ;;
    multi-document-caller) printf '\n---\njobs: {}\n' >>"$biz_work/current-caller.yaml" ;;
    malformed-signature) printf '{}' >"$biz_work/signature.json" ;;
    trailing-signature) printf '\n{}\n' >>"$biz_work/signature.json" ;;
    wrong-digest) printf '[{"critical":{"image":{"docker-manifest-digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}}]' >"$biz_work/signature.json" ;;
    wrong-reference) printf '[{"critical":{"image":{"docker-manifest-digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"identity":{"docker-reference":"ghcr.io/untrusted/manifests"}}}]' >"$biz_work/signature.json" ;;
    signature-wrong-event) printf 'workflow_dispatch\n' >"$biz_work/certificate-event" ;;
    signature-wrong-run) signature_filter='.[0].optional["devantler.run-id"]="39869859802"' ;;
    signature-wrong-attempt) signature_filter='.[0].optional["devantler.run-attempt"]="2"' ;;
    signature-wrong-source) signature_filter='.[0].optional["devantler.source-sha"]="1111111111111111111111111111111111111111"' ;;
    signature-wrong-workflow) signature_filter='.[0].optional["devantler.workflow-ref"]="devantler-tech/.github/.github/workflows/publish-app.yaml@1111111111111111111111111111111111111111"' ;;
    signature-missing-claims) signature_filter='del(.[0].optional)' ;;
    signature-wrong-ref) signature_filter='.[0].optional["devantler.source-ref"]="v1.0.1"' ;;
    signature-wrong-version) signature_filter='.[0].optional["devantler.version"]="1.0.1"' ;;
    signature-wrong-repository) signature_filter='.[0].optional["devantler.repository"]="devantler-tech/another-product"' ;;
    signature-wrong-manifest-claim) signature_filter='.[0].optional["devantler.manifests-digest"]="sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"' ;;
  esac
  if [ "$filter" != . ]; then
    jq "$filter" "$biz_work/list.json" >"$biz_work/next.json"
    mv "$biz_work/next.json" "$biz_work/list.json"
  fi
  if [ "$signature_filter" != . ]; then
    jq "$signature_filter" "$biz_work/signature.json" >"$biz_work/next-signature.json"
    mv "$biz_work/next-signature.json" "$biz_work/signature.json"
  fi
  if business_evidence >"$biz_work/refusal" 2>"$biz_work/error"; then
    echo "business evidence accepted $BIZ_CASE" >&2
    exit 1
  fi
  test ! -s "$biz_work/refusal"
done
export BIZ_CASE=valid
business_fixture
for forbidden in --output --source-file --source-root --consumer --verify-only --source-sha --digest --tag; do
  if business_evidence "$forbidden" ignored >"$biz_work/refusal" 2>"$biz_work/error"; then
    echo "business evidence accepted forbidden/duplicate option $forbidden" >&2
    exit 1
  fi
  test ! -s "$biz_work/refusal"
done
test ! -e "$biz_work/cluster-was-called"
test ! -e "$test_repo_root/ignored"
for approval in publish-workflow-approved-revisions.tsv publish-workflow-approved-revisions.identities.tsv \
  publish-workflow-canonical-approvals.tsv publish-workflow-bootstrap-publications.json; do
  cmp "$test_repo_root/scripts/$approval" "$biz_work/before-$approval"
done
echo 'business manifest evidence binds the maintained release and never grants deployment authority'
