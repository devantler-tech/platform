#!/usr/bin/env bash

# Contract for the default-off Crossplane sync exporter (#2986).
#
# The failure this component exists to prevent is a silent one: a managed
# resource reporting Ready=True while Synced=False, which every Ready-keyed
# health check reads as healthy. The exporter has the same failure shape itself
# — a wrong GVK, a renamed metric, or a scrape target that no longer matches the
# kube-state-metrics listen address all yield ZERO series, and an empty result
# is indistinguishable from a healthy cluster. None of those would fail a schema
# validation, so they are asserted here instead.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly coroot_dir="${root_dir}/k8s/bases/infrastructure/coroot"
readonly exporter_component="${coroot_dir}/components/crossplane-sync-exporter"
readonly local_cluster="${root_dir}/k8s/clusters/local"
readonly prod_cluster="${root_dir}/k8s/clusters/prod"
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

# Exact-value assertion. A substring match is wrong for every value below,
# because the failure being guarded against is a RENAME — and a renamed value is
# usually a LONGER one (`crossplane` -> `crossplane_mr`), which a substring match
# accepts. Anchoring to the whole line is what makes the assertion mean what it
# says.
require_line() {
  local haystack="$1"
  local value="$2"
  local description="$3"
  local line

  # Exact string comparison on the trimmed line, deliberately not a regex: every
  # value here contains `.`, `[`, `-` or `/`, and escaping them for grep is both
  # error-prone and easy to get subtly wrong in the permissive direction.
  while IFS= read -r line; do
    line="${line#"${line%%[![:space:]]*}"}"
    line="${line%"${line##*[![:space:]]}"}"
    [ "${line}" = "${value}" ] && return 0
  done <<<"${haystack}"

  fail "${description}"
}

reject_text() {
  local haystack="$1"
  local needle="$2"
  local description="$3"

  if grep -Fq -- "$needle" <<<"${haystack}"; then
    fail "${description}"
  fi
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
  "'scripts/tests/test-crossplane-sync-exporter.sh'" \
  "${ci_workflow}" ||
  fail 'the Crossplane sync exporter contract must trigger manifest validation'

grep -Fq \
  'run: bash scripts/tests/test-crossplane-sync-exporter.sh' \
  "${ci_workflow}" ||
  fail 'CI must execute the Crossplane sync exporter contract'

# Default-off: the exporter must not appear anywhere it has not been explicitly
# activated. A component is only reachable through a `components:` reference, so
# an accidental reference is the way this ships before its activation change.
for surface in "${coroot_dir}" "${local_cluster}" "${prod_cluster}"; do
  surface_rendered="$(kubectl kustomize "${surface}")" ||
    fail "the ${surface} surface must render"
  reject_text \
    "${surface_rendered}" \
    'crossplane-sync-exporter' \
    "the exporter must remain default-off until an activation change enables it (${surface})"
done

rendered="$(kubectl kustomize "${exporter_component}")" ||
  fail 'the default-off Crossplane sync-exporter component must render'

config_map="$(
  extract_resource ConfigMap crossplane-sync-exporter <<<"${rendered}"
)" || fail 'the component must render the exporter ConfigMap'

# Coroot's bundled Prometheus exposes no scrape configuration, so remote-write is
# the only ingest path into the store the alerting rule will query.
require_line \
  "${config_map}" \
  '- url: http://coroot-prometheus.observability.svc.cluster.local:9090/api/v1/write' \
  'the exporter must remote-write into the Prometheus Coroot queries'

# The GVK is matched exactly by CustomResourceStateMetrics — there is no wildcard
# and no `categories: managed` selector — so a drifted group or version silently
# exports nothing at all.
require_line \
  "${config_map}" \
  'group: repo.github.m.upbound.io' \
  'the exporter must target the GitHub repository managed-resource group'
require_line \
  "${config_map}" \
  'version: v1alpha1' \
  'the exporter must target the served Repository version'
require_line \
  "${config_map}" \
  'kind: Repository' \
  'the exporter must target the Repository kind'

# The emitted series name is metricNamePrefix + name. The scrape config filters on
# the joined result, so renaming either half without the other drops every series
# while both files still look individually correct. This assertion is the coupling.
require_line \
  "${config_map}" \
  'metricNamePrefix: crossplane' \
  'the exported series must carry the crossplane prefix the scrape filter expects'
require_line \
  "${config_map}" \
  '- name: managed_resource_condition' \
  'the exported series must carry the name the scrape filter expects'
require_line \
  "${config_map}" \
  'regex: crossplane_managed_resource_condition' \
  'the scrape filter must keep exactly the series the exporter emits'

# Ready is exported alongside Synced deliberately: the defect being detected is
# the PAIR, so a rule cannot express it if the conditions are filtered apart.
require_line \
  "${config_map}" \
  'path: [status, conditions]' \
  'the exporter must read every condition, not a single hard-coded one'
require_line \
  "${config_map}" \
  'valueFrom: [status]' \
  'the condition status must supply the gauge value'
require_line \
  "${config_map}" \
  'condition: [type]' \
  'each series must be labelled with its condition type'

# The agent scrapes kube-state-metrics inside the pod, so this target must stay
# equal to the kube-state-metrics listen address asserted on the Deployment below.
require_line \
  "${config_map}" \
  '- 127.0.0.1:8080' \
  'the agent must scrape kube-state-metrics over loopback'

deployment="$(
  extract_resource Deployment crossplane-sync-exporter <<<"${rendered}"
)" || fail 'the component must render the exporter Deployment'
require_line \
  "${deployment}" \
  'serviceAccountName: crossplane-sync-exporter' \
  'the exporter must use its dedicated service account'
