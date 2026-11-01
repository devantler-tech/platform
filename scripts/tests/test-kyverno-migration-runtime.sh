#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
workflow="$root/.github/workflows/publish-kyverno-migration-runtime.yaml"
dockerfile="$root/k8s/bases/infrastructure/controllers/kyverno/MigrationRuntime.Dockerfile"
patch="$root/k8s/bases/infrastructure/controllers/kyverno/migration-v1.19.2-runtime.patch"
fail() { printf 'FAIL: migration runtime: %s\n' "$1" >&2; exit 1; }
[[ -f "$workflow" && -f "$dockerfile" && -f "$patch" ]] || fail 'publisher inputs missing'
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
   "$(yq -r '.env.PATCHED_SOURCE_TREE' "$workflow")" == a21cf56e1e85e371753d0a6d4ca3e0a65e235dd2 &&
   "$(yq -r '.env.IMAGE' "$workflow")" == ghcr.io/devantler-tech/platform-kyverno-migrate ]] || fail 'source or destination changed'
# shellcheck disable=SC2016
[[ "$(yq -r '.env.IMAGE_TAG' "$workflow")" == 'v1.19.2-migration.1-${{ github.sha }}' ]] || fail 'immutable image tag missing'
while IFS= read -r action; do
  [[ "$action" =~ @[0-9a-f]{40}$ ]] || fail 'action must be commit-pinned'
done < <(yq -r '.jobs[].steps[]|select(has("uses"))|.uses' "$workflow")
for job in verify publish; do
  # shellcheck disable=SC2016
  JOB="$job" yq -e '
    [.jobs[strenv(JOB)].steps[] | select(."working-directory" == "upstream")
     | select((.run // "" | split("\n")
       | map(select(. == "test \"$(git write-tree)\" = \"${PATCHED_SOURCE_TREE}\""))
       | length) == 1)] | length == 1
  ' "$workflow" >/dev/null || fail "exact source tree proof missing in $job"
  # shellcheck disable=SC2016
  JOB="$job" yq -e '
    [.jobs[strenv(JOB)].steps[] | select(."working-directory" == "upstream")
     | select((.run // "" | split("\n")
       | map(select(. == "git tag v1.19.2"))
       | length) == 1)] | length == 1
  ' "$workflow" >/dev/null || fail "release tag binding missing in $job"
  JOB="$job" yq -e '[.jobs[strenv(JOB)].steps[] | select(.with.repository == "kyverno/kyverno" and .with.ref == "e63308f1e16c05cd009ac4864be6ec25c66b49be" and .with."persist-credentials" == false)] | length == 1' "$workflow" >/dev/null || fail 'upstream checkout not exact'
  JOB="$job" yq -e '[.jobs[strenv(JOB)].steps[] | select(.with.file == "./k8s/bases/infrastructure/controllers/kyverno/MigrationRuntime.Dockerfile" and .with.provenance == "mode=max" and .with.sbom == true)] | length == 1' "$workflow" >/dev/null || fail 'tested build provenance missing'
