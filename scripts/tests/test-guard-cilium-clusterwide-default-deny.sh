#!/usr/bin/env bash
#
# Pins guard-cilium-clusterwide-default-deny.sh in all three directions:
# a silent default-deny is refused (1), a stated intent passes (0), and anything
# the guard cannot decide — including finding no clusterwide policy — is 2.
# The RED case is the exact policy shape that took prod DNS down (#3404, #3407).

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly guard="${root_dir}/scripts/guard-cilium-clusterwide-default-deny.sh"

tmp_dir="$(mktemp -d)"
readonly tmp_dir
trap 'rm -rf "${tmp_dir}"' EXIT

assertions=0
failures=0

# expect <name> <expected-exit> <stderr-substring-or-empty> <file>...
expect() {
  local name="$1" want="$2" needle="$3"
  shift 3
  local got=0
  bash "$guard" --rendered "$@" >"${tmp_dir}/out" 2>"${tmp_dir}/err" || got=$?
  assertions=$((assertions + 1))
  if [ "$got" -ne "$want" ]; then
    failures=$((failures + 1))
    printf 'FAIL %s: exit %s, want %s\n' "$name" "$got" "$want" >&2
    sed 's/^/  | /' "${tmp_dir}/err" >&2
    return 0
  fi
  if [ -n "$needle" ]; then
    assertions=$((assertions + 1))
    if ! grep -qF -- "$needle" "${tmp_dir}/err"; then
      failures=$((failures + 1))
      printf 'FAIL %s: stderr does not contain %s\n' "$name" "$needle" >&2
      sed 's/^/  | /' "${tmp_dir}/err" >&2
    fi
  fi
}

fixture() {
  local path="${tmp_dir}/$1.yaml"
  cat >"$path"
  printf '%s' "$path"
}

# RED: the pre-#3407 shape — an egressDeny, no egress allow, no enableDefaultDeny.
pre_3407="$(fixture pre-3407 <<'EOF'
apiVersion: cilium.io/v2
kind: CiliumClusterwideNetworkPolicy
metadata:
  name: deny-workload-instance-metadata-egress
spec:
  endpointSelector:
    matchExpressions:
      - key: k8s:io.kubernetes.pod.namespace
        operator: NotIn
        values: [crossplane-system]
  egressDeny:
    - toCIDR: [169.254.169.254/32]
EOF
)"
expect 'pre-#3407 policy is refused' 1 'deny-workload-instance-metadata-egress spec: egressDeny rules with no egress allow rules' "$pre_3407"
expect 'the refusal says what to do' 1 'egress: false' "$pre_3407"

explicit_false="$(fixture explicit-false <<'EOF'
apiVersion: cilium.io/v2
kind: CiliumClusterwideNetworkPolicy
metadata: {name: subtract-one}
spec:
  endpointSelector: {}
  enableDefaultDeny: {egress: false}
  egressDeny:
    - toCIDR: [169.254.169.254/32]
EOF
)"
expect 'explicit enableDefaultDeny false passes' 0 '' "$explicit_false"

# A null is pruned by the API server, so Cilium applies its default (true): only a
# real boolean states the intent.
null_value="$(fixture null-value <<'EOF'
apiVersion: cilium.io/v2
kind: CiliumClusterwideNetworkPolicy
metadata: {name: null-intent}
spec:
  endpointSelector: {}
  enableDefaultDeny: {egress: null}
  egressDeny:
    - toCIDR: [169.254.169.254/32]
EOF
)"
expect 'a null enableDefaultDeny direction is refused' 1 'null-intent spec: egressDeny' "$null_value"

string_value="$(fixture string-value <<'EOF'
apiVersion: cilium.io/v2
kind: CiliumClusterwideNetworkPolicy
metadata: {name: string-intent}
spec:
  endpointSelector: {}
  enableDefaultDeny: {egress: "false"}
  egressDeny:
    - toCIDR: [169.254.169.254/32]
EOF
)"
expect 'a non-boolean enableDefaultDeny direction is refused' 1 'string-intent spec: egressDeny' "$string_value"

explicit_true="$(fixture explicit-true <<'EOF'
apiVersion: cilium.io/v2
kind: CiliumClusterwideNetworkPolicy
metadata: {name: lock-down}
spec:
  endpointSelector: {}
  enableDefaultDeny: {egress: true}
  egressDeny:
    - toCIDR: [169.254.169.254/32]
EOF
)"
expect 'explicit enableDefaultDeny true passes' 0 '' "$explicit_true"

with_allow="$(fixture with-allow <<'EOF'
apiVersion: cilium.io/v2
kind: CiliumClusterwideNetworkPolicy
metadata: {name: allow-list}
spec:
  endpointSelector: {}
  egress:
    - toEntities: [kube-apiserver]
  egressDeny:
    - toCIDR: [169.254.169.254/32]
EOF
)"
expect 'deny alongside allow rules is ordinary allow-list semantics' 0 '' "$with_allow"

