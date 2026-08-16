#!/usr/bin/env bash

# Regression contract for the Crossplane provider egress policy.
#
# Crossplane core and its providers need a narrow set of external HTTPS
# destinations. Cilium's toFQDNs policy keeps that set closed at L3/L4. Adding
# serverNames changes the rule into an Envoy-intercepted path; in production the
# proxy accepted the policy verdict but never completed the TCP handshake,
# leaving every GitHub managed resource and provider package reconciliation
# unable to reach its upstream service.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly hetzner_dir="${root_dir}/k8s/providers/hetzner"
readonly bootstrap_dir="${root_dir}/k8s/providers/hetzner/bootstrap"
readonly controllers_dir="${root_dir}/k8s/providers/hetzner/infrastructure/controllers"
readonly infrastructure_dir="${root_dir}/k8s/providers/hetzner/infrastructure"
readonly apps_dir="${root_dir}/k8s/providers/hetzner/apps"
readonly helm_policy_rules="${root_dir}/scripts/tests/crossplane-egress-policy-rules.yaml"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

bootstrap_rendered="$(kubectl kustomize "${bootstrap_dir}")" ||
  fail 'the deployed Hetzner bootstrap overlay must render'
rendered="$(kubectl kustomize "${controllers_dir}")" ||
  fail 'the deployed Hetzner controller overlay must render'
infrastructure_rendered="$(kubectl kustomize "${infrastructure_dir}")" ||
  fail 'the deployed Hetzner infrastructure overlay must render'
apps_rendered="$(kubectl kustomize "${apps_dir}")" ||
  fail 'the deployed Hetzner apps overlay must render'

effective_policy_render="${bootstrap_rendered}"$'\n---\n'"${rendered}"$'\n---\n'"${infrastructure_rendered}"$'\n---\n'"${apps_rendered}"

command -v yq >/dev/null 2>&1 ||
  fail 'mikefarah yq v4.9+ is required to inspect generated policy templates'
command -v kyverno >/dev/null 2>&1 ||
  fail 'Kyverno CLI is required to evaluate generated and mutated policies'
command -v ksail >/dev/null 2>&1 ||
  fail 'KSail is required to inspect Helm-rendered policy resources'

yq_capability="$(
  yq e -N -r \
    '["crossplane-system"] | any_c(. == "crossplane-system")' \
    - 2>/dev/null <<<'null'
)" || fail 'mikefarah yq v4.9+ must support e, -N, and any_c'
[ "${yq_capability}" = 'true' ] ||
  fail 'mikefarah yq v4.9+ capability probe returned an unexpected result'

test_temp_root="$(mktemp -d /tmp/crossplane-egress-policy.XXXXXX)"
readonly test_temp_root
# shellcheck disable=SC2329 # Invoked indirectly by the EXIT trap.
cleanup() {
  rm -rf "${test_temp_root}"
}
trap cleanup EXIT

select_crossplane_policy() {
  awk '
    function reset_document() {
      document = ""
      is_cilium_policy = 0
      in_metadata = 0
      is_crossplane_namespace = 0
      is_allow_crossplane = 0
    }

    function emit_if_selected() {
      if (is_cilium_policy && is_crossplane_namespace && is_allow_crossplane) {
        printf "%s", document
      }
      reset_document()
    }

    BEGIN { reset_document() }

    /^---$/ {
      emit_if_selected()
      next
    }

    {
      document = document $0 ORS
      if ($0 == "kind: CiliumNetworkPolicy") {
        is_cilium_policy = 1
      }
      if ($0 == "metadata:") {
        in_metadata = 1
      } else if (in_metadata && $0 !~ /^ /) {
        in_metadata = 0
      }
      if (in_metadata && $0 == "  namespace: crossplane-system") {
        is_crossplane_namespace = 1
      }
      if (in_metadata && $0 == "  name: allow-crossplane") {
        is_allow_crossplane = 1
      }
    }

    END { emit_if_selected() }
  ' <<<"$1"
}

