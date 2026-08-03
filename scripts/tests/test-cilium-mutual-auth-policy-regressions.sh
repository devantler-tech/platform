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

write_fixture() {
  local name="$1"
  local selector="$2"

  cat >"${fixture}" <<EOF
---
apiVersion: cilium.io/v2
kind: CiliumClusterwideNetworkPolicy
metadata:
  name: ${name}
spec:
  endpointSelector: {}
  ingress:
    - fromEndpoints:
${selector}
      authentication:
        mode: required
EOF
}

run_guard() {
  CILIUM_POLICY_FIXTURE="${fixture}" PATH="${fake_bin}:${PATH}" bash "${guard}"
}

require_rejection() {
  local name="$1"
  local selector="$2"
  local output

  write_fixture "${name}" "${selector}"
  if output="$(run_guard 2>&1)"; then
    fail "the guard accepted effectively empty selector ${name}"
  fi
  grep -Fq -- "${name}" <<<"${output}" ||
    fail "the guard failed without identifying effectively empty selector ${name}"
}

require_rejection broad-empty-map '        - {}'
require_rejection broad-empty-labels '        - matchLabels: {}'
require_rejection broad-empty-expressions '        - matchExpressions: []'
require_rejection broad-empty-fields $'        - matchLabels: {}\n          matchExpressions: []'

write_fixture narrow-labels $'        - matchLabels:\n            k8s:io.kubernetes.pod.namespace: trusted'
run_guard >/dev/null || fail 'the guard rejected a selector with an effective label requirement'

printf 'PASS: Cilium mutual-auth policy guard rejects every empty selector representation\n'
