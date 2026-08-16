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
readonly controllers_dir="${root_dir}/k8s/providers/hetzner/infrastructure/controllers"
readonly infrastructure_dir="${root_dir}/k8s/providers/hetzner/infrastructure"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

rendered="$(kubectl kustomize "${controllers_dir}")" ||
  fail 'the deployed Hetzner controller overlay must render'
infrastructure_rendered="$(kubectl kustomize "${infrastructure_dir}")" ||
  fail 'the deployed Hetzner infrastructure overlay must render'

command -v yq >/dev/null 2>&1 ||
  fail 'yq is required to inspect Kyverno generated policy templates'

select_crossplane_policies() {
  local required_name="${2-}"
  local include_standard="${3-0}"
  local include_clusterwide="${4-0}"

  awk '
    function reset_document() {
      document = ""
      is_selected_policy_kind = 0
      is_clusterwide_policy = 0
      in_metadata = 0
      is_crossplane_namespace = 0
      has_required_name = 0
    }

    function emit_if_selected() {
      if (is_selected_policy_kind &&
          (is_clusterwide_policy || is_crossplane_namespace) &&
          (required_name == "" || has_required_name)) {
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
      if ($0 == "kind: CiliumNetworkPolicy" ||
          (include_standard == "1" && $0 == "kind: NetworkPolicy")) {
        is_selected_policy_kind = 1
      }
      if (include_clusterwide == "1" &&
          $0 == "kind: CiliumClusterwideNetworkPolicy") {
        is_selected_policy_kind = 1
        is_clusterwide_policy = 1
      }
      if ($0 == "metadata:") {
        in_metadata = 1
      } else if (in_metadata && $0 !~ /^ /) {
        in_metadata = 0
      }
      if (in_metadata && $0 == "  namespace: crossplane-system") {
        is_crossplane_namespace = 1
      }
      if (in_metadata && $0 == "  name: " required_name) {
        has_required_name = 1
      }
    }

    END { emit_if_selected() }
  ' required_name="${required_name}" include_standard="${include_standard}" \
    include_clusterwide="${include_clusterwide}" <<<"$1"
}

select_crossplane_policy() {
  select_crossplane_policies "$1" 'allow-crossplane' '0' '0'
}

select_crossplane_namespace_policies() {
  select_crossplane_policies "$1" '' '1' '1'
}

assert_generated_policy_contract() {
  local candidate_render="$1"
  local actual_generators
  local expected_generators
  local actual_contract
  local expected_contract

  actual_generators="$(
    # shellcheck disable=SC2016 # $policy is a yq variable.
    yq e -N -r '
      select(.kind == "ClusterPolicy" or .kind == "Policy") |
      . as $policy |
      .spec.rules[] |
      select(.generate != null) |
      select((.generate | @json) |
        test("CiliumNetworkPolicy|NetworkPolicy|CiliumClusterwideNetworkPolicy")) |
      ($policy.kind + "/" + ($policy.metadata.namespace // "") + "/" +
        $policy.metadata.name + "|" + .name)
    ' - <<<"${candidate_render}" | awk 'NF'
  )"
  expected_generators="$(
    printf '%s\n' \
      'ClusterPolicy//add-default-deny|generate-default-deny' \
      'ClusterPolicy//add-default-deny|generate-allow-dns' \
      'ClusterPolicy//add-default-deny|generate-default-deny-networkpolicy'
  )"
  [ "${actual_generators}" = "${expected_generators}" ] ||
    fail 'Kyverno must keep exactly the three reviewed generated network-policy rules'

  actual_contract="$(
    # shellcheck disable=SC2016 # $policy is a yq variable.
    yq e -N -r '
      select(.kind == "ClusterPolicy" or .kind == "Policy") |
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
    ' - <<<"${candidate_render}" | awk 'NF'
  )"
  expected_contract="$(
    printf '%s\n' \
      'ClusterPolicy//add-default-deny|generate-default-deny|cilium.io/v2|CiliumNetworkPolicy|default-deny|{{request.object.metadata.name}}|true|true|["Namespace"]|["kube-system","kube-public","kube-node-lease"]|{"egressDeny":[{}],"enableDefaultDeny":{"egress":true,"ingress":true},"endpointSelector":{},"ingressDeny":[{}]}' \
      'ClusterPolicy//add-default-deny|generate-allow-dns|cilium.io/v2|CiliumNetworkPolicy|allow-dns|{{request.object.metadata.name}}|true|true|["Namespace"]|["kube-system","kube-public","kube-node-lease"]|{"egress":[{"toEndpoints":[{"matchLabels":{"k8s-app":"kube-dns","k8s:io.kubernetes.pod.namespace":"kube-system"}}],"toPorts":[{"ports":[{"port":"53","protocol":"UDP"},{"port":"53","protocol":"TCP"}]}]}],"endpointSelector":{}}' \
      'ClusterPolicy//add-default-deny|generate-default-deny-networkpolicy|networking.k8s.io/v1|NetworkPolicy|default-deny|{{request.object.metadata.name}}|true|true|["Namespace"]|["kube-system","kube-public","kube-node-lease"]|{"podSelector":{},"policyTypes":["Ingress","Egress"]}'
  )"
  [ "${actual_contract}" = "${expected_contract}" ] ||
    fail 'Kyverno generated Crossplane policies must remain the exact default-deny and DNS-only contract'
}

