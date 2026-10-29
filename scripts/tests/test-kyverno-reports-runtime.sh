#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
workflow="$root/.github/workflows/publish-kyverno-reports-hotfix.yaml"
dockerfile="$root/k8s/bases/infrastructure/controllers/kyverno/ReportsWatchPlan.Dockerfile"
patch="$root/k8s/bases/infrastructure/controllers/kyverno/reports-v1.19.2-watch-plan.patch"
fail() { printf 'FAIL: reports runtime: %s\n' "$1" >&2; exit 1; }
[[ "$(yq -r '.permissions|length' "$workflow")" == 0 ]] || fail 'deny permissions by default'
[[ "$(yq -r '.jobs.verify.permissions|length' "$workflow")" == 1 &&
   "$(yq -r '.jobs.verify.permissions.contents' "$workflow")" == read ]] || fail 'verification must remain read-only'
[[ "$(yq -r '.jobs.publish.permissions|length' "$workflow")" == 3 &&
   "$(yq -r '.jobs.publish.permissions.contents' "$workflow")" == read &&
   "$(yq -r '.jobs.publish.permissions.packages' "$workflow")" == write &&
   "$(yq -r '.jobs.publish.permissions."id-token"' "$workflow")" == write ]] || fail 'publisher permissions changed'
[[ "$(yq -r '.jobs.publish.if' "$workflow")" == "github.event_name == 'push' && github.ref == 'refs/heads/main'" &&
   "$(yq -r '.jobs.publish.needs' "$workflow")" == verify ]] || fail 'only tested main pushes may publish'
[[ "$(yq -r '.env.SOURCE_COMMIT' "$workflow")" == e63308f1e16c05cd009ac4864be6ec25c66b49be &&
   "$(yq -r '.env.PATCHED_SOURCE_TREE' "$workflow")" == 23a55f74d9147dfefe6be994600ad52daeb8ee11 &&
   "$(yq -r '.env.IMAGE' "$workflow")" == ghcr.io/devantler-tech/platform-kyverno-reports ]] || fail 'source or destination changed'
# shellcheck disable=SC2016
[[ "$(yq -r '.env.IMAGE_TAG' "$workflow")" == 'v1.19.2-watch-plan.2-${{ github.sha }}' ]] || fail 'immutable image tag missing'
while IFS= read -r action; do
  [[ "$action" =~ @[0-9a-f]{40}$ ]] || fail 'action must be commit-pinned'
done < <(yq -r '.jobs[].steps[]|select(has("uses"))|.uses' "$workflow")
for job in verify publish; do
  # Match executable source proof in each job independently.
  # shellcheck disable=SC2016
  JOB="$job" yq -e '
    [.jobs[strenv(JOB)].steps[] | select(."working-directory" == "upstream")
     | select((.run // "" | split("\n")
       | map(select(. == "test \"$(git write-tree)\" = \"${PATCHED_SOURCE_TREE}\""))
       | length) == 1)] | length == 1
  ' "$workflow" >/dev/null || fail "exact source tree proof missing in $job"
  JOB="$job" yq -e '[.jobs[strenv(JOB)].steps[] | select(.with.repository == "kyverno/kyverno" and .with.ref == "e63308f1e16c05cd009ac4864be6ec25c66b49be" and .with."persist-credentials" == false)] | length == 1' "$workflow" >/dev/null || fail 'upstream checkout not exact'
  JOB="$job" yq -e '[.jobs[strenv(JOB)].steps[] | select(.with.file == "./k8s/bases/infrastructure/controllers/kyverno/ReportsWatchPlan.Dockerfile" and .with.provenance == "mode=max" and .with.sbom == true)] | length == 1' "$workflow" >/dev/null || fail 'tested build provenance missing'
