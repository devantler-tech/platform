#!/usr/bin/env bash
# Exercise canonical publication through the real generator without network or cluster access.
set -euo pipefail
test_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
test_work="$(mktemp -d)"
trap 'rm -rf "$test_work"' EXIT
export PUBLISH_BOOTSTRAP_FILE="$test_work/bootstrap-none.json"
printf '{"schemaVersion":1,"publications":[]}\n' >"$PUBLISH_BOOTSTRAP_FILE"
export CANONICAL_TEST_ROOT="$test_root" CANONICAL_TEST_WORK="$test_work"
# The signer the fixtures observe is the first commit the record approves for this consumer, so
# withdrawing a retired commit from the record does not strand the test on it.
CANONICAL_TEST_SHA="$(awk -F'\t' '$1==".github" && $2=="publish-manifests" {print $4; exit}' "$test_root/scripts/publish-workflow-canonical-approvals.tsv")"
[[ "$CANONICAL_TEST_SHA" =~ ^[0-9a-f]{40}$ ]] || { echo 'The record approves no canonical commit for .github' >&2; exit 1; }
export CANONICAL_TEST_SHA
mkdir -p "$test_work/bin"
cp "$test_root/scripts/publish-workflow-approved-revisions.tsv" "$test_work/seed.tsv"
# A first-publication consumer has no production observed row yet. Supply an
# independent synthetic observation for this unit fixture, never a deployment claim.
while IFS=$'\t' read -r consumer workflow _rest; do
  if ! awk -F'\t' -v consumer="$consumer" '$1==consumer {found=1} END {exit !found}' "$test_work/seed.tsv"; then
    template=$(awk -F'\t' -v consumer="$consumer" -v workflow="$workflow" 'BEGIN {OFS="\t"}
      NR>1 && $2==workflow {$1=consumer; print; exit}' "$test_work/seed.tsv")
    [[ -n "$template" ]] || { echo 'No observed-row fixture for the registered workflow' >&2; exit 1; }
    printf '%s\n' "$template" >>"$test_work/seed.tsv"
  fi
done < <("$test_root/scripts/report-publish-workflow-signing-revisions.sh" --list-consumers)
{
  head -n 1 "$test_work/seed.tsv"
  tail -n +2 "$test_work/seed.tsv" | LC_ALL=C sort
} >"$test_work/seed.sorted"
mv "$test_work/seed.sorted" "$test_work/seed.tsv"
# The approval guard must see the same simulated sources as this fixture's rows.
# Do not rewrite production matchers or infer applied evidence from its receipt.
mkdir -p "$test_work/consumers"
cp -R "$test_root/k8s" "$test_root/talos" "$test_work/consumers/"
export PUBLISH_CONSUMER_ROOT="$test_work/consumers"
APPROVED_REVISIONS_FILE="$test_work/seed.tsv" \
  bash "$test_root/scripts/write-publish-workflow-matchers.sh" >"$test_work/seed-matchers.log"
cat >"$test_work/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  'api repos/devantler-tech/.github/commits/v7.9.2 --jq .sha')
    printf '%040d\n' 1 ;;
  'api --method GET repos/devantler-tech/.github/actions/runs --raw-field branch=v7.9.2 --raw-field event=push --raw-field per_page=100')
    jq -n --arg sha "$(printf '%040d' 1)" --arg signer "$CANONICAL_TEST_SHA" \
      --arg variation "${CANONICAL_RUN_CASE:-complete}" \
      '{total_count:1,workflow_runs:[{head_branch:"v7.9.2",path:".github/workflows/cd.yaml",head_sha:$sha,conclusion:"success",referenced_workflows:[{path:("devantler-tech/.github/.github/workflows/publish-manifests.yaml@"+$signer),sha:$signer}]}]} |
       if $variation=="incomplete-page" then .total_count=101
       elif $variation=="ambiguous-family" then .workflow_runs[0].referenced_workflows += [{path:("devantler-tech/actions/.github/workflows/publish-manifests.yaml@"+$signer),sha:$signer}]
       elif $variation=="mismatched-ref" then .workflow_runs[0].referenced_workflows[0].sha=("8"*40)
       elif $variation=="unknown-family" then .workflow_runs[0].referenced_workflows[0].path=("other/repository/.github/workflows/publish-manifests.yaml@"+$signer)
       else . end' ;;
  *) printf 'Unexpected request: %s\n' "$*" >&2; exit 91 ;;
esac
STUB
cat >"$test_work/observe" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == .github ]]; then
  printf '7.9.2\tsha256:%064d\tdevantler-tech/.github/.github/workflows/publish-manifests.yaml@%s\n' 1 "$CANONICAL_TEST_SHA"
else
  awk -F'\t' -v repo="$1" '$1==repo {print $3 "\t" $4 "\t" $5}' "$CANONICAL_TEST_WORK/seed.tsv"