wrong_direction="$(fixture wrong-direction <<'EOF'
apiVersion: cilium.io/v2
kind: CiliumClusterwideNetworkPolicy
metadata: {name: wrong-direction}
spec:
  endpointSelector: {}
  enableDefaultDeny: {ingress: false}
  egressDeny:
    - toCIDR: [169.254.169.254/32]
EOF
)"
expect 'enableDefaultDeny for the OTHER direction does not count' 1 'no enableDefaultDeny.egress' "$wrong_direction"

ingress_only="$(fixture ingress-only <<'EOF'
apiVersion: cilium.io/v2
kind: CiliumClusterwideNetworkPolicy
metadata: {name: ingress-deny}
spec:
  endpointSelector: {}
  ingressDeny:
    - fromEntities: [world]
EOF
)"
expect 'deny-only ingress is refused and named' 1 'ingressDeny rules with no ingress allow rules' "$ingress_only"

multi_spec="$(fixture multi-spec <<'EOF'
apiVersion: cilium.io/v2
kind: CiliumClusterwideNetworkPolicy
metadata: {name: multi}
specs:
  - endpointSelector: {}
    enableDefaultDeny: {egress: false}
    egressDeny:
      - toCIDR: [169.254.169.254/32]
  - endpointSelector: {}
    egressDeny:
      - toCIDR: [169.254.169.254/32]
EOF
)"
expect 'one unguarded spec among guarded ones is refused by index' 1 'CiliumClusterwideNetworkPolicy/multi specs[1]: egressDeny' "$multi_spec"

both_fields="$(fixture both-fields <<'EOF'
apiVersion: cilium.io/v2
kind: CiliumClusterwideNetworkPolicy
metadata: {name: both}
spec:
  endpointSelector: {}
  egressDeny:
    - toCIDR: [169.254.169.254/32]
specs:
  - endpointSelector: {}
    enableDefaultDeny: {egress: false}
    egressDeny:
      - toCIDR: [169.254.169.254/32]
EOF
)"
expect 'a deny-only spec beside compliant specs is still checked (Cilium applies both)' 1 'CiliumClusterwideNetworkPolicy/both spec: egressDeny' "$both_fields"

multi_doc="$(fixture multi-doc <<'EOF'
apiVersion: cilium.io/v2
kind: CiliumClusterwideNetworkPolicy
metadata: {name: good}
spec:
  endpointSelector: {}
  enableDefaultDeny: {egress: false}
  egressDeny:
    - toCIDR: [169.254.169.254/32]
---
apiVersion: cilium.io/v2
kind: CiliumClusterwideNetworkPolicy
metadata: {name: bad}
spec:
  endpointSelector: {}
  egressDeny:
    - toCIDR: [169.254.169.254/32]
EOF
)"
expect 'a bad policy in the second document of a stream is found' 1 'CiliumClusterwideNetworkPolicy/bad spec:' "$multi_doc"
expect 'every rendered file is checked, not just the first' 1 'CiliumClusterwideNetworkPolicy/bad' "$explicit_false" "$multi_doc"

namespaced="$(fixture namespaced <<'EOF'
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata: {name: namespaced, namespace: demo}
spec:
  endpointSelector: {}
  egressDeny:
    - toCIDR: [169.254.169.254/32]
EOF
)"
expect 'no clusterwide policy at all is cannot-check' 2 'nothing was verified' "$namespaced"
expect 'namespaced policies are out of scope beside a checked clusterwide one' 0 '' "$explicit_false" "$namespaced"

other_group="$(fixture other-group <<'EOF'
apiVersion: example.com/v1
kind: CiliumClusterwideNetworkPolicy
metadata: {name: not-cilium}
spec:
  egressDeny:
    - toCIDR: [169.254.169.254/32]
EOF
)"
expect 'a same-named kind from another API group is not Cilium' 0 '' "$explicit_false" "$other_group"

no_api="$(fixture no-api <<'EOF'
kind: CiliumClusterwideNetworkPolicy
metadata: {name: no-api}
spec:
  endpointSelector: {}
  egressDeny:
    - toCIDR: [169.254.169.254/32]
EOF
)"
expect 'a missing apiVersion is cannot-check, never skipped' 2 'CiliumClusterwideNetworkPolicy/no-api' "$explicit_false" "$no_api"

bare_group="$(fixture bare-group <<'EOF'
apiVersion: cilium.io/
kind: CiliumClusterwideNetworkPolicy
metadata: {name: bare-group}
spec:
  endpointSelector: {}
EOF
)"
expect 'cilium.io with no version is cannot-check' 2 'CiliumClusterwideNetworkPolicy/bare-group' "$explicit_false" "$bare_group"

no_spec="$(fixture no-spec <<'EOF'
apiVersion: cilium.io/v2
kind: CiliumClusterwideNetworkPolicy
metadata: {name: no-spec}
EOF
)"
expect 'a policy with no spec is cannot-check' 2 'no spec or specs' "$no_spec"

broken="$(fixture broken <<'EOF'
apiVersion: cilium.io/v2
kind: CiliumClusterwideNetworkPolicy
metadata: {name: broken
EOF
)"
expect 'unparseable YAML is cannot-check' 2 'could not parse' "$broken"

