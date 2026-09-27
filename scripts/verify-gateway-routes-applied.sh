#!/usr/bin/env bash
set -euo pipefail

# Cilium's operator turns every HTTPRoute into the gateway's Envoy config and
# records the spec it applied as `observedGeneration` on the route's status
# parents. The operator can keep running while its Gateway API controller never
# started (#4198): Flux, the HelmReleases and the pods all read healthy, yet no
# route change reaches the gateway, and a Flagger canary gets no traffic. So the
# deploy ends by checking that the gateway serves what it was sent:
#
# - every route attached to a Cilium gateway is applied at its current
#   generation, and was not rejected (Accepted or ResolvedRefs False) at it;
# - every Gateway a route names exists, so a mistyped parent cannot drop a
#   route unnoticed;
# - no running operator logged the failed CRD check, because a controller that
#   never started leaves already-applied routes looking current.
#
# Route problems can be transient right after a deploy, so the check polls until
# they clear or the timeout passes. A failed operator start never recovers on its
# own, so that fails at once. Every API read is bounded, so a hung API server
# cannot hold the production lane much past the timeout.

kubectl_bin="${GATEWAY_ROUTES_KUBECTL_BIN:-kubectl}"
timeout_seconds="${GATEWAY_ROUTES_TIMEOUT_SECONDS:-300}"
interval="${GATEWAY_ROUTES_INTERVAL_SECONDS:-10}"
readonly controller='io.cilium/gateway-controller'
readonly request_timeout='30s'
# Cilium 1.20's operator logs this when the Gateway API cell's CRD check fails,
# after which the controller does not start and is never retried.
readonly failed_start='Required GatewayAPI resources are not found'

if ! [[ "${timeout_seconds}" =~ ^[0-9]+$ ]] ||
  ! [[ "${interval}" =~ ^[0-9]+([.][0-9]+)?$ ]]; then
  echo "::error::Gateway route verification settings must be a whole-number timeout and a non-negative interval."
  exit 1
fi

tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

kubectl_prod() {
  "${kubectl_bin}" --context admin@prod --request-timeout="${request_timeout}" "$@"
}

