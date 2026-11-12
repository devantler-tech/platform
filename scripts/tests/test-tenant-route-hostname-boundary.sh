#!/usr/bin/env bash
# Negative coverage for the tenant hostname boundary on the shared Gateway.
#
# The boundary is enforced by `restrict-tenant-route-hostnames`, which
# matches HTTPRoute. That leaves a gap unless the other route kinds cannot reach
# the shared listener at all: an HTTPS listener accepts GRPCRoute as well as
# HTTPRoute by default, so a tenant able to create a GRPCRoute could claim
# another service's hostname under the wildcard certificate without the policy
# ever running.
#
# Two layers close it, and this test pins BOTH — either alone is bypassable:
#   1. tenant RBAC grants only `httproutes` (+ `referencegrants`);
#   2. every Gateway listener pins `allowedRoutes.kinds` to HTTPRoute, covering
#      routes created by any other principal.
# The policy's allow-list and default-deny behavior have their own real Kyverno
# fixtures under tests/restrict-tenant-route-hostnames; this test owns the
# separate route-kind boundary those fixtures cannot exercise.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"

role_file="${repo_root}/k8s/bases/infrastructure/cluster-roles/gateway-tenant-edit.yaml"
# The prod overlay is what carries every listener: two come from the base
# Gateway and more are appended by the hetzner JSON6902 patches, which a
# per-file check would never see.
overlay="${repo_root}/k8s/providers/hetzner/infrastructure"

workdir="$(mktemp -d)"
trap 'rm -rf "${workdir}"' EXIT
render="${workdir}/render.yaml"
gateway="${workdir}/gateway.yaml"

fail() {
  echo "::error::$1"
  exit 1
}

# --- Layer 1: tenant RBAC grants only HTTPRoute -----------------------------
# Positive control first: a typo'd path or a renamed role would otherwise make
# every assertion below pass over an empty document set.
role_name="$(yq eval 'select(.kind=="ClusterRole") | .metadata.name' "${role_file}")"
[ "${role_name}" = "gateway-tenant-edit" ] ||
  fail "expected ClusterRole gateway-tenant-edit in ${role_file}, found '${role_name}'"

granted="$(yq eval \
  'select(.kind=="ClusterRole") | .rules[] | select(.apiGroups[] == "gateway.networking.k8s.io") | .resources[]' \
  "${role_file}" | sort | tr '\n' ' ')"
[ -n "${granted// /}" ] ||
  fail "gateway-tenant-edit grants no gateway.networking.k8s.io resources at all — the rule shape changed, so this test is no longer checking anything"

expected_grant="httproutes referencegrants "
[ "${granted}" = "${expected_grant}" ] ||
  fail "gateway-tenant-edit must grant exactly 'httproutes referencegrants'; found '${granted}'. Any other route kind bypasses restrict-tenant-route-hostnames — extend that policy and the listener kinds before widening this."

# --- Layer 2: every listener pins allowedRoutes.kinds to HTTPRoute ----------
kubectl kustomize "${overlay}" >"${render}" 2>"${workdir}/render.err" ||
  fail "prod infrastructure overlay failed to build: $(tail -5 "${workdir}/render.err")"

yq eval 'select(.kind=="Gateway" and .metadata.name=="platform")' "${render}" >"${gateway}"
listener_count="$(yq eval '.spec.listeners | length' "${gateway}")"
# The live Gateway had 4 listeners when this boundary was written. Requiring at
# least that many keeps an empty or mis-selected render from passing vacuously —
# adding a listener is fine, silently losing them all is not.
case "${listener_count}" in
  '' | null | 0) fail "no Gateway/platform listeners found in the rendered overlay — the render or the selector changed, so this test is checking nothing" ;;
esac
[ "${listener_count}" -ge 4 ] ||
  fail "expected at least 4 Gateway listeners in the prod overlay, found ${listener_count} — did the ascoachingogvaner listener patch stop applying?"

http_namespaces="$(yq eval -r '.spec.listeners[] | select(.protocol == "HTTP") | .allowedRoutes.namespaces.from' "${gateway}")"
[ "${http_namespaces}" = 'Same' ] ||
  fail 'plain HTTP must accept only the platform-owned redirect namespace; tenant backends must use HTTPS'

# Compare the kinds as a JOINED STRING, never as an array. yq's `!=` does not
# do deep array equality — `(… | sort) != ["HTTPRoute"]` evaluates to true even
# for a correctly pinned listener, which reported all four as unpinned when this
# test was written. It failed closed there, but the same shape fails OPEN when
# the sense is reversed, so keep the comparison on strings.
unpinned="$(yq eval \
  '[.spec.listeners[] | select(((.allowedRoutes.kinds // []) | map(.kind) | sort | join(",")) != "HTTPRoute")] | .[].name' \
  "${gateway}" | tr '\n' ' ')"
[ -z "${unpinned// /}" ] ||
  fail "these Gateway listeners do not pin allowedRoutes.kinds to exactly [HTTPRoute]: ${unpinned}— an unpinned HTTPS listener also accepts GRPCRoute, which restrict-tenant-route-hostnames does not match."

