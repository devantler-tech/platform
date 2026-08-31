#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly workflow="${root_dir}/.github/workflows/publish-kubescape-storage-hotfix.yaml"
readonly patch_file="${root_dir}/k8s/bases/infrastructure/controllers/kubescape/storage-v0.0.297-sqlite-contention.patch"
readonly helm_release="${root_dir}/k8s/bases/infrastructure/controllers/kubescape/helm-release.yaml"
readonly source_commit='b35788b68337134fc2514574cde1ba7f1225fd43'
readonly image_repository='ghcr.io/devantler-tech/platform-kubescape-storage'
readonly image_tag='v0.0.297-sqlite-contention.4-c674d63c1c391b47839a4a6246945b70ddfe1c01@sha256:e21df062e2c3598fec5b8c2170c5cc68ceeceb412379c3aef986b42c9a46241e'

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
[[ "$(yq -er '.env.IMAGE_TAG' "${workflow}")" == 'v0.0.297-sqlite-contention.4-${{ github.sha }}' ]] ||
  fail 'the workflow image tag must identify the short-transaction revision'

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
# Identity is the action path; the commit pin is asserted separately below. Binding identity to one
# specific SHA made every reviewed version bump read as a missing step, so dependency automation
# could never update these actions and a present, correctly pinned step was reported as absent.
readonly buildx_action='docker/setup-buildx-action'
readonly build_action='docker/build-push-action'

step_count() {
  ACTION="$1" yq -er '
    [.jobs.publish.steps[] | select((.uses // "") | split("@") | .[0] == strenv(ACTION))] | length
  ' "${workflow}"
}

step_ref() {
  ACTION="$1" yq -er '
    .jobs.publish.steps[] |
    select((.uses // "") | split("@") | .[0] == strenv(ACTION)) |
    (.uses // "") | split("@") | .[1] // "unpinned"
  ' "${workflow}"
}

step_index() {
  ACTION="$1" yq -er '
    .jobs.publish.steps |
    to_entries |
    .[] |
    select((.value.uses // "") | split("@") | .[0] == strenv(ACTION)) |
    .key
  ' "${workflow}"
}

buildx_count="$(step_count "${buildx_action}")" ||
  fail 'could not inspect the publish job Buildx setup'
readonly buildx_count
[[ "${buildx_count}" == '1' ]] ||
  fail 'the publish job must contain exactly one Buildx setup step'

buildx_ref="$(step_ref "${buildx_action}")" ||
  fail 'could not read the publish Buildx setup pin'
readonly buildx_ref
[[ "${buildx_ref}" =~ ^[0-9a-f]{40}$ ]] ||
  fail 'the publish Buildx setup step must be pinned to a commit SHA, not a mutable tag'

build_count="$(step_count "${build_action}")" ||
  fail 'could not inspect the publish job image build'
readonly build_count
[[ "${build_count}" == '1' ]] ||
  fail 'the publish job must contain exactly one image-build step'

build_ref="$(step_ref "${build_action}")" ||
  fail 'could not read the publish image-build pin'
readonly build_ref
[[ "${build_ref}" =~ ^[0-9a-f]{40}$ ]] ||
  fail 'the publish image-build step must be pinned to a commit SHA, not a mutable tag'

buildx_driver="$(ACTION="${buildx_action}" yq -er '
  .jobs.publish.steps[] |
  select((.uses // "") | split("@") | .[0] == strenv(ACTION)) |
  .with.driver
' "${workflow}")" || fail 'the publish Buildx step has no explicit driver'
readonly buildx_driver
[[ "${buildx_driver}" == 'docker-container' ]] ||
  fail 'the publish Buildx step must select the attestation-capable container driver'

buildx_index="$(step_index "${buildx_action}")" ||
  fail 'could not locate the publish Buildx setup step'
readonly buildx_index
image_build_index="$(step_index "${build_action}")" ||
  fail 'could not locate the publish image-build step'
readonly image_build_index
((buildx_index < image_build_index)) ||
  fail 'the Buildx container driver must be active before the attested image build'
# IMAGE/DIGEST must expand in the workflow, not here.
# shellcheck disable=SC2016
grep -qF 'cosign sign --yes "${IMAGE}@${DIGEST}"' "${workflow}" ||
  fail 'the published compatibility image must be keylessly signed by digest'

[[ "$(grep -c '^diff --git ' "${patch_file}")" == '6' ]] ||
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
grep -qF 'diff --git a/pkg/registry/file/containerprofile_aggregator_test.go b/pkg/registry/file/containerprofile_aggregator_test.go' "${patch_file}" ||
  fail 'the compatibility patch lacks a cross-artifact transaction regression seam'
grep -qF 'diff --git a/pkg/registry/file/containerprofile_storage_test.go b/pkg/registry/file/containerprofile_storage_test.go' "${patch_file}" ||
  fail 'the compatibility patch lacks a foreground-writer regression test'
grep -qF 'Workers:                 1' "${patch_file}" ||
  fail 'container-profile maintenance must use one background writer'
grep -qF 'TestNewContainerProfileProcessorUsesOneMaintenanceWriter' "${patch_file}" ||
  fail 'the compatibility patch must prove background writer concurrency is bounded'
grep -qF 'TestBeginTransactionLeavesWriterAvailableDuringReadPhase' "${patch_file}" ||
  fail 'the compatibility patch must prove profile reads do not reserve SQLite write access'
grep -qF 'TestConsolidateKeyDoesNotHoldWriterAcrossProfileProcessing' "${patch_file}" ||
  fail 'the compatibility patch must prove maintenance does not hold one transaction across artifact processing'
grep -qF 'TestUpdateProfileKeepsTimeSeriesPendingUntilAllProfileWritesSucceed' "${patch_file}" ||
  fail 'the compatibility patch must prove every partial profile-write boundary remains retryable'
grep -qF 'source rows must be retired only after every profile write succeeds' "${patch_file}" ||
  fail 'time-series observations must stay pending until all derived writes succeed'
grep -qF 'the next maintenance tick must replay and converge after the failed write clears' "${patch_file}" ||
  fail 'the partial-write failpoints must prove replay convergence, not only row retention'
grep -qF 'finalize only the source rows in one short transaction' "${patch_file}" ||
  fail 'source-row retirement must remain atomic without spanning derived profile writes'
grep -qF 'persistence can run between maintenance writes' "${patch_file}" ||
  fail 'container-profile maintenance must release SQLite between idempotent artifact writes'
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