# Writes one line per problem, then a final `checked=<n>` line counting the
# Cilium parent attachments it evaluated:
#   not-applied <route> parent=<p> generation=<n> applied=<m|none>
#   rejected <route> parent=<p> conditions=<Type/Reason,...>
#   missing-gateway <route> parent=<p>
# A parent names a Gateway by group, kind, namespace, name, section and port,
# with the Gateway API defaults. A parent on another controller's Gateway is
# skipped. The status entry must come from Cilium; the lowest observedGeneration
# across its conditions is what it has applied, and a False Accepted or
# ResolvedRefs condition counts as a rejection only when that condition itself
# observed the current generation.
# shellcheck disable=SC2016 # jq program, not shell expansion
readonly route_program='
  def parent($ns): {
    group: (.group // "gateway.networking.k8s.io"),
    kind: (.kind // "Gateway"),
    namespace: (.namespace // $ns),
    name: .name,
    sectionName: (.sectionName // null),
    port: (.port // null)
  };
  def ref_label: "\(.namespace)/\(.name)\(if .sectionName then "/\(.sectionName)" else "" end)";
  ([$classes[0].items[] | select(.spec.controllerName == $controller) | .metadata.name]) as $cilium_classes
  | ([$gateways[0].items[] | "\(.metadata.namespace)/\(.metadata.name)"]) as $all_gateways
  | ([$gateways[0].items[]
      | select(.spec.gatewayClassName as $c | $cilium_classes | any(. == $c))
      | "\(.metadata.namespace)/\(.metadata.name)"]) as $cilium_gateways
  | if ($cilium_gateways | length) == 0 then
      error("no Gateway uses a GatewayClass controlled by \($controller)")
    else . end
  | [$routes[0].items[] as $route
      | $route.metadata.namespace as $ns
      | $route.metadata.generation as $gen
      | ($route.spec.parentRefs // [])[]
      | parent($ns) as $p
      | select($p.group == "gateway.networking.k8s.io" and $p.kind == "Gateway")
      | "\($p.namespace)/\($p.name)" as $g
      | {route: "\($ns)/\($route.metadata.name)", parent: ($p | ref_label), generation: $gen}
      | if ($all_gateways | any(. == $g) | not) then . + {missing: true}
        elif ($cilium_gateways | any(. == $g) | not) then empty
        else
          ([$route.status.parents[]?
            | select(.controllerName == $controller)
            | select((.parentRef | parent($ns)) == $p)
            | (.conditions // [])[]] ) as $conditions
          | . + {
              applied: ([$conditions[] | .observedGeneration | numbers] | min),
              rejected: [$conditions[]
                         | select((.type == "Accepted" or .type == "ResolvedRefs")
                                  and .status == "False"
                                  and (.observedGeneration // -1) >= $gen)
                         | "\(.type)/\(.reason // "none")"]
            }
        end] as $parents
  | ($parents[]
      | if .missing then "missing-gateway \(.route) parent=\(.parent)"
        elif (.rejected | length) > 0 then "rejected \(.route) parent=\(.parent) conditions=\(.rejected | join(","))"
        elif .applied == null or .applied < .generation then
          "not-applied \(.route) parent=\(.parent) generation=\(.generation) applied=\(.applied // "none")"
        else empty end),
    "checked=\([$parents[] | select(.missing | not)] | length)"
'

read_routes() {
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
    "${route_program}" >"${tmp_dir}/routes.txt" 2>"${tmp_dir}/error.log"
}

# Exit 0 when no running operator logged the failed start, 1 when one did (its
# pods are written to broken.txt), and 2 when the pods or their logs cannot be
# read or no operator is running.
probe_operators() {
  local pod
  : >"${tmp_dir}/broken.txt"
  if ! kubectl_prod -n kube-system get pods -l io.cilium/app=operator \
    --field-selector=status.phase=Running -o name >"${tmp_dir}/pods.txt" 2>"${tmp_dir}/error.log"; then
    return 2
  fi
  if [[ ! -s "${tmp_dir}/pods.txt" ]]; then
    echo "no running Cilium operator pod" >"${tmp_dir}/error.log"
    return 2
  fi
  while IFS= read -r pod; do
    if ! kubectl_prod -n kube-system logs "${pod}" -c cilium-operator \
      >"${tmp_dir}/operator.log" 2>"${tmp_dir}/error.log"; then
      return 2
    fi
    if grep -Fq -- "${failed_start}" "${tmp_dir}/operator.log"; then
      printf '%s\n' "${pod}" >>"${tmp_dir}/broken.txt"
    fi
  done <"${tmp_dir}/pods.txt"
  [[ ! -s "${tmp_dir}/broken.txt" ]] || return 1
}

operator_hint() {
  cat <<'EOF'
The Cilium operator runs its Gateway API controller only if the CRD check
succeeds when a leader starts, and never retries it (#4198). Roll the operator
through GitOps by changing the restart marker in
k8s/bases/infrastructure/controllers/cilium/helm-release.yaml.
EOF
}

deadline=$((SECONDS + timeout_seconds))
problems=''
read_error=''
while :; do
  checked=''
  if read_routes; then
    checked="$(sed -n 's/^checked=//p' "${tmp_dir}/routes.txt")"
  fi
  # A read that produced no count evaluated nothing, so it can never pass.
  if [[ "${checked}" =~ ^[0-9]+$ ]]; then
    read_error=''
    problems="$(grep -v '^checked=' "${tmp_dir}/routes.txt" || true)"
    if [[ -z "${problems}" ]]; then
      probe=0
      probe_operators || probe=$?
      if ((probe == 0)); then
        echo "✅ The gateway applied all ${checked} HTTPRoute parent attachments at their current generation, and no Cilium operator failed to start its Gateway API controller."
        exit 0
      fi
      if ((probe == 1)); then
        echo "::error::A Cilium operator logged \"${failed_start}\", so its Gateway API controller is not running and later route changes will not reach the gateway:"
        sed 's/^/  /' "${tmp_dir}/broken.txt"
        operator_hint
        exit 1
      fi
      read_error="$(head -c 2000 "${tmp_dir}/error.log")"
      read_error="${read_error:-the Cilium operator logs could not be read}"
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
  echo "::error::Could not evaluate the gateway within ${timeout_seconds}s, so it is unknown whether route changes reach it:"
  printf '%s\n' "${read_error}"
  exit 1
fi

echo "::error::The gateway is not serving these HTTPRoutes as declared after ${timeout_seconds}s:"
while IFS= read -r line; do
  printf '  %s\n' "${line}"
done <<<"${problems}"
if grep -q '^missing-gateway ' <<<"${problems}"; then
  echo "A missing-gateway route names a Gateway that does not exist; fix its parentRefs."
fi
if grep -q '^rejected ' <<<"${problems}"; then
  echo "A rejected route was refused by the gateway; its conditions name the reason, such as a listener it may not attach to or a backend it cannot resolve."
fi
if grep -q '^not-applied ' <<<"${problems}"; then
  operator_hint
fi
exit 1
