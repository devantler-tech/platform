#!/usr/bin/env bash

# Proves that the isolated data-product-controller chart cannot render a child
# into another namespace while retaining the EKS authorization exemption.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly rules_path="${root_dir}/scripts/tests/isolated-chart-namespace-rules.yaml"
readonly prod_rules_path="${root_dir}/scripts/tests/production-authorization-rules.yaml"
readonly component_path="${root_dir}/k8s/bases/apps/data-product-controller"

for tool in helm jq kubectl ksail yq; do
  command -v "${tool}" >/dev/null || {
    printf 'FAIL: %s is required\n' "${tool}" >&2
    exit 1
  }
done

test_root="$(mktemp -d /tmp/isolated-chart-namespace-rules.XXXXXX)"
readonly test_root
# cleanup removes the temporary fixtures created by this invocation.
cleanup() {
  rm -rf "${test_root}"
}
trap cleanup EXIT

# run_fixture validates one authored fixture against the isolated-chart rules.
run_fixture() {
  local path="$1"
  ksail --config "${root_dir}/ksail.prod.yaml" workload validate "${path}" \
    --skip-helm-render \
    --rules "${rules_path}" 2>&1
}

# assert_accepted requires a namespace-local fixture to pass validation.
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

# assert_rejected requires the rendered-child namespace rule to reject a fixture.
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

# A correct targetNamespace is NOT sufficient, and this fixture is why the kind
# is refused outright rather than conditioned. targetNamespace relocates the
# NAMESPACED grandchildren a source renders; a ClusterRoleBinding in that same
# source still reconciles cluster-wide, and no grandchild is ever presented to
# this rule. The reviewed render contains no Kustomization at all, so refusing
# the kind costs nothing real and is the only bound this rule can enforce.
assert_rejected 'same-name-kustomization-with-target-namespace' 'apiVersion: kustomize.toolkit.fluxcd.io/v1
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

# A CLUSTER-SCOPED KIND OUTSIDE THE REVIEWED SET CANNOT BE CONTAINED BY A
# NAMESPACE FIELD. The API server accepts and then ignores `metadata.namespace`
# on every cluster-scoped object, so before the kind allowlist these two
# satisfied the namespaced branch on a field that does not survive apply and
# landed cluster-wide. Enumerating cluster-scoped kinds to refuse cannot close
# this — the next unlisted kind passes exactly as these did — so the branch
# names the kinds the pinned render actually produces and refuses the rest.
assert_rejected 'unreviewed-cluster-scoped-crd' 'apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: evilthings.example.invalid
  namespace: data-product-controller
spec:
  group: example.invalid
  names:
    kind: EvilThing
    plural: evilthings
  scope: Cluster
  versions: []'

assert_rejected 'unreviewed-mutating-webhook' 'apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: hijack-everything
  namespace: data-product-controller
webhooks: []'

# The chart installs only its own namespaced description API. A same-name CRD
# must not introduce a cluster-scoped product or an external conversion hook.
reviewed_crd='apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: dataproducts.data.devantler.tech
spec:
  group: data.devantler.tech
  scope: Namespaced
  names:
    kind: DataProduct
    plural: dataproducts
  versions:
    - name: v1alpha1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object'
assert_accepted 'reviewed-product-crd' "${reviewed_crd}"
assert_rejected 'cluster-scoped-product-crd' "${reviewed_crd/Namespaced/Cluster}"
assert_rejected 'conversion-webhook-product-crd' "${reviewed_crd}
  conversion:
    strategy: Webhook"

# A namespaced kind the reviewed render does not produce is refused by the same
# allowlist. This is the control proving the clause above is a KIND test and not
# a cluster-scope test: nothing about a ConfigMap escapes the namespace, and it
# is refused anyway, because an unreviewed kind in a digest bump is exactly what
# the containment bound requires someone to read before it ships.
assert_rejected 'unreviewed-namespaced-kind' 'apiVersion: v1
kind: ConfigMap
metadata:
  name: data-product-controller
  namespace: data-product-controller
data:
  key: value'

