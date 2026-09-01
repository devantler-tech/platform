#!/usr/bin/env bash

# Proves that the isolated data-product-controller chart cannot render a child
# into another namespace while retaining the EKS authorization exemption.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly rules_path="${root_dir}/scripts/tests/isolated-chart-namespace-rules.yaml"
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

printf 'PASS: isolated chart children are namespace-local and the exact pinned chart renders cleanly\n'
