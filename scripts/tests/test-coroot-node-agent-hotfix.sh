#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly workflow="${root_dir}/.github/workflows/publish-coroot-node-agent-hotfix.yaml"
readonly patch_file="${root_dir}/k8s/bases/infrastructure/coroot/node-agent-v1.38.1-alert-remediation.patch"
readonly cluster_agent_patch="${root_dir}/k8s/bases/infrastructure/coroot/cluster-agent-v1.11.8-otel-log-api.patch"
readonly dockerfile="${root_dir}/k8s/bases/infrastructure/coroot/CorootNodeAgent.Dockerfile"
readonly source_tag='v1.38.1'
readonly source_commit='e335bf21a4657e9c91f56945f726e28b5f56a589'
readonly upstream_image='ghcr.io/coroot/coroot-node-agent:1.38.1@sha256:a1488ca3e541703fe4b4cdefeff2dae992cc9f45affd18f9f41e5b12a8016cae'
readonly image_repository='ghcr.io/devantler-tech/platform-coroot-node-agent'
readonly go_version='1.26.10'

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
  fail 'the workflow must pin the reviewed Coroot node-agent source commit'
[[ "$(yq -er '.env.UPSTREAM_IMAGE' "${workflow}")" == "${upstream_image}" ]] ||
  fail 'the workflow must use the immutable upstream runtime image'
[[ "$(yq -er '.env.IMAGE' "${workflow}")" == "${image_repository}" ]] ||
  fail 'the workflow image destination drifted'
# The literal GitHub expression is the contract.
# shellcheck disable=SC2016
[[ "$(yq -er '.env.IMAGE_TAG' "${workflow}")" == 'v1.38.1-alerts.1-${{ github.sha }}' ]] ||
  fail 'the image tag must identify the reviewed alert-remediation revision'

[[ "$(yq -er '.env.COROOT_NODE_AGENT_SOURCE_TAG' "${workflow}")" == "${source_tag}" ]] ||
  fail 'the workflow must name the release its pinned source commit belongs to'
[[ "$(yq -er '[.jobs[].steps[] | select(.uses == "actions/setup-go*") | .with."go-version"] | unique | join(",")' "${workflow}")" == "${go_version}" ]] ||
  fail 'both jobs must compile with the reviewed patched Go release'
# shellcheck disable=SC2016
[[ "$(grep -cF 'test "$(git rev-parse "refs/tags/${EXPECTED_SOURCE_TAG}^{commit}")" = "${EXPECTED_SOURCE_COMMIT}"' "${workflow}")" == '2' ]] ||
  fail 'both jobs must bind the release tag to the pinned source commit'
[[ "$(grep -cF 'go version -m coroot-node-agent | grep -qE' "${workflow}")" == '2' ]] ||
  fail 'both jobs must prove the binary names its release, not a pseudo-version'
audit_step="$(yq -er '.jobs.verify.steps[] | select(.name == "Refuse advisories the built binary reaches") | .run' "${workflow}")" ||
  fail 'the verify job must audit the built binary before publication can run'
# shellcheck disable=SC2016
grep -qF -- '-mode binary -format json coroot-node-agent >"${report}"' <<<"${audit_step}" ||
  fail 'the audit must read the built binary and keep its report out of the public log'
grep -qF '.SBOM.modules' <<<"${audit_step}" ||
  fail 'a report that examined no modules must fail the audit'
# shellcheck disable=SC2016
grep -qF 'test "${reached}" = 0' <<<"${audit_step}" ||
  fail 'a reached advisory must stop the workflow'
grep -qF '.path != "github.com/coroot/coroot-cluster-agent"' <<<"${audit_step}" ||
  fail 'publication must refuse an advisory listed for the locally built cluster agent'
grep -qF '.path == "stdlib"' <<<"${audit_step}" ||
  fail 'an advisory index that omits the standard library must not pass the cluster-agent check'
[[ "$(yq -er '.jobs.publish.needs' "${workflow}")" == 'verify' ]] ||
  fail 'publication must wait for the audit in the verify job'

[[ "$(yq -er '.env.COROOT_CLUSTER_AGENT_SOURCE_COMMIT' "${workflow}")" == 'dfc1a098eaa4bde3b10d4225241b2421a2704679' ]] ||
  fail 'the cluster-agent copy must come from the exact reviewed release commit'
[[ "$(grep -c '^diff --git ' "${cluster_agent_patch}")" == '2' ]] ||
  fail 'the cluster-agent patch must touch only the two reviewed log API files'
# shellcheck disable=SC2016
[[ "$(grep -cF 'git -C ../cluster-agent apply "${CLUSTER_AGENT_PATCH}"' "${workflow}")" == '2' ]] ||
  fail 'both jobs must apply the reviewed cluster-agent patch'

grep -qF 'repository: coroot/coroot-node-agent' "${workflow}" ||
  fail 'the workflow must check out the upstream Coroot node-agent source explicitly'
# shellcheck disable=SC2016
grep -qF 'ref: ${{ env.COROOT_NODE_AGENT_SOURCE_COMMIT }}' "${workflow}" ||
  fail 'the upstream checkout must use the exact pinned commit'
grep -qF 'go test -tags slicelabels ./ebpftracer ./node ./logs' "${workflow}" ||
  fail 'all patched Coroot node-agent packages must run tests before publish'
grep -qF 'CGO_ENABLED=1 go build -mod=readonly -tags slicelabels' "${workflow}" ||
  fail 'the reviewed source must be compiled before image creation'
# shellcheck disable=SC2016
grep -qF 'cosign sign --yes "${IMAGE}@${DIGEST}"' "${workflow}" ||
  fail 'the published compatibility image must be keylessly signed by digest'

expected_patch_files="$(printf 'diff --git a/%s b/%s\n' \
  go.mod go.mod \
  go.sum go.sum \
  ebpftracer/tls.go ebpftracer/tls.go \
  ebpftracer/tls_test.go ebpftracer/tls_test.go \
  ebpftracer/tracer.go ebpftracer/tracer.go \
  ebpftracer/tracer_test.go ebpftracer/tracer_test.go \
  logs/journald_reader.go logs/journald_reader.go \
  logs/journald_reader_test.go logs/journald_reader_test.go \
  logs/otel.go logs/otel.go \
  node/collector.go node/collector.go \
  node/collector_test.go node/collector_test.go \
  node/net_dump_linux_test.go node/net_dump_linux_test.go \
  node/net_dump_retry.go node/net_dump_retry.go \
  node/net_dump_retry_test.go node/net_dump_retry_test.go \
  node/net_linux.go node/net_linux.go | LC_ALL=C sort)"
[[ "$(grep '^diff --git ' "${patch_file}" | LC_ALL=C sort)" == "${expected_patch_files}" ]] ||
  fail 'the compatibility patch must touch exactly the reviewed implementation and regression-test paths'
grep -qF 'l7PerfBufferSizePages = 512' "${patch_file}" ||
  fail 'the L7 perf buffer must have sixteen times the upstream burst capacity'
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
grep -qF 'return errors.Is(err, os.ErrNotExist)' "${patch_file}" ||
  fail 'sdjournal wraps the missing-journal errno, so its check must unwrap it'
grep -qF 'if isJournalMissing(err)' "${patch_file}" ||
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
