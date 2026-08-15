#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly guard="${root_dir}/scripts/tests/test-cilium-mutual-auth-policy.sh"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

tmp_dir="$(mktemp -d)"
readonly tmp_dir
trap 'rm -rf "${tmp_dir}"' EXIT
readonly fixture="${tmp_dir}/rendered.yaml"
readonly fake_bin="${tmp_dir}/bin"
mkdir -p "${fake_bin}"

cat >"${fake_bin}/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
cat "${CILIUM_POLICY_FIXTURE:?}"
EOF
chmod +x "${fake_bin}/kubectl"

run_guard() {
  CILIUM_POLICY_FIXTURE="${fixture}" PATH="${fake_bin}:${PATH}" bash "${guard}"
}

cat >"${fixture}" <<'EOF'
---
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata:
  name: broad-cnp-empty-map
spec:
  endpointSelector: {}
  ingress:
    - fromEndpoints:
        - {}
      authentication:
        mode: required
---
apiVersion: cilium.io/v2
kind: CiliumClusterwideNetworkPolicy
metadata:
  name: broad-ccnp-empty-labels
spec:
  endpointSelector: {}
  ingress:
    - fromEndpoints:
        - matchLabels: {}
      authentication:
        mode: required
---
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata:
  name: broad-cnp-empty-expressions
spec:
  endpointSelector: {}
  ingress:
    - fromEndpoints:
        - matchExpressions: []
      authentication:
        mode: required
---
apiVersion: cilium.io/v2
kind: CiliumClusterwideNetworkPolicy
metadata:
  name: broad-ccnp-specs-empty-fields
specs:
  - endpointSelector: {}
    ingress:
      - fromEndpoints:
          - matchLabels: {}
            matchExpressions: []
        authentication:
          mode: required
---
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata:
  name: broad-cnp-omitted-sources
spec:
  endpointSelector: {}
  ingress:
    - authentication:
        mode: required
---
apiVersion: cilium.io/v2
kind: CiliumClusterwideNetworkPolicy
metadata:
  name: broad-ccnp-empty-sources
specs:
  - endpointSelector: {}
    ingress:
      - fromEndpoints: []
        authentication:
          mode: required
EOF

if broad_output="$(run_guard 2>&1)"; then
  fail 'the guard accepted broad authentication rules across CNP, CCNP, spec, and specs forms'
fi
for expected_name in \
  broad-cnp-empty-map \
  broad-ccnp-empty-labels \
  broad-cnp-empty-expressions \
  broad-ccnp-specs-empty-fields \
  broad-cnp-omitted-sources \
  broad-ccnp-empty-sources; do
  grep -Fq -- "${expected_name}" <<<"${broad_output}" ||
    fail "the guard did not identify ${expected_name}"
done

cat >"${fixture}" <<'EOF'
---
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata:
  name: narrow-cnp-spec
spec:
  endpointSelector: {}
  ingress:
    - fromEndpoints:
        - matchLabels:
            k8s:io.kubernetes.pod.namespace: trusted
      authentication:
        mode: required
---
apiVersion: cilium.io/v2
kind: CiliumClusterwideNetworkPolicy
metadata:
  name: narrow-ccnp-specs
specs:
  - endpointSelector: {}
    ingress:
      - fromEndpoints:
          - matchExpressions:
              - key: k8s:io.kubernetes.pod.namespace
                operator: In
                values:
                  - trusted
        authentication:
          mode: required
---
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata:
  name: narrow-cnp-entity
spec:
  endpointSelector: {}
  ingress:
    - fromEntities:
        - cluster
      authentication:
        mode: required
---
apiVersion: cilium.io/v2
kind: CiliumClusterwideNetworkPolicy
metadata:
  name: narrow-ccnp-cidr
spec:
  endpointSelector: {}
  ingress:
    - fromCIDR:
        - 10.0.0.0/8
      authentication:
        mode: required
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: cilium
  namespace: kube-system
spec:
  values:
    authentication:
      enabled: true
      mutual:
        spire:
          enabled: true
EOF
run_guard >/dev/null || fail 'the guard rejected narrow CNP/CCNP rules using spec/specs'

cat >"${fixture}" <<'EOF'
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: cilium
  namespace: kube-system
spec:
  values:
    authentication:
      enabled: true
      mutual:
        spire:
          enabled: true
EOF
if orphan_output="$(run_guard 2>&1)"; then
  fail 'the guard accepted enabled Cilium authentication with no policy consumer'
fi
grep -Fq -- 'no required-authentication policy is rendered' <<<"${orphan_output}" ||
  fail 'the orphaned-authentication failure did not explain the missing policy consumer'

cat >"${fixture}" <<'EOF'
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: cilium
  namespace: kube-system
spec:
  values:
    authentication:
      enabled: false
      mutual:
        spire:
          enabled: false
EOF
run_guard >/dev/null || fail 'the guard rejected disabled authentication with no policy consumer'

printf 'PASS: Cilium authentication guard covers every production policy shape and consumer state\n'
