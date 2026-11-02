#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
policy="${repo_root}/k8s/bases/infrastructure/cluster-policies/best-practices/restrict-tenant-network-policies.yaml"
tests="${repo_root}/tests/restrict-tenant-network-policies"
work="$(mktemp -d)"
output_file="${work}/output"
trap 'rm -rf "${work}"' EXIT

# `kyverno test` treats a resource the policy does not evaluate as Excluded, and an
# Excluded row satisfies `result: fail` too — so the declarative test can pass
# vacuously if the policy silently stops matching. Exercise the policy directly as a
# second gate, asserting the exact allow/deny counts rather than an exit code.
apply() {
  local resources="$1" values="$2" userinfo="$3"
  apply_status=0
  kyverno apply "${policy}" \
    --resource "${resources}" \
    --values-file "${values}" \
    --userinfo "${userinfo}" \
    --remove-color >"${output_file}" 2>&1 || apply_status=$?
  if [[ "${apply_status}" -gt 1 ]]; then
    cat "${output_file}" >&2
    exit "${apply_status}"
  fi
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
#    used to be refused now admits. The unmanaged namespace control is an explicit
#    match-condition skip; a namespaceSelector previously omitted its result.
apply "${tests}/resources.yaml" "${tests}/values.yaml" "${tests}/user-info.yaml"
expect_summary "pass: 18, fail: 78, warn: 0, error: 0, skip: 1" \
  "tenant CiliumNetworkPolicy boundary"

# 2. Carve-out: kyverno's background controller owns the generated floor. These
#    are the verbatim generated bodies and each carries a reserved name, so a
#    regression here would block the platform from creating its own default-deny.
apply "${tests}/kyverno-author/resources.yaml" \
  "${tests}/kyverno-author/values.yaml" "${tests}/kyverno-author/user-info.yaml"
expect_summary "pass: 0, fail: 0, warn: 0, error: 0, skip: 3" \
  "generated default-deny/allow-dns/allow-cnpg-operator must remain creatable by kyverno"

# 3. CONTROL for 2: the identical bodies, in the identical namespace, submitted
#    by the TENANT. Only the applying identity differs, and all three are refused.
#    Besides the reserved name, allow-dns reaches CoreDNS by naming kube-system
#    through the namespace label and allow-cnpg-operator names cnpg-system, the
#    cross-namespace selector a tenant is refused (#3399, #3655). default-deny's
#    one empty rule per direction names no source or destination, which is refused
#    because a sourceless rule WITH toPorts allows every peer on that port. Without
#    toPorts, as here, Cilium allows nothing (#3501), so the refusal is conservative,
#    not a hole. The generated floor is exempt only through its applying identity.
apply "${tests}/kyverno-author/resources.yaml" \
  "${tests}/kyverno-author/values.yaml" "${tests}/user-info.yaml"
expect_summary "pass: 0, fail: 3, warn: 0, error: 0, skip: 0" \
  "control: a tenant must NOT be able to author the reserved generated names"

# 4. Carve-out: platform-authored policies applied by flux-system's
#    kustomize-controller stay permitted even when broad.
apply "${tests}/platform-author/resources.yaml" \
  "${tests}/platform-author/values.yaml" "${tests}/platform-author/user-info.yaml"
expect_summary "pass: 0, fail: 0, warn: 0, error: 0, skip: 1" \
  "platform-authored policies must stay permitted"

# 5. CONTROL for 4: the identical broad body submitted by the TENANT is refused.
apply "${tests}/platform-author/resources.yaml" \
  "${tests}/platform-author/values.yaml" "${tests}/user-info.yaml"
expect_summary "pass: 0, fail: 1, warn: 0, error: 0, skip: 0" \
  "control: a tenant must NOT be able to submit the broad platform body"

# 6. Node/provider fields are forbidden even when empty beside a valid rule.
apply "${tests}/node-group-resources.yaml" "${tests}/values.yaml" "${tests}/user-info.yaml"
expect_summary "pass: 0, fail: 16, warn: 0, error: 0, skip: 0" \
  "tenant node/provider selectors must remain forbidden"

# 7. The day-one policy platform-tenant-template ships must stay admissible
#    under the tenant's own identity. Tightening this boundary has made a freshly
#    generated tenant inadmissible three times, each found only by the
#    template's scheduled run (#3652); this makes the next one fail here.
apply "${tests}/tenant-scaffold/resources.yaml" \
  "${tests}/tenant-scaffold/values.yaml" "${tests}/tenant-scaffold/user-info.yaml"
expect_summary "pass: 1, fail: 0, warn: 0, error: 0, skip: 0" \
  "the platform-tenant-template scaffold must be admissible on day one"

# 8. Static action guard. Every count above is a policy verdict; none of them says
#    what admission does on a violation. Audit would admit every shape the counts
#    above prove is refused, with the whole suite still green.
if ! yq -o=json '.' "${policy}" | jq -e '
  .kind == "ValidatingPolicy" and .spec.failurePolicy == "Fail" and
  .spec.validationActions == ["Deny"] and .spec.evaluation.background.enabled == false and
  .spec.matchConstraints.resourceRules == [{
    apiGroups: ["cilium.io"], apiVersions: ["*"], operations: ["CREATE", "UPDATE", "DELETE"],
    resources: ["ciliumnetworkpolicies"]}]
' >/dev/null; then
  echo "::error::scope, operations or fail-closed disposition of ${policy} changed"
  exit 1
fi

# 9. One result per resource cannot say which check refused it, and a resource can
#    fail more than one. Run each check alone against the rows recorded for it in
#    per-check.yaml, and require those rows to go red when it accepts everything.
checks=(
  cnp-ingress-sources-must-be-constrained
  cnp-egress-destinations-must-be-constrained
  cnp-must-not-opt-out-of-default-deny
  cnp-generated-policy-names-are-reserved
  cnp-must-use-the-singular-spec-container
)
if [[ "$(yq '.spec.validations | length' "${policy}")" != "${#checks[@]}" ]]; then
  echo "::error::the policy's checks and the ${#checks[@]} names here differ"
  exit 1
fi
for index in "${!checks[@]}"; do
  check="${checks[${index}]}"
  fail_rows=0
  for rows_file in "${tests}/per-check.yaml" "${tests}"/*/per-check.yaml; do
    fixture="$(dirname "${rows_file}")"
    case_dir="${work}/${index}-$(basename "${fixture}")"
    mkdir -p "${case_dir}"
    CHECK="${check}" yq -o=json '.results | map(select(.rule == strenv(CHECK)))' "${rows_file}" \
      >"${case_dir}/rows.json"
    [[ "$(jq 'length' "${case_dir}/rows.json")" -gt 0 ]] || continue
    refused="$(jq '[.[] | select(.result == "fail") | .resources[]] | length' "${case_dir}/rows.json")"
    fail_rows=$((fail_rows + refused))
    yq -o=json '{"resources": .resources, "variables": .variables, "userinfo": .userinfo}' "${rows_file}" \
      >"${case_dir}/inputs.json"
    for variant in kept weakened; do
      target="${case_dir}/${variant}"
      mkdir -p "${target}"
      find "${fixture}" -maxdepth 1 -type f -name '*.yaml' ! -name 'kyverno-test.yaml' ! -name 'per-check.yaml' \
        -exec cp {} "${target}/" \;
      if [[ "${variant}" == kept ]]; then
        INDEX="${index}" yq '.spec.validations = [.spec.validations[env(INDEX)]]' "${policy}" >"${target}/policy.yaml"
      else
        INDEX="${index}" yq '.spec.validations = [.spec.validations[env(INDEX)] | .expression = "true"]' \
          "${policy}" >"${target}/policy.yaml"
      fi
      jq --slurpfile inputs "${case_dir}/inputs.json" \
        '{apiVersion: "cli.kyverno.io/v1alpha1", kind: "Test", metadata: {name: "check"},
          policies: ["policy.yaml"], resources: $inputs[0].resources,
          variables: $inputs[0].variables, userinfo: $inputs[0].userinfo,
          results: map(del(.rule))}' "${case_dir}/rows.json" >"${target}/kyverno-test.yaml"
    done
    if ! kyverno test "${case_dir}/kept" --remove-color >"${case_dir}/kept.log" 2>&1; then
      cat "${case_dir}/kept.log"
      echo "::error::${check}: its rows in ${rows_file} do not hold when it runs alone"
      exit 1
    fi
    if [[ "${refused}" -gt 0 ]] && kyverno test "${case_dir}/weakened" --remove-color >"${case_dir}/weakened.log" 2>&1; then
      echo "::error::${check}: its fail rows in ${rows_file} still hold with the check removed"
      exit 1
    fi
  done
  if [[ "${fail_rows}" -eq 0 ]]; then
    echo "::error::${check}: no fixture proves this check refuses anything"
    exit 1
  fi
  echo "ok: ${check} (${fail_rows} refused)"
done

# 10. The CLI cannot send a DELETE or an UPDATE's old object, so these go through the
#     pinned admission engine. A tenant holds delete on CiliumNetworkPolicy, so
#     without the DELETE rows a tenant could remove the generated floor.
tenant='system:serviceaccount:tenant-ns:tenant'
kyverno_sa='system:serviceaccount:kyverno:kyverno-background-controller'
flux_sa='system:serviceaccount:flux-system:kustomize-controller'
cnp() {
  jq -cn --arg name "$1" '{apiVersion: "cilium.io/v2", kind: "CiliumNetworkPolicy",
    metadata: {name: $name, namespace: "tenant-ns"},
    spec: {endpointSelector: {}, ingress: [{fromEndpoints: [{matchLabels: {app: "web"}}]}]}}'
}
control() {
  local label="$1" actor="$2" operation="$3" name="$4" expected="$5" object='null' old='null'
  [[ "${operation}" == DELETE ]] || object="$(cnp "${name}")"
  [[ "${operation}" == CREATE ]] || old="$(cnp "${name}")"
  jq -cn --arg label "${label}" --arg actor "${actor}" --arg operation "${operation}" \
    --arg expected "${expected}" --argjson object "${object}" --argjson old "${old}" '
    {label: $label, policy: "restrict-tenant-network-policies", actor: $actor, operation: $operation,
     expected: $expected, namespaces: [{metadata: {name: "tenant-ns", labels: {"app.kubernetes.io/managed-by": "ksail"}}}]}
    + (if $object == null then {} else {object: $object} end)
    + (if $old == null then {} else {oldObject: $old} end)'
}
hold_control() {
  local label="$1" actor="$2" operation="$3" namespace="$4" name="$5" expected="$6" managed="$7"
  yq -o=json '.' "${repo_root}/k8s/providers/hetzner/apps/business-site/cilium-network-policy-staging-hold.yaml" |
    jq -c --arg label "${label}" --arg actor "${actor}" --arg operation "${operation}" \
      --arg namespace "${namespace}" --arg name "${name}" --arg expected "${expected}" --arg managed "${managed}" '
      .metadata.namespace = $namespace | .metadata.name = $name | . as $resource |
      {label: $label, policy: "restrict-tenant-network-policies", actor: $actor,
       operation: $operation, expected: $expected,
       namespaces: [{metadata: {name: $namespace, labels:
         (if $managed == "" then {} else {"app.kubernetes.io/managed-by": $managed} end)}}]}
      + (if $operation == "DELETE" then {} else {object: $resource} end)
      + (if $operation == "CREATE" then {} else {oldObject: $resource} end)'
}
{
  control tenant-deletes-own-policy "${tenant}" DELETE allow-web pass
  for reserved in default-deny allow-dns allow-cnpg-operator; do
    control "tenant-deletes-${reserved}" "${tenant}" DELETE "${reserved}" fail
    control "tenant-updates-${reserved}" "${tenant}" UPDATE "${reserved}" fail
    control "kyverno-deletes-${reserved}" "${kyverno_sa}" DELETE "${reserved}" skip
    control "flux-deletes-${reserved}" "${flux_sa}" DELETE "${reserved}" skip
  done
  control tenant-updates-own-policy "${tenant}" UPDATE allow-web pass
  # The committed rollout hold is protected independently of namespace labels.
  # UPDATE carries the same already-invalid old body; DELETE carries only oldObject.
  for managed in kro ''; do
    for operation in CREATE UPDATE DELETE; do
      hold_control "tenant-hold-${operation}-${managed:-unlabelled}" \
        system:serviceaccount:business-site:business-site "${operation}" \
        business-site business-site-staging-hold fail "${managed}"
      hold_control "spoofed-controller-hold-${operation}-${managed:-unlabelled}" \
        system:serviceaccount:business-site:kustomize-controller "${operation}" \
        business-site business-site-staging-hold fail "${managed}"
      for actor in "${kyverno_sa}" "${flux_sa}"; do
        hold_control "platform-hold-${operation}-${managed:-unlabelled}-${actor}" \
          "${actor}" "${operation}" business-site business-site-staging-hold skip "${managed}"
      done
      hold_control "other-name-${operation}-${managed:-unlabelled}" "${tenant}" \
        "${operation}" business-site tenant-allow-gateway \
        "$(if [[ -n "${managed}" ]]; then echo pass; else echo skip; fi)" "${managed}"
      hold_control "other-namespace-${operation}-${managed:-unlabelled}" "${tenant}" \
        "${operation}" other-tenant business-site-staging-hold \
        "$(if [[ -n "${managed}" ]]; then echo pass; else echo skip; fi)" "${managed}"
    done
  done
} >"${work}/controls.jsonl"
(
  cd "${repo_root}/scripts/tests/tenant-webapp-admission"
  go test -mod=readonly -count=1 -timeout 60s ./...
  go build -mod=readonly -o "${work}/admission" .
)
"${work}/admission" "${policy}" <"${work}/controls.jsonl"

echo "Tenant CiliumNetworkPolicy policy enforced the expected boundary, and both"
echo "platform carve-outs were proven to depend on the applying identity."
