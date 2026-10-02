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

# Both helpers keep the validator's output. A bare non-zero exit is not proof of
# rejection: a missing ksail, an unknown flag, or an unparsable fixture also exits
# non-zero, and would otherwise let assert_rejected pass without the rule ever
# firing. Rejections must therefore name the rule that refused the fixture.
run_validate() {
  local path="$1"
  ksail workload validate "${path}" --skip-helm-render --rules "${rules_path}" 2>&1
}

assert_rejected() {
  local name="$1"
  local manifest="$2"
  local expected_rule="$3"
  local path="${test_root}/${name}.yaml"
  local output
  printf '%s\n' "${manifest}" >"${path}"
  if output="$(run_validate "${path}")"; then
    printf 'FAIL: unsafe fixture %s passed effective authorization validation\n' "${name}" >&2
    printf '%s\n' "${output}" >&2
    exit 1
  fi
  if ! printf '%s\n' "${output}" | grep -qF "rule \"${expected_rule}\""; then
    printf 'FAIL: fixture %s was refused, but not by rule %s\n' "${name}" "${expected_rule}" >&2
    printf '%s\n' "${output}" >&2
    exit 1
  fi
}

assert_accepted() {
  local name="$1"
  local manifest="$2"
  local path="${test_root}/${name}.yaml"
  local output
  printf '%s\n' "${manifest}" >"${path}"
  if ! output="$(run_validate "${path}")"; then
    printf 'FAIL: least-privilege fixture %s failed effective authorization validation\n' "${name}" >&2
    printf '%s\n' "${output}" >&2
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
    namespace: aws' 'restrict-aws-service-account-bindings'

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
    namespace: controller' 'reject-new-cluster-admin-bindings'

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
    namespace: attacker' 'reject-new-cluster-admin-bindings'

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

assert_rejected 'deny-only-clusterwide-egress' 'apiVersion: cilium.io/v2
kind: CiliumClusterwideNetworkPolicy
metadata:
  name: chart-shipped-deny
spec:
  endpointSelector: {}
  egressDeny:
    - toCIDR: [169.254.169.254/32]' 'require-explicit-clusterwide-default-deny'

assert_rejected 'deny-only-clusterwide-ingress-in-specs' 'apiVersion: cilium.io/v2
kind: CiliumClusterwideNetworkPolicy
metadata:
  name: chart-shipped-specs-deny
specs:
  - endpointSelector: {}
    ingress:
      - fromEntities: [cluster]
  - endpointSelector: {}
    ingressDeny:
      - fromEntities: [world]' 'require-explicit-clusterwide-default-deny'

assert_accepted 'clusterwide-deny-with-explicit-intent' 'apiVersion: cilium.io/v2
kind: CiliumClusterwideNetworkPolicy
metadata:
  name: subtract-metadata
spec:
  endpointSelector: {}
  enableDefaultDeny:
    egress: false
  egressDeny:
    - toCIDR: [169.254.169.254/32]
---
apiVersion: cilium.io/v2
kind: CiliumClusterwideNetworkPolicy
metadata:
  name: allow-list-with-deny
spec:
  endpointSelector: {}
  ingress:
    - fromEntities: [cluster]
  ingressDeny:
    - fromEntities: [world]
---
apiVersion: example.com/v1
kind: CiliumClusterwideNetworkPolicy
metadata:
  name: same-kind-other-group
spec:
  egressDeny:
    - anything: true'

# A Cilium policy with no rule in any direction is rejected by the agent
# (Valid=False) although the API server accepts it, so nothing in it is
# enforced. The add-default-deny ClusterPolicy generated exactly this shape into
# every namespace for ~101 days (#3501).
assert_rejected 'cilium-policy-empty-direction-lists' 'apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata:
  name: default-deny
  namespace: example
spec:
  endpointSelector: {}
  ingress: []
  egress: []
  enableDefaultDeny:
    ingress: true
    egress: true' 'require-cilium-policy-rules'

assert_rejected 'cilium-policy-no-spec' 'apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata:
  name: no-spec
  namespace: example' 'require-cilium-policy-rules'

assert_rejected 'clusterwide-policy-empty-second-spec' 'apiVersion: cilium.io/v2
kind: CiliumClusterwideNetworkPolicy
metadata:
  name: one-empty-spec
specs:
  - endpointSelector: {}
    egress:
      - toEntities: [kube-apiserver]
  - endpointSelector: {}
    ingressDeny: []
    egressDeny: []' 'require-cilium-policy-rules'

# The generated copies are not rendered documents: they exist only inside the
# Kyverno rule that writes them, so the template itself must be checked.
assert_rejected 'kyverno-generates-empty-cilium-policy' 'apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: generates-empty-default-deny
spec:
  rules:
    - name: generate-default-deny
      match:
        any:
          - resources:
              kinds: [Namespace]
      generate:
        apiVersion: cilium.io/v2
        kind: CiliumNetworkPolicy
        name: default-deny
        namespace: "{{request.object.metadata.name}}"
        data:
          spec:
            endpointSelector: {}
            ingress: []
            egress: []' 'require-generated-cilium-policy-rules'

assert_rejected 'kyverno-foreach-generates-empty-cilium-policy' 'apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: foreach-generates-empty-policy
spec:
  rules:
    - name: generate-per-entry
      match:
        any:
          - resources:
              kinds: [ConfigMap]
      generate:
        foreach:
          - list: request.object.data
            apiVersion: cilium.io/v2
            kind: CiliumNetworkPolicy
            name: "{{element}}"
            namespace: "{{request.object.metadata.namespace}}"
            data:
              spec:
                endpointSelector: {}' 'require-generated-cilium-policy-rules'

# One empty rule per direction is valid and allows nothing; a non-Cilium kind
# of the same name, a generated standard NetworkPolicy (where an absent rule
# list is the normal default-deny) and a clone without inline data stay out of
# scope.
assert_accepted 'cilium-default-deny-by-selection' 'apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata:
  name: default-deny
  namespace: example
spec:
  endpointSelector: {}
  ingress:
    - {}
  egress:
    - {}
  enableDefaultDeny:
    ingress: true
    egress: true
---
apiVersion: cilium.io/v2
kind: CiliumClusterwideNetworkPolicy
metadata:
  name: deny-only-with-intent
spec:
  endpointSelector: {}
  enableDefaultDeny:
    egress: false
  egressDeny:
    - toCIDR: [169.254.169.254/32]
---
apiVersion: example.com/v1
kind: CiliumNetworkPolicy
metadata:
  name: same-kind-other-group
  namespace: example
spec: {}
---
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: generates-valid-policies
spec:
  rules:
    - name: generate-cilium-default-deny
      match:
        any:
          - resources:
              kinds: [Namespace]
      generate:
        apiVersion: cilium.io/v2
        kind: CiliumNetworkPolicy
        name: default-deny
        namespace: "{{request.object.metadata.name}}"
        data:
          spec:
            endpointSelector: {}
            ingress:
              - {}
            egress:
              - {}
    - name: generate-standard-default-deny
      match:
        any:
          - resources:
              kinds: [Namespace]
      generate:
        apiVersion: networking.k8s.io/v1
        kind: NetworkPolicy
        name: default-deny
        namespace: "{{request.object.metadata.name}}"
        data:
          spec:
            podSelector: {}
            policyTypes: [Ingress, Egress]
    - name: clone-cilium-policy
      match:
        any:
          - resources:
              kinds: [Namespace]
      generate:
        apiVersion: cilium.io/v2
        kind: CiliumNetworkPolicy
        name: copied
        namespace: "{{request.object.metadata.name}}"
        clone:
          namespace: kube-system
          name: template'

# The real generator and the copy Flux applies in oauth2-proxy, as committed and
# with their directions emptied the way they shipped before #3501, so the rules
# are proven against the real documents' layout rather than only hand-written
# fixtures.
readonly default_deny_policy="${root_dir}/k8s/bases/infrastructure/cluster-policies/best-practices/add-default-deny.yaml"
readonly oauth2_proxy_default_deny="${root_dir}/k8s/bases/infrastructure/controllers/oauth2-proxy/cilium-network-policy-default-deny.yaml"
assert_accepted 'committed-default-deny' "$(cat "${default_deny_policy}")
---
$(cat "${oauth2_proxy_default_deny}")"
assert_rejected 'committed-default-deny-generator-emptied' "$(yq '
  (.spec.rules[] | select(.name == "generate-default-deny") | .generate.data.spec)
    |= (.ingress = [] | .egress = [])' "${default_deny_policy}")" 'require-generated-cilium-policy-rules'
assert_rejected 'committed-oauth2-proxy-default-deny-emptied' "$(yq '
  .spec.ingress = [] | .spec.egress = []' "${oauth2_proxy_default_deny}")" 'require-cilium-policy-rules'

printf 'PASS: effective authorization rules reject privilege paths and rule-less Cilium policies, and accept the data-product controller RBAC and valid Cilium policies\n'