done
grep -Fq -- '--certificate-identity "https://github.com/devantler-tech/platform/.github/workflows/publish-kyverno-reports-hotfix.yaml@refs/heads/main"' "$workflow" || fail 'publisher identity changed'
grep -Fq -- '--certificate-oidc-issuer "https://token.actions.githubusercontent.com"' "$workflow" || fail 'publisher issuer changed'
expected_headers='diff --git a/cmd/internal/registry_security_test.go b/cmd/internal/registry_security_test.go
diff --git a/go.mod b/go.mod
diff --git a/go.sum b/go.sum
diff --git a/pkg/controllers/report/resource/controller.go b/pkg/controllers/report/resource/controller.go
diff --git a/pkg/controllers/report/resource/controller_test.go b/pkg/controllers/report/resource/controller_test.go
diff --git a/pkg/controllers/report/resource/watch_kinds.go b/pkg/controllers/report/resource/watch_kinds.go
diff --git a/pkg/controllers/report/resource/watch_plan_test.go b/pkg/controllers/report/resource/watch_plan_test.go
diff --git a/third_party/docker-credential-acr-env/COMPATIBILITY.md b/third_party/docker-credential-acr-env/COMPATIBILITY.md
diff --git a/third_party/docker-credential-acr-env/LICENSE b/third_party/docker-credential-acr-env/LICENSE
diff --git a/third_party/docker-credential-acr-env/go.mod b/third_party/docker-credential-acr-env/go.mod
diff --git a/third_party/docker-credential-acr-env/pkg/credhelper/helper.go b/third_party/docker-credential-acr-env/pkg/credhelper/helper.go
diff --git a/third_party/docker-credential-acr-env/pkg/credhelper/helper_test.go b/third_party/docker-credential-acr-env/pkg/credhelper/helper_test.go
diff --git a/third_party/docker-credential-acr-env/pkg/credhelper/host_boundary_test.go b/third_party/docker-credential-acr-env/pkg/credhelper/host_boundary_test.go
diff --git a/third_party/docker-credential-acr-env/pkg/registry/const.go b/third_party/docker-credential-acr-env/pkg/registry/const.go
diff --git a/third_party/docker-credential-acr-env/pkg/registry/registry.go b/third_party/docker-credential-acr-env/pkg/registry/registry.go
diff --git a/third_party/docker-credential-acr-env/pkg/token/token.go b/third_party/docker-credential-acr-env/pkg/token/token.go'
[[ "$(grep '^diff --git ' "$patch")" == "$expected_headers" ]] || fail 'patch changes unreviewed paths'
grep -Fxq 'FROM docker.io/library/golang:1.26.10-bookworm@sha256:23ce71337b726608afb1bc00986e630a05034a87a87ea1fc2cd1ae5cf33387eb AS builder' "$dockerfile" || fail 'patched compiler pin changed'
grep -Fxq 'ENV GOTOOLCHAIN=local' "$dockerfile" || fail 'implicit compiler switching enabled'
grep -Fxq 'RUN timeout 900s go test -mod=readonly -race ./pkg/controllers/report/resource ./pkg/controllers/report/aggregate ./pkg/webhooks/resource/validation' "$dockerfile" || fail 'controller race regressions changed'
grep -Fxq '    && timeout 900s golangci-lint run --timeout=14m' "$dockerfile" || fail 'full lint gate changed'
grep -Fq './cmd/internal/... ./pkg/image/... ./pkg/tracing/... ./pkg/metrics/...' "$dockerfile" || fail 'runtime dependency regressions missing'
grep -Fq 'github.com/chrismellard/docker-credential-acr-env/pkg/credhelper' "$dockerfile" || fail 'legacy credential boundary regressions missing'
# Keep the actual artifact's symbol table for precise executable vulnerability analysis.
# shellcheck disable=SC2016
grep -Fq -- '-ldflags="-w -X github.com/kyverno/kyverno/pkg/version.BuildVersion=${VERSION}"' "$dockerfile" || fail 'executable symbol metadata must be preserved'
# shellcheck disable=SC2016
grep -Fq 'test "$(go version /out/reports-controller)" = "/out/reports-controller: go1.26.10"' "$dockerfile" || fail 'actual compiler proof missing'
grep -Fq "^golang.org/x/crypto/openpgp(/|$)" "$dockerfile" || fail 'actual unsafe dependency refusal missing'
grep -Fq 'go install golang.org/x/vuln/cmd/govulncheck@v1.8.0' "$dockerfile" || fail 'pinned vulnerability scanner changed'
grep -Fq 'timeout 300s govulncheck ./cmd/reports-controller' "$dockerfile" || fail 'source vulnerability scan missing'
grep -Fq 'timeout 120s govulncheck -mode=binary /out/reports-controller' "$dockerfile" || fail 'executable vulnerability scan missing'
grep -Fxq 'FROM reg.kyverno.io/kyverno/reports-controller:v1.19.2@sha256:5fb6d3e738050dbce84f60ecabaede7665fbe0578d07c1e8c912443b035e79c3' "$dockerfile" || fail 'runtime release pin changed'
grep -Fxq 'RUN ["/ko-app/reports-controller", "--help"]' "$dockerfile" || fail 'actual runtime help check missing'
grep -Fq 'scripts/tests/test-kyverno-reports-runtime.sh' "$root/.github/workflows/ci.yaml" || fail 'required CI does not run this guard'
printf 'Kyverno reports runtime publisher contract passed.\n'
