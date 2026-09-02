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

# A wildcard is not the only way to reach cluster-admin. `bind` and `escalate`
# on rbac.authorization.k8s.io are privilege-escalation primitives in their own
# right, and `create` on clusterrolebindings lets the controller mint a binding
# at runtime — an object that never passes through either chart validation.
# These fixtures carry no `*` at all, so they pass every wildcard check.

assert_rejected 'rbac-write-cluster-role' 'apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: data-product-controller
rules:
  - apiGroups: ["rbac.authorization.k8s.io"]
    resources: ["clusterrolebindings"]
    verbs: ["create"]'

assert_rejected 'rbac-bind-cluster-role' 'apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: data-product-controller
rules:
  - apiGroups: ["rbac.authorization.k8s.io"]
    resources: ["clusterroles"]
    verbs: ["bind"]'

assert_rejected 'escalate-verb-cluster-role' 'apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: data-product-controller
rules:
  - apiGroups: ["rbac.authorization.k8s.io"]
    resources: ["clusterroles"]
    verbs: ["escalate"]'

# The negative control for the three above: a read-only RBAC grant carries none
# of that power, so tightening the rule must not sweep it up.
assert_accepted 'rbac-read-cluster-role' 'apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: data-product-controller
rules:
  - apiGroups: ["rbac.authorization.k8s.io"]
    resources: ["clusterroles"]
    verbs: ["get", "list", "watch"]'

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

# A cluster-scoped binding that DECLARES the release namespace. The API server
# silently discards `metadata.namespace` on a cluster-scoped object, so this
# object reaches the cluster as an unreviewed cluster-admin grant to a subject
# in another namespace. It must be judged by kind — never accepted on the
# strength of a field that does not survive apply.
assert_rejected 'namespaced-clusterrolebinding' 'apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: totally-unreviewed-escalation
  namespace: data-product-controller
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
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

assert_rejected 'wildcard-verb-cluster-role' 'apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: data-product-controller
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["*"]'

assert_rejected 'aggregated-cluster-role' 'apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: data-product-controller
aggregationRule:
  clusterRoleSelectors:
    - matchLabels:
        rbac.authorization.k8s.io/aggregate-to-admin: "true"
rules: []'

assert_accepted 'local-subject-role-binding' 'apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: data-product-controller-leader-election
  namespace: data-product-controller
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: data-product-controller-leader-election
subjects:
  - kind: ServiceAccount
    name: data-product-controller
    namespace: data-product-controller'

assert_rejected 'foreign-subject-role-binding' 'apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: data-product-controller-edit
  namespace: data-product-controller
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: edit
subjects:
  - kind: ServiceAccount
    name: kustomize-controller
    namespace: flux-system'

assert_rejected 'subjectless-role-binding' 'apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: data-product-controller-edit
  namespace: data-product-controller
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: data-product-controller-leader-election'

# A Kustomization that OMITS targetNamespace is not thereby namespace-local.
# Flux preserves whatever namespaces the remote artifact declares, so this
# reconciles grandchildren into `aws`, `flux-system` or cluster scope — and
# none of those objects is ever presented to this rule. Sharing the reviewed
# name is what previously carried it past the emitter exception.
assert_rejected 'same-name-kustomization-without-target-namespace' 'apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: data-product-controller
  namespace: data-product-controller
spec:
  interval: 10m
  prune: false
  sourceRef:
    kind: OCIRepository
    name: data-product-controller'

# The negative control: an explicit, correct targetNamespace is what the
# emitter exception is actually for, and must keep passing.
assert_accepted 'same-name-kustomization-with-target-namespace' 'apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: data-product-controller
  namespace: data-product-controller
spec:
  interval: 10m
  prune: false
  sourceRef:
    kind: OCIRepository
    name: data-product-controller
  targetNamespace: data-product-controller'

assert_rejected 'namespaced-flux-kustomization' 'apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: nested
  namespace: data-product-controller
spec:
  interval: 10m
  prune: false
  sourceRef:
    kind: OCIRepository
    name: nested
  targetNamespace: aws'

assert_rejected 'namespaced-helm-release' 'apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: nested
  namespace: data-product-controller
spec:
  interval: 10m
  targetNamespace: flux-system
  chartRef:
    kind: OCIRepository
    name: nested'

assert_accepted 'reviewed-name-emitter-local' 'apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: data-product-controller
  namespace: data-product-controller
spec:
  interval: 10m
  chartRef:
    kind: OCIRepository
    name: data-product-controller'

assert_rejected 'reviewed-name-emitter-redirecting' 'apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: data-product-controller
  namespace: data-product-controller
spec:
  interval: 10m
  targetNamespace: flux-system
  chartRef:
    kind: OCIRepository
    name: data-product-controller'

# A reviewed emitter must not take VALUE INPUTS THIS RENDER CANNOT SEE. The
# proof below is a filesystem render of the pinned artifact: it resolves chart
# defaults plus the inline `spec.values` carried in this manifest, and every
# child it produces is evaluated by the rule above. `spec.valuesFrom` points at
# a Secret or ConfigMap materialized only in the cluster, so Flux renders a
# DIFFERENT value set from the one proved here — and this component ships an
# ExternalSecret, so such a Secret genuinely exists. Values decide which
# templates render at all, so a runtime value can enable RBAC or a
# foreign-namespace child that neither this render nor the rule ever inspected.
# The component is staged off every deploy overlay, so cluster admission never
# re-checks it: this render is the ONLY control standing over these manifests,
# and it must therefore be proof about what is actually installed.
#
assert_rejected 'reviewed-emitter-runtime-values-from' 'apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: data-product-controller
  namespace: data-product-controller
spec:
  interval: 10m
  chartRef:
    kind: OCIRepository
    name: data-product-controller
  valuesFrom:
    - kind: Secret
      name: data-product-controller-runtime-values'

# Negative control: the reviewed chart REALLY USES postRenderers, to add
# imagePullSecrets and topology constraints to its own Deployments. Refusing
# runtime value sources must not take this shape with it — an earlier draft of
# the clause above refused `postRenderers` too and rejected the live component,
# which only the pinned render at the end of this file caught. Whether a
# post-render patch can move a child across namespaces after the rule has
# accepted it is a real question, but it needs its own proof, not a blanket
# refusal of the mechanism the component depends on.
assert_accepted 'reviewed-emitter-post-renderer-local' 'apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: data-product-controller
  namespace: data-product-controller
spec:
  interval: 10m
  chartRef:
    kind: OCIRepository
    name: data-product-controller
  postRenderers:
    - kustomize:
        patches:
          - target:
              kind: Deployment
              name: data-product-controller
            patch: |
              - op: add
                path: /spec/template/spec/imagePullSecrets
                value:
                  - name: ghcr-auth'


# Negative control: INLINE values are the mechanism the reviewed chart actually
# uses, and this render can see them, so the clause above must not refuse them.
# Without this control a blanket refusal of value configuration would satisfy
# every other assertion in this file while breaking the real component.
assert_accepted 'reviewed-emitter-inline-values' 'apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: data-product-controller
  namespace: data-product-controller
spec:
  interval: 10m
  chartRef:
    kind: OCIRepository
    name: data-product-controller
  values:
    controller:
      replicas: 2'

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
