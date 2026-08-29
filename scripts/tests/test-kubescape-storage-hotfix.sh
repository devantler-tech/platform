#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly workflow="${root_dir}/.github/workflows/publish-kubescape-storage-hotfix.yaml"
readonly patch_file="${root_dir}/k8s/bases/infrastructure/controllers/kubescape/storage-v0.0.297-sqlite-contention.patch"
readonly helm_release="${root_dir}/k8s/bases/infrastructure/controllers/kubescape/helm-release.yaml"
readonly source_commit='b35788b68337134fc2514574cde1ba7f1225fd43'
readonly image_repository='ghcr.io/devantler-tech/platform-kubescape-storage'
readonly image_tag='v0.0.297-sqlite-contention.2-d6c1111e8c7cc34aab21d55441b0cd5c50e9bbf4@sha256:c20facb86f1ceba7d1769c1bdc383e89a2d9d4f49dc176ef59932d7ec2c6bd01'

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v yq >/dev/null 2>&1 || fail 'yq v4 is required'
[[ -f "${workflow}" ]] || fail 'the Kubescape storage compatibility-image workflow is missing'
[[ -f "${patch_file}" ]] || fail 'the Kubescape storage compatibility patch is missing'

[[ "$(yq -er '.permissions | length' "${workflow}")" == '0' ]] ||
  fail 'the hotfix workflow must deny permissions by default'
[[ "$(yq -er '.jobs.publish.permissions.contents' "${workflow}")" == 'read' ]] ||
  fail 'the publish job needs only read access to repository contents'
[[ "$(yq -er '.jobs.publish.permissions.packages' "${workflow}")" == 'write' ]] ||
  fail 'the publish job must scope package write permission to itself'
[[ "$(yq -er '.jobs.publish.permissions."id-token"' "${workflow}")" == 'write' ]] ||
  fail 'the publish job must scope OIDC signing permission to itself'

[[ "$(yq -er '.env.KUBESCAPE_STORAGE_SOURCE_COMMIT' "${workflow}")" == "${source_commit}" ]] ||
  fail 'the workflow must pin the reviewed v0.0.297 source commit'
[[ "$(yq -er '.env.IMAGE' "${workflow}")" == "${image_repository}" ]] ||
  fail 'the workflow image destination drifted'
# The literal GitHub expression is the contract.
# shellcheck disable=SC2016
[[ "$(yq -er '.env.IMAGE_TAG' "${workflow}")" == 'v0.0.297-sqlite-contention.3-${{ github.sha }}' ]] ||
  fail 'the workflow image tag must identify the foreground-safe transaction revision'

grep -qF 'repository: kubescape/storage' "${workflow}" ||
  fail 'the workflow must check out the upstream storage source explicitly'
# The literal GitHub expression is the contract.
# shellcheck disable=SC2016
grep -qF 'ref: ${{ env.KUBESCAPE_STORAGE_SOURCE_COMMIT }}' "${workflow}" ||
  fail 'the upstream checkout must use the exact pinned commit'
grep -qF 'persist-credentials: false' "${workflow}" ||
  fail 'workflow checkouts must not persist credentials'
grep -qF 'git apply --check' "${workflow}" ||
  fail 'the compatibility patch must be checked before application'
grep -qF 'go test ./pkg/registry/file' "${workflow}" ||
  fail 'the patched upstream storage package must run its tests before publish'
readonly buildx_action='docker/setup-buildx-action@bb05f3f5519dd87d3ba754cc423b652a5edd6d2c'
readonly build_action='docker/build-push-action@53b7df96c91f9c12dcc8a07bcb9ccacbed38856a'
buildx_count="$(BUILD_ACTION="${buildx_action}" yq -er '
  [.jobs.publish.steps[] | select(.uses == strenv(BUILD_ACTION))] | length
' "${workflow}")" || fail 'could not inspect the publish job Buildx setup'
readonly buildx_count
[[ "${buildx_count}" == '1' ]] ||
  fail 'the publish job must contain exactly one pinned Buildx setup step'

buildx_driver="$(BUILD_ACTION="${buildx_action}" yq -er '
  .jobs.publish.steps[] |
  select(.uses == strenv(BUILD_ACTION)) |
  .with.driver