static_crossplane_policy_inventory() {
  yq e -N -r '
    select(
      ((.kind == "CiliumNetworkPolicy" or .kind == "NetworkPolicy") and
        .metadata.namespace == "crossplane-system") or
      (.kind == "CiliumClusterwideNetworkPolicy" and
        .spec.nodeSelector == null and
        (
          .spec.endpointSelector == {} or
          .spec.endpointSelector == null or
          .spec.endpointSelector.matchLabels."k8s:io.kubernetes.pod.namespace" ==
            "crossplane-system" or
          (
            .spec.endpointSelector.matchLabels."k8s:io.kubernetes.pod.namespace" == null and
            ([.spec.endpointSelector.matchExpressions[]? |
              select(.key == "k8s:io.kubernetes.pod.namespace")] | length == 0)
          ) or
          ([.spec.endpointSelector.matchExpressions[]? |
            select(.key == "k8s:io.kubernetes.pod.namespace") |
            select(
              (.operator == "In" and
                ((.values // []) | any_c(. == "crossplane-system"))) or
              (.operator == "NotIn" and
                (((.values // []) | any_c(. == "crossplane-system")) | not)) or
              .operator == "Exists"
            )] | length > 0)
        ))
    ) |
    [.kind, (.metadata.namespace // ""), .metadata.name] |
    join("|")
  ' - <<<"$1" | awk 'NF'
}

assert_generated_policy_contract() {
  local candidate_render="$1"
  local case_dir
  local generator_dir
  local generator_output_dir
  local mutation_dir
  local mutation_output_dir
  local policy_bundle
  local kyverno_result
  local applicable_generators
  local actual_generators
  local expected_generators
  local actual_contract
  local expected_contract
  local expected_mutated_contract
  local actual_mutated_contract
  local mutated_files

  case_dir="$(mktemp -d "${test_temp_root}/kyverno.XXXXXX")"
  policy_bundle="${case_dir}/policies.yaml"
  generator_dir="${case_dir}/generators"
  generator_output_dir="${case_dir}/generated"
  mutation_dir="${case_dir}/mutations"
  mutation_output_dir="${case_dir}/mutated"
  printf '%s\n' "${candidate_render}" >"${policy_bundle}"

  # Ask Kyverno itself which generators apply to the real Crossplane Namespace.
  # This preserves names, selectors, expressions, any/all blocks, exclusions,
  # and future matcher semantics instead of reimplementing them in yq.
  # shellcheck disable=SC2016 # $index is a yq variable.
  CROSSPLANE_GENERATOR_DIR="${generator_dir}" yq e -N \
    -s 'strenv(CROSSPLANE_GENERATOR_DIR) + "/" + ($index | tostring) + "-" + .metadata.name + ".yaml"' \
    'select(
      (.kind == "ClusterPolicy" or .kind == "Policy") and
      ([.spec.rules[] |
        select(
          .generate.kind == "CiliumNetworkPolicy" or
          .generate.kind == "NetworkPolicy" or
          .generate.kind == "CiliumClusterwideNetworkPolicy" or
          ([.generate.foreach[]?.kind] |
            any_c(
              . == "CiliumNetworkPolicy" or
              . == "NetworkPolicy" or
              . == "CiliumClusterwideNetworkPolicy"
            ))
        )] | length > 0)
    )' "${policy_bundle}"

  if ! kyverno_result="$(
    kyverno apply "${generator_dir}" \
      --resource "${controllers_dir}/crossplane/namespace.yaml" \
      --output "${generator_output_dir}" \
      --remove-color 2>&1
  )"; then
    printf '%s\n' "${kyverno_result}" >&2
    fail 'Kyverno must evaluate the deployed network-policy generators'
  fi

  applicable_generators="$(
    yq e -N -r '
      select(
        (.kind == "CiliumNetworkPolicy" or
          .kind == "NetworkPolicy" or
          .kind == "CiliumClusterwideNetworkPolicy") and
        .metadata.labels."generate.kyverno.io/policy-name" != null
      ) |
      .metadata.labels."generate.kyverno.io/policy-name"
    ' "${generator_output_dir}"/*.yaml | LC_ALL=C sort -u
  )"
  [ "${applicable_generators}" = 'add-default-deny' ] ||
    fail 'add-default-deny must be the only Kyverno policy that generates a network policy for Crossplane'

  actual_generators="$(
    # shellcheck disable=SC2016 # $rule is a yq variable.
    yq e -N -r '
      select(
        (.kind == "ClusterPolicy" or .kind == "Policy") and
        .metadata.name == "add-default-deny"
      ) |
      .spec.rules[] as $rule |
      (
        $rule |
        select(
          .generate.kind == "CiliumNetworkPolicy" or
          .generate.kind == "NetworkPolicy" or
          .generate.kind == "CiliumClusterwideNetworkPolicy"
        ) |
        (.name + "|" + .generate.kind)
      ),
      (
        $rule.generate.foreach[]? |
        select(
          .kind == "CiliumNetworkPolicy" or
          .kind == "NetworkPolicy" or
          .kind == "CiliumClusterwideNetworkPolicy"
        ) |
        ($rule.name + "|foreach|" + .kind)
      )
    ' "${policy_bundle}" | awk 'NF'
  )"
  expected_generators="$(
    printf '%s\n' \
      'generate-default-deny|CiliumNetworkPolicy' \
      'generate-allow-dns|CiliumNetworkPolicy' \
      'generate-default-deny-networkpolicy|NetworkPolicy'
  )"
  [ "${actual_generators}" = "${expected_generators}" ] ||
    fail 'Kyverno must keep exactly the three reviewed generated network-policy rules'

  actual_contract="$(
    # shellcheck disable=SC2016 # $policy is a yq variable.
    yq e -N -r '
      select(
        (.kind == "ClusterPolicy" or .kind == "Policy") and
        .metadata.name == "add-default-deny"
      ) |
      . as $policy |
      .spec.rules[] |
      select(.generate.kind == "CiliumNetworkPolicy" or
        .generate.kind == "NetworkPolicy" or
        .generate.kind == "CiliumClusterwideNetworkPolicy") |
      [($policy.kind + "/" + ($policy.metadata.namespace // "") + "/" +
        $policy.metadata.name), .name, .generate.apiVersion, .generate.kind,
        .generate.name, .generate.namespace,
        (.generate.generateExisting | tostring),
        (.generate.synchronize | tostring),
        (.match.any[0].resources.kinds | @json),
        (.exclude.any[0].resources.names | @json),
        (.generate.data.spec | @json)] |
      join("|")
    ' "${policy_bundle}" | awk 'NF'
  )"
  expected_contract="$(
    printf '%s\n' \
      'ClusterPolicy//add-default-deny|generate-default-deny|cilium.io/v2|CiliumNetworkPolicy|default-deny|{{request.object.metadata.name}}|true|true|["Namespace"]|["kube-system","kube-public","kube-node-lease"]|{"enableDefaultDeny":{"egress":true,"ingress":true},"endpointSelector":{}}' \
      'ClusterPolicy//add-default-deny|generate-allow-dns|cilium.io/v2|CiliumNetworkPolicy|allow-dns|{{request.object.metadata.name}}|true|true|["Namespace"]|["kube-system","kube-public","kube-node-lease"]|{"egress":[{"toEndpoints":[{"matchLabels":{"k8s-app":"kube-dns","k8s:io.kubernetes.pod.namespace":"kube-system"}}],"toPorts":[{"ports":[{"port":"53","protocol":"UDP"},{"port":"53","protocol":"TCP"}]}]}],"endpointSelector":{}}' \
      'ClusterPolicy//add-default-deny|generate-default-deny-networkpolicy|networking.k8s.io/v1|NetworkPolicy|default-deny|{{request.object.metadata.name}}|true|true|["Namespace"]|["kube-system","kube-public","kube-node-lease"]|{"podSelector":{},"policyTypes":["Ingress","Egress"]}'
  )"
  [ "${actual_contract}" = "${expected_contract}" ] ||
    fail 'Kyverno generated Crossplane policies must remain the exact default-deny and DNS-only contract'

  # Evaluate admission-time and mutate-existing rules against both the real
  # Namespace trigger and the real policy target. Every emitted form of
  # allow-crossplane must remain byte-for-byte equivalent at the spec level.
  # shellcheck disable=SC2016 # $index is a yq variable.
  CROSSPLANE_MUTATION_DIR="${mutation_dir}" yq e -N \
    -s 'strenv(CROSSPLANE_MUTATION_DIR) + "/" + ($index | tostring) + "-" + .metadata.name + ".yaml"' \
    'select(
      (.kind == "ClusterPolicy" or .kind == "Policy") and
      ([.spec.rules[] | select(.mutate != null)] | length > 0)
    )' "${policy_bundle}"

  if ! kyverno_result="$(
    kyverno apply "${mutation_dir}" \
      --resource "${controllers_dir}/crossplane/namespace.yaml" \
      --resource "${controllers_dir}/crossplane/cilium-network-policy.yaml" \
      --target-resource "${controllers_dir}/crossplane/cilium-network-policy.yaml" \
      --output "${mutation_output_dir}" \
      --remove-color 2>&1
  )"; then
    printf '%s\n' "${kyverno_result}" >&2
    fail 'Kyverno must evaluate deployed mutations of allow-crossplane'
  fi

  expected_mutated_contract="$(
    yq e -o=json -I=0 '.spec | sort_keys(..)' \
      "${controllers_dir}/crossplane/cilium-network-policy.yaml"
  )"
  mutated_files=("${mutation_output_dir}"/*.yaml)
  actual_mutated_contract="$(
    yq e -o=json -I=0 '
      select(
        .kind == "CiliumNetworkPolicy" and
        .metadata.namespace == "crossplane-system" and
        .metadata.name == "allow-crossplane"
      ) |
      .spec |
      sort_keys(..)
    ' "${mutated_files[@]}" | LC_ALL=C sort -u
  )"
  [ -z "${actual_mutated_contract}" ] ||
    [ "${actual_mutated_contract}" = "${expected_mutated_contract}" ] ||
    fail 'Kyverno mutations must not change the reviewed allow-crossplane spec'
}

policy="$(select_crossplane_policy "${rendered}")"
readonly policy

selected_policy_count="$(awk '/^kind: CiliumNetworkPolicy$/ { count++ } END { print count + 0 }' <<<"${policy}")"
[ "${selected_policy_count}" -eq 1 ] ||
  fail 'the deployed overlay must contain exactly one allow-crossplane CiliumNetworkPolicy'

expected_static_policy='CiliumNetworkPolicy|crossplane-system|allow-crossplane'
static_crossplane_policies="$(static_crossplane_policy_inventory "${effective_policy_render}")"
[ "${static_crossplane_policies}" = "${expected_static_policy}" ] ||
  fail 'allow-crossplane must be the only static policy that can select Crossplane pods across deployed layers'

# Kyverno materializes default-deny and allow-dns policies after the static
# layers are applied. Inspect generators across every deployed Hetzner layer so
# the effective additive allow-set is covered too.
assert_generated_policy_contract "${effective_policy_render}"

# kubectl kustomize intentionally stops at HelmRelease CRs. KSail walks every
# deployed Hetzner layer, renders the declared charts in-process, then applies
# these CEL rules to every chart child. Require a chart-only Deployment marker
# so a skipped Helm pass cannot look indistinguishable from a clean inventory.
if ! helm_validation="$(
  ksail workload validate "${hetzner_dir}" \
    --rules "${helm_policy_rules}" 2>&1
)"; then
  printf '%s\n' "${helm_validation}" >&2
  fail 'Helm-rendered Crossplane resources must satisfy the egress-policy contract'
fi
if [[ "${helm_validation}" != *'observe-crossplane-chart-deployment'* ]] ||
  [[ "${helm_validation}" != *'Deployment/crossplane-system/crossplane'* ]]; then
  printf '%s\n' "${helm_validation}" >&2
  fail 'KSail must prove it inspected the rendered Crossplane chart'
fi

assert_helm_rules_reject() {
  local fixture_name="$1"
  local fixture_body="$2"
  local failure_message="$3"
  local fixture_path="${test_temp_root}/${fixture_name}.yaml"

  printf '%s\n' "${fixture_body}" >"${fixture_path}"
  if ksail workload validate "${fixture_path}" \
    --skip-helm-render \
    --rules "${helm_policy_rules}" >/dev/null 2>&1; then
    fail "${failure_message}"
  fi
}

helm_network_policy_fixture=$'apiVersion: networking.k8s.io/v1\nkind: NetworkPolicy\nmetadata:\n  name: chart-world-egress\n  namespace: crossplane-system\nspec:\n  podSelector: {}\n  policyTypes: [Egress]\n  egress:\n  - {}'
assert_helm_rules_reject \
  'helm-network-policy' \
  "${helm_network_policy_fixture}" \
  'the Helm-render guard must reject an additional Crossplane NetworkPolicy'

helm_foreach_generator_fixture=$'apiVersion: kyverno.io/v1\nkind: ClusterPolicy\nmetadata:\n  name: chart-foreach-network-policy\nspec:\n  rules:\n  - name: chart-foreach-network-policy\n    match:\n      resources:\n        kinds: [Namespace]\n    generate:\n      foreach:\n      - list: "[request.object.metadata.name]"\n        apiVersion: cilium.io/v2\n        kind: CiliumNetworkPolicy\n        name: chart-world-egress\n        namespace: "{{element}}"\n        data:\n          spec:\n            endpointSelector: {}\n            egress:\n            - toEntities: [world]'
assert_helm_rules_reject \
  'helm-foreach-generator' \
  "${helm_foreach_generator_fixture}" \
  'the Helm-render guard must reject a Kyverno foreach network-policy generator'

helm_mutation_fixture=$'apiVersion: kyverno.io/v1\nkind: ClusterPolicy\nmetadata:\n  name: chart-network-policy-mutation\nspec:\n  rules:\n  - name: chart-network-policy-mutation\n    match:\n      resources:\n        kinds: [CiliumNetworkPolicy]\n    mutate:\n      patchStrategicMerge:\n        spec:\n          egress:\n          - toEntities: [world]'
assert_helm_rules_reject \
  'helm-policy-mutation' \
  "${helm_mutation_fixture}" \
  'the Helm-render guard must reject a Kyverno network-policy mutation'

unexpected_generate_policy=$'apiVersion: kyverno.io/v1\nkind: ClusterPolicy\nmetadata:\n  name: generate-crossplane-world-egress\nspec:\n  rules:\n  - name: generate-world-egress\n    match:\n      resources:\n        kinds: [Namespace]\n    generate:\n      generateExisting: true\n      apiVersion: cilium.io/v2\n      kind: CiliumNetworkPolicy\n      name: allow-world\n      namespace: "{{request.object.metadata.name}}"\n      synchronize: true\n      data:\n        spec:\n          endpointSelector: {}\n          egress:\n          - toEntities: [world]'
render_with_unexpected_generate="${effective_policy_render}"$'\n---\n'"${unexpected_generate_policy}"
if (assert_generated_policy_contract "${render_with_unexpected_generate}") >/dev/null 2>&1; then
  fail 'the regression guard must reject an additional Kyverno-generated network policy'
fi

unrelated_generate_policy=$'apiVersion: kyverno.io/v1\nkind: ClusterPolicy\nmetadata:\n  name: generate-monitoring-network-policy\nspec:\n  rules:\n  - name: generate-monitoring-egress\n    match:\n      any:\n      - resources:\n          kinds: [Pod]\n    generate:\n      generateExisting: true\n      apiVersion: networking.k8s.io/v1\n      kind: NetworkPolicy\n      name: monitoring-egress\n      namespace: monitoring\n      synchronize: true\n      data:\n        spec:\n          podSelector: {}\n          egress:\n          - {}'
render_with_unrelated_generate="${effective_policy_render}"$'\n---\n'"${unrelated_generate_policy}"
if ! (assert_generated_policy_contract "${render_with_unrelated_generate}") >/dev/null 2>&1; then
  fail 'the regression guard must ignore Kyverno generators that cannot target Crossplane'
fi

positive_filtered_generate_policy=$'apiVersion: kyverno.io/v1\nkind: ClusterPolicy\nmetadata:\n  name: generate-filtered-network-policies\nspec:\n  rules:\n  - name: name-filtered\n    match:\n      resources:\n        kinds: [Namespace]\n        names: [monitoring]\n    generate:\n      apiVersion: cilium.io/v2\n      kind: CiliumNetworkPolicy\n      name: monitoring-egress\n      namespace: "{{request.object.metadata.name}}"\n      data:\n        spec:\n          endpointSelector: {}\n          egress:\n          - toEntities: [world]\n  - name: selector-filtered\n    match:\n      resources:\n        kinds: [Namespace]\n        selector:\n          matchLabels:\n            team: monitoring\n    generate:\n      apiVersion: networking.k8s.io/v1\n      kind: NetworkPolicy\n      name: monitoring-default-deny\n      namespace: "{{request.object.metadata.name}}"\n      data:\n        spec:\n          podSelector: {}\n          policyTypes: [Ingress, Egress]'
positive_filtered_foreach_policy=$'apiVersion: kyverno.io/v1\nkind: ClusterPolicy\nmetadata:\n  name: foreach-filtered-network-policy\nspec:\n  rules:\n  - name: foreach-name-filtered\n    match:\n      resources:\n        kinds: [Namespace]\n        names: [monitoring]\n    generate:\n      foreach:\n      - list: "[request.object.metadata.name]"\n        apiVersion: cilium.io/v2\n        kind: CiliumNetworkPolicy\n        name: monitoring-foreach-egress\n        namespace: "{{element}}"\n        data:\n          spec:\n            endpointSelector: {}\n            egress:\n            - toEntities: [world]'
render_with_positive_filters="${effective_policy_render}"$'\n---\n'"${positive_filtered_generate_policy}"$'\n---\n'"${positive_filtered_foreach_policy}"
if ! (assert_generated_policy_contract "${render_with_positive_filters}") >/dev/null 2>&1; then
  fail 'the regression guard must honor positive Kyverno name and selector filters, including foreach generation'
fi

foreach_generate_world_policy=$'apiVersion: kyverno.io/v1\nkind: ClusterPolicy\nmetadata:\n  name: foreach-crossplane-world-egress\nspec:\n  rules:\n  - name: foreach-generate-world-egress\n    match:\n      resources:\n        kinds: [Namespace]\n        names: [crossplane-system]\n    generate:\n      foreach:\n      - list: "[request.object.metadata.name]"\n        apiVersion: cilium.io/v2\n        kind: CiliumNetworkPolicy\n        name: allow-world-{{elementIndex}}\n        namespace: "{{element}}"\n        data:\n          spec:\n            endpointSelector: {}\n            egress:\n            - toEntities: [world]'
render_with_foreach_generate="${effective_policy_render}"$'\n---\n'"${foreach_generate_world_policy}"
if (assert_generated_policy_contract "${render_with_foreach_generate}") >/dev/null 2>&1; then
  fail 'the regression guard must reject foreach-generated Crossplane network policies'
fi

mutate_existing_world_policy=$'apiVersion: kyverno.io/v1\nkind: ClusterPolicy\nmetadata:\n  name: mutate-crossplane-world-egress\nspec:\n  rules:\n  - name: mutate-existing-crossplane-policy\n    match:\n      resources:\n        kinds: [Namespace]\n        names: [crossplane-system]\n    mutate:\n      mutateExistingOnPolicyUpdate: true\n      targets:\n      - apiVersion: cilium.io/v2\n        kind: CiliumNetworkPolicy\n        name: allow-crossplane\n        namespace: crossplane-system\n      patchStrategicMerge:\n        spec:\n          egress:\n          - toEntities: [world]'
render_with_mutate_existing="${effective_policy_render}"$'\n---\n'"${mutate_existing_world_policy}"
if (assert_generated_policy_contract "${render_with_mutate_existing}") >/dev/null 2>&1; then
  fail 'the regression guard must reject Kyverno mutate-existing widening of allow-crossplane'
fi

additional_world_policy=$'apiVersion: cilium.io/v2\nkind: CiliumNetworkPolicy\nmetadata:\n  name: additional-world-egress\n  namespace: crossplane-system\nspec:\n  endpointSelector: {}\n  egress:\n  - toEntities: [world]'
rendered_with_additional_world_policy="${effective_policy_render}"$'\n---\n'"${additional_world_policy}"
if [ "$(static_crossplane_policy_inventory "${rendered_with_additional_world_policy}")" = "${expected_static_policy}" ]; then
  fail 'the regression guard must reject an additional Crossplane CiliumNetworkPolicy'
fi

standard_world_policy=$'apiVersion: networking.k8s.io/v1\nkind: NetworkPolicy\nmetadata:\n  name: additional-standard-world-egress\n  namespace: crossplane-system\nspec:\n  podSelector: {}\n  policyTypes: [Egress]\n  egress:\n  - {}'
rendered_with_standard_world_policy="${effective_policy_render}"$'\n---\n'"${standard_world_policy}"
if [ "$(static_crossplane_policy_inventory "${rendered_with_standard_world_policy}")" = "${expected_static_policy}" ]; then
  fail 'the regression guard must reject an additional standard Crossplane NetworkPolicy'
fi

clusterwide_world_policy=$'apiVersion: cilium.io/v2\nkind: CiliumClusterwideNetworkPolicy\nmetadata:\n  name: additional-clusterwide-world-egress\nspec:\n  endpointSelector:\n    matchLabels:\n      k8s:io.kubernetes.pod.namespace: crossplane-system\n  egress:\n  - toEntities: [world]'
rendered_with_clusterwide_world_policy="${effective_policy_render}"$'\n---\n'"${clusterwide_world_policy}"
if [ "$(static_crossplane_policy_inventory "${rendered_with_clusterwide_world_policy}")" = "${expected_static_policy}" ]; then
  fail 'the regression guard must reject a Cilium cluster-wide policy that can select Crossplane pods'
fi

unrelated_clusterwide_policy=$'apiVersion: cilium.io/v2\nkind: CiliumClusterwideNetworkPolicy\nmetadata:\n  name: monitoring-only-egress\nspec:\n  endpointSelector:\n    matchLabels:\n      k8s:io.kubernetes.pod.namespace: monitoring\n  egress:\n  - toEntities: [world]'
rendered_with_unrelated_clusterwide="${effective_policy_render}"$'\n---\n'"${unrelated_clusterwide_policy}"
if [ "$(static_crossplane_policy_inventory "${rendered_with_unrelated_clusterwide}")" != "${expected_static_policy}" ]; then
  fail 'the regression guard must ignore cluster-wide policies constrained to another namespace'
fi

if grep -Eq '^[[:space:]]*serverNames:' <<<"${policy}"; then
  fail 'Crossplane HTTPS egress must stay on the direct L3/L4 path, not the broken SNI proxy path'
fi

assert_no_unrestricted_egress() {
  local candidate_policy="$1"

  if grep -Eq "^[[:space:]]*-[[:space:]]+['\"]?(world|all)['\"]?([[:space:]]+#.*)?$" <<<"${candidate_policy}" ||
    grep -Eq "^[[:space:]]*-[[:space:]]+toEntities:[[:space:]]*(\\[[[:space:]]*['\"]?(world|all)['\"]?[[:space:]]*\\]|['\"]?(world|all)['\"]?)([[:space:]]+#.*)?$" <<<"${candidate_policy}"; then
    fail 'Crossplane must not receive unrestricted world or all-entity egress'
  fi

  if grep -Eq '^[[:space:]]*-[[:space:]]+toCIDR(Set)?:' <<<"${candidate_policy}"; then
    fail 'Crossplane must not receive CIDR-based egress'
  fi
}

https_egress_contract() {
  awk '
    /^  - toFQDNs:$/ { in_https_egress = 1 }
    in_https_egress && /^  - / && !/^  - toFQDNs:$/ { exit }
    in_https_egress { print }
  ' <<<"$1"
}

expected_https_egress=$'  - toFQDNs:\n    - matchName: ghcr.io\n    - matchName: pkg-containers.githubusercontent.com\n    - matchName: api.github.com\n    - matchName: xpkg.upbound.io\n    - matchName: d3qrbvrml4iuq4.cloudfront.net\n    - matchName: sts.eu-central-1.amazonaws.com\n    - matchName: iam.amazonaws.com\n    - matchName: api.ui.com\n    toPorts:\n    - ports:\n      - port: "443"\n        protocol: TCP'
readonly expected_https_egress

assert_direct_https_contract() {
  local candidate_policy="$1"
  local actual_contract

  actual_contract="$(https_egress_contract "${candidate_policy}")"
  [ "${actual_contract}" = "${expected_https_egress}" ] ||
    fail 'Crossplane HTTPS egress must remain the exact direct FQDN-scoped TCP/443 contract'
}

assert_kube_api_contract() {
  local candidate_policy="$1"
  local actual_contract
  local expected_contract

  actual_contract="$(
    awk '
      /^  - toEntities:$/ { in_kube_api_egress = 1 }
      in_kube_api_egress && /^  - / && !/^  - toEntities:$/ { exit }
      in_kube_api_egress { print }
    ' <<<"${candidate_policy}"
  )"
  expected_contract=$'  - toEntities:\n    - kube-apiserver'
  [ "${actual_contract}" = "${expected_contract}" ] ||
    fail 'Crossplane Kubernetes API egress must remain the exact kube-apiserver entity rule'
}

assert_egress_shape() {
  local candidate_policy="$1"
  local actual_rules
  local expected_rules

  actual_rules="$(awk '/^  - / { print }' <<<"${candidate_policy}")"
  expected_rules=$'  - toEntities:\n  - toFQDNs:\n  - toEndpoints:'
  [ "${actual_rules}" = "${expected_rules}" ] ||
    fail 'Crossplane egress must contain only the reviewed Kubernetes API, HTTPS, and DNS rules'
}

dns_egress_contract() {
  awk '
    /^  - toEndpoints:$/ { in_dns_egress = 1 }
    in_dns_egress && /^  endpointSelector:/ { exit }
    in_dns_egress { print }
  ' <<<"$1"
}

assert_dns_contract() {
  local candidate_policy="$1"
  local actual_contract
  local expected_contract

  actual_contract="$(dns_egress_contract "${candidate_policy}")"
  expected_contract=$'  - toEndpoints:\n    - matchLabels:\n        k8s-app: kube-dns\n        k8s:io.kubernetes.pod.namespace: kube-system\n    toPorts:\n    - ports:\n      - port: "53"\n        protocol: UDP\n      - port: "53"\n        protocol: TCP\n      rules:\n        dns:\n        - matchName: ghcr.io\n        - matchName: pkg-containers.githubusercontent.com\n        - matchName: api.github.com\n        - matchName: xpkg.upbound.io\n        - matchName: d3qrbvrml4iuq4.cloudfront.net\n        - matchName: sts.eu-central-1.amazonaws.com\n        - matchName: iam.amazonaws.com\n        - matchName: api.ui.com\n        - matchPattern: \x27*.cluster.local\x27'
  [ "${actual_contract}" = "${expected_contract}" ] ||
    fail 'Crossplane DNS egress must remain the exact reviewed resolver, port, and name contract'
}

assert_policy_scope() {
  local candidate_policy="$1"
  local actual_scope
  local expected_scope

  actual_scope="$(awk '/^  namespace:|^  endpointSelector:/ { print }' <<<"${candidate_policy}")"
  expected_scope=$'  namespace: crossplane-system\n  endpointSelector: {}'
  [ "${actual_scope}" = "${expected_scope}" ] ||
    fail 'Crossplane egress must select every pod in only the crossplane-system namespace'
}

assert_no_unrestricted_egress "${policy}"
assert_direct_https_contract "${policy}"
assert_kube_api_contract "${policy}"
assert_egress_shape "${policy}"
assert_dns_contract "${policy}"
assert_policy_scope "${policy}"

# Prove the regression guard rejects scope, selector, resolver, transport,
# proxy, and broad egress drift instead of merely checking that reviewed
# entries are present.
wildcard_selector=$'- toFQDNs:\n    - matchPattern: "*.github.com"'
wildcard_policy="${policy/- toFQDNs:/${wildcard_selector}}"
if (assert_direct_https_contract "${wildcard_policy}") >/dev/null 2>&1; then
  fail 'the regression guard must reject wildcard Crossplane FQDN selectors'
fi

extra_fqdn_rule=$'  - toFQDNs:\n    - matchPattern: "*"\n    toPorts:\n    - ports:\n      - port: "443"\n        protocol: TCP\n  endpointSelector: {}'
empty_selector='  endpointSelector: {}'
extra_fqdn_policy="${policy/${empty_selector}/${extra_fqdn_rule}}"
if (assert_egress_shape "${extra_fqdn_policy}") >/dev/null 2>&1; then
  fail 'the regression guard must reject additional Crossplane FQDN egress rules'
fi

block_world_rule=$'- toEntities:\n    - world\n  - toFQDNs:'
block_world_policy="${policy/- toFQDNs:/${block_world_rule}}"
if (assert_no_unrestricted_egress "${block_world_policy}") >/dev/null 2>&1; then
  fail 'the regression guard must reject block-style world egress'
fi

flow_world_rule=$'- toEntities: [world]\n  - toFQDNs:'
flow_world_policy="${policy/- toFQDNs:/${flow_world_rule}}"
if (assert_no_unrestricted_egress "${flow_world_policy}") >/dev/null 2>&1; then
  fail 'the regression guard must reject flow-style world egress'
fi

flow_all_policy="${policy/$'  - toEntities:\n    - kube-apiserver'/$'  - toEntities: [all]'}"
if (assert_no_unrestricted_egress "${flow_all_policy}") >/dev/null 2>&1; then
  fail 'the regression guard must reject flow-style all-entity egress'
fi

commented_world_policy="${policy/kube-apiserver/world # unrestricted}"
if (assert_no_unrestricted_egress "${commented_world_policy}") >/dev/null 2>&1; then
  fail 'the regression guard must reject commented world egress'
fi

commented_all_policy="${policy/$'  - toEntities:\n    - kube-apiserver'/$'  - toEntities: [all] # unrestricted'}"
if (assert_no_unrestricted_egress "${commented_all_policy}") >/dev/null 2>&1; then
  fail 'the regression guard must reject commented all-entity egress'
fi

host_entity_policy="${policy/kube-apiserver/host}"
if (assert_kube_api_contract "${host_entity_policy}") >/dev/null 2>&1; then
  fail 'the regression guard must reject a changed Kubernetes API entity'
fi

kube_api_port_rule=$'    - kube-apiserver\n    toPorts:\n    - ports:\n      - port: "6443"\n        protocol: TCP'
kube_api_port_policy="${policy/    - kube-apiserver/${kube_api_port_rule}}"
if (assert_kube_api_contract "${kube_api_port_policy}") >/dev/null 2>&1; then
  fail 'the regression guard must reject a restricted Kubernetes API transport'
fi

widened_port_policy="${policy/port: \"443\"/port: \"8443\"}"
if (assert_direct_https_contract "${widened_port_policy}") >/dev/null 2>&1; then
  fail 'the regression guard must reject a changed Crossplane egress port'
fi

widened_protocol_policy="${policy/protocol: TCP/protocol: UDP}"
if (assert_direct_https_contract "${widened_protocol_policy}") >/dev/null 2>&1; then
  fail 'the regression guard must reject a changed Crossplane egress protocol'
fi

l7_proxy_rule=$'protocol: TCP\n      rules:\n        http:\n        - method: GET'
l7_proxy_policy="${policy/protocol: TCP/${l7_proxy_rule}}"
if (assert_direct_https_contract "${l7_proxy_policy}") >/dev/null 2>&1; then
  fail 'the regression guard must reject HTTPS L7 proxy rules'
fi

cidr_rule=$'- toCIDR:\n    - 0.0.0.0/0\n  - toEndpoints:'
cidr_policy="${policy/- toEndpoints:/${cidr_rule}}"
if (assert_no_unrestricted_egress "${cidr_policy}") >/dev/null 2>&1; then
  fail 'the regression guard must reject CIDR-based Crossplane egress'
fi

cidr_set_rule=$'- toCIDRSet:\n    - cidr: 0.0.0.0/0\n  - toEndpoints:'
cidr_set_policy="${policy/- toEndpoints:/${cidr_set_rule}}"
if (assert_no_unrestricted_egress "${cidr_set_policy}") >/dev/null 2>&1; then
  fail 'the regression guard must reject CIDR-set-based Crossplane egress'
fi

dns_rule_prefix=$'      rules:\n        dns:\n        - matchName: ghcr.io'
dns_without_ghcr_prefix=$'      rules:\n        dns:'
dns_missing_name_policy="${policy/${dns_rule_prefix}/${dns_without_ghcr_prefix}}"
if (assert_dns_contract "${dns_missing_name_policy}") >/dev/null 2>&1; then
  fail 'the regression guard must reject an incomplete Crossplane DNS allow-list'
fi

wrong_namespace_policy="${policy/namespace: crossplane-system/namespace: other-system}"
if (assert_policy_scope "${wrong_namespace_policy}") >/dev/null 2>&1; then
  fail 'the regression guard must reject a Crossplane policy in the wrong namespace'
fi

label_selector=$'endpointSelector:\n    matchLabels:\n      app: crossplane'
empty_selector_without_indent='endpointSelector: {}'
label_selector_policy="${policy/${empty_selector_without_indent}/${label_selector}}"
if (assert_policy_scope "${label_selector_policy}") >/dev/null 2>&1; then
  fail 'the regression guard must reject a non-empty Crossplane endpoint selector'
fi

printf 'PASS: Crossplane keeps direct HTTPS egress closed to the reviewed FQDN set\n'

exit 0
