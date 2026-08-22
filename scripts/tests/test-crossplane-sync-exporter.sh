#!/usr/bin/env bash

# Contract for the Crossplane sync exporter (#2986).
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
readonly hetzner_infrastructure="${root_dir}/k8s/providers/hetzner/infrastructure"
readonly docker_infrastructure="${root_dir}/k8s/providers/docker/infrastructure"
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

count_literal() {
  local text="$1"
  local needle="$2"
  local count=0

  while [[ "${text}" == *"${needle}"* ]]; do
    text="${text#*"${needle}"}"
    count=$((count + 1))
  done

  printf '%s\n' "${count}"
}

stuck_query_excludes_only_paused() {
  local query="$1"
  local paused_selector='crossplane_managed_resource_condition{condition="Synced",reason!="ReconcilePaused"}'
  local unfiltered_selector='crossplane_managed_resource_condition{condition="Synced"}'

  [ "$(count_literal "${query}" "${paused_selector}")" -eq 4 ] &&
    [ "$(count_literal "${query}" "${unfiltered_selector}")" -eq 0 ]
}

# Collapse each `rules[]` entry into one canonical signature line:
#
#   apiGroups=[g1,g2] resources=[r1] verbs=[v1,v2]
#
# Line-at-a-time assertions cannot express what an RBAC rule actually MEANS. A
# group, a resource and a verb set only grant anything when they sit in the SAME
# entry, and independent `require_line` checks pass just as happily when they are
# split across two rules — which grants something quite different from what the
# assertions appear to say. Signatures make the grouping the thing being
# asserted, so a split rule fails.
#
# Values within a key keep their document order; every rule here is written in
# the canonical order kustomize emits, so this stays an exact comparison rather
# than a set comparison that could mask a duplicated or reordered verb.
rule_signatures() {
  awk '
    function flush() {
      if (started) {
        printf "apiGroups=[%s] resources=[%s] verbs=[%s]\n", groups, resources, verbs
      }
      groups = ""; resources = ""; verbs = ""; key = ""
    }
    function append(current, value) {
      return current == "" ? value : current "," value
    }

    /^rules:[[:space:]]*$/ { in_rules = 1; next }
    # Any other column-0 key ends the rules block.
    /^[^[:space:]-]/ { if (in_rules) { flush(); started = 0; in_rules = 0 } }
    !in_rules { next }

    /^-[[:space:]]/ {
      flush()
      started = 1
      sub(/^-[[:space:]]*/, "")
    }
    /^[[:space:]]*[a-zA-Z]+:[[:space:]]*$/ {
      key = $0
      gsub(/[[:space:]]|:/, "", key)
      next
    }
    /^[[:space:]]*-[[:space:]]/ {
      value = $0
      sub(/^[[:space:]]*-[[:space:]]*/, "", value)
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", value)
      if (key == "apiGroups") { groups = append(groups, value) }
      else if (key == "resources") { resources = append(resources, value) }
      else if (key == "verbs") { verbs = append(verbs, value) }
    }
    END { flush() }
  '
}

