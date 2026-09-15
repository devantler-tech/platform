#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly workflow="${root_dir}/.github/workflows/publish-coroot-node-agent-hotfix.yaml"
readonly patch_file="${root_dir}/k8s/bases/infrastructure/coroot/node-agent-v1.35.8-alert-remediation.patch"
readonly dockerfile="${root_dir}/k8s/bases/infrastructure/coroot/CorootNodeAgent.Dockerfile"
readonly source_commit='ee62018e73b90b1549f5b81ba0d3d6e28851a1df'
readonly upstream_image='ghcr.io/coroot/coroot-node-agent:1.35.8@sha256:a08143e4ea42420d4d8b8b2cd21f67dc76743d6e0e37ab9a97b304171e670b93'
readonly image_repository='ghcr.io/devantler-tech/platform-coroot-node-agent'

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v yq >/dev/null 2>&1 || fail 'yq v4 is required'
[[ -f "${workflow}" ]] || fail 'the Coroot node-agent compatibility-image workflow is missing'
[[ -f "${patch_file}" ]] || fail 'the Coroot node-agent compatibility patch is missing'
[[ -f "${dockerfile}" ]] || fail 'the Coroot node-agent compatibility Dockerfile is missing'

[[ "$(yq -er '.permissions | length' "${workflow}")" == '0' ]] ||
  fail 'the hotfix workflow must deny permissions by default'
[[ "$(yq -er '.jobs.publish.permissions.contents' "${workflow}")" == 'read' ]] ||
  fail 'the publish job needs only read access to repository contents'
[[ "$(yq -er '.jobs.publish.permissions.packages' "${workflow}")" == 'write' ]] ||
  fail 'the publish job must scope package write permission to itself'
[[ "$(yq -er '.jobs.publish.permissions."id-token"' "${workflow}")" == 'write' ]] ||
  fail 'the publish job must scope OIDC signing permission to itself'

[[ "$(yq -er '.env.COROOT_NODE_AGENT_SOURCE_COMMIT' "${workflow}")" == "${source_commit}" ]] ||
  fail 'the workflow must pin the reviewed Coroot node-agent v1.35.8 source commit'
[[ "$(yq -er '.env.UPSTREAM_IMAGE' "${workflow}")" == "${upstream_image}" ]] ||
  fail 'the workflow must use the immutable upstream v1.35.8 runtime image'
[[ "$(yq -er '.env.IMAGE' "${workflow}")" == "${image_repository}" ]] ||
  fail 'the workflow image destination drifted'
# The literal GitHub expression is the contract.
# shellcheck disable=SC2016
[[ "$(yq -er '.env.IMAGE_TAG' "${workflow}")" == 'v1.35.8-alerts.1-${{ github.sha }}' ]] ||
  fail 'the image tag must identify the reviewed alert-remediation revision'

grep -qF 'repository: coroot/coroot-node-agent' "${workflow}" ||
  fail 'the workflow must check out the upstream Coroot node-agent source explicitly'
# shellcheck disable=SC2016
grep -qF 'ref: ${{ env.COROOT_NODE_AGENT_SOURCE_COMMIT }}' "${workflow}" ||
  fail 'the upstream checkout must use the exact pinned commit'
grep -qF 'go test ./ebpftracer ./node ./logs' "${workflow}" ||
  fail 'all patched Coroot node-agent packages must run tests before publish'
grep -qF 'CGO_ENABLED=1 go build -mod=readonly' "${workflow}" ||
  fail 'the reviewed source must be compiled before image creation'
# shellcheck disable=SC2016
grep -qF 'cosign sign --yes "${IMAGE}@${DIGEST}"' "${workflow}" ||
  fail 'the published compatibility image must be keylessly signed by digest'

[[ "$(grep -c '^diff --git ' "${patch_file}")" == '8' ]] ||
  fail 'the compatibility patch must touch only the four implementations and four regression-test files'
grep -qF 'l7PerfBufferSizePages = 128' "${patch_file}" ||
  fail 'the L7 perf buffer must have four times the upstream burst capacity'
grep -qF 'TestL7PerfBufferHasBurstHeadroom' "${patch_file}" ||
  fail 'the compatibility patch lacks an executable L7 buffer regression test'
grep -qF 'isExpectedCapabilityError' "${patch_file}" ||
  fail 'optional TLS symbol absence needs an explicit severity classifier'
grep -qF 'klog.WarningfDepth' "${patch_file}" ||
  fail 'expected TLS symbol absence must remain visible as a warning'
grep -qF 'klog.ErrorfDepth' "${patch_file}" ||
  fail 'unexpected TLS uprobe failures must remain errors'
grep -qF 'journald is unavailable at' "${patch_file}" ||
  fail 'Talos journal absence must remain visible without becoming a log-error alert'
grep -qF 'if os.IsNotExist(err)' "${patch_file}" ||
  fail 'only a genuinely absent Talos journal path may be demoted'
grep -qF 'journal permission denied' "${patch_file}" ||
  fail 'unexpected journal failures need an executable severity-boundary regression test'
grep -qF 'resolveInstanceMetadata' "${patch_file}" ||
  fail 'declared providers must bypass automatic metadata discovery'
grep -qF 'metadata discovery must be skipped when the provider is declared' "${patch_file}" ||
  fail 'the compatibility patch lacks a declared-provider regression test'

grep -qF "FROM ${upstream_image}" "${dockerfile}" ||
  fail 'the compatibility image must inherit only from the immutable upstream runtime image'
grep -qF 'COPY --chmod=0755 coroot-node-agent /usr/bin/coroot-node-agent' "${dockerfile}" ||
  fail 'the compatibility image must replace only the reviewed agent binary'

printf 'Coroot node-agent compatibility-image contract is valid.\n'
