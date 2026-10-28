#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
workflow="$root/.github/workflows/publish-kyverno-background-hotfix.yaml"
patch="$root/k8s/bases/infrastructure/controllers/kyverno/background-v1.19.2-downstream-cleanup.patch"
dockerfile="$root/k8s/bases/infrastructure/controllers/kyverno/BackgroundDownstreamCleanup.Dockerfile"
fail() { printf 'FAIL: background hotfix: %s\n' "$1" >&2; exit 1; }
[[ -f "$workflow" && -f "$patch" && -f "$dockerfile" ]] || fail 'publisher inputs missing'
[[ "$(yq -r '.permissions|length' "$workflow")" == 0 ]] || fail 'deny permissions by default'
[[ "$(yq -r '.jobs.verify.permissions|length' "$workflow")" == 1 &&
   "$(yq -r '.jobs.verify.permissions.contents' "$workflow")" == read ]] || fail 'verification must remain read-only'
[[ "$(yq -r '.jobs.publish.permissions|length' "$workflow")" == 3 &&
   "$(yq -r '.jobs.publish.permissions.contents' "$workflow")" == read &&
   "$(yq -r '.jobs.publish.permissions.packages' "$workflow")" == write &&
   "$(yq -r '.jobs.publish.permissions."id-token"' "$workflow")" == write ]] || fail 'publisher permissions changed'
[[ "$(yq -r '.jobs.publish.if' "$workflow")" == "github.event_name == 'push' && github.ref == 'refs/heads/main'" &&
   "$(yq -r '.jobs.publish.needs' "$workflow")" == verify ]] || fail 'publish only tested main pushes'
[[ "$(yq -r '.env.SOURCE_COMMIT' "$workflow")" == e63308f1e16c05cd009ac4864be6ec25c66b49be &&
   "$(yq -r '.env.PATCHED_SOURCE_TREE' "$workflow")" == f800e2778a0fdf3e724757591857fb332d45698a &&
   "$(yq -r '.env.IMAGE' "$workflow")" == ghcr.io/devantler-tech/platform-kyverno-background ]] || fail 'source or destination changed'
# This expression belongs to GitHub, not the shell.
# shellcheck disable=SC2016
[[ "$(yq -r '.env.IMAGE_TAG' "$workflow")" == 'v1.19.2-downstream-cleanup.1-${{ github.sha }}' ]] || fail 'immutable source tag missing'
while IFS= read -r action; do
  [[ "$action" =~ @[0-9a-f]{40}$ ]] || fail 'action must be commit-pinned'
done < <(yq -r '.jobs[].steps[]|select(has("uses"))|.uses' "$workflow")
for job in verify publish; do
  # Match an executable line rather than a comment or a check in the other job.
  # shellcheck disable=SC2016
  JOB="$job" yq -e '
    [.jobs[strenv(JOB)].steps[] | select(."working-directory" == "upstream")
     | select((.run // "" | split("\n")
       | map(select(. == "test \"$(git write-tree)\" = \"${PATCHED_SOURCE_TREE}\""))
       | length) == 1)] | length == 1
  ' "$workflow" >/dev/null || fail "patched source tree check missing or duplicated in $job"
  JOB="$job" yq -e '[.jobs[strenv(JOB)].steps[] | select(.with.repository == "kyverno/kyverno" and .with.ref == "e63308f1e16c05cd009ac4864be6ec25c66b49be" and .with."persist-credentials" == false)] | length == 1' "$workflow" >/dev/null || fail 'upstream checkout not exact'
  JOB="$job" yq -e '[.jobs[strenv(JOB)].steps[] | select(.with.file == "./k8s/bases/infrastructure/controllers/kyverno/BackgroundDownstreamCleanup.Dockerfile" and .with.provenance == "mode=max" and .with.sbom == true)] | length == 1' "$workflow" >/dev/null || fail 'tested build provenance missing'
done
grep -Fq -- '--certificate-identity "https://github.com/devantler-tech/platform/.github/workflows/publish-kyverno-background-hotfix.yaml@refs/heads/main"' "$workflow" || fail 'publisher certificate identity changed'
grep -Fq -- '--certificate-oidc-issuer "https://token.actions.githubusercontent.com"' "$workflow" || fail 'publisher issuer changed'
expected_headers='diff --git a/pkg/background/gpol/downstream_absent_test.go b/pkg/background/gpol/downstream_absent_test.go
diff --git a/pkg/background/gpol/dynamic_watcher.go b/pkg/background/gpol/dynamic_watcher.go'
[[ "$(grep '^diff --git ' "$patch")" == "$expected_headers" ]] || fail 'patch must contain only the reviewed implementation and regressions'
grep -Fq 'go test -mod=readonly -race ./pkg/background/...' "$dockerfile" || fail 'real affected package race tests missing'
grep -Fq 'FROM reg.kyverno.io/kyverno/background-controller:v1.19.2@sha256:c46e124f21ef8a6bb4b48de57106808b1007edcd5ddd4ff7df5d23b9efa60ce2' "$dockerfile" || fail 'runtime release pin changed'
grep -Fq 'RUN ["/ko-app/background-controller", "--help"]' "$dockerfile" || fail 'real executable check missing'
grep -Fq 'scripts/tests/test-kyverno-background-hotfix.sh' "$root/.github/workflows/ci.yaml" || fail 'required CI does not run this guard'
printf 'Kyverno background compatibility publisher contract passed.\n'
