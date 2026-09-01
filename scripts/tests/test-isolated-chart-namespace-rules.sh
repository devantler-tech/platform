#!/usr/bin/env bash

# Proves that the isolated data-product-controller chart cannot render a child
# into another namespace while retaining the EKS authorization exemption.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly rules_path="${root_dir}/scripts/tests/isolated-chart-namespace-rules.yaml"
readonly prod_rules_path="${root_dir}/scripts/tests/production-authorization-rules.yaml"
readonly component_path="${root_dir}/k8s/bases/apps/data-product-controller"

test_root="$(mktemp -d /tmp/isolated-chart-namespace-rules.XXXXXX)"
readonly test_root
cleanup() {
  rm -rf "${test_root}"
}
trap cleanup EXIT

run_fixture() {
  local path="$1"
  ksail --config "${root_dir}/ksail.prod.yaml" workload validate "${path}" \
    --skip-helm-render \
    --rules "${rules_path}" 2>&1
}

assert_accepted() {
  local name="$1"
  local manifest="$2"
  local path="${test_root}/${name}.yaml"
  local output
  printf '%s\n' "${manifest}" >"${path}"
  if ! output="$(run_fixture "${path}")"; then
    printf 'FAIL: namespace-local fixture %s was rejected\n' "${name}" >&2
    printf '%s\n' "${output}" >&2
    exit 1
  fi
}

assert_rejected() {
  local name="$1"
  local manifest="$2"
  local path="${test_root}/${name}.yaml"
  local output
  printf '%s\n' "${manifest}" >"${path}"
  if output="$(run_fixture "${path}")"; then
    printf 'FAIL: foreign-namespace fixture %s passed isolated-chart validation\n' "${name}" >&2
    printf '%s\n' "${output}" >&2
    exit 1
  fi
  if ! printf '%s\n' "${output}" | grep -qF \
    'rule "restrict-data-product-controller-rendered-child-namespaces"'; then
    printf 'FAIL: fixture %s was refused, but not by the rendered-child namespace rule\n' "${name}" >&2
    printf '%s\n' "${output}" >&2
    exit 1
  fi
}

assert_accepted 'namespace-local-deployment' 'apiVersion: apps/v1
kind: Deployment
metadata:
  name: data-product-controller
  namespace: data-product-controller
spec:
  selector:
    matchLabels:
      app: data-product-controller
  template:
    metadata:
      labels:
        app: data-product-controller
    spec:
      containers:
        - name: controller
          image: example.invalid/controller:test'

assert_rejected 'foreign-namespace-deployment' 'apiVersion: apps/v1
kind: Deployment
metadata:
  name: data-product-controller
  namespace: aws
spec:
  selector:
    matchLabels:
      app: data-product-controller
  template:
    metadata:
      labels:
        app: data-product-controller
    spec:
      containers:
        - name: controller
          image: example.invalid/controller:test'

assert_rejected 'foreign-namespace-declaration' 'apiVersion: v1
kind: Namespace
metadata:
  name: aws'

assert_rejected 'namespace-omitted-workload' 'apiVersion: apps/v1
kind: Deployment
metadata:
  name: data-product-controller
spec:
  selector:
    matchLabels:
      app: data-product-controller
  template:
    metadata:
      labels:
        app: data-product-controller
    spec:
      containers:
        - name: controller
          image: example.invalid/controller:test'

# The cluster-scoped RBAC branch is the one an attacker-shaped chart revision
# would aim at: it is the only branch that accepts an object carrying no
# namespace at all. Each fixture below moves exactly ONE field away from the
# reviewed shape, so a rule that stops pinning that field fails precisely one
# of them rather than the whole group.

assert_accepted 'reviewed-cluster-role' 'apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: data-product-controller
rules:
  - apiGroups: [""]
    resources: ["configmaps"]
    verbs: ["get"]'

assert_accepted 'reviewed-cluster-role-binding' 'apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: data-product-controller
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: data-product-controller
subjects:
  - kind: ServiceAccount
    name: data-product-controller
    namespace: data-product-controller'

assert_rejected 'foreign-named-cluster-role' 'apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: cluster-admin-shadow
rules:
  - apiGroups: ["*"]
    resources: ["*"]
    verbs: ["*"]'

assert_rejected 'privileged-role-ref-binding' 'apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: data-product-controller
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
  - kind: ServiceAccount
    name: data-product-controller
    namespace: data-product-controller'

assert_rejected 'external-subject-binding' 'apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: data-product-controller
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: data-product-controller
subjects:
  - kind: ServiceAccount
    name: kustomize-controller
    namespace: flux-system'

# A binding whose subject list mixes one reviewed subject with one foreign
# subject proves the check is `all`, not `exists`.
assert_rejected 'mixed-subject-binding' 'apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: data-product-controller
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: data-product-controller
subjects:
  - kind: ServiceAccount
    name: data-product-controller
    namespace: data-product-controller
  - kind: ServiceAccount
    name: kustomize-controller
    namespace: flux-system'

# A cluster-scoped subject carries no namespace at all. It must fail closed
# rather than pass vacuously.
assert_rejected 'cluster-scoped-subject-binding' 'apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: data-product-controller
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: data-product-controller
subjects:
  - kind: Group
    apiGroup: rbac.authorization.k8s.io
    name: system:authenticated'

assert_rejected 'subjectless-binding' 'apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: data-product-controller
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: data-product-controller'

# This is the enabled control: KSail resolves the immutable OCI digest from the
# staged-off component, Helm-renders that exact artifact, and evaluates every
# child under the same rule without adding the component to the deploy overlay.
if ! output="$(
  ksail --config "${root_dir}/ksail.prod.yaml" workload validate "${component_path}" \
    --rules "${rules_path}" 2>&1
)"; then
  printf 'FAIL: the pinned data-product-controller chart failed namespace validation\n' >&2
  printf '%s\n' "${output}" >&2
  exit 1
fi

# Second control, against the repository's PRODUCTION authorization rule suite
# rather than this file's bespoke namespace rule. While the component is staged
# off, it is absent from every deploy overlay, so cluster admission never
# evaluates it and the namespace rule above would otherwise be the ONLY control
# standing over these manifests. Rendering the same pinned artifact through the
# production suite means the staged-off chart is held to the same authorization
# controls as everything that is actually deployed.
if ! output="$(
  ksail --config "${root_dir}/ksail.prod.yaml" workload validate "${component_path}" \
    --rules "${prod_rules_path}" 2>&1
)"; then
  printf 'FAIL: the pinned data-product-controller chart failed production authorization validation\n' >&2
  printf '%s\n' "${output}" >&2
  exit 1
fi

printf 'PASS: isolated chart children are namespace-local, and the exact pinned chart renders cleanly under both the namespace rule and the production authorization suite\n'
