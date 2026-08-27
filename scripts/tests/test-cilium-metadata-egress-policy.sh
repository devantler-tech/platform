#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly policy_name="deny-workload-instance-metadata-egress"
readonly deleted_policy="${root_dir}/k8s/providers/hetzner/infrastructure/controllers/cilium/cilium-clusterwide-network-policy.yaml"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

tmp_dir="$(mktemp -d)"
readonly tmp_dir
trap 'rm -rf "${tmp_dir}"' EXIT
readonly prod_render="${tmp_dir}/prod.yaml"
readonly rendered_policy="${tmp_dir}/policy.yaml"
readonly controllers_render="${tmp_dir}/controllers.yaml"
readonly crossplane_policy="${tmp_dir}/crossplane-policy.yaml"
readonly local_render="${tmp_dir}/local.yaml"

kubectl kustomize "${root_dir}/k8s/providers/hetzner/infrastructure" >"${prod_render}" ||
  fail 'the production Hetzner infrastructure build did not render'
policy_count="$(
  yq ea \
    "[select(.kind == \"CiliumClusterwideNetworkPolicy\" and .metadata.name == \"${policy_name}\")] | length" \
    "${prod_render}"
)" || fail 'the production policy count could not be read'
[[ "${policy_count}" == 1 ]] ||
  fail "the production build rendered ${policy_count} metadata-egress policies instead of one"
yq ea \
  "select(.kind == \"CiliumClusterwideNetworkPolicy\" and .metadata.name == \"${policy_name}\")" \
  "${prod_render}" >"${rendered_policy}" ||
  fail 'the production metadata-egress policy could not be extracted'

kubectl kustomize "${root_dir}/k8s/providers/hetzner/infrastructure/controllers/crossplane" >"${controllers_render}" ||
  fail 'the production Crossplane controller build did not render'
yq ea \
  'select(.kind == "CiliumNetworkPolicy" and .metadata.namespace == "crossplane-system" and .metadata.name == "allow-crossplane")' \
  "${controllers_render}" >"${crossplane_policy}" ||
  fail 'the Crossplane egress policy could not be extracted'

kubectl kustomize "${root_dir}/k8s/providers/docker/infrastructure" >"${local_render}" ||
  fail 'the local Docker infrastructure build did not render'
if grep -Fq -- "name: ${policy_name}" "${local_render}"; then
  fail 'the Hetzner metadata policy leaked into the local Docker build'
fi

[[ ! -e "${deleted_policy}" ]] ||
  fail 'the deliberately deleted Cilium mutual-auth policy file was restored'

yq -e '.apiVersion == "cilium.io/v2" and .kind == "CiliumClusterwideNetworkPolicy"' \
  "${rendered_policy}" >/dev/null ||
  fail 'the rendered control is not a CiliumClusterwideNetworkPolicy'
yq -e '(.spec.endpointSelector | keys | length) == 1 and
  (.spec.endpointSelector.matchExpressions | length) == 2 and
  ([.spec.endpointSelector.matchExpressions[] |
    select(.key == "k8s:io.kubernetes.pod.namespace" and
      .operator == "Exists" and
      has("values") == false)] | length) == 1 and
  ([.spec.endpointSelector.matchExpressions[] |
    select(.key == "k8s:io.kubernetes.pod.namespace" and
      .operator == "NotIn" and
      (.values | length) == 1 and
      .values[0] == "crossplane-system")] | length) == 1' \
  "${rendered_policy}" >/dev/null ||
  fail 'the cluster-wide policy must select workload namespaces, exclude reserved identities, and exclude Crossplane'
yq -e '.spec | has("nodeSelector") == false' "${rendered_policy}" >/dev/null ||
  fail 'the workload policy must not select Talos hosts'
yq -e '.spec.egressDeny | length == 1' "${rendered_policy}" >/dev/null ||
  fail 'the policy must have exactly one deny rule'
yq -e '.spec.egressDeny[0] | (keys | length) == 1' "${rendered_policy}" >/dev/null ||
  fail 'the deny rule must not carry additional destinations'
yq -e '.spec.egressDeny[0] | has("toCIDR")' "${rendered_policy}" >/dev/null ||
  fail 'the deny rule must use a CIDR destination'
yq -e '.spec.egressDeny[0].toCIDR | length == 1' "${rendered_policy}" >/dev/null ||
  fail 'the deny rule must contain exactly one CIDR'
yq -e '.spec.egressDeny[0].toCIDR[0] == "169.254.169.254/32"' \
  "${rendered_policy}" >/dev/null ||
  fail 'the deny rule must target only the instance metadata address'

yq -e '(.spec.egressDeny | length) == 1 and
  (.spec.egressDeny[0] | keys | length) == 1 and
  (.spec.egressDeny[0].toCIDR | length) == 1 and
  .spec.egressDeny[0].toCIDR[0] == "169.254.169.254/32"' \
  "${crossplane_policy}" >/dev/null ||
  fail 'the existing Crossplane policy must carry the same exact metadata deny'

printf 'PASS: production denies workload metadata egress without selecting Talos hosts\n'
