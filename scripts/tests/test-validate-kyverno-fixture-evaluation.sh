#!/usr/bin/env bash
# Exercises scripts/validate-kyverno-fixture-evaluation.sh against the real
# fixtures and against copies with one policy broken each, so every check is
# proven to fire for the reason it names while `kyverno test` stays green.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
validator="${repo_root}/scripts/validate-kyverno-fixture-evaluation.sh"
policies="k8s/bases/infrastructure/cluster-policies/best-practices"

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

failures=0
fail() {
  echo "FAIL: $*"
  failures=$((failures + 1))
}

# copy <name>: a copy of the fixtures and the policies they load.
copy() {
  mkdir -p "${work}/$1"
  cp -R "${repo_root}/tests" "${repo_root}/k8s" "${work}/$1/"
  printf '%s\n' "${work}/$1"
}

# expect_pass <name> <root> [tests-dir]
expect_pass() {
  local output
  if ! output="$(cd "$2" && bash "${validator}" "${3:-tests}" 2>&1)"; then
    fail "$1: expected success, got:"$'\n'"${output}"
    return
  fi
  echo "ok: $1"
}

# expect_fail <name> <root> <exit code> <message fragment> [tests-dir]
expect_fail() {
  local output rc=0
  output="$(cd "$2" && bash "${validator}" "${5:-tests}" 2>&1)" || rc=$?
  if [ "${rc}" -ne "$3" ]; then
    fail "$1: expected exit $3, got ${rc}:"$'\n'"${output}"
    return
  fi
  if ! printf '%s\n' "${output}" | grep -qF -- "$4"; then
    fail "$1: exit $3 but missing \"$4\":"$'\n'"${output}"
    return
  fi
  echo "ok: $1"
}

# kyverno_passes <name> <root> [tests-dir]: the break must be invisible to kyverno itself.
kyverno_passes() {
  if ! (cd "$2" && kyverno test "${3:-tests}" --remove-color >/dev/null 2>&1); then
    fail "$1: kyverno test already fails, so this case proves nothing"
    return 1
  fi
}

expect_pass "the committed fixtures" "${repo_root}"

# #3145: kubectl's shorthand kind makes every rule match nothing.
root="$(copy shorthand-kinds)"
sed -i.bak -E 's#team\.github\.m\.upbound\.io/\*/(Team|TeamMembership|TeamRepository)$#\1.team.github.m.upbound.io#' \
  "${root}/${policies}/restrict-github-team-management.yaml"
if kyverno_passes "shorthand kinds" "${root}"; then
  expect_fail "shorthand kinds" "${root}" 1 "declares result: fail (kyverno reported it Excluded)"
fi

# #3152: a deleted rule leaves its rows Excluded.
root="$(copy deleted-rule)"
yq -i 'del(.spec.rules[] | select(.name == "secretstore-no-shared-vault-role"))' \
  "${root}/${policies}/restrict-tenant-secret-stores.yaml"
if kyverno_passes "deleted rule" "${root}"; then
  expect_fail "deleted rule" "${root}" 1 "names rule secretstore-no-shared-vault-role of policy restrict-tenant-secret-stores, which the policies it loads do not define"
fi

# #3152: a renamed rule is the same break under another name.
root="$(copy renamed-rule)"
yq -i '(.spec.rules[] | select(.name == "secretstore-no-shared-vault-role") | .name) = "secretstore-vault-role"' \
  "${root}/${policies}/restrict-tenant-secret-stores.yaml"
if kyverno_passes "renamed rule" "${root}"; then
  expect_fail "renamed rule" "${root}" 1 "names rule secretstore-no-shared-vault-role"
fi

# A rule named only by `skip` rows passes the Excluded check, so only the
# existence check can catch its removal.
root="$(copy skip-only)"
mkdir -p "${root}/skip-only"
mv "${root}/tests/restrict-tenant-issuer-refs" "${root}/skip-only/"
yq -i '.results |= map(select(.result == "skip"))' "${root}/skip-only/restrict-tenant-issuer-refs/kyverno-test.yaml"
expect_pass "a skip-only fixture whose rule exists" "${root}" skip-only
yq -i 'del(.spec.rules[] | select(.name == "certificate-no-cluster-scoped-issuer"))' \
  "${root}/${policies}/restrict-tenant-issuer-refs.yaml"
if kyverno_passes "skip-only rule removed" "${root}" skip-only; then
  expect_fail "skip-only rule removed" "${root}" 1 "names rule certificate-no-cluster-scoped-issuer" skip-only
fi

# A fixture whose expectation is wrong still fails through kyverno itself.
root="$(copy wrong-expectation)"
yq -i '(.results[] | select(.result == "fail") | .result) = "pass"' \
  "${root}/tests/restrict-tenant-issuer-refs/kyverno-test.yaml"
expect_fail "a failing kyverno test" "${root}" 1 "kyverno test tests failed"

expect_fail "a missing tests directory" "${repo_root}" 2 "tests directory not found" no-such-dir

if [ "${failures}" -gt 0 ]; then
  echo "${failures} case(s) failed"
  exit 1
fi
echo "All kyverno fixture evaluation cases passed."