# THE RENDERED Namespace OVERWRITES THE REVIEWED ONE ON APPLY, so accepting it
# by name alone let a future digest ship a weaker enforcement label — and every
# workload in the isolated namespace would then run unrestricted while this rule
# stayed green. The component is staged off every deploy overlay, so no cluster
# admission control re-checks it afterwards.
assert_rejected 'privileged-rendered-namespace' 'apiVersion: v1
kind: Namespace
metadata:
  name: data-product-controller
  labels:
    pod-security.kubernetes.io/enforce: privileged'

# Omitting the label is the same admission outcome by another route, so it fails
# closed rather than being read as "unchanged".
assert_rejected 'unlabelled-rendered-namespace' 'apiVersion: v1
kind: Namespace
metadata:
  name: data-product-controller'

# The negative control for both: the reviewed namespace, exactly as the base
# declares it, must keep passing. Without this a blanket refusal of rendered
# Namespaces would satisfy the two assertions above while rejecting the real
# component.
assert_accepted 'reviewed-restricted-namespace' 'apiVersion: v1
kind: Namespace
metadata:
  name: data-product-controller
  labels:
    pod-security.kubernetes.io/enforce: restricted
    pod-security.kubernetes.io/enforce-version: latest'

# Fetch and render explicitly: KSail can report success after skipping a failed
# Helm render. An immutable artifact fetch or a Flux post-render failure must
# stop this check, rather than validate only the authored HelmRelease.
release="${component_path}/helm-release.yaml"
source="${component_path}/oci-repository.yaml"
# Flux substitutes this reviewed public ConfigMap before Helm sees either values
# or post-renderer patches. Compile only its domain variable here; never import
# ambient environment variables or runtime Secret inputs into the fixture.
prod_domain="$(yq -o=json '.data.domain' "${root_dir}/k8s/clusters/prod/bootstrap/config-map.yaml" |
  jq -ser '
    if length == 1 and (.[0] | type == "string" and length <= 253 and
      test("^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$"))
    then .[0] else error("invalid production bootstrap domain") end
  ')"
readonly prod_domain
readonly harbour_origin="https://harbour-data.${prod_domain}"
readonly registry_origin="https://data-products.${prod_domain}"
readonly kit_origin="https://product-ui.${prod_domain}"

# substitute_prod_domain supports the fixture's one reviewed Flux expression.
# Unbraced Kustomize $patch directives survive; unresolved, malformed or newly
# introduced braced variables fail closed instead of becoming empty strings.
substitute_prod_domain() {
  jq -se --arg domain "${prod_domain}" '
    if length == 1 and (.[0] | type == "object") then .[0]
    else error("expected one HelmRelease object") end |
    walk(if type == "string" then gsub("\\$\\{domain\\}"; $domain) else . end) |
    if any(.. | strings; contains("${"))
    then error("unresolved or unsupported Flux variable") else . end
  ' "$1"
}

url="$(yq -r '.spec.url' "${source}")"
digest="$(yq -r '.spec.ref.digest' "${source}")"
[[ "${digest}" =~ ^sha256:[a-f0-9]{64}$ ]] || {
  printf 'FAIL: chart source must carry an immutable sha256 digest\n' >&2
  exit 1
}
render_dir="${test_root}/render"
mkdir -p "${render_dir}"
# This chart is public. Keep the check independent of host credential helpers.
printf '{"auths":{}}\n' >"${test_root}/registry.json"
helm pull "${url}@${digest}" --registry-config "${test_root}/registry.json" \
  --destination "${render_dir}"