# Repository mode derives its layers from the Flux wiring, so a layer the guard was
# never told about is still checked. The synthetic tree below wires ONE cluster to a
# layer path that exists nowhere in the real repository; the pre-#3407 policy there
# must be refused (1), not missed.
# expect_tree <name> <expected-exit> <stderr-substring> <tree>
expect_tree() {
  local name="$1" want="$2" needle="$3" tree="$4" got=0
  bash "$guard" "$tree" >"${tmp_dir}/out" 2>"${tmp_dir}/err" || got=$?
  assertions=$((assertions + 2))
  if [ "$got" -ne "$want" ]; then
    failures=$((failures + 1))
    printf 'FAIL %s: exit %s, want %s\n' "$name" "$got" "$want" >&2
    sed 's/^/  | /' "${tmp_dir}/err" >&2
  fi
  if ! grep -qF -- "$needle" "${tmp_dir}/err"; then
    failures=$((failures + 1))
    printf 'FAIL %s: stderr does not contain %s\n' "$name" "$needle" >&2
    sed 's/^/  | /' "${tmp_dir}/err" >&2
  fi
}

# wire_cluster <tree> <cluster> <spec.path>  — a cluster overlay whose only Flux
# Kustomization points at <spec.path> in this repository's OCI source.
wire_cluster() {
  local dir="$1/k8s/clusters/$2"
  mkdir -p "$dir"
  printf 'resources:\n  - flux-kustomization.yaml\n' >"${dir}/kustomization.yaml"
  cat >"${dir}/flux-kustomization.yaml" <<EOF
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata: {name: layer, namespace: flux-system}
spec:
  interval: 1h
  path: $3
  prune: true
  sourceRef: {kind: OCIRepository, name: flux-system}
EOF
}

tree="${tmp_dir}/tree-new-layer"
wire_cluster "$tree" edge ./layers/brand-new
mkdir -p "${tree}/k8s/clusters/base" "${tree}/k8s/layers/brand-new"
printf 'resources:\n  - policy.yaml\n' >"${tree}/k8s/layers/brand-new/kustomization.yaml"
cp "$pre_3407" "${tree}/k8s/layers/brand-new/policy.yaml"
expect_tree 'a layer found only through the Flux wiring is checked' 1 \
  'deny-workload-instance-metadata-egress spec: egressDeny' "$tree"

# Kustomize also accepts `kustomization.yml`; a layer using it must be read, not refused.
tree="${tmp_dir}/tree-yml-layer"
wire_cluster "$tree" edge ./layers/yml
mkdir -p "${tree}/k8s/layers/yml"
printf 'resources:\n  - policy.yaml\n' >"${tree}/k8s/layers/yml/kustomization.yml"
cp "$pre_3407" "${tree}/k8s/layers/yml/policy.yaml"
expect_tree 'a layer with kustomization.yml is checked' 1 \
  'deny-workload-instance-metadata-egress spec: egressDeny' "$tree"

# A cluster directory the guard cannot read must stop the check, never be skipped.
tree="${tmp_dir}/tree-unreadable-cluster"
wire_cluster "$tree" edge ./layers/brand-new
mkdir -p "${tree}/k8s/clusters/other" "${tree}/k8s/layers/brand-new"
printf 'resources:\n  - policy.yaml\n' >"${tree}/k8s/layers/brand-new/kustomization.yaml"
cp "$explicit_false" "${tree}/k8s/layers/brand-new/policy.yaml"
expect_tree 'a cluster directory with no kustomization file is cannot-check' 2 \
  "cluster overlay 'other' has no kustomization file" "$tree"

tree="${tmp_dir}/tree-no-wiring"
mkdir -p "${tree}/k8s/clusters/edge"
printf 'resources: []\n' >"${tree}/k8s/clusters/edge/kustomization.yaml"
expect_tree 'a cluster that wires no layer is cannot-check' 2 \
  "renders no Flux Kustomization" "$tree"

tree="${tmp_dir}/tree-missing-layer"
wire_cluster "$tree" edge ./layers/gone
expect_tree 'a wired layer with no kustomization.yaml is cannot-check' 2 \
  "'layers/gone' has no kustomization file" "$tree"

# GREEN on the real tree: every Flux entrypoint renders and the committed policy passes.
got=0
bash "$guard" "$root_dir" >"${tmp_dir}/out" 2>"${tmp_dir}/err" || got=$?
assertions=$((assertions + 1))
if [ "$got" -ne 0 ] || ! grep -qF 'OK' "${tmp_dir}/out"; then
  failures=$((failures + 1))
  printf 'FAIL the repository tree: exit %s\n' "$got" >&2
  sed 's/^/  | /' "${tmp_dir}/err" >&2
fi

printf '%d assertion(s), %d failure(s)\n' "$assertions" "$failures"
[ "$failures" -eq 0 ] || exit 1
printf 'test-guard-cilium-clusterwide-default-deny: OK\n'