# Assert one whole rule, by signature, and assert there is EXACTLY one of it.
#
# Counting rather than matching is what makes the "exactly one ... rule" wording
# on the assertions below true. A presence check answers "at least one", so a
# role carrying the same grant twice satisfies it, and `reject_unexpected_rules`
# cannot catch that either: a duplicate is by definition a member of the allowed
# set. The pair would then enforce "at least one of each, and no other KIND of
# rule" while claiming to enforce "exactly one of each".
#
# The duplicate grants no additional Kubernetes permission — RBAC is a union, so
# two identical rules authorise exactly what one does. It is asserted anyway
# because a role that accumulated a duplicate is a role someone edited without
# noticing what was already there, and that is the edit most likely to widen the
# next grant by mistake.
#
# The comparison stays the exact trimmed-whole-line one used everywhere else in
# this file, now applied to a unit that carries meaning.
require_rule() {
  local cluster_role="$1"
  local signature="$2"
  local description="$3"
  local line matches=0

  while IFS= read -r line; do
    line="${line#"${line%%[![:space:]]*}"}"
    line="${line%"${line##*[![:space:]]}"}"
    [ "${line}" = "${signature}" ] && matches=$((matches + 1))
  done <<<"$(rule_signatures <<<"${cluster_role}")"

  [ "${matches}" -eq 1 ] ||
    fail "${description} (found ${matches} matching rules, expected exactly 1)"
}

# The closed half of the contract: every rule present must be one of the rules
# named here.
#
# `require_rule` only proves a signature is PRESENT, which is a one-directional
# claim. A third entry granting get/list/watch on some unrelated resource — core
# `configmaps`, say — satisfies both required-rule assertions, carries no
# wildcard, and uses no mutating verb, so it passes every other check in this
# file while quietly widening what the exporter can read.
#
# Least privilege is a statement about the WHOLE role, not about the rules
# someone remembered to assert, so the allowed set has to be closed. This closes
# it in one direction only — no rule outside the allowed set — which is why
# `require_rule` counts rather than merely matching: a duplicate of an allowed
# rule is a member of the allowed set, so nothing here would reject it.
reject_unexpected_rules() {
  local cluster_role="$1"
  shift

  local actual expected_signature matched
  while IFS= read -r actual; do
    [ -z "${actual}" ] && continue
    matched=0
    for expected_signature in "$@"; do
      if [ "${actual}" = "${expected_signature}" ]; then
        matched=1
        break
      fi
    done
    [ "${matched}" -eq 1 ] ||
      fail "the exporter must hold no RBAC rule beyond the two it needs (found: ${actual})"
  done <<<"$(rule_signatures <<<"${cluster_role}")"
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

# Activated on the provider that runs Coroot, and only there.
#
# The surfaces below are the provider infrastructure layers, because that is
# where a `components:` reference actually resolves into rendered resources.
# `k8s/clusters/{local,prod}` are not usable here: they render the Flux
# Kustomization wiring and no Coroot resource at all, so an assertion against
# them holds whatever the component reference says.
hetzner_rendered="$(kubectl kustomize "${hetzner_infrastructure}")" ||
  fail "the ${hetzner_infrastructure} surface must render"
require_text \
  "${hetzner_rendered}" \
  'crossplane-sync-exporter' \
  'the exporter must reach the provider layer prod deploys'

alerter="$(
  extract_resource CronJob crossplane-sync-alerter <<<"${hetzner_rendered}"
)" || fail 'the provider layer must render the Crossplane sync alerter CronJob'
alerter_script="$(
  yq eval -r '.spec.jobTemplate.spec.template.spec.containers[] | select(.name == "alerter") | .command[2]' \
    <<<"${alerter}"
)" || fail 'the rendered Crossplane sync alerter script must be readable'

# Paused resources are deliberately still evidence that the exporter is alive,
# so coverage must count them. They are not stuck, though: ReconcilePaused is an
# explicit operator choice and must be removed from every instant/range selector
# in the stuck query before its grace-window joins run.
# The needle is the literal rendered shell program; expanding its command
# substitution would execute the assertion instead of comparing it.
# shellcheck disable=SC2016
require_text \
  "${alerter_script}" \
  'COV=$(q '\''count(crossplane_managed_resource_condition{condition="Synced"})'\'')' \
  'exporter coverage must retain paused managed-resource series'

stuck_line="$(
  while IFS= read -r line; do
    if [[ "${line}" == *'STUCK='* ]]; then
      printf '%s\n' "${line}"
    fi
  done <<<"${alerter_script}"
)"
[ -n "${stuck_line}" ] || fail 'the rendered alerter must carry its stuck-resource query'
stuck_query="${stuck_line#*q }"
stuck_query="${stuck_query:1:${#stuck_query}-3}"

stuck_query_excludes_only_paused "${stuck_query}" ||
  fail 'the stuck query must exclude ReconcilePaused from all four metric selectors'