# Introduce the apex listener before narrowing the original listener. Homepage
# already uses a port-only parent, so the more-specific listener can take over
# its hostname without changing the app layer in the same deployment.
apex_listeners="$(yq -o=json -I=0 '[.spec.listeners[] | select(.name == "https-apex")]' "${gateway}")"
jq -e 'length == 1 and .[0].hostname == "${domain}" and .[0].port == 443 and .[0].protocol == "HTTPS" and .[0].allowedRoutes.namespaces.from == "All"' <<<"${apex_listeners}" >/dev/null ||
  fail 'expected exactly one production apex HTTPS listener on port 443 with the existing route namespace scope'
original_listeners="$(yq -o=json -I=0 '[.spec.listeners[] | select(.name == "https")]' "${gateway}")"
jq -e 'length == 1 and (.[0] | has("hostname") | not) and .[0].port == 443 and .[0].protocol == "HTTPS"' <<<"${original_listeners}" >/dev/null ||
  fail 'keep the original hostname-less HTTPS listener until the separately observed narrowing stage'
jq -e --argjson original "${original_listeners}" '.[0].tls == $original[0].tls and .[0].tls.mode == "Terminate" and (.[0].tls.certificateRefs | length) == 1' <<<"${apex_listeners}" >/dev/null ||
  fail 'the apex listener must terminate TLS with the same existing certificate reference as the original HTTPS listener'

# --- Layer 3: what the policy's CLI fixtures cannot show ---------------------
# A ValidatingPolicy's operations only decide which requests the webhook
# receives, so the CLI evaluates a DELETE fixture the same way as a create.
policy="${repo_root}/k8s/bases/infrastructure/cluster-policies/best-practices/restrict-tenant-route-hostnames.yaml"
action="$(yq -r '.spec.validationActions | join(",")' "${policy}")"
[ "${action}" = "Deny" ] ||
  fail "restrict-tenant-route-hostnames must set validationActions: [Deny] (found: '${action}')"

# Retired tenant resources must stay removable after their allow-list entry is gone.
rules="$(yq -o=json -I=0 '[.spec.matchConstraints.resourceRules[] | {"group": .apiGroups[0], "resource": .resources[0], "operations": (.operations | sort)}]' "${policy}")"
expected_rules='[{"group":"gateway.networking.k8s.io","resource":"httproutes","operations":["CREATE","UPDATE"]}]'
[ "${rules}" = "${expected_rules}" ] ||
  fail "restrict-tenant-route-hostnames must match HTTPRoute on CREATE and UPDATE only (found: ${rules})"

# The namespace selector is the policy's only scope, so an allow-listed tenant
# whose namespace lost the label would fall outside the policy altogether.
selector_values="$(yq -r '.spec.matchConstraints.namespaceSelector.matchExpressions[] | select(.key == "app.kubernetes.io/managed-by") | .values[]' "${policy}" | sort | tr '\n' ' ')"
[ "${selector_values}" = "kro ksail " ] ||
  fail "restrict-tenant-route-hostnames must select namespaces managed by ksail or kro (found: '${selector_values}')"
for tenant in wedding-app ascoachingogvaner; do
  namespace_file="${repo_root}/k8s/bases/apps/${tenant}/namespace.yaml"
  owner="$(yq -r '.metadata.labels."app.kubernetes.io/managed-by" // ""' "${namespace_file}")"
  [ "${owner}" = "ksail" ] ||
    fail "${namespace_file} must carry app.kubernetes.io/managed-by: ksail or its routes escape restrict-tenant-route-hostnames (found: '${owner}')"
done

# Homepage must remain attached only to the shared Gateway's HTTPS port while
# its listener name changes. A name-only parent would lose the apex route when
# Cilium isolates a new, more-specific listener; an unconstrained parent could
# attach to a future plaintext listener. Check the shared input AND its actual
# production apps render, rather than assuming an overlay preserves the base.
apps_render="${workdir}/apps.yaml"
kubectl kustomize "${repo_root}/k8s/providers/hetzner/apps" >"${apps_render}" 2>"${workdir}/apps.err" ||
  fail "prod apps overlay failed to build: $(tail -5 "${workdir}/apps.err")"
for route_input in "${repo_root}/k8s/bases/apps/homepage/http-route.yaml" "${apps_render}"; do
  homepage_count="$(yq -o=json -I=0 'select(.kind == "HTTPRoute" and .metadata.namespace == "homepage" and .metadata.name == "homepage")' "${route_input}" | jq -s 'length')"
  [ "${homepage_count}" = 1 ] || fail "expected exactly one Homepage route in ${route_input}"
  homepage_parents="$(yq -o=json -I=0 'select(.kind == "HTTPRoute" and .metadata.namespace == "homepage" and .metadata.name == "homepage") | .spec.parentRefs' "${route_input}")"
  jq -e 'length == 1 and .[0].name == "platform" and .[0].namespace == "kube-system" and .[0].port == 443 and (.[0].group // "gateway.networking.k8s.io") == "gateway.networking.k8s.io" and (.[0].kind // "Gateway") == "Gateway"' <<<"${homepage_parents}" >/dev/null ||
    fail "Homepage must have exactly one parent on Gateway kube-system/platform, constrained to HTTPS port 443 (${route_input})"
done

echo "Tenant route-kind boundary holds: RBAC grants HTTPRoute only and all ${listener_count} listeners pin kinds to HTTPRoute."
