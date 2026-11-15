#!/usr/bin/env bash
# Run the restrict-ci-identity-bindings native admission policy offline against every
# rendered production layer (platform#6270), so a binding that would widen the
# EKS CI identity fails CI before the admission controller refuses it.
#
# GUARD_REPO_ROOT (default: the current directory) holds the overlays; the
# policy is always read from this repository's checkout.
#
# Only kustomize renders are checked here: bindings that HelmRelease charts,
# KRO or generating policies create exist only in the cluster, where the API
# server enforces the same policy at admission.

set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="${GUARD_REPO_ROOT:-.}"
policy_dir="${script_dir}/../k8s/providers/hetzner/infrastructure/ci-identity"
policy="${policy_dir}/validating-admission-policy-ci-identity-bindings.yaml"
binding="${policy_dir}/validating-admission-policy-binding-ci-identity-bindings.yaml"
overlays=(
  k8s/providers/hetzner/apps
  k8s/providers/hetzner/infrastructure
  k8s/providers/hetzner/infrastructure/controllers
  k8s/clusters/prod/bootstrap
  k8s/clusters/prod
)

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
rendered="${work}/rendered.yaml"
: >"${rendered}"
for overlay in "${overlays[@]}"; do
  kubectl kustomize "${repo_root}/${overlay}" >>"${rendered}"
  printf '\n---\n' >>"${rendered}"
done

# A render with no binding at all would pass vacuously; the approved binding
# must be among the evaluated documents.
approved="$(yq -N 'select(.kind == "RoleBinding" and .metadata.namespace == "aws" and .metadata.name == "aws-managed-resources") | .metadata.name' "${rendered}")"
if [[ "${approved}" != aws-managed-resources ]]; then
  printf '::error::the approved aws/aws-managed-resources RoleBinding is not in the render\n' >&2
  exit 1
fi

# Only RBAC bindings are evaluated: the native policy matches nothing else, and
# kyverno cannot map the custom kinds elsewhere in the render.
bindings="${work}/bindings.yaml"
yq 'select(.kind == "RoleBinding" or .kind == "ClusterRoleBinding")' "${rendered}" >"${bindings}"

if ! kyverno apply "${policy}" "${binding}" --resource "${bindings}" >"${work}/apply.log" 2>&1; then
  cat "${work}/apply.log" >&2
  printf '::error::a rendered binding grants to the EKS CI identity outside the approved RoleBinding\n' >&2
  exit 1
fi
grep -E '^pass: [1-9][0-9]*, fail: 0, warn: 0, error: 0' "${work}/apply.log" || {
  cat "${work}/apply.log" >&2
  printf '::error::kyverno apply evaluated no binding or reported errors\n' >&2
  exit 1
}
