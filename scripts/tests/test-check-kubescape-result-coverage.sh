#!/usr/bin/env bash
#
# Cover check-kubescape-result-coverage.sh (#4262).
#
# The load-bearing case is the negative control the issue asks for: remove ONE expected result
# from a set that passes, and the check must fail naming it. Every surface gets that case, plus
# the fail-closed cases where an empty or inconsistent read must be UNKNOWN rather than clean.
#
# Every bad input is the passing fixture changed ONE way, and each change is asserted to have
# landed before the check runs, so no case can pass because its fixture failed to build.

set -euo pipefail

cd "$(dirname "$0")/../.."
readonly CHECK='scripts/check-kubescape-result-coverage.sh'
readonly NOW=1790000000
readonly DIGEST_WEB='sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
readonly DIGEST_JOB='sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'
readonly DIGEST_DNS='sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc'

tmp="$(mktemp -d)"
# A test that exits 0 without having run is worse than one that errors. Bash 3.2 reports
# $? as 0 to an EXIT trap for a `set -u` abort, so completion is recorded explicitly.
finished=0
cleanup() {
  local rc=$?
  rm -rf "${tmp}"
  if [ "${finished}" != 1 ] && [ "${rc}" -eq 0 ]; then
    printf 'test-check-kubescape-result-coverage: aborted before finishing; reporting failure rather than a clean pass\n' >&2
    rc=1
  fi
  exit "${rc}"
}
trap cleanup EXIT

failures=0
ok() { printf 'ok: %s\n' "$1"; }
bad() {
  printf 'FAIL: %s\n' "$1" >&2
  failures=$((failures + 1))
}

# The passing fixture: one scanned namespace with a Deployment and a CronJob, and one unscanned
# namespace (kube-system, listed in the real reviewed list) whose workload has no result at all.
base="${tmp}/base"
mkdir -p "${base}"
list() { jq -n "{apiVersion: \"v1\", kind: \"List\", items: $1}"; }
# shellcheck disable=SC2016 # a jq expression; $k and $n are jq variables
ks_labels='{"kubescape.io/workload-kind": $k, "kubescape.io/workload-namespace": "app", "kubescape.io/workload-name": $n}'

list '[{metadata: {name: "app"}}, {metadata: {name: "kube-system"}}]' >"${base}/namespaces.json"
list '[{kind: "Deployment", metadata: {namespace: "app", name: "web"}},
       {kind: "CronJob", metadata: {namespace: "app", name: "nightly"}},
       {kind: "Deployment", metadata: {namespace: "kube-system", name: "coredns"}}]' \
  >"${base}/workloads.json"
jq -n --arg web "registry.test/web@${DIGEST_WEB}" --arg job "registry.test/job@${DIGEST_JOB}" \
  --arg dns "registry.test/dns@${DIGEST_DNS}" '{items: [
    {metadata: {namespace: "app", name: "web-5d8f-x", creationTimestamp: "2026-01-01T00:00:00Z",
       ownerReferences: [{kind: "ReplicaSet", name: "web-5d8f", controller: true}]},
     spec: {containers: [{name: "web"}]},
     status: {phase: "Running", startTime: "2026-01-01T00:00:00Z",
       containerStatuses: [{name: "web", image: "registry.test/web:1", imageID: $web}]}},
    {metadata: {namespace: "app", name: "nightly-1-y", creationTimestamp: "2026-01-01T00:00:00Z",
       ownerReferences: [{kind: "Job", name: "nightly-1", controller: true}]},
     spec: {containers: [{name: "job"}]},
     status: {phase: "Running", startTime: "2026-01-01T00:00:00Z",
       containerStatuses: [{name: "job", image: "registry.test/job:1", imageID: $job}]}},
    {metadata: {namespace: "kube-system", name: "coredns-z", creationTimestamp: "2026-01-01T00:00:00Z",
       ownerReferences: [{kind: "ReplicaSet", name: "coredns-1", controller: true}]},
     spec: {containers: [{name: "dns"}]},
     status: {phase: "Running", startTime: "2026-01-01T00:00:00Z",
       containerStatuses: [{name: "dns", image: "registry.test/dns:1", imageID: $dns}]}}]}' \
  >"${base}/pods.json"
