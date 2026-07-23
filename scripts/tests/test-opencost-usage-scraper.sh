#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly opencost_dir="${root_dir}/k8s/bases/infrastructure/opencost"
readonly ci_workflow="${root_dir}/.github/workflows/ci.yaml"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

require_text() {
  local haystack="$1"
  local needle="$2"
  local description="$3"

  grep -Fq -- "$needle" <<<"${haystack}" || fail "${description}"
}

extract_resource() {
  local kind="$1"
  local name="$2"

  awk -v wanted_kind="${kind}" -v wanted_name="${name}" '
    function reset_document() {
      document = ""
      resource_kind = ""
      resource_name = ""
      in_metadata = 0
    }

    function emit_if_match() {
      if (!found && resource_kind == wanted_kind && resource_name == wanted_name) {
        printf "%s", document
        found = 1
      }
      reset_document()
    }

    BEGIN { reset_document() }
    /^---[[:space:]]*$/ { emit_if_match(); next }
    {
      if (!found) {
        document = document $0 ORS
        if ($0 ~ /^kind:[[:space:]]*/) {
          resource_kind = $0
          sub(/^kind:[[:space:]]*/, "", resource_kind)
        } else if ($0 ~ /^metadata:[[:space:]]*$/) {
          in_metadata = 1
        } else if ($0 ~ /^[^[:space:]]/ && $0 !~ /^metadata:/) {
          in_metadata = 0
        } else if (in_metadata && $0 ~ /^  name:[[:space:]]*/) {
          resource_name = $0
          sub(/^  name:[[:space:]]*/, "", resource_name)
        }
      }
    }
    END {
      if (!found) {
        emit_if_match()
      }
      if (!found) {
        exit 1
      }
    }
  '
}

grep -Fq \
  "'scripts/tests/test-opencost-usage-scraper.sh'" \
  "${ci_workflow}" ||
  fail 'the OpenCost usage scraper contract must trigger manifest validation'

grep -Fq \
  'run: bash scripts/tests/test-opencost-usage-scraper.sh' \
  "${ci_workflow}" ||
  fail 'CI must execute the OpenCost usage scraper contract'

rendered="$(kubectl kustomize "${opencost_dir}")" ||
  fail 'the OpenCost base must render'

config_map="$(
  extract_resource ConfigMap opencost-usage-scraper <<<"${rendered}"
)" || fail 'the OpenCost base must render the usage-scraper ConfigMap'
require_text \
  "${config_map}" \
  'url: http://coroot-prometheus.observability.svc.cluster.local:9090/api/v1/write' \
  'the usage scraper must remote-write into the Prometheus OpenCost already queries'
require_text \
  "${config_map}" \
  'role: node' \
  'the usage scraper must discover every Kubernetes node'
require_text \
  "${config_map}" \
  "replacement: /api/v1/nodes/\$1/proxy/metrics/cadvisor" \
  'the usage scraper must scrape the kubelet cAdvisor endpoint through the API server'
require_text \
  "${config_map}" \
  'regex: container_(cpu_usage_seconds_total|memory_working_set_bytes)' \
  'the usage scraper must retain the CPU and memory series OpenCost needs'

deployment="$(
  extract_resource Deployment opencost-usage-scraper <<<"${rendered}"
)" || fail 'the OpenCost base must render the usage-scraper Deployment'
require_text \
  "${deployment}" \
  'serviceAccountName: opencost-usage-scraper' \
  'the usage scraper must use its dedicated service account'
require_text \
  "${deployment}" \
  'ghcr.io/coroot/prometheus:2.55.1-ubi9-0@sha256:' \
  'the usage scraper image must be immutable and match the deployed Coroot Prometheus line'
require_text \
  "${deployment}" \
  '--enable-feature=agent' \
  'the scraper must run Prometheus in lightweight agent mode'
require_text \
  "${deployment}" \
  'platform.devantler.tech/replica-floor: exempt' \
  'the intentionally singleton scraper must document its replica-floor exemption'
require_text \
  "${deployment}" \
  'readOnlyRootFilesystem: true' \
  'the usage scraper must keep its root filesystem immutable'

cluster_role="$(
  extract_resource ClusterRole opencost-usage-scraper <<<"${rendered}"
)" || fail 'the OpenCost base must render the usage-scraper ClusterRole'
require_text \
  "${cluster_role}" \
  '- nodes/proxy' \
  'the usage scraper must be allowed to read kubelet metrics through the node proxy'
require_text \
  "${cluster_role}" \
  '- watch' \
  'the usage scraper must be allowed to watch the node discovery surface'

extract_resource \
  ClusterRoleBinding \
  opencost-usage-scraper <<<"${rendered}" >/dev/null ||
  fail 'the OpenCost base must bind the usage-scraper ClusterRole'
extract_resource \
  ServiceAccount \
  opencost-usage-scraper <<<"${rendered}" >/dev/null ||
  fail 'the OpenCost base must render the usage-scraper ServiceAccount'

printf 'PASS: OpenCost receives bounded cAdvisor CPU and memory metrics through Coroot Prometheus\n'