done
grep -Fq -- '--certificate-identity "https://github.com/devantler-tech/platform/.github/workflows/publish-kyverno-migration-runtime.yaml@refs/heads/main"' "$workflow" || fail 'publisher identity changed'
grep -Fq -- '--certificate-oidc-issuer "https://token.actions.githubusercontent.com"' "$workflow" || fail 'publisher issuer changed'
expected_headers='diff --git a/cmd/platform-migrate/main.go b/cmd/platform-migrate/main.go
diff --git a/cmd/platform-migrate/main_test.go b/cmd/platform-migrate/main_test.go
diff --git a/go.mod b/go.mod
diff --git a/go.sum b/go.sum'
[[ "$(grep '^diff --git ' "$patch")" == "$expected_headers" ]] || fail 'patch changes more than the scoped entry point and dependencies'
grep -Fxq 'FROM docker.io/library/golang:1.26.10-bookworm@sha256:23ce71337b726608afb1bc00986e630a05034a87a87ea1fc2cd1ae5cf33387eb AS builder' "$dockerfile" || fail 'patched compiler pin changed'
grep -Fxq 'ENV GOTOOLCHAIN=local' "$dockerfile" || fail 'implicit compiler switching enabled'
grep -Fq 'go list -mod=readonly -deps ./cmd/platform-migrate > /tmp/migration-packages' "$dockerfile" || fail 'actual dependency scope not observed'
grep -Fq "github.com/kyverno/kyverno/pkg/engine/|golang.org/x/crypto/openpgp|github.com/chrismellard/docker-credential-acr-env|go.etcd.io/etcd/client/" "$dockerfile" || fail 'unrelated capability boundary missing'
grep -Fxq 'RUN timeout 300s go test -mod=readonly -race ./cmd/platform-migrate ./cmd/cli/kubectl-kyverno/commands/migrate' "$dockerfile" || fail 'released migration and actual TLS API regressions missing'
grep -Fq 'go install golang.org/x/vuln/cmd/govulncheck@v1.8.0' "$dockerfile" || fail 'reviewed scanner missing'
grep -Fq 'timeout 300s govulncheck ./cmd/platform-migrate' "$dockerfile" || fail 'source scan missing'
grep -Fq 'timeout 120s govulncheck -mode=binary -format json /out/kubectl-kyverno > /tmp/binary-scan.json' "$dockerfile" || fail 'compiled binary scan missing'
grep -Fq "grep -Eq '\"scan_level\": *\"symbol\"' /tmp/binary-scan.json" "$dockerfile" || fail 'symbol-level scan proof missing'
grep -Fq 'timeout 120s govulncheck -mode=extract /out/kubectl-kyverno > /tmp/binary-extract.json' "$dockerfile" || fail 'symbol extraction proof missing'
grep -Fq "grep -Eq '\"pkgSymbols\": *\\[[^]]+\\]' /tmp/binary-extract.json" "$dockerfile" || fail 'non-empty symbol proof missing'
# shellcheck disable=SC2016
grep -Fq "test \"\$(tr '\\n' ' ' < /tmp/binary-findings)\" = 'GO-2025-3615 GO-2026-5337 GO-2026-5594 '" "$dockerfile" || fail 'accepted advisories changed'
# shellcheck disable=SC2016
grep -Fq 'test "$(go version /out/kubectl-kyverno)" = "/out/kubectl-kyverno: go1.26.10"' "$dockerfile" || fail 'actual compiler proof missing'
# shellcheck disable=SC2016
grep -Fq 'test "$(/out/kubectl-kyverno --version)" = "kubectl-kyverno version ${VERSION}"' "$dockerfile" || fail 'actual immutable executable identity missing'
grep -Fq 'unknown command "apply"' "$dockerfile" || fail 'unsupported-command refusal missing'
if grep -Eq -- '-ldflags="[^"]*-[sw][ "]' "$dockerfile"; then fail 'stripped symbols defeat the binary scan'; fi
grep -Fq 'github\.com/kyverno/kyverno[[:space:]]+v1\.19\.2[[:space:]]' "$dockerfile" || fail 'executable release identity proof missing'
grep -Fq 'vcs\.modified=false$' "$dockerfile" || fail 'clean source proof missing'
grep -Fq 'T main\.main$' "$dockerfile" || fail 'retained symbol proof missing'
grep -Fxq 'FROM reg.kyverno.io/kyverno/kyverno-cli:v1.19.2@sha256:322ec5044d600930cb3af0e56dc33154a2d1d1fba58f4cadad3f47be7e78b11c' "$dockerfile" || fail 'runtime release pin changed'
grep -Fxq 'USER 65534:65534' "$dockerfile" || fail 'migration Job execution identity changed'
grep -Fxq 'RUN ["/ko-app/kubectl-kyverno", "migrate", "--help"]' "$dockerfile" || fail 'real non-root runtime check missing'
grep -Fq 'scripts/tests/test-kyverno-migration-runtime.sh' "$root/.github/workflows/ci.yaml" || fail 'required CI does not run this guard'
printf 'Kyverno migration runtime publisher contract passed.\n'