charts=("${render_dir}"/*.tgz)
[[ "${#charts[@]}" == 1 && -f "${charts[0]}" ]] || {
  printf 'FAIL: immutable chart fetch must produce exactly one archive\n' >&2
  exit 1
}
release_name="$(yq -r '.spec.releaseName // .metadata.name' "${release}")"
yq -o=json '.' "${release}" >"${render_dir}/release-unsubstituted.json"
substitute_prod_domain "${render_dir}/release-unsubstituted.json" >"${render_dir}/release.json"

# Exercise the actual input trees independently: neither values nor patches may
# silently retain an unknown variable, malformed token or unsupported expansion.
for scope in values postRenderers; do
  for token in "\${unreviewed_domain}" "\${domain" "\${domain:-example.invalid}"; do
    jq --arg scope "${scope}" --arg token "${token}" '
      .spec[$scope] |= walk(if type == "string"
        then gsub("\\$\\{domain\\}"; $token) else . end)
    ' "${render_dir}/release-unsubstituted.json" >"${test_root}/unresolved-release.json"
    if substitute_prod_domain "${test_root}/unresolved-release.json" \
      >"${test_root}/unresolved-result.json" 2>"${test_root}/unresolved.log"; then
      printf 'FAIL: %s accepted an unresolved or malformed Flux expression\n' "${scope}" >&2
      exit 1
    fi
    grep -qF 'unresolved or unsupported Flux variable' "${test_root}/unresolved.log" || {
      printf 'FAIL: %s substitution failed without identifying its unresolved expression\n' "${scope}" >&2
      exit 1
    }
  done
done

jq '.spec.values' "${render_dir}/release.json" >"${render_dir}/values.json"
helm template "${release_name}" "${charts[0]}" --namespace data-product-controller \
  --include-crds --values "${render_dir}/values.json" >"${render_dir}/resources.yaml"

renderer_count="$(jq '.spec.postRenderers | length' "${render_dir}/release.json")"
for ((index = 0; index < renderer_count; index++)); do
  jq ".spec.postRenderers[${index}].kustomize" "${render_dir}/release.json" >"${render_dir}/renderer.json"
  jq -n --slurpfile renderer "${render_dir}/renderer.json" \
    '{apiVersion: "kustomize.config.k8s.io/v1beta1", kind: "Kustomization",
      resources: ["resources.yaml"]} + $renderer[0]' >"${render_dir}/kustomization.yaml"
  kubectl kustomize "${render_dir}" >"${render_dir}/next.yaml"
  mv "${render_dir}/next.yaml" "${render_dir}/resources.yaml"
done

# Literal inventory prevents an empty render, CRD omission, or a lost workload
# from masquerading as containment. New chart kinds require explicit review.
yq ea -o=json '[.]' "${render_dir}/resources.yaml" | jq -e '
  [.[] | [.kind, .metadata.name]] | sort == ([
    ["CustomResourceDefinition", "dataproducts.data.devantler.tech"],
    ["ClusterRole", "data-product-controller"],
    ["ClusterRoleBinding", "data-product-controller"],
    ["ServiceAccount", "data-product-controller"],
    ["Role", "data-product-controller-leader-election"],
    ["RoleBinding", "data-product-controller-leader-election"],
    ["Service", "data-product-controller"],
    ["Service", "data-product-controller-harbour"],
    ["Deployment", "data-product-controller"],
    ["Deployment", "data-product-controller-harbour"],
    ["DataProduct", "harbour-observations"]
  ] | sort)
' >/dev/null || {
  printf 'FAIL: pinned chart child inventory is incomplete or unreviewed\n' >&2
  exit 1
}

# Product endpoints remain outside the registry's SSO origin and use root paths
# matching the sample server's independently published OpenAPI contract.
yq ea -o=json '[.]' "${render_dir}/resources.yaml" | jq -e --arg harbour "${harbour_origin}" '
  [.[] | select(.kind == "DataProduct" and .metadata.name == "harbour-observations") | (
    .spec.id == $harbour and
    .spec.outputs[0].url == ($harbour + "/api/observations") and
    .spec.outputs[0].contractUrl == ($harbour + "/openapi.json") and
    .spec.ui.url == ($harbour + "/ui")
  )] == [true]
' >/dev/null || {
  printf 'FAIL: rendered sample descriptor must use independently served root endpoints\n' >&2
  exit 1
}

# Both independent workloads must enact the bounded contract, rather than merely
# accepting values that an older chart ignores. The sample owns its publication
# URL; the authenticated registry and portable UI host may supply cosmetic hints.
yq ea -o=json '[.]' "${render_dir}/resources.yaml" >"${render_dir}/resources.json"
image_digest="$(yq -r '.spec.values.image.digest' "${release}")"
[[ "${image_digest}" =~ ^sha256:[a-f0-9]{64}$ ]] || {
  printf 'FAIL: trial image must carry an immutable sha256 digest\n' >&2
  exit 1
}
# trial_contract_matches checks the standard registry listener and complete
# rendered presentation contract, publication endpoints, host, admission rule
# and the exact controller-local leader-election Event grant.
trial_contract_matches() {
  jq -e --arg image "ghcr.io/devantler-tech/data-product-controller@${image_digest}" \
    --arg harbour "${harbour_origin}" \
    --arg registry "${registry_origin}" --arg kit "${kit_origin}" \
    --arg cel_rule "self.apiVersion != 'data-product-ui/v1' || !self.capabilities.exists(c, c == 'appearance')" '
      ([$registry,$kit] | sort) as $host_origins |
      ([.[] | select(.kind == "Deployment" and .metadata.name == "data-product-controller") |
        .spec.template.spec.serviceAccountName]) as $accounts |
      $accounts == ["data-product-controller"] and
      [.[] | select(.kind == "Role" and .metadata.name == "data-product-controller-leader-election") |
        (.metadata.namespace == "data-product-controller" and
         [.rules[] | select(.resources | index("events"))] ==
           [{apiGroups:[""],resources:["events"],verbs:["create","patch"]}])] == [true] and
      [.[] | select(.kind == "RoleBinding" and .metadata.name == "data-product-controller-leader-election") |
        (.metadata.namespace == "data-product-controller" and
         .roleRef == {apiGroup:"rbac.authorization.k8s.io",kind:"Role",name:"data-product-controller-leader-election"} and
         .subjects == [{kind:"ServiceAccount",name:$accounts[0],namespace:"data-product-controller"}])] == [true] and
      [.[] | select(.kind == "ClusterRole") | .rules[] |
        select(.resources | index("events"))] == [] and
      ([.[] | select(.kind == "Deployment") | .metadata.name] | sort) ==
        ["data-product-controller", "data-product-controller-harbour"] and
      [.[] | select(.kind == "Deployment") | .spec.template.spec.containers[0] |
        (.image == $image and
         ([.env[]? | select(.name == "REGISTRY_UI_ENABLED")] == []) and
         ([.env[] | select(.name == "UI_CONTRACT_ENABLED") | .value] == ["true"]) and
         ([.env[] | select(.name == "UI_APPEARANCE_ENABLED") | .value] == ["true"]))] == [true,true] and
      [.[] | select(.kind == "Service" and .metadata.name == "data-product-controller") |
        (.spec.selector["app.kubernetes.io/component"] == "controller" and
         [.spec.ports[] | select(.name == "http") | [.port,.targetPort]] == [[80,"registry"]])] == [true] and
      [.[] | select(.kind == "Deployment" and .metadata.name == "data-product-controller") |
        [.spec.template.spec.containers[0].ports[] | select(.name == "registry") | .containerPort]] == [[8082]] and
      [.[] | select(.kind == "Deployment" and .metadata.name == "data-product-controller-harbour") |
        .spec.template.spec.containers[0].env |
        ([.[] | select(.name == "PUBLIC_BASE_URL") | .value] == [$harbour] and
         [.[] | select(.name == "UI_HOST_ORIGINS") |
           (.value | split(",") | sort) == $host_origins] == [true])] == [true] and
      [.[] | select(.kind == "DataProduct" and .metadata.name == "harbour-observations") |
        (.spec.ui.contract | .hostOrigins |= sort) == {"apiVersion":"data-product-ui/v2",
          "hostOrigins":$host_origins,
          "capabilities":["status","resize","appearance"]}] == [true] and
      [.[] | select(.kind == "CustomResourceDefinition" and .metadata.name == "dataproducts.data.devantler.tech") |
        .spec.versions[] | select(.name == "v1alpha1") |
        .schema.openAPIV3Schema.properties.spec.properties.ui.properties.contract |
        (.properties.apiVersion.enum == ["data-product-ui/v1","data-product-ui/v2"] and
         .properties.capabilities.items.enum == ["status","resize","appearance"] and
         .properties.capabilities.maxItems == 3 and
         ."x-kubernetes-validations" == [{message:"Appearance requires data-product-ui/v2",rule:$cel_rule}])] == [true]
    ' "$1" >/dev/null
}
trial_contract_matches "${render_dir}/resources.json" || {
  printf 'FAIL: rendered trial must retain local Event writes, the default registry, appearance gates, immutable images and the bounded approved-origin contract\n' >&2
  exit 1
}

# Negative controls operate on the actual signed chart children and prove that a
# lost listener, retired flag, lost gate, publication mismatch, inappropriate host
# or disabled admission rule fails this assertion. Event writes must also retain
# their required verbs and single, namespace-local controller recipient.
for broken in listener service retired-flag gate publication origin missing-portable-origin origin-desynchronization admission event-grant event-verbs event-subject; do
  case "${broken}" in
  event-grant) jq 'map(if .kind == "Role" then
      .rules |= map(select((.resources | index("events")) == null)) else . end)' \
    "${render_dir}/resources.json" >"${test_root}/appearance-${broken}.json" ;;
  event-verbs) jq 'map(if .kind == "Role" then
      (.rules[] | select(.resources | index("events")) | .verbs) += ["delete"] else . end)' \
    "${render_dir}/resources.json" >"${test_root}/appearance-${broken}.json" ;;
  event-subject) jq 'map(if .kind == "RoleBinding" then
      .subjects += [{kind:"User",name:"other-user"}] else . end)' \
    "${render_dir}/resources.json" >"${test_root}/appearance-${broken}.json" ;;
  listener) jq 'map(if .kind == "Deployment" and .metadata.name == "data-product-controller" then
      (.spec.template.spec.containers[0].ports[] | select(.name == "registry") | .containerPort) = 8083
      else . end)' "${render_dir}/resources.json" >"${test_root}/appearance-${broken}.json" ;;
  service) jq 'map(if .kind == "Service" and .metadata.name == "data-product-controller" then
      (.spec.ports[] | select(.name == "http") | .targetPort) = "wrong-port"
      else . end)' "${render_dir}/resources.json" >"${test_root}/appearance-${broken}.json" ;;
  retired-flag) jq 'map(if .kind == "Deployment" and .metadata.name == "data-product-controller" then
      .spec.template.spec.containers[0].env += [{name:"REGISTRY_UI_ENABLED",value:"false"}]
      else . end)' "${render_dir}/resources.json" >"${test_root}/appearance-${broken}.json" ;;
  admission) jq 'map(if .kind == "CustomResourceDefinition" and .metadata.name == "dataproducts.data.devantler.tech" then
      (.spec.versions[] | select(.name == "v1alpha1") |
       .schema.openAPIV3Schema.properties.spec.properties.ui.properties.contract."x-kubernetes-validations"[].rule) = "true"
      else . end)' "${render_dir}/resources.json" >"${test_root}/appearance-${broken}.json" ;;
  gate) jq 'map(if .kind == "Deployment" then
      (.spec.template.spec.containers[0].env[] | select(.name == "UI_APPEARANCE_ENABLED") | .value) = "false"
      else . end)' "${render_dir}/resources.json" >"${test_root}/appearance-${broken}.json" ;;
  publication) jq --arg registry "${registry_origin}" 'map(if .kind == "Deployment" and .metadata.name == "data-product-controller-harbour" then
      (.spec.template.spec.containers[0].env[] | select(.name == "PUBLIC_BASE_URL") | .value) = $registry
      else . end)' "${render_dir}/resources.json" >"${test_root}/appearance-${broken}.json" ;;
  origin) jq --arg harbour "${harbour_origin}" 'map(if .kind == "DataProduct" then
      .spec.ui.contract.hostOrigins = [$harbour] else . end)' \
    "${render_dir}/resources.json" >"${test_root}/appearance-${broken}.json" ;;
  missing-portable-origin) jq --arg registry "${registry_origin}" 'map(if .kind == "DataProduct" then
      .spec.ui.contract.hostOrigins = [$registry] else . end)' \
    "${render_dir}/resources.json" >"${test_root}/appearance-${broken}.json" ;;
  origin-desynchronization) jq --arg registry "${registry_origin}" 'map(if .kind == "Deployment" and .metadata.name == "data-product-controller-harbour" then
      (.spec.template.spec.containers[0].env[] | select(.name == "UI_HOST_ORIGINS") | .value) = $registry
      else . end)' "${render_dir}/resources.json" >"${test_root}/appearance-${broken}.json" ;;
  esac
  if trial_contract_matches "${test_root}/appearance-${broken}.json"; then
    printf 'FAIL: actual trial render accepted broken workspace contract %s\n' "${broken}" >&2
    exit 1
  fi
done

# Evaluate the actual Deployment children with the policy that admits them in
# production. Pod-level security defaults alone do not satisfy its container
# pattern, so namespace containment and Kubernetes schema checks are insufficient.
if ! kyverno apply \
  "${root_dir}/k8s/bases/infrastructure/cluster-policies/best-practices/validate-pod-security.yaml" \
  --resource "${render_dir}/resources.yaml" --detailed-results \
  >"${test_root}/pod-security.log" 2>&1; then
  cat "${test_root}/pod-security.log" >&2
  printf 'FAIL: actual chart workloads violate production pod security\n' >&2
  exit 1
fi
# This exact summary format is a contract with the shared pinned Kyverno CLI
# from .github/scripts/kyverno-version.sh. Review the assertion when upgrading
# that CLI; unknown formats must not silently count unevaluated rules as green.
grep -qF 'pass: 6, fail: 0, warn: 0, error: 0, skip: 0' "${test_root}/pod-security.log" || {
  cat "${test_root}/pod-security.log" >&2
  printf 'FAIL: all three pod-security rules must evaluate both actual Deployments\n' >&2
  exit 1
}

# Removing the container assertion from a real rendered child must fail the
# named admission rule, even though its Pod still declares runAsNonRoot=true.
yq 'select(.kind == "Deployment" and .metadata.name == "data-product-controller") |
  del(.spec.template.spec.containers[0].securityContext.runAsNonRoot)' \
  "${render_dir}/resources.yaml" >"${test_root}/pod-only-security.yaml"
if kyverno apply \
  "${root_dir}/k8s/bases/infrastructure/cluster-policies/best-practices/validate-pod-security.yaml" \
  --resource "${test_root}/pod-only-security.yaml" --detailed-results \
  >"${test_root}/pod-only-security.log" 2>&1; then
  printf 'FAIL: a real chart workload lost its required container security assertion\n' >&2
  exit 1
fi
grep -qF 'autogen-validate-container-security failed at path /spec/template/spec/containers/0/securityContext/runAsNonRoot/' \
  "${test_root}/pod-only-security.log" || {
  cat "${test_root}/pod-only-security.log" >&2
  printf 'FAIL: container security rejection did not identify the production admission rule\n' >&2
  exit 1
}

# Check both the authored component and the final chart children, including
# CRDs and patches, against the same two suites used by production CI.
kubectl kustomize "${component_path}" >"${test_root}/authored.yaml"
for path in "${test_root}/authored.yaml" "${render_dir}/resources.yaml"; do
  for suite in "${rules_path}" "${prod_rules_path}"; do
    if ! output="$(ksail --config "${root_dir}/ksail.prod.yaml" workload validate "${path}" \
      --skip-helm-render --rules "${suite}" 2>&1)"; then
      printf 'FAIL: %s failed %s\n%s\n' "${path}" "${suite}" "${output}" >&2
      exit 1
    fi
  done
done

# Mutate an actual post-rendered child, not a handwritten fixture. The same
# namespace rule must refuse it by name, then accept the unchanged render.
yq 'select(.kind == "Deployment" and .metadata.name == "data-product-controller") |
  .metadata.namespace = "foreign-namespace"' "${render_dir}/resources.yaml" \
  >"${test_root}/foreign-rendered-child.yaml"
if output="$(run_fixture "${test_root}/foreign-rendered-child.yaml")"; then
  printf 'FAIL: an actual rendered child escaped its namespace\n%s\n' "${output}" >&2
  exit 1
fi
printf '%s\n' "${output}" | grep -qF \
  'rule "restrict-data-product-controller-rendered-child-namespaces"' || {
  printf 'FAIL: actual child rejection did not identify the namespace rule\n%s\n' "${output}" >&2
  exit 1
}
run_fixture "${render_dir}/resources.yaml" >"${test_root}/restored.log"
printf 'PASS: immutable chart inventory and Flux patches pass both suites; actual foreign child is rejected by the namespace rule\n'
