#!/usr/bin/env bash
set -euo pipefail

# Cilium's operator turns every HTTPRoute into the gateway's Envoy config and
# records the spec it applied as `observedGeneration` on the route's status
# parents. The operator can keep running while its Gateway API controller never
# started (#4198): Flux, the HelmReleases and the pods all read healthy, yet no
# route change reaches the gateway, and a Flagger canary gets no traffic. A
# route whose status lags its `metadata.generation` is the one signal that
# shows it, so the deploy waits until every route attached to a Cilium gateway
# is applied at its current generation, and fails when that does not happen.

kubectl_bin="${GATEWAY_ROUTES_KUBECTL_BIN:-kubectl}"
timeout_seconds="${GATEWAY_ROUTES_TIMEOUT_SECONDS:-300}"
interval="${GATEWAY_ROUTES_INTERVAL_SECONDS:-10}"
readonly controller='io.cilium/gateway-controller'

if ! [[ "${timeout_seconds}" =~ ^[0-9]+$ ]] ||
  ! [[ "${interval}" =~ ^[0-9]+([.][0-9]+)?$ ]]; then
  echo "::error::Gateway route verification settings must be a whole-number timeout and a non-negative interval."
  exit 1
fi

tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

kubectl_prod() {
  "${kubectl_bin}" --context admin@prod "$@"
}

# Writes one line per declared Gateway parent that the controller has not
# applied at the route's current generation, then a final `checked=<n>` line.
# A parent counts only when the Gateway exists and its class belongs to Cilium,
# so a route aimed at another controller never blocks the deploy. The matching
# status parent must come from Cilium and name the same group, kind,
# namespace, name, section and port; the lowest observedGeneration across its
# conditions is what the controller has applied.
# shellcheck disable=SC2016 # jq program, not shell expansion
readonly lag_program='
  def parent($ns): {
    group: (.group // "gateway.networking.k8s.io"),
    kind: (.kind // "Gateway"),
    namespace: (.namespace // $ns),
    name: .name,
    sectionName: (.sectionName // null),
    port: (.port // null)
  };
  ([$classes[0].items[] | select(.spec.controllerName == $controller) | .metadata.name]) as $cilium_classes
  | ([$gateways[0].items[]
      | select(.spec.gatewayClassName as $c | $cilium_classes | any(. == $c))
      | "\(.metadata.namespace)/\(.metadata.name)"]) as $cilium_gateways
  | if ($cilium_gateways | length) == 0 then
      error("no Gateway uses a GatewayClass controlled by \($controller)")
    else . end
  | [$routes[0].items[] as $route
      | $route.metadata.namespace as $ns
      | ($route.spec.parentRefs // [])[]
      | parent($ns) as $p
      | select($p.group == "gateway.networking.k8s.io" and $p.kind == "Gateway")
      | select("\($p.namespace)/\($p.name)" as $g | $cilium_gateways | any(. == $g))
      | ([$route.status.parents[]?
          | select(.controllerName == $controller)
          | select((.parentRef | parent($ns)) == $p)
          | (.conditions // [])[]
          | .observedGeneration
          | numbers] | min) as $applied
      | {
          route: "\($ns)/\($route.metadata.name)",
          parent: "\($p.namespace)/\($p.name)\(if $p.sectionName then "/\($p.sectionName)" else "" end)",
          generation: $route.metadata.generation,
          applied: $applied
        }] as $parents
  | ($parents[]
      | select(.applied == null or .applied < .generation)
      | "\(.route) parent=\(.parent) generation=\(.generation) applied=\(.applied // "none")"),
    "checked=\($parents | length)"
'

read_lag() {
  local kind
  for kind in gatewayclasses gateways httproutes; do
    if ! kubectl_prod get "${kind}.gateway.networking.k8s.io" -A -o json \
      >"${tmp_dir}/${kind}.json" 2>"${tmp_dir}/error.log"; then
      return 1
    fi
  done
  jq -r -n \
    --arg controller "${controller}" \
    --slurpfile classes "${tmp_dir}/gatewayclasses.json" \
    --slurpfile gateways "${tmp_dir}/gateways.json" \
    --slurpfile routes "${tmp_dir}/httproutes.json" \
    "${lag_program}" >"${tmp_dir}/lag.txt" 2>"${tmp_dir}/error.log"
}

deadline=$((SECONDS + timeout_seconds))
lagging=''
read_error=''
while :; do
  checked=''
  if read_lag; then
    checked="$(sed -n 's/^checked=//p' "${tmp_dir}/lag.txt")"
  fi
  # A read that produced no count evaluated nothing, so it can never pass.
  if [[ "${checked}" =~ ^[0-9]+$ ]]; then
    read_error=''
    lagging="$(grep -v '^checked=' "${tmp_dir}/lag.txt" || true)"
    if [[ -z "${lagging}" ]]; then
      echo "✅ The gateway applied all ${checked} HTTPRoute parent attachments at their current generation."
      exit 0
    fi
  else
    read_error="$(head -c 2000 "${tmp_dir}/error.log")"
    read_error="${read_error:-the route evaluation produced no result}"
  fi

  if ((SECONDS >= deadline)); then
    break
  fi
  sleep "${interval}"
done

if [[ -n "${read_error}" ]]; then
  echo "::error::Could not evaluate the gateway's HTTPRoutes within ${timeout_seconds}s, so it is unknown whether route changes reach the gateway:"
  printf '%s\n' "${read_error}"
  exit 1
fi

echo "::error::The gateway has not applied these HTTPRoutes at their current generation after ${timeout_seconds}s, so their changes are not being served:"
while IFS= read -r line; do
  printf '  %s\n' "${line}"
done <<<"${lagging}"
cat <<'EOF'
The Cilium operator is most likely running without its Gateway API controller,
which starts only if its CRD check succeeds when a leader starts (#4198). Look
for "Required GatewayAPI resources are not found" in the operator leader's log,
then roll the operator through GitOps by changing the restart marker in
k8s/bases/infrastructure/controllers/cilium/helm-release.yaml.
EOF
exit 1