jq -n "{items: [
    {metadata: {namespace: \"app\", name: \"deployment-web\", labels: (\"Deployment\" as \$k | \"web\" as \$n | ${ks_labels})},
     spec: {controls: {\"C-0001\": {}, \"C-0002\": {}}}},
    {metadata: {namespace: \"app\", name: \"cronjob-nightly\", labels: (\"CronJob\" as \$k | \"nightly\" as \$n | ${ks_labels})},
     spec: {controls: {\"C-0001\": {}}}}]}" >"${base}/posture.json"
# The LIST surface: the same objects with `.spec` stripped, exactly as the aggregated API serves it.
jq '{items: [.items[] | del(.spec)]}' "${base}/posture.json" >"${base}/posture-list.json"
jq -n --arg web "registry.test/web@${DIGEST_WEB}" --arg at "$((NOW - 3600))" '{items: [
    {metadata: {namespace: "app", name: "deployment-web-web",
       annotations: {"kubescape.io/image-id": $web, "kubescape.io/timestamp": $at}}}]}' \
  >"${base}/vulnerability.json"
profile='{items: [{metadata: {namespace: "app", name: "replicaset-web-5d8f",
    labels: {"kubescape.io/learning-period": "24h"},
    annotations: {"kubescape.io/status": "completed", "kubescape.io/completion": "complete"}}}]}'
jq -n "${profile}" >"${base}/profiles.json"
jq -n "${profile}" >"${base}/neighborhoods.json"

# variant <name> <file> <jq-filter> — copy the passing fixture and change ONE file.
variant() {
  local dir="${tmp}/$1"
  rm -rf "${dir}"
  cp -R "${base}" "${dir}"
  jq "$3" "${base}/$2" >"${dir}/$2"
  if cmp -s "${base}/$2" "${dir}/$2"; then
    printf 'FAIL: variant %s left %s unchanged; the case would be vacuous\n' "$1" "$2" >&2
    exit 1
  fi
  printf '%s' "${dir}"
}

# expect <name> <want-exit> <want-substring> <dir> — the substring is looked for in stdout+stderr.
expect() {
  local name="$1" want="$2" needle="$3" dir="$4" rc=0
  bash "${CHECK}" --from-dir "${dir}" --now "${NOW}" >"${tmp}/out" 2>&1 || rc=$?
  if [ "${rc}" -ne "${want}" ]; then
    bad "${name}: exit ${rc}, want ${want} ($(tr '\n' ' ' <"${tmp}/out"))"
    return
  fi
  if [ -n "${needle}" ] && ! grep -qF -- "${needle}" "${tmp}/out"; then
    bad "${name}: output lacks '${needle}' ($(tr '\n' ' ' <"${tmp}/out"))"
    return
  fi
  ok "${name}"
}

expect "complete result set passes" 0 "COVERAGE posture expected=2 current=2" "${base}"
expect "unscanned namespace and Job pods are out of scope" 0 \
  "COVERAGE vulnerability expected=1 current=1" "${base}"
expect "one runtime pair is expected" 0 "COVERAGE runtime expected=1 current=1" "${base}"

# Negative controls: one expected result removed, per surface.
d="$(variant posture-missing posture.json '.items |= map(select(.metadata.name != "deployment-web"))')"
jq '{items: [.items[] | del(.spec)]}' "${d}/posture.json" >"${d}/posture-list.json"
expect "a removed posture result fails" 1 "MISSING posture Deployment/app/web" "${d}"

d="$(variant posture-empty posture.json '(.items[] | select(.metadata.name == "cronjob-nightly") | .spec.controls) = null')"
expect "a posture result with no controls fails" 1 "EMPTY posture CronJob/app/nightly" "${d}"

d="$(variant posture-namespace-mismatch posture.json '
  (.items[] | select(.metadata.name == "deployment-web") | .metadata.namespace) = "other"')"
expect "a posture result whose label namespace disagrees with metadata is UNKNOWN" 2 \
  "posture result identity disagrees with metadata namespace" "${d}"

d="$(variant namespace-snapshot-race workloads.json '
  .items += [{kind: "Deployment", metadata: {namespace: "late", name: "late-web"}}]')"
expect "a workload observed after the namespace snapshot remains in scope" 1 \
  "MISSING posture Deployment/late/late-web" "${d}"

d="$(variant vuln-missing vulnerability.json '.items = []')"
expect "a removed vulnerability result is UNKNOWN when it empties the read" 2 "vulnerability read back empty" "${d}"
d="$(variant vuln-other vulnerability.json ".items[0].metadata.annotations[\"kubescape.io/image-id\"] = \"registry.test/other@sha256:$(printf 'd%.0s' $(seq 64))\"")"
expect "a vulnerability result for another image leaves the running image missing" 1 \
  "MISSING vulnerability registry.test/web:1 ${DIGEST_WEB}" "${d}"

d="$(variant vuln-image-identity-unknown pods.json '.items[0].status.containerStatuses += [
  {name: "sidecar", image: "registry.test/sidecar:1", imageID: "containerd://not-a-digest"}
]')"
expect "a running image without a SHA-256 identity is UNKNOWN" 2 \
  "running image identity has no SHA-256 digest: app/web-5d8f-x container=sidecar" "${d}"

d="$(variant vuln-regular-status-missing pods.json '
  .items[0].spec.containers += [{name: "sidecar"}]')"
expect "a configured regular container without a status is UNKNOWN" 2 \
  "running pod is missing long-lived container status: app/web-5d8f-x container=sidecar" "${d}"

d="$(variant vuln-init-sidecar-status-missing pods.json '
  .items[0].spec.initContainers = [{name: "sidecar-init", restartPolicy: "Always"}]')"
expect "a configured init sidecar without a status is UNKNOWN" 2 \
  "running pod is missing long-lived container status: app/web-5d8f-x init-sidecar=sidecar-init" "${d}"

d="$(variant vuln-init-sidecar-identity-unknown pods.json '
  .items[0].spec.initContainers = [{name: "sidecar-init", restartPolicy: "Always"}] |
  .items[0].status.initContainerStatuses = [
    {name: "sidecar-init", image: "registry.test/sidecar-init:1", imageID: "containerd://not-a-digest",
     state: {running: {startedAt: "2026-01-01T00:00:00Z"}}}
  ]')"
expect "a running init sidecar without a SHA-256 identity is UNKNOWN" 2 \
  "running image identity has no SHA-256 digest: app/web-5d8f-x container=sidecar-init" "${d}"

d="$(variant vuln-init-sidecar-missing pods.json ".items[0].spec.initContainers = [
  {name: \"sidecar-init\", restartPolicy: \"Always\"}
] | .items[0].status.initContainerStatuses = [
  {name: \"sidecar-init\", image: \"registry.test/sidecar-init:1\", imageID: \"registry.test/sidecar-init@${DIGEST_JOB}\",
   state: {running: {startedAt: \"2026-01-01T00:00:00Z\"}}}
]")"
expect "a running init sidecar joins the vulnerability expected set" 1 \
  "MISSING vulnerability registry.test/sidecar-init:1 ${DIGEST_JOB}" "${d}"

d="$(variant vuln-one-shot-init-ignored pods.json '
  .items[0].spec.initContainers = [{name: "setup"}] |
  .items[0].status.initContainerStatuses = [
    {name: "setup", image: "registry.test/setup:1", imageID: "containerd://not-a-digest",
     state: {running: {startedAt: "2026-01-01T00:00:00Z"}}}
  ]')"
expect "a running one-shot init container stays outside the long-lived expected set" 0 \
  "COVERAGE vulnerability expected=1 current=1" "${d}"

d="$(variant vuln-stale vulnerability.json ".items[0].metadata.annotations[\"kubescape.io/timestamp\"] = \"$((NOW - 8 * 86400))\"")"
expect "a vulnerability result older than seven days is stale" 1 "STALE vulnerability registry.test/web:1" "${d}"

d="$(variant vuln-future vulnerability.json ".items[0].metadata.annotations[\"kubescape.io/timestamp\"] = \"$((NOW + 3600))\"")"
expect "a vulnerability result timestamped in the future is UNKNOWN" 2 \
  "vulnerability result timestamp is invalid or in the future" "${d}"

d="$(variant runtime-missing neighborhoods.json '.items[0].metadata.name = "replicaset-web-other"')"
expect "a missing half of the runtime pair fails" 1 "MISSING runtime app/replicaset-web-5d8f networkneighborhood" "${d}"

d="$(variant runtime-revision-missing pods.json '
  .items[0].metadata.ownerReferences[0] = {kind: "StatefulSet", name: "web", controller: true}')"
expect "a managed runtime pod without its revision label is UNKNOWN" 2 \
  "running managed pod has no controller revision" "${d}"

d="$(variant runtime-partial profiles.json '.items[0].metadata.annotations["kubescape.io/completion"] = "partial"')"
expect "a partial runtime profile fails" 1 "PARTIAL runtime app/replicaset-web-5d8f applicationprofile" "${d}"

d="$(variant runtime-stale profiles.json '.items[0].metadata.annotations["kubescape.io/status"] = "ready"')"
expect "a profile still learning past its period is stale" 1 "STALE runtime app/replicaset-web-5d8f applicationprofile status=ready" "${d}"

d="$(variant runtime-period-malformed profiles.json '
  .items[0].metadata.annotations["kubescape.io/status"] = "ready" |
  .items[0].metadata.labels["kubescape.io/learning-period"] = "tomorrow"')"
expect "a present malformed runtime learning period is UNKNOWN" 2 \
  "runtime result has malformed learning period" "${d}"

d="$(variant runtime-pending pods.json "(.items[0].status.startTime) = \"$(date -u -r $((NOW - 3600)) +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d @$((NOW - 3600)) +%Y-%m-%dT%H:%M:%SZ)\"")"
jq '.items[0].metadata.annotations["kubescape.io/status"] = "ready"' "${base}/profiles.json" >"${d}/profiles.json"
expect "a profile inside its learning period is pending, not a failure" 0 "PENDING runtime app/replicaset-web-5d8f applicationprofile" "${d}"

# Fail closed.
d="$(variant empty-profiles profiles.json '.items = []')"
expect "an empty profile read is UNKNOWN" 2 "profiles read back empty" "${d}"

d="$(variant short-read posture.json '.items |= .[:1]')"
expect "posture objects read back short is UNKNOWN" 2 "2 listed but 1 read back by name" "${d}"

d="$(variant not-a-list pods.json '{}')"
expect "a malformed input is UNKNOWN" 2 "pods.json is not a Kubernetes List" "${d}"

d="$(variant all-unscanned workloads.json '.items[].metadata.namespace = "kube-system"')"
jq '.items[].metadata.namespace = "kube-system"' "${base}/pods.json" >"${d}/pods.json"
expect "an empty expected set is UNKNOWN" 2 "the expected set is empty" "${d}"

d="${tmp}/missing-file"
rm -rf "${d}" && cp -R "${base}" "${d}" && rm "${d}/profiles.json"
expect "a missing input file is UNKNOWN" 2 "--from-dir is missing profiles.json" "${d}"

finished=1
if [ "${failures}" -ne 0 ]; then
  printf 'test-check-kubescape-result-coverage: %d case(s) failed\n' "${failures}" >&2
  exit 1
fi
printf 'test-check-kubescape-result-coverage: all cases passed\n'
