#!/usr/bin/env bash

# Behavioral negative controls for the effective production authorization
# rules. KSail evaluates the same CEL against Helm-rendered chart children in
# CI, so these fixtures prove the rule file refuses concrete privilege paths.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly rules_path="${root_dir}/scripts/tests/production-authorization-rules.yaml"

test_root="$(mktemp -d /tmp/production-authorization-rules.XXXXXX)"
readonly test_root
cleanup() {
  rm -rf "${test_root}"
}
trap cleanup EXIT

assert_rejected() {
  local name="$1"
  local manifest="$2"
  local path="${test_root}/${name}.yaml"
  printf '%s\n' "${manifest}" >"${path}"
  if ksail workload validate "${path}" --skip-helm-render --rules "${rules_path}" >/dev/null 2>&1; then
    printf 'FAIL: unsafe fixture %s passed effective authorization validation\n' "${name}" >&2
    exit 1
  fi
}

assert_accepted() {
  local name="$1"
  local manifest="$2"
  local path="${test_root}/${name}.yaml"
  printf '%s\n' "${manifest}" >"${path}"
  if ! ksail workload validate "${path}" --skip-helm-render --rules "${rules_path}" >/dev/null 2>&1; then
    printf 'FAIL: least-privilege fixture %s failed effective authorization validation\n' "${name}" >&2
    exit 1
  fi
}

assert_rejected 'aws-shadow-binding' 'apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: aws-shadow
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: view
subjects:
  - kind: ServiceAccount
    name: aws
    namespace: aws'

assert_rejected 'cluster-admin-binding' 'apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: unreviewed-controller
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
  - kind: ServiceAccount
    name: controller
    namespace: controller'

assert_rejected 'forged-known-cluster-admin-binding' 'apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: flux-operator
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
  - kind: ServiceAccount
    name: attacker
    namespace: attacker'

assert_accepted 'data-product-controller-rbac' 'apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: data-product-controller
rules:
  - apiGroups: [data.devantler.tech]
    resources: [dataproducts]
    verbs: [get, list, watch]
  - apiGroups: [data.devantler.tech]
    resources: [dataproducts/status]
    verbs: [get, patch, update]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: data-product-controller-leader-election
  namespace: data-product-controller
rules:
  - apiGroups: [coordination.k8s.io]
    resources: [leases]
    verbs: [get, list, watch, create, update, patch, delete]'

printf 'PASS: effective authorization rules reject privilege paths and accept the data-product controller RBAC\n'
