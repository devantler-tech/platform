#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly workflow="${root_dir}/.github/workflows/publish-kubescape-node-agent-hotfix.yaml"
readonly patch_file="${root_dir}/k8s/bases/infrastructure/controllers/kubescape/node-agent-v0.3.219-alert-classification.patch"
readonly source_commit='4956ea26aabadb9daff42c4717842dd7bda26aa3'
readonly upstream_image='quay.io/kubescape/node-agent:v0.3.219@sha256:2044ed750f5e20a7c5150479963e2b33108c304a40d526c0de7ed412495467c6'
readonly image_repository='ghcr.io/devantler-tech/platform-kubescape-node-agent'

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v yq >/dev/null 2>&1 || fail 'yq v4 is required'
[[ -f "${workflow}" ]] || fail 'the Kubescape node-agent compatibility-image workflow is missing'
[[ -f "${patch_file}" ]] || fail 'the Kubescape node-agent compatibility patch is missing'

[[ "$(yq -er '.permissions | length' "${workflow}")" == '0' ]] ||
  fail 'the hotfix workflow must deny permissions by default'
[[ "$(yq -er '.jobs.publish.permissions.contents' "${workflow}")" == 'read' ]] ||
  fail 'the publish job needs only read access to repository contents'
[[ "$(yq -er '.jobs.publish.permissions.packages' "${workflow}")" == 'write' ]] ||
  fail 'the publish job must scope package write permission to itself'
[[ "$(yq -er '.jobs.publish.permissions."id-token"' "${workflow}")" == 'write' ]] ||
  fail 'the publish job must scope OIDC signing permission to itself'

[[ "$(yq -er '.env.KUBESCAPE_NODE_AGENT_SOURCE_COMMIT' "${workflow}")" == "${source_commit}" ]] ||
  fail 'the workflow must pin the reviewed v0.3.219 source commit'
[[ "$(yq -er '.env.UPSTREAM_IMAGE' "${workflow}")" == "${upstream_image}" ]] ||
  fail 'the workflow must extract tracers only from the immutable upstream image'
[[ "$(yq -er '.env.IMAGE' "${workflow}")" == "${image_repository}" ]] ||
  fail 'the workflow image destination drifted'
# The literal GitHub expression is the contract.
# shellcheck disable=SC2016
[[ "$(yq -er '.env.IMAGE_TAG' "${workflow}")" == 'v0.3.219-alerts.1-${{ github.sha }}' ]] ||
  fail 'the workflow image tag must identify the reviewed alert-classification revision'

grep -qF 'repository: kubescape/node-agent' "${workflow}" ||
  fail 'the workflow must check out the upstream node-agent source explicitly'
# shellcheck disable=SC2016
grep -qF 'ref: ${{ env.KUBESCAPE_NODE_AGENT_SOURCE_COMMIT }}' "${workflow}" ||
  fail 'the upstream checkout must use the exact pinned commit'
# These variables must expand in the workflow, not in this contract test.
# shellcheck disable=SC2016
grep -qF 'docker create --platform linux/amd64 "${UPSTREAM_IMAGE}"' "${workflow}" ||
  fail 'the workflow must source tracers from the pinned upstream image'
# shellcheck disable=SC2016
grep -qF 'docker cp "${container_id}:/root/tracers.tar" tracers.tar' "${workflow}" ||
  fail 'the workflow must copy the upstream tracer bundle into the build context'
grep -qF 'go test -exec sudo ./pkg/containerprofilemanager/v1 ./pkg/objectcache/containerprofilecache ./pkg/rulemanager ./pkg/sbommanager/v1' "${workflow}" ||
  fail 'all patched node-agent packages must run tests before publish'
# shellcheck disable=SC2016
grep -qF 'cosign sign --yes "${IMAGE}@${DIGEST}"' "${workflow}" ||
  fail 'the published compatibility image must be keylessly signed by digest'

[[ "$(grep -c '^diff --git ' "${patch_file}")" == '6' ]] ||
  fail 'the compatibility patch must touch only the four implementations and two regression tests'
grep -qF 'func shouldLogSBOMSaveError(err error) bool' "${patch_file}" ||
  fail 'SBOM storage errors need an explicit severity classifier'
grep -qF 'func shouldLogSBOMGenerationError(err error) bool' "${patch_file}" ||
  fail 'SBOM scanner errors need an explicit lifecycle-race classifier'
grep -qF 'TestShouldLogSBOMSaveError' "${patch_file}" ||
  fail 'the compatibility patch lacks the conflict-classification regression test'
grep -qF 'TestShouldLogSBOMGenerationError' "${patch_file}" ||
  fail 'the compatibility patch lacks the vanished-filesystem regression test'
grep -qF 'Warning("timeout while adding container to the container profile manager"' "${patch_file}" ||
  fail 'bounded container-profile setup timeouts must remain visible without paging'
grep -qF 'Warning("timeout while adding container to the container-profile cache"' "${patch_file}" ||
  fail 'bounded cache setup timeouts must remain visible without paging'
grep -qF 'Warning("RuleManager - container exited before shared data became ready"' "${patch_file}" ||
  fail 'completed-container rule setup races must remain visible without paging'
grep -qF 'Error("RuleManager - failed to get shared container data"' "${patch_file}" ||
  fail 'unexpected live-container rule setup failures must remain errors'
grep -qF 'TestContainerExited' "${patch_file}" ||
  fail 'the compatibility patch lacks a lifecycle-channel regression test'

printf 'Kubescape node-agent compatibility-image contract is valid.\n'