policy="$(select_crossplane_policy "${rendered}")"
readonly policy

selected_policy_count="$(awk '/^kind: CiliumNetworkPolicy$/ { count++ } END { print count + 0 }' <<<"${policy}")"
[ "${selected_policy_count}" -eq 1 ] ||
  fail 'the deployed overlay must contain exactly one allow-crossplane CiliumNetworkPolicy'

static_crossplane_policies="$(select_crossplane_namespace_policies "${rendered}")"
[ "${static_crossplane_policies}" = "${policy}" ] ||
  fail 'allow-crossplane must be the only statically rendered controller policy that can select Crossplane pods'

# Kyverno materializes default-deny and allow-dns policies after the static
# controller layer is applied. Inspect their templates from the deployed
# infrastructure overlay so the effective additive allow-set is covered too.
assert_generated_policy_contract "${infrastructure_rendered}"

unexpected_generate_policy=$'apiVersion: kyverno.io/v1\nkind: ClusterPolicy\nmetadata:\n  name: generate-crossplane-world-egress\nspec:\n  rules:\n  - name: generate-world-egress\n    match:\n      any:\n      - resources:\n          kinds: [Namespace]\n    generate:\n      generateExisting: true\n      apiVersion: cilium.io/v2\n      kind: CiliumNetworkPolicy\n      name: allow-world\n      namespace: "{{request.object.metadata.name}}"\n      synchronize: true\n      data:\n        spec:\n          endpointSelector: {}\n          egress:\n          - toEntities: [world]'
infrastructure_with_unexpected_generate="${infrastructure_rendered}"$'\n---\n'"${unexpected_generate_policy}"
if (assert_generated_policy_contract "${infrastructure_with_unexpected_generate}") >/dev/null 2>&1; then
  fail 'the regression guard must reject an additional Kyverno-generated network policy'
fi

additional_world_policy=$'apiVersion: cilium.io/v2\nkind: CiliumNetworkPolicy\nmetadata:\n  name: additional-world-egress\n  namespace: crossplane-system\nspec:\n  endpointSelector: {}\n  egress:\n  - toEntities: [world]'
rendered_with_additional_world_policy="${rendered}"$'\n---\n'"${additional_world_policy}"
if [ "$(select_crossplane_namespace_policies "${rendered_with_additional_world_policy}")" = "${policy}" ]; then
  fail 'the regression guard must reject an additional Crossplane CiliumNetworkPolicy'
fi

standard_world_policy=$'apiVersion: networking.k8s.io/v1\nkind: NetworkPolicy\nmetadata:\n  name: additional-standard-world-egress\n  namespace: crossplane-system\nspec:\n  podSelector: {}\n  policyTypes: [Egress]\n  egress:\n  - {}'
rendered_with_standard_world_policy="${rendered}"$'\n---\n'"${standard_world_policy}"
if [ "$(select_crossplane_namespace_policies "${rendered_with_standard_world_policy}")" = "${policy}" ]; then
  fail 'the regression guard must reject an additional standard Crossplane NetworkPolicy'
fi

clusterwide_world_policy=$'apiVersion: cilium.io/v2\nkind: CiliumClusterwideNetworkPolicy\nmetadata:\n  name: additional-clusterwide-world-egress\nspec:\n  endpointSelector:\n    matchLabels:\n      k8s:io.kubernetes.pod.namespace: crossplane-system\n  egress:\n  - toEntities: [world]'
rendered_with_clusterwide_world_policy="${rendered}"$'\n---\n'"${clusterwide_world_policy}"
if [ "$(select_crossplane_namespace_policies "${rendered_with_clusterwide_world_policy}")" = "${policy}" ]; then
  fail 'the regression guard must reject a Cilium cluster-wide policy that can select Crossplane pods'
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

expected_https_egress=$'  - toFQDNs:\n    - matchName: ghcr.io\n    - matchName: pkg-containers.githubusercontent.com\n    - matchName: api.github.com\n    - matchName: xpkg.upbound.io\n    - matchName: d3qrbvrml4iuq4.cloudfront.net\n    - matchName: sts.eu-central-1.amazonaws.com\n    - matchName: iam.amazonaws.com\n    toPorts:\n    - ports:\n      - port: "443"\n        protocol: TCP'
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
  expected_contract=$'  - toEndpoints:\n    - matchLabels:\n        k8s-app: kube-dns\n        k8s:io.kubernetes.pod.namespace: kube-system\n    toPorts:\n    - ports:\n      - port: "53"\n        protocol: UDP\n      - port: "53"\n        protocol: TCP\n      rules:\n        dns:\n        - matchName: ghcr.io\n        - matchName: pkg-containers.githubusercontent.com\n        - matchName: api.github.com\n        - matchName: xpkg.upbound.io\n        - matchName: d3qrbvrml4iuq4.cloudfront.net\n        - matchName: sts.eu-central-1.amazonaws.com\n        - matchName: iam.amazonaws.com\n        - matchPattern: \x27*.cluster.local\x27'
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
