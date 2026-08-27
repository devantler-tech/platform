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

extract_policy() {
  awk -v policy_name="${policy_name}" '
    function reset_document() {
      document = ""
      is_clusterwide_policy = 0
      is_target = 0
    }

    function finish_document() {
      if (is_clusterwide_policy && is_target) {
        count++
        selected = document
      }
      reset_document()
    }

    BEGIN { reset_document() }
    /^---[[:space:]]*$/ { finish_document(); next }
    {
      document = document $0 ORS
      if ($0 ~ /^kind:[[:space:]]*CiliumClusterwideNetworkPolicy[[:space:]]*$/) {
        is_clusterwide_policy = 1
      }
      if ($0 == "  name: " policy_name) {
        is_target = 1
      }
    }
    END {
      finish_document()
      if (count != 1) {
        printf "expected exactly one rendered %s policy, found %d\n", policy_name, count > "/dev/stderr"
        exit 1
      }
      printf "%s", selected
    }
  '
}

tmp_dir="$(mktemp -d)"
readonly tmp_dir
trap 'rm -rf "${tmp_dir}"' EXIT
readonly rendered_policy="${tmp_dir}/policy.yaml"
readonly local_render="${tmp_dir}/local.yaml"

kubectl kustomize "${root_dir}/k8s/providers/hetzner/infrastructure" |
  extract_policy >"${rendered_policy}" ||
  fail 'the production build does not render exactly one metadata-egress deny policy'

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
yq -e '(.spec.endpointSelector | type) == "!!map" and (.spec.endpointSelector | length) == 0' \
  "${rendered_policy}" >/dev/null ||
  fail 'the policy must select every workload endpoint'
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

printf 'PASS: production denies workload metadata egress without selecting Talos hosts\n'