# Mutation controls keep this from becoming a self-affirming source check. The
# contract must fail if the pause exemption is removed, and also if a future edit
# swaps in ReconcileError and suppresses a genuine failed reconciliation.
ablated_query="${stuck_query//,reason!=\"ReconcilePaused\"/}"
if stuck_query_excludes_only_paused "${ablated_query}"; then
  fail 'test control: removing the paused exclusion must break the query contract'
fi
wrong_reason_query="${stuck_query//ReconcilePaused/ReconcileError}"
if stuck_query_excludes_only_paused "${wrong_reason_query}"; then
  fail 'test control: excluding a real reconciliation error must break the query contract'
fi

# Negative control, and the load-bearing one. The exporter reads
# `repo.github.m.upbound.io/Repository` resources, which exist only where
# Crossplane and the github-config app are installed — the Hetzner overlay. So
# the SHARED Coroot base must not carry the activation: anyone who opts Coroot
# in on another provider would otherwise inherit a permanently empty sensor plus
# cluster-wide RBAC for CRDs that are not present.
#
# This surface is what makes the pair non-vacuous. The Docker infrastructure
# layer is checked below as well, but it leaves Coroot itself commented out, so
# on its own it would also pass with the component referenced from the base —
# it cannot distinguish default-off from Coroot-absent.
coroot_rendered="$(kubectl kustomize "${coroot_dir}")" ||
  fail "the ${coroot_dir} surface must render"
reject_text \
  "${coroot_rendered}" \
  'crossplane-sync-exporter' \
  'the shared coroot base must keep the exporter default-off'

docker_rendered="$(kubectl kustomize "${docker_infrastructure}")" ||
  fail "the ${docker_infrastructure} surface must render"
reject_text \
  "${docker_rendered}" \
  'crossplane-sync-exporter' \
  'the exporter must stay out of the opt-in local provider layer'

rendered="$(kubectl kustomize "${exporter_component}")" ||
  fail 'the Crossplane sync-exporter component must render'

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
require_rule \
  "${cluster_role}" \
  'apiGroups=[repo.github.m.upbound.io] resources=[repositories] verbs=[get,list,watch]' \
  'the exporter must hold exactly one read-only rule on the managed resource it exports'

# The DISCOVERY half of the RBAC, and the reason this component shipped dead
# (#3068). kube-state-metrics resolves a CustomResourceStateMetrics
# `groupVersionKind` through the CRD API before it can build a store for it, so
# the managed-resource grant above is necessary but NOT sufficient: without this
# rule the reflector retry-loops on a forbidden list and the exporter emits zero
# condition series while reporting itself 1/1 Ready.
#
# That is the same silent-sensor shape the rest of this contract guards, one
# level down — an absent series is indistinguishable from a healthy fleet — and
# nothing else in the pipeline catches it, because every assertion above passes
# on a manifest whose exporter cannot read a single resource.
#
# `resourceNames` cannot narrow this: Kubernetes RBAC only honours it for
# get/update/delete/patch, never for list or watch, and the discovery path
# lists. Reading CRD *definitions* is schema access, which is what keeps this
# consistent with the scoping rationale on the rule above: managed-resource
# access, where provider configuration actually lives, stays pinned to the
# kinds listed there.
require_rule \
  "${cluster_role}" \
  'apiGroups=[apiextensions.k8s.io] resources=[customresourcedefinitions] verbs=[get,list,watch]' \
  'the exporter must hold exactly one read-only rule granting the CRD read its GVK discovery requires'

# Closes the set: the two rules above are the ONLY rules this role may carry.
reject_unexpected_rules \
  "${cluster_role}" \
  'apiGroups=[repo.github.m.upbound.io] resources=[repositories] verbs=[get,list,watch]' \
  'apiGroups=[apiextensions.k8s.io] resources=[customresourcedefinitions] verbs=[get,list,watch]'

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

printf 'PASS: the activated Crossplane sync exporter keeps its least-privilege metric contract\n'
