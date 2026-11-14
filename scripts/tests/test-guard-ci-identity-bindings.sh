#!/usr/bin/env bash
# Proves guard-ci-identity-bindings.sh passes the real production render and
# fails on the same bad inputs the admission policy refuses (platform#6270).
set -euo pipefail

repo="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
guard="${repo}/scripts/guard-ci-identity-bindings.sh"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

(cd "${repo}" && "${guard}" >/dev/null) || {
  echo "FAIL: the production render was refused" >&2
  exit 1
}

fresh_copy() {
  rm -rf "${work}/root"
  mkdir -p "${work}/root"
  cp -R "${repo}/k8s" "${work}/root/k8s"
}

# A refusal counts only for its stated reason: a render or Kyverno failure must
# not pass as the policy refusing a binding.
expect_refused() {
  local label="$1" reason="$2" output
  if output="$(GUARD_REPO_ROOT="${work}/root" "${guard}" 2>&1)"; then
    echo "FAIL: ${label} was admitted" >&2
    exit 1
  fi
  case "${reason}" in
    policy)
      if ! grep -qE "^pass: [0-9]+, fail: [1-9][0-9]*, warn: [0-9]+, error: 0," <<<"${output}" ||
        ! grep -q "restrict-ci-identity-bindings" <<<"${output}"; then
        printf "FAIL: %s was refused for another reason:\n%s\n" "${label}" "${output}" >&2
        exit 1
      fi
      ;;
    missing)
      if ! grep -q "approved aws/aws-managed-resources RoleBinding is not in the render" <<<"${output}"; then
        printf "FAIL: %s was refused for another reason:\n%s\n" "${label}" "${output}" >&2
        exit 1
      fi
      ;;
  esac
}

aws_dir="k8s/providers/hetzner/apps/aws"

# A ClusterRoleBinding granting the CI identity.
fresh_copy
cat >"${work}/root/${aws_dir}/zz-widen.yaml" <<'YAML'
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: aws-cluster-admin
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
  - kind: ServiceAccount
    name: aws
    namespace: aws
YAML
printf '  - zz-widen.yaml\n' >>"${work}/root/${aws_dir}/kustomization.yaml"
expect_refused "a ClusterRoleBinding to aws/aws" policy

# The approved binding re-pointed at a stronger role.
fresh_copy
sed -i.bak 's/^  kind: Role$/  kind: ClusterRole/; s/^  name: aws-managed-resources$/  name: aws-managed-resources/' \
  "${work}/root/${aws_dir}/role-binding.yaml"
expect_refused "the approved binding re-pointed at a ClusterRole" policy

# A render without the approved binding must not pass vacuously.
fresh_copy
grep -v 'role-binding.yaml' "${work}/root/${aws_dir}/kustomization.yaml" >"${work}/k.yaml"
cp "${work}/k.yaml" "${work}/root/${aws_dir}/kustomization.yaml"
expect_refused "a render without the approved binding" missing

echo "ok — the CI identity guard passes production and refuses three widening shapes"
