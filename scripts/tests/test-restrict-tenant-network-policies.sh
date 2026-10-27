#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
policy="${repo_root}/k8s/bases/infrastructure/cluster-policies/best-practices/restrict-tenant-network-policies.yaml"
tests="${repo_root}/tests/restrict-tenant-network-policies"
work_dir="$(mktemp -d)"
output_file="${work_dir}/output"
trap 'rm -rf -- "${work_dir}"' EXIT

# `kyverno test` treats a rule that matches nothing as Excluded, and an Excluded
# row satisfies `result: fail` too — so the declarative test can pass vacuously
# if a rule silently stops matching. Exercise the policy directly as a second
# gate, asserting the exact allow/deny counts rather than an exit code.
apply() {
  local resources="$1" values="$2" userinfo="$3"
  apply_status=0
  kyverno apply "${policy}" \
    --resource "${resources}" \
    --values-file "${values}" \
    --userinfo "${userinfo}" \
    --remove-color >"${output_file}" 2>&1 || apply_status=$?
}

expect_summary() {
  local expected="$1" what="$2"
  if ! grep -Fq "${expected}" "${output_file}"; then
    echo "::error::${what}: expected '${expected}'"
    sed -n '1,80p' "${output_file}"
    exit 1
  fi
}

# 1. The tenant fixture set: 78 boundary-dissolving shapes denied, the ordinary
#    additive allow-lists admitted. A drop in the fail count means a shape that
#    used to be refused now admits.
apply "${tests}/resources.yaml" "${tests}/values.yaml" "${tests}/user-info.yaml"
expect_summary "pass: 309, fail: 78, warn: 0, error: 0, skip: 0" \
  "tenant CiliumNetworkPolicy boundary"

# 2. Carve-out: kyverno's background controller owns the generated floor. These
#    are the verbatim generated bodies and each carries a reserved name, so a
#    regression here would block the platform from creating its own default-deny.
apply "${tests}/kyverno-author/resources.yaml" \
  "${tests}/kyverno-author/values.yaml" "${tests}/kyverno-author/user-info.yaml"
expect_summary "pass: 0, fail: 0, warn: 0, error: 0, skip: 0" \
  "generated default-deny/allow-dns/allow-cnpg-operator must remain creatable by kyverno"

# 3. CONTROL for 2: the identical bodies, in the identical namespace, submitted
#    by the TENANT. Only the applying identity differs. Without this the zero
#    above is indistinguishable from the policy simply not matching. All three
#    bodies fail the reserved-name rule; allow-dns ALSO fails the egress rule,
#    because it reaches CoreDNS by naming kube-system through the namespace
#    label, and allow-cnpg-operator ALSO fails the ingress rule for naming
#    cnpg-system — the cross-namespace selector a tenant is refused (#3399,
#    #3655). default-deny ALSO fails both the ingress and the egress rule: its
#    one empty rule per direction names no source or destination, which those
#    rules refuse because a sourceless rule WITH toPorts allows every peer on
#    that port. Without toPorts, as here, Cilium allows nothing (#3501), so the
#    refusal is conservative, not a hole. The generated floor is exempt from all
#    of that only through its applying identity, which is the point.
apply "${tests}/kyverno-author/resources.yaml" \
  "${tests}/kyverno-author/values.yaml" "${tests}/user-info.yaml"
expect_summary "pass: 8, fail: 7, warn: 0, error: 0, skip: 0" \
  "control: a tenant must NOT be able to author the reserved generated names"

# 4. Carve-out: platform-authored policies applied by flux-system's
#    kustomize-controller stay permitted even when broad.
apply "${tests}/platform-author/resources.yaml" \
  "${tests}/platform-author/values.yaml" "${tests}/platform-author/user-info.yaml"
expect_summary "pass: 0, fail: 0, warn: 0, error: 0, skip: 0" \
  "platform-authored policies must stay permitted"

# 5. CONTROL for 4: the identical broad body submitted by the TENANT is refused
#    by both the ingress and the egress rule.
apply "${tests}/platform-author/resources.yaml" \
  "${tests}/platform-author/values.yaml" "${tests}/user-info.yaml"
expect_summary "pass: 2, fail: 2, warn: 0, error: 0, skip: 0" \
  "control: a tenant must NOT be able to submit the broad platform body"

# 6. Node/provider fields are forbidden even when empty beside a valid rule.
apply "${tests}/node-group-resources.yaml" "${tests}/values.yaml" "${tests}/user-info.yaml"
expect_summary "pass: 48, fail: 16, warn: 0, error: 0, skip: 0" \
  "tenant node/provider selectors must remain forbidden"

# 7. The day-one policy platform-tenant-template ships must stay admissible
#    under the tenant's own identity. Tightening this boundary has made a freshly
#    generated tenant inadmissible three times, each found only by the
#    template's scheduled run (#3652); this makes the next one fail here. Four
#    rules evaluate it, and none may refuse it.
apply "${tests}/tenant-scaffold/resources.yaml" \
  "${tests}/tenant-scaffold/values.yaml" "${tests}/tenant-scaffold/user-info.yaml"
expect_summary "pass: 4, fail: 0, warn: 0, error: 0, skip: 0" \
  "the platform-tenant-template scaffold must be admissible on day one"

