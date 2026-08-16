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
readonly crossplane_dir="${root_dir}/k8s/providers/hetzner/infrastructure/controllers/crossplane"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

rendered="$(kubectl kustomize "${crossplane_dir}")" ||
  fail 'the Crossplane controller component must render'

policy="$(
  awk '
    /^kind: CiliumNetworkPolicy$/ { in_policy = 1 }
    in_policy { print }
    in_policy && /^---$/ { exit }
  ' <<<"${rendered}"
)"
readonly policy

grep -Fq -- 'name: allow-crossplane' <<<"${policy}" ||
  fail 'the rendered component must contain the allow-crossplane policy'

if grep -Eq '^[[:space:]]*serverNames:' <<<"${policy}"; then
  fail 'Crossplane HTTPS egress must stay on the direct L3/L4 path, not the broken SNI proxy path'
fi

assert_no_unrestricted_egress() {
  local candidate_policy="$1"

  if grep -Eq '^[[:space:]]*-[[:space:]]+world$' <<<"${candidate_policy}"; then
    fail 'Crossplane must not receive unrestricted world egress'
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

assert_no_unrestricted_egress "${policy}"
assert_direct_https_contract "${policy}"

# Prove the regression guard rejects selector, transport, proxy, and broad
# egress drift instead of merely checking that reviewed entries are present.
wildcard_selector=$'- toFQDNs:\n    - matchPattern: "*.github.com"'
wildcard_policy="${policy/- toFQDNs:/${wildcard_selector}}"
if (assert_direct_https_contract "${wildcard_policy}") >/dev/null 2>&1; then
  fail 'the regression guard must reject wildcard Crossplane FQDN selectors'
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

printf 'PASS: Crossplane keeps direct HTTPS egress closed to the reviewed FQDN set\n'

exit 0