require_text \
  "${deployment}" \
  'registry.k8s.io/kube-state-metrics/kube-state-metrics:v2.17.0@sha256:' \
  'the kube-state-metrics image must be immutable'
require_text \
  "${deployment}" \
  'ghcr.io/coroot/prometheus:2.55.1-ubi9-0@sha256:' \
  'the agent image must be immutable and match the deployed Coroot Prometheus line'
require_line \
  "${deployment}" \
  '- --enable-feature=agent' \
  'the scraper must run Prometheus in lightweight agent mode'

# Built-in kube-state metrics would duplicate what Coroot's cluster agent already
# sends, so the exporter must stay restricted to the custom resource state.
require_line \
  "${deployment}" \
  '- --custom-resource-state-only=true' \
  'the exporter must not emit a second copy of the built-in kube-state metrics'
require_line \
  "${deployment}" \
  '- --host=127.0.0.1' \
  'kube-state-metrics must not be reachable from outside the pod'
require_line \
  "${deployment}" \
  '- --port=8080' \
  'the kube-state-metrics port must match the loopback scrape target'
require_line \
  "${deployment}" \
  'platform.devantler.tech/replica-floor: exempt' \
  'the intentionally singleton exporter must document its replica-floor exemption'
require_line \
  "${deployment}" \
  'readOnlyRootFilesystem: true' \
  'the exporter must keep its root filesystem immutable'
require_text \
  "${deployment}" \
  'CKV_K8S_38=' \
  'the service-account token mount must carry its documented justification'

cluster_role="$(
  extract_resource ClusterRole crossplane-sync-exporter <<<"${rendered}"
)" || fail 'the component must render the exporter ClusterRole'
require_line \
  "${cluster_role}" \
  '- repo.github.m.upbound.io' \
  'the exporter must be limited to the managed-resource group it exports'
require_line \
  "${cluster_role}" \
  '- repositories' \
  'the exporter must be limited to the resource it exports'

# A metrics exporter able to read every custom resource could also read provider
# configuration it has no need for.
reject_text \
  "${cluster_role}" \
  "- '*'" \
  'the exporter must not receive wildcard API access'
for verb in create update patch delete deletecollection; do
  reject_text \
    "${cluster_role}" \
    "- ${verb}" \
    "the exporter must stay read-only (found ${verb})"
done

extract_resource \
  ClusterRoleBinding \
  crossplane-sync-exporter <<<"${rendered}" >/dev/null ||
  fail 'the component must bind the exporter ClusterRole'
extract_resource \
  ServiceAccount \
  crossplane-sync-exporter <<<"${rendered}" >/dev/null ||
  fail 'the component must render the exporter ServiceAccount'

printf 'PASS: the default-off Crossplane sync exporter keeps its least-privilege metric contract\n'
