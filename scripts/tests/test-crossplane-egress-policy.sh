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

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

rendered="$(kubectl kustomize "${controllers_dir}")" ||
  fail 'the deployed Hetzner controller overlay must render'

select_crossplane_policy() {
  awk '
    function reset_document() {
      document = ""
      is_cilium_policy = 0
      in_metadata = 0
      is_allow_crossplane = 0
    }

    function emit_if_selected() {
      if (is_cilium_policy && is_allow_crossplane) {
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
      if (in_metadata && $0 == "  name: allow-crossplane") {
        is_allow_crossplane = 1
      }
    }

    END { emit_if_selected() }
  ' <<<"$1"
}

policy="$(select_crossplane_policy "${rendered}")"
readonly policy

selected_policy_count="$(awk '/^kind: CiliumNetworkPolicy$/ { count++ } END { print count + 0 }' <<<"${policy}")"
[ "${selected_policy_count}" -eq 1 ] ||
  fail 'the deployed overlay must contain exactly one allow-crossplane CiliumNetworkPolicy'

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
