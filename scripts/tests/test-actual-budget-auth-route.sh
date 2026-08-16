#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly actual_budget_dir="${root_dir}/k8s/bases/apps/actual-budget"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

rendered="$(kubectl kustomize "${actual_budget_dir}")" ||
  fail 'the Actual Budget base must render'

route="$(
  yq eval '
    select(.kind == "HTTPRoute" and .metadata.name == "actual-budget")
  ' - <<<"${rendered}"
)" || fail 'the rendered Actual Budget HTTPRoute must be extractable'

[[ -n "${route}" ]] ||
  fail 'the Actual Budget HTTPRoute must be present in the rendered base'

backend="$(
  yq eval -r '
    .spec.rules[0].backendRefs[0] |
    [.namespace, .name, (.port | tostring)] | join(":")
  ' - <<<"${route}"
)" || fail 'the rendered Actual Budget auth backend must be extractable'

[[ "${backend}" == 'oauth2-proxy:oauth2-proxy:80' ]] ||
  fail 'public Actual Budget traffic must pass through oauth2-proxy'

if yq eval -e '
  .spec.rules[].filters[]? |
  select(.type == "RequestHeaderModifier") |
  .requestHeaderModifier.set[]? |
  select(.name == "X-Auth-Request-Redirect")
' - <<<"${route}" >/dev/null 2>&1; then
  fail 'the route must not replace each original request URI with a static post-login redirect'
fi

printf 'PASS: Actual Budget routes through oauth2-proxy without replacing deep-link redirects\n'
