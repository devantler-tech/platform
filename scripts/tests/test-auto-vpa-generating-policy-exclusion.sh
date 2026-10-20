#!/usr/bin/env bash
# The platform's copy of the catalog's auto-vpa GeneratingPolicy adds one thing:
# a namespaceSelector that keeps it out of the system namespaces, where prod
# ships static VPAs (k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers/).
# A generated VPA there would put a second VPA on the same pods. A kyverno test
# cannot assert that a GeneratingPolicy generates nothing, so this script does
# it: for each policy and each excluded namespace, a test that expects a VPA
# there must fail with "Not found". The same test with the selector removed must
# pass, so a probe that could never see a generation cannot read as excluded
# (platform#5130).
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
policies="${repo_root}/k8s/bases/infrastructure/cluster-policies/best-practices"
static_vpas="${repo_root}/k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers"

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

# The classic policy's exclusions, plus every namespace prod gives static VPAs.
required="$(
  {
    printf '%s\n' kube-system kube-public kube-node-lease kyverno
    kubectl kustomize "${static_vpas}" |
      yq -N 'select(.kind == "VerticalPodAutoscaler") | .metadata.namespace'
  } | sort -u
)"
if [[ -z "${required}" ]]; then
  echo "FAIL: no namespaces to check" >&2
  exit 1
fi

checked=0
for kind in deployment statefulset daemonset; do
  name="auto-vpa-${kind}"
  policy="${policies}/${name}.yaml"
  fixtures="${repo_root}/tests/auto-vpa-generating-policy-${kind}"
  excluded="$(yq -N "select(.metadata.name == \"${name}\")
    | .spec.matchConstraints.namespaceSelector.matchExpressions[]
    | select(.key == \"kubernetes.io/metadata.name\" and .operator == \"NotIn\")
    | .values[]" "${policy}" | sort -u)"
  missing="$(comm -23 <(printf '%s\n' "${required}") <(printf '%s\n' "${excluded}"))"
  if [[ -n "${missing}" ]]; then
    echo "FAIL: ${name} does not exclude: ${missing//$'\n'/ }" >&2
    exit 1
  fi
  yq "(select(.metadata.name == \"${name}\") | .spec.matchConstraints)
    |= del(.namespaceSelector)" "${policy}" >"${work}/${kind}-unscoped.yaml"

  while IFS= read -r ns; do
    case_dir="${work}/${kind}-${ns}"
    mkdir -p "${case_dir}"
    sed "s/^  namespace: apps-ns\$/  namespace: ${ns}/" "${fixtures}/resources.yaml" >"${case_dir}/resources.yaml"
    sed "s/^  namespace: apps-ns\$/  namespace: ${ns}/" "${fixtures}/generated-vpa.yaml" >"${case_dir}/generated-vpa.yaml"
    grep -q "namespace: ${ns}\$" "${case_dir}/resources.yaml"
    grep -q "namespace: ${ns}\$" "${case_dir}/generated-vpa.yaml"
    cat >"${case_dir}/values.yaml" <<EOF
apiVersion: cli.kyverno.io/v1alpha1
kind: Values
namespaceSelector:
  - name: ${ns}
    labels:
      kubernetes.io/metadata.name: ${ns}
EOF
    for variant in scoped unscoped; do
      if [[ "${variant}" == scoped ]]; then
        under_test="${policy}"
      else
        under_test="${work}/${kind}-unscoped.yaml"
      fi
      cat >"${case_dir}/kyverno-test.yaml" <<EOF
apiVersion: cli.kyverno.io/v1alpha1
kind: Test
metadata:
  name: ${name}-${ns}-${variant}
policies:
  - ${under_test}
resources:
  - resources.yaml
variables: values.yaml
results:
  - policy: ${name}
    resources:
      - ${ns}/shared
    generatedResource: generated-vpa.yaml
    kind: $(yq -N '.kind' "${case_dir}/resources.yaml")
    result: pass
EOF
      status=0
      kyverno test "${case_dir}" >"${case_dir}/${variant}.log" 2>&1 || status=$?
      if [[ "${variant}" == scoped ]]; then
        if [[ "${status}" -eq 0 ]] || ! grep -q 'Not found' "${case_dir}/${variant}.log"; then
          echo "FAIL: ${name} generated a VPA in ${ns}:" >&2
          cat "${case_dir}/${variant}.log" >&2
          exit 1
        fi
      elif [[ "${status}" -ne 0 ]]; then
        echo "FAIL: control: ${name} without its selector generated nothing in ${ns}, so the probe proves nothing:" >&2
        cat "${case_dir}/${variant}.log" >&2
        exit 1
      fi
    done
    checked=$((checked + 1))
  done <<<"${required}"
done

echo "OK: ${checked} policy/namespace cases generate no VPA, and each generates one without the selector"
