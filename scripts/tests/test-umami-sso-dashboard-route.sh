#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly apps_dir="${root_dir}/k8s/providers/hetzner/apps"
readonly controllers_dir="${root_dir}/k8s/providers/hetzner/infrastructure/controllers"
readonly expected_host="analytics.\${domain}"
readonly expected_router="Host(\`analytics.\${domain}\`)|umami"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

apps_rendered="$(kubectl kustomize "${apps_dir}")" ||
  fail 'the Hetzner apps layer must render'
controllers_rendered="$(kubectl kustomize "${controllers_dir}")" ||
  fail 'the Hetzner infrastructure-controllers layer must render'

collector="$(
  yq eval '
    select(.kind == "Canary" and .metadata.namespace == "umami" and .metadata.name == "umami-umami")
  ' - <<<"${apps_rendered}"
)" || fail 'the rendered Umami collector Canary must be extractable'

[[ -n "${collector}" ]] || fail 'the Umami collector Canary must remain rendered'

collector_prefixes="$(
  yq eval -r '.spec.service.match[].uri.prefix' - <<<"${collector}" | LC_ALL=C sort
)" || fail 'the Umami public collector prefixes must be extractable'

[[ "${collector_prefixes}" == $'/api/send\n/script.js' ]] ||
  fail 'the public Umami route must remain limited to /api/send and /script.js'
[[ "$(yq eval -r '.spec.service.apex.annotations."gethomepage.dev/enabled"' - <<<"${collector}")" == 'false' ]] ||
  fail 'the collector-only route must stay hidden from Homepage to avoid a duplicate tile'

dashboard_route="$(
  yq eval '
    select(.kind == "HTTPRoute" and .metadata.namespace == "umami" and .metadata.name == "umami-dashboard")
  ' - <<<"${apps_rendered}"
)" || fail 'the rendered Umami dashboard HTTPRoute must be extractable'

[[ -n "${dashboard_route}" ]] ||
  fail 'the SSO-protected Umami dashboard HTTPRoute must be present'

[[ "$(yq eval -r '.spec.hostnames | join(",")' - <<<"${dashboard_route}")" == "${expected_host}" ]] ||
  fail 'the Umami dashboard route must claim only the analytics host'
[[ "$(yq eval -r '.spec.rules[0].matches[0].path | [.type, .value] | join(":")' - <<<"${dashboard_route}")" == 'PathPrefix:/' ]] ||
  fail 'the SSO dashboard route must cover the browser UI below /'

dashboard_backend="$(
  yq eval -r '
    .spec.rules[0].backendRefs[0] |
    [.namespace, .name, (.port | tostring)] | join(":")
  ' - <<<"${dashboard_route}"
)" || fail 'the rendered Umami dashboard auth backend must be extractable'

[[ "${dashboard_backend}" == 'oauth2-proxy:oauth2-proxy:80' ]] ||
  fail 'Umami dashboard traffic must pass through oauth2-proxy'
[[ "$(yq eval -r '.metadata.annotations."gethomepage.dev/enabled"' - <<<"${dashboard_route}")" == 'true' ]] ||
  fail 'the authenticated Umami dashboard route must restore the Homepage tile'

if yq eval -e '
  .spec.rules[].filters[]? |
  select(.type == "RequestHeaderModifier") |
  .requestHeaderModifier.set[]? |
  select(.name == "X-Auth-Request-Redirect")
' - <<<"${dashboard_route}" >/dev/null 2>&1; then
  fail 'the dashboard route must preserve the requested deep link through SSO'
fi

reference_grant="$(
  yq eval '
    select(.kind == "ReferenceGrant" and .metadata.namespace == "oauth2-proxy" and .metadata.name == "allow-oauth2-proxy-backends")
  ' - <<<"${controllers_rendered}"
)" || fail 'the oauth2-proxy ReferenceGrant must be extractable'

yq eval -e '
  .spec.from[] |
  select(.group == "gateway.networking.k8s.io" and .kind == "HTTPRoute" and .namespace == "umami")
' - <<<"${reference_grant}" >/dev/null ||
  fail 'oauth2-proxy must permit the cross-namespace Umami dashboard route'

auth_config="$(
  yq eval -r '
    select(.kind == "ConfigMap" and .metadata.namespace == "oauth2-proxy" and .metadata.name == "auth-proxy-config") |
    .data."dynamic.yaml"
  ' - <<<"${controllers_rendered}"
)" || fail 'the auth-proxy routing config must be extractable'

[[ "$(yq eval -r '.http.routers.umami | [.rule, .service] | join("|")' - <<<"${auth_config}")" == "${expected_router}" ]] ||
  fail 'auth-proxy must route the analytics host to the Umami service'
[[ "$(yq eval -r '.http.services.umami.loadBalancer.servers[0].url' - <<<"${auth_config}")" == 'http://umami-umami-primary.umami.svc.cluster.local:80' ]] ||
  fail 'auth-proxy must use the Flagger primary Service for the Umami dashboard'

auth_policy="$(
  yq eval '
    select(.kind == "CiliumNetworkPolicy" and .metadata.namespace == "oauth2-proxy" and .metadata.name == "allow-auth-proxy")
  ' - <<<"${controllers_rendered}"
)" || fail 'the auth-proxy network policy must be extractable'

yq eval -e '
  .spec.egress[] |
  select(.toEndpoints[]?.matchLabels."k8s:io.kubernetes.pod.namespace" == "umami") |
  .toPorts[].ports[] |
  select(.port == "3000" and .protocol == "TCP")
' - <<<"${auth_policy}" >/dev/null ||
  fail 'auth-proxy must be allowed to reach Umami pods on TCP 3000'

umami_policy="$(
  yq eval '
    select(.kind == "CiliumNetworkPolicy" and .metadata.namespace == "umami" and .metadata.name == "allow-umami")
  ' - <<<"${apps_rendered}"
)" || fail 'the Umami network policy must be extractable'

yq eval -e '
  .spec.ingress[] |
  select([
    .fromEndpoints[]? |
    select(
      .matchLabels.app == "auth-proxy" and
      .matchLabels."k8s:io.kubernetes.pod.namespace" == "oauth2-proxy"
    )
  ] | length > 0) |
  .toPorts[].ports[] |
  select(.port == "3000" and .protocol == "TCP")
' - <<<"${umami_policy}" >/dev/null ||
  fail 'Umami must admit dashboard traffic only from auth-proxy on TCP 3000'

printf 'PASS: Umami collector stays public-only while its dashboard routes through SSO\n'