# 8. Static action guard. Every count above is a RULE result; none of them says
#    what admission actually does on a violation. Enforcement lives in each
#    rule's own `failureAction`, because the deprecated top-level
#    spec.validationFailureAction defaults an action-less rule to Audit — which
#    would admit every shape the counts above prove is "denied", with the whole
#    suite still green. Assert the action itself, not just the verdicts.
rule_count="$(yq '.spec.rules | length' "${policy}")"
enforced="$(yq '[.spec.rules[] | select(.validate.failureAction == "Enforce")] | length' "${policy}")"
top_level="$(yq '.spec.validationFailureAction // "absent"' "${policy}")"
if [[ "${rule_count}" -lt 1 ]]; then
  echo "::error::no rules found in ${policy}"
  exit 1
fi
if [[ "${enforced}" != "${rule_count}" ]]; then
  echo "::error::only ${enforced}/${rule_count} rules set validate.failureAction: Enforce"
  exit 1
fi
if [[ "${top_level}" != "absent" ]]; then
  echo "::error::spec.validationFailureAction is deprecated; enforcement must be rule-level"
  exit 1
fi

# The public staging hold is controlled by the platform before application
# reconciliation is enabled. Check the real policy and committed hold, retaining
# all ordinary tenant/floor fixtures above. A nonmatching rule cannot satisfy an
# exact one-denial summary; permitted actors must also return a successful exit.
hold="${repo_root}/k8s/providers/hetzner/apps/business-site/cilium-network-policy-staging-hold.yaml"
yq '.namespaceSelector += [{"name":"business-site","labels":{"app.kubernetes.io/managed-by":"kro"}}]' \
  "${tests}/values.yaml" >"${work_dir}/hold-values.yaml"
cat >"${work_dir}/hold-tenant.yaml" <<'USER'
apiVersion: cli.kyverno.io/v1alpha1
kind: UserInfo
metadata:
  name: fictional-business-tenant
userInfo:
  username: system:serviceaccount:business-site:business-site
  groups:
    - system:serviceaccounts
    - system:serviceaccounts:business-site
    - system:authenticated
USER
for operation in CREATE UPDATE DELETE; do
  yq ".globalValues.\"request.operation\" = \"${operation}\"" \
    "${work_dir}/hold-values.yaml" >"${work_dir}/operation.yaml"
  apply "${hold}" "${work_dir}/operation.yaml" "${work_dir}/hold-tenant.yaml"
  expect_summary "pass: 4, fail: 1, warn: 0, error: 0, skip: 0" "tenant staging hold ${operation}"
  [[ "${apply_status}" -eq 1 ]] || { echo "staging denial did not fail admission" >&2; exit 1; }
  for author in platform-author kyverno-author; do
    apply "${hold}" "${work_dir}/operation.yaml" "${tests}/${author}/user-info.yaml"
    expect_summary "pass: 0, fail: 0, warn: 0, error: 0, skip: 0" "platform staging hold ${operation}"
    [[ "${apply_status}" -eq 0 ]] || { echo "trusted platform operation failed" >&2; exit 1; }
  done
done
# A controller name in a tenant namespace does not inherit the platform carve-out.
yq '.userInfo.username = "system:serviceaccount:business-site:kustomize-controller"' \
  "${work_dir}/hold-tenant.yaml" >"${work_dir}/spoofed-controller.yaml"
apply "${hold}" "${work_dir}/hold-values.yaml" "${work_dir}/spoofed-controller.yaml"
expect_summary "pass: 4, fail: 1, warn: 0, error: 0, skip: 0" "tenant cannot borrow a controller name"
[[ "${apply_status}" -eq 1 ]] || exit 1
# Reserve only this name in this namespace; other ordinary policies stay usable.
for change in '.metadata.namespace = "other-tenant"' '.metadata.name = "tenant-allow-gateway"'; do
  yq "${change}" "${hold}" >"${work_dir}/ordinary-policy.yaml"
  apply "${work_dir}/ordinary-policy.yaml" "${work_dir}/hold-values.yaml" "${work_dir}/hold-tenant.yaml"
  expect_summary "pass: 4, fail: 0, warn: 0, error: 0, skip: 0" "ordinary tenant policy"
  [[ "${apply_status}" -eq 0 ]] || exit 1
done
# The same reserved-owner rule protects the generated floor. An old body that
# is invalid for a tenant must not exempt that tenant's subsequent UPDATE.
yq '.globalValues."request.operation" = "UPDATE"' \
  "${tests}/kyverno-author/values.yaml" >"${work_dir}/floor-update.yaml"
apply "${tests}/kyverno-author/resources.yaml" "${work_dir}/floor-update.yaml" "${tests}/user-info.yaml"
expect_summary "pass: 8, fail: 3, warn: 0, error: 0, skip: 4" "reserved floor UPDATE ownership"
[[ "${apply_status}" -eq 1 ]] || exit 1
apply "${tests}/kyverno-author/resources.yaml" "${work_dir}/floor-update.yaml" "${tests}/kyverno-author/user-info.yaml"
expect_summary "pass: 0, fail: 0, warn: 0, error: 0, skip: 0" "generated floor controller UPDATE"
[[ "${apply_status}" -eq 0 ]] || exit 1

echo "Tenant CiliumNetworkPolicy policy enforced the expected boundary, and both"
echo "platform carve-outs were proven to depend on the applying identity."
