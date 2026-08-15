#!/usr/bin/env bash
# Negative coverage for the tenant hostname boundary on the shared Gateway.
#
# The boundary is enforced by `restrict-tenant-http-route-hostnames`, which
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
# Layer 3 re-checks that the hostname policy itself still denies, so a future
# edit cannot leave the allow-list admitting everything.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"

role_file="${repo_root}/k8s/bases/infrastructure/cluster-roles/gateway-tenant-edit.yaml"
policy="${repo_root}/k8s/bases/infrastructure/cluster-policies/best-practices/restrict-tenant-http-route-hostnames.yaml"
fixtures="${repo_root}/tests/restrict-tenant-http-route-hostnames/resources.yaml"
# The prod overlay is what carries every listener: two come from the base
# Gateway and two more are appended by the hetzner JSON6902 patch, which a
# per-file check would never see.
overlay="${repo_root}/k8s/providers/hetzner/infrastructure"

workdir="$(mktemp -d)"
trap 'rm -rf "${workdir}"' EXIT
render="${workdir}/render.yaml"
gateway="${workdir}/gateway.yaml"
output_file="${workdir}/kyverno.out"

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
  fail "gateway-tenant-edit must grant exactly 'httproutes referencegrants'; found '${granted}'. Any other route kind bypasses restrict-tenant-http-route-hostnames — extend that policy and the listener kinds before widening this."

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

# Compare the kinds as a JOINED STRING, never as an array. yq's `!=` does not
# do deep array equality — `(… | sort) != ["HTTPRoute"]` evaluates to true even
# for a correctly pinned listener, which reported all four as unpinned when this
# test was written. It failed closed there, but the same shape fails OPEN when
# the sense is reversed, so keep the comparison on strings.
unpinned="$(yq eval \
  '[.spec.listeners[] | select(((.allowedRoutes.kinds // []) | map(.kind) | sort | join(",")) != "HTTPRoute")] | .[].name' \
  "${gateway}" | tr '\n' ' ')"
[ -z "${unpinned// /}" ] ||
  fail "these Gateway listeners do not pin allowedRoutes.kinds to exactly [HTTPRoute]: ${unpinned}— an unpinned HTTPS listener also accepts GRPCRoute, which restrict-tenant-http-route-hostnames does not match."

# --- Layer 3: the hostname policy still denies ------------------------------
# `kyverno test` reports a missing named rule as Excluded and can pass
# vacuously, so exercise the policy directly (same second gate as
# test-restrict-tenant-secret-stores.sh): the fixture must admit 3 and deny 4.
if kyverno apply "${policy}" \
  --resource "${fixtures}" \
  --remove-color >"${output_file}" 2>&1; then
  fail "tenant HTTPRoute hostname policy admitted every fixture; no deny rule executed"
fi

expected_summary="pass: 3, fail: 4, warn: 0, error: 0, skip: 0"
if ! grep -Fq "${expected_summary}" "${output_file}"; then
  echo "::error::tenant HTTPRoute hostname policy returned an unexpected allow/deny verdict"
  sed -n '1,80p' "${output_file}"
  exit 1
fi

echo "Tenant route boundary holds: RBAC grants HTTPRoute only, all ${listener_count} listeners pin kinds to HTTPRoute, and the hostname policy enforced 3-admit/4-deny."