fi
STUB
cat >"$test_work/pin" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == .github ]]; then
  printf 'devantler-tech/.github/.github/workflows/publish-manifests.yaml@%s\n' "${CANONICAL_PIN_SHA:-$CANONICAL_TEST_SHA}"
else
  awk -F'\t' -v repo="$1" '$1==repo {print $6}' "$CANONICAL_TEST_WORK/seed.tsv"
fi
STUB
cat >"$test_work/release" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
awk -F'\t' -v workflow="$1" '$2==workflow {print $7; exit}' "$CANONICAL_TEST_WORK/seed.tsv"
STUB
chmod +x "$test_work/bin/gh" "$test_work/observe" "$test_work/pin" "$test_work/release"

answer="$(PATH="$test_work/bin:$PATH" bash -c 'source "$1"; signer_for_tag .github publish-manifests v7.9.2' bash "$test_root/scripts/generate-publish-workflow-approved-revisions.sh")"
[[ "$answer" == "devantler-tech/.github/.github/workflows/publish-manifests.yaml@$CANONICAL_TEST_SHA" ]] || {
  echo 'Canonical signing identity was lost' >&2; exit 1;
}

for variation in incomplete-page ambiguous-family mismatched-ref unknown-family; do
  if CANONICAL_RUN_CASE="$variation" PATH="$test_work/bin:$PATH" bash -c 'source "$1"; signer_for_tag .github publish-manifests v7.9.2' bash "$test_root/scripts/generate-publish-workflow-approved-revisions.sh" >"$test_work/answer" 2>"$test_work/refusal"; then
    echo "Unsafe publication evidence accepted: $variation" >&2; exit 1
  fi
  [[ ! -s "$test_work/answer" ]]
done

cp "$test_work/seed.tsv" "$test_work/approved.tsv"
run_generator() {
  APPROVED_REVISIONS_FILE="$test_work/approved.tsv" \
  APPROVED_REVISION_OBSERVER="$test_work/observe" APPROVED_REVISION_PIN_RESOLVER="$test_work/pin" \
  APPROVED_REVISION_RELEASE_RESOLVER="$test_work/release" \
    bash "$test_root/scripts/generate-publish-workflow-approved-revisions.sh"
}
run_generator
cmp "$test_work/seed.tsv" "$test_work/approved.tsv"
grep -qF "devantler-tech/.github/.github/workflows/publish-manifests.yaml@$CANONICAL_TEST_SHA" "$test_work/approved.identities.tsv"
cp "$test_work/approved.identities.tsv" "$test_work/identities-before"
run_generator
cmp "$test_work/identities-before" "$test_work/approved.identities.tsv"

# Every commit the record approves for this consumer is admitted as its current pin: while a
# publisher moves from one reviewed commit to the next, the record holds both (#4713).
approved_commits="$(awk -F'\t' '$1==".github" && $2=="publish-manifests" {print $4}' "$test_root/scripts/publish-workflow-canonical-approvals.tsv")"
[[ -n "$approved_commits" ]] || { echo 'The record approves no canonical commit for .github; the cases below would prove nothing' >&2; exit 1; }
while IFS= read -r approved_commit; do
  export CANONICAL_PIN_SHA="$approved_commit"
  if ! run_generator >"$test_work/admitted" 2>&1; then
    echo "Approved canonical pin $approved_commit was refused" >&2; cat "$test_work/admitted" >&2; exit 1
  fi
  grep -qF "devantler-tech/.github/.github/workflows/publish-manifests.yaml@$approved_commit" "$test_work/approved.identities.tsv"
  cp "$test_work/seed.tsv" "$test_work/approved.tsv"
  cp "$test_work/identities-before" "$test_work/approved.identities.tsv"
done <<<"$approved_commits"
unset CANONICAL_PIN_SHA

export CANONICAL_PIN_SHA=7777777777777777777777777777777777777777
if run_generator >"$test_work/refusal" 2>&1; then
  echo 'Unapproved canonical current pin was admitted' >&2; exit 1
fi
grep -q 'canonical publisher.*not approved' "$test_work/refusal"
cmp "$test_work/seed.tsv" "$test_work/approved.tsv"
cmp "$test_work/identities-before" "$test_work/approved.identities.tsv"
unset CANONICAL_PIN_SHA

# An unapproved canonical commit must not refresh either output.
export CANONICAL_TEST_SHA=7777777777777777777777777777777777777777
if run_generator >"$test_work/refusal" 2>&1; then
  echo 'Unapproved canonical signer was admitted' >&2; exit 1
fi
grep -q 'canonical publisher.*not approved' "$test_work/refusal"
cmp "$test_work/seed.tsv" "$test_work/approved.tsv"
cmp "$test_work/identities-before" "$test_work/approved.identities.tsv"
printf 'PASS: canonical signing identity, retained compatibility, idempotence, every approved pin and unapproved refusal\n'
