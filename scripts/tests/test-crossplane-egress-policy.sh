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

if grep -Eq '^[[:space:]]*-[[:space:]]+world$' <<<"${policy}"; then
  fail 'Crossplane must not receive unrestricted world egress'
fi

actual_fqdns="$(
  awk '
    /^[[:space:]]*-[[:space:]]+toFQDNs:$/ { in_fqdns = 1; next }
    in_fqdns && /^[[:space:]]+toPorts:$/ { exit }
    in_fqdns && /^[[:space:]]*-[[:space:]]+matchName:/ { print $3 }
  ' <<<"${policy}" | sort
)"
readonly actual_fqdns

expected_fqdns="$(
  printf '%s\n' \
    api.github.com \
    d3qrbvrml4iuq4.cloudfront.net \
    ghcr.io \
    iam.amazonaws.com \
    pkg-containers.githubusercontent.com \
    sts.eu-central-1.amazonaws.com \
    xpkg.upbound.io | sort
)"
readonly expected_fqdns

[ "${actual_fqdns}" = "${expected_fqdns}" ] ||
  fail "Crossplane must retain exactly the seven reviewed FQDN destinations (got: ${actual_fqdns//$'\n'/, })"

https_port_contract() {
  awk '
    /^[[:space:]]*-[[:space:]]+toFQDNs:$/ { in_fqdns = 1; next }
    in_fqdns && /^[[:space:]]+toPorts:$/ { in_ports = 1; next }
    in_ports && /^[[:space:]]*-[[:space:]]+toEndpoints:$/ { exit }
    in_ports && /^[[:space:]]*-[[:space:]]+port:/ {
      port = $3
      gsub(/"/, "", port)
      print "port=" port
    }
    in_ports && /^[[:space:]]+protocol:/ { print "protocol=" $2 }
  ' <<<"$1"
}

assert_https_only() {
  local candidate_policy="$1"
  local actual_contract

  actual_contract="$(https_port_contract "${candidate_policy}")"
  [ "${actual_contract}" = $'port=443\nprotocol=TCP' ] ||
    fail "the reviewed FQDN destinations must remain exactly TCP/443 (got: ${actual_contract//$'\n'/, })"
}

assert_https_only "${policy}"

# Prove the regression guard rejects both dimensions of a widened transport
# contract instead of merely checking that an HTTPS entry is present.
widened_port_policy="${policy/port: \"443\"/port: \"8443\"}"
if (assert_https_only "${widened_port_policy}") >/dev/null 2>&1; then
  fail 'the regression guard must reject a changed Crossplane egress port'
fi

widened_protocol_policy="${policy/protocol: TCP/protocol: UDP}"
if (assert_https_only "${widened_protocol_policy}") >/dev/null 2>&1; then
  fail 'the regression guard must reject a changed Crossplane egress protocol'
fi

printf 'PASS: Crossplane keeps direct HTTPS egress closed to the reviewed FQDN set\n'

exit 0