' "${workflow}")" || fail 'the publish Buildx step has no explicit driver'
readonly buildx_driver
[[ "${buildx_driver}" == 'docker-container' ]] ||
  fail 'the publish Buildx step must select the attestation-capable container driver'

buildx_index="$(BUILD_ACTION="${buildx_action}" yq -er '
  .jobs.publish.steps |
  to_entries |
  .[] |
  select(.value.uses == strenv(BUILD_ACTION)) |
  .key
' "${workflow}")" || fail 'could not locate the publish Buildx setup step'
readonly buildx_index
image_build_index="$(BUILD_ACTION="${build_action}" yq -er '
  .jobs.publish.steps |
  to_entries |
  .[] |
  select(.value.uses == strenv(BUILD_ACTION)) |
  .key
' "${workflow}")" || fail 'could not locate the publish image-build step'
readonly image_build_index
((buildx_index < image_build_index)) ||
  fail 'the Buildx container driver must be active before the attested image build'
# IMAGE/DIGEST must expand in the workflow, not here.
# shellcheck disable=SC2016
grep -qF 'cosign sign --yes "${IMAGE}@${DIGEST}"' "${workflow}" ||
  fail 'the published compatibility image must be keylessly signed by digest'

[[ "$(grep -c '^diff --git ' "${patch_file}")" == '5' ]] ||
  fail 'the compatibility patch must touch only implementation and regression-test files'
grep -qF 'diff --git a/pkg/registry/file/sqlite.go b/pkg/registry/file/sqlite.go' "${patch_file}" ||
  fail 'the compatibility patch does not modify SQLite pool setup'
grep -qF 'diff --git a/pkg/registry/file/sqlite_test.go b/pkg/registry/file/sqlite_test.go' "${patch_file}" ||
  fail 'the compatibility patch lacks an executable busy-timeout regression test'
grep -qF 'conn.SetBusyTimeout(60 * time.Second)' "${patch_file}" ||
  fail 'the compatibility patch must apply the upstream-reviewed 60-second timeout'
grep -qF 'assert.Equal(t, int64(60000), busyTimeoutMilliseconds)' "${patch_file}" ||
  fail 'the compatibility patch must prove the effective SQLite timeout'
grep -qF 'diff --git a/pkg/registry/file/containerprofile_processor.go b/pkg/registry/file/containerprofile_processor.go' "${patch_file}" ||
  fail 'the compatibility patch does not bound background SQLite writers'
grep -qF 'diff --git a/pkg/registry/file/containerprofile_processor_test.go b/pkg/registry/file/containerprofile_processor_test.go' "${patch_file}" ||
  fail 'the compatibility patch lacks a background-writer regression test'
grep -qF 'diff --git a/pkg/registry/file/containerprofile_storage_test.go b/pkg/registry/file/containerprofile_storage_test.go' "${patch_file}" ||
  fail 'the compatibility patch lacks a foreground-writer regression test'
grep -qF 'Workers:                 1' "${patch_file}" ||
  fail 'container-profile maintenance must use one background writer'
grep -qF 'TestNewContainerProfileProcessorUsesOneMaintenanceWriter' "${patch_file}" ||
  fail 'the compatibility patch must prove background writer concurrency is bounded'
grep -qF 'TestBeginTransactionLeavesWriterAvailableDuringReadPhase' "${patch_file}" ||
  fail 'the compatibility patch must prove profile reads do not reserve SQLite write access'
if grep -qF 'ImmediateTransaction' "${patch_file}"; then
  fail 'read-heavy profile transactions must not reserve SQLite write access before their write phase'
fi

storage_repository="$(yq -er '
  .spec.values.storage.image.repository |
  select(tag == "!!str" and length > 0)
' "${helm_release}")" || fail 'the deployed Kubescape storage image repository is missing'
readonly storage_repository
[[ "${storage_repository}" == "${image_repository}" ]] ||
  fail 'the deployed Kubescape storage image must use the reviewed compatibility repository'

storage_tag="$(yq -er '
  .spec.values.storage.image.tag |
  select(tag == "!!str" and length > 0)
' "${helm_release}")" || fail 'the deployed Kubescape storage image tag is missing'
readonly storage_tag
[[ "${storage_tag}" == "${image_tag}" ]] ||
  fail 'the deployed compatibility image must match the signed immutable tag and digest'

printf 'Kubescape storage compatibility-image contract is valid.\n'
