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

# #3392: a results entry naming a resource the test never loads gets no row at all.
root="$(copy renamed-resource)"
yq -i '(.results[].resources[] | select(. == "tenant-ns/attack-toservices-foreign-named")) = "tenant-ns/attack-toservices-foreign-renamed"' \
  "${root}/tests/restrict-tenant-network-policies/kyverno-test.yaml"
if kyverno_passes "renamed resource" "${root}"; then
  expect_fail "renamed resource" "${root}" 1 "tenant-ns/attack-toservices-foreign-renamed, but kyverno ran no assertion for it"
fi

# A generate row names the object it generates, which need not share its trigger's name.
root="$(copy generated-name)"
yq -i '(.spec.rules[] | select(.name == "generate-vpa-for-deployment") | .generate.name) = "{{request.object.metadata.name}}-vpa"' \
  "${root}/${policies}/auto-vpa.yaml"
for expected in "${root}"/tests/auto-vpa/deployment-owner-expected.yaml "${root}"/tests/auto-vpa/recreated/*expected*.yaml; do
  [ "$(yq '.metadata.name' "${expected}")" = "deployment-owner" ] || continue
  yq -i '.metadata.name = "deployment-owner-vpa"' "${expected}"
done
if kyverno_passes "generated object named apart from its trigger" "${root}"; then
  expect_pass "generated object named apart from its trigger" "${root}"
fi

# A generate row names only the object it generates, so the triggers of one entry that all
# generate the same fixed-name object share its name, and kyverno drops a misspelled one
# without a row or a failure.
root="$(copy generated-fixed-name)"
fixture="${root}/tests/generated-fixed-name"
mkdir -p "${fixture}"
cat >"${fixture}/policy.yaml" <<'EOF'
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: generate-fixed-name
spec:
  rules:
    - name: generate-defaults
      match:
        any:
          - resources:
              kinds: [Deployment]
      generate:
        apiVersion: v1
        kind: ConfigMap
        name: defaults
        namespace: "{{request.object.metadata.namespace}}"
        synchronize: false
        data:
          data:
            key: value
EOF
for name in first second; do
  cat >"${fixture}/${name}.yaml" <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${name}
  namespace: apps
spec:
  selector:
    matchLabels:
      app: ${name}
  template:
    metadata:
      labels:
        app: ${name}
    spec:
      containers:
        - name: app
          image: nginx
EOF
done
cat >"${fixture}/defaults-expected.yaml" <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: defaults
  namespace: apps
data:
  key: value
EOF
cat >"${fixture}/kyverno-test.yaml" <<'EOF'
apiVersion: cli.kyverno.io/v1alpha1
kind: Test
metadata:
  name: generated-fixed-name
policies: [policy.yaml]
resources: [first.yaml, second.yaml]
results:
  - policy: generate-fixed-name
    rule: generate-defaults
    resources: [apps/first, apps/second]
    kind: Deployment
    result: pass
    generatedResource: defaults-expected.yaml
EOF
if kyverno_passes "two triggers of one fixed-name generated object" "${root}"; then
  expect_pass "two triggers of one fixed-name generated object" "${root}"
fi
yq -i '.results[0].resources[1] = "apps/secnod"' "${fixture}/kyverno-test.yaml"
if kyverno_passes "a misspelled trigger of a fixed-name generated object" "${root}"; then
  expect_fail "a misspelled trigger of a fixed-name generated object" "${root}" 1 "declares 2 triggers that generate defaults, but kyverno ran assertions for only 1 of them"
fi

# A fixture whose results were all removed asserts nothing while kyverno stays green.
root="$(copy no-results)"
yq -i '.results = []' "${root}/tests/restrict-tenant-issuer-refs/kyverno-test.yaml"
if kyverno_passes "no results" "${root}"; then
  expect_fail "no results" "${root}" 1 "declares no results"
fi

# An autogen rule Kyverno never generates (the policy sets autogen-controllers: none) reads
# as Excluded, so a skip row naming one passes kyverno and the Excluded check alike.
root="$(copy autogen-disabled)"
yq -i '.results += [{"policy": "prioritise-hcloud-volume-pods", "rule": "autogen-prioritise-hcloud-claims", "kind": "Pod", "resources": ["observability/no-volumes"], "result": "skip"}]' \
  "${root}/tests/prioritise-hcloud-volume-pods/kyverno-test.yaml"
if kyverno_passes "autogen rule the policy never generates" "${root}"; then
  expect_fail "autogen rule the policy never generates" "${root}" 1 "prioritise-hcloud-volume-pods generates no such rule"
fi

# Kyverno generates no autogen rule for any rule of a policy where one rule filters by
# selector or mutates with a JSON patch, as add-security-context does both, even for a base
# rule that matches plain Pods.
root="$(copy autogen-policy-filtered)"
yq -i '.results += [{"policy": "add-security-context", "rule": "autogen-add-baseline-context-optin-namespaces", "kind": "Deployment", "resources": ["observability/operator-deploy"], "result": "skip"}]' \
  "${root}/tests/add-baseline-context/kyverno-test.yaml"
if kyverno_passes "autogen rule of a policy that filters by selector" "${root}"; then
  expect_fail "autogen rule of a policy that filters by selector" "${root}" 1 "a match or exclude filters by name, names, selector"
  expect_fail "autogen rule of a policy that mutates with a JSON patch" "${root}" 1 "add-security-context gets no autogen rules because a rule mutates with patchesJson6902"
fi

# A policy may itself name a rule `autogen-*`, and kyverno evaluates that rule under its own
# name, so the name must not be read as a generated rule's.
root="${work}/literal-autogen-rule"
fixture="${root}/fixture/literal-autogen-rule"
mkdir -p "${fixture}"
cat >"${fixture}/policy.yaml" <<'EOF'
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: literal-autogen-rule
  annotations:
    pod-policies.kyverno.io/autogen-controllers: none
spec:
  validationFailureAction: Enforce
  rules:
    - name: autogen-require-team
      match:
        any:
          - resources:
              kinds: [ConfigMap]
      validate:
        message: every ConfigMap names its team
        pattern:
          metadata:
            labels:
              team: "?*"
EOF
cat >"${fixture}/labelled.yaml" <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: labelled
  namespace: apps
  labels:
    team: platform
EOF
cat >"${fixture}/kyverno-test.yaml" <<'EOF'
apiVersion: cli.kyverno.io/v1alpha1
kind: Test
metadata:
  name: literal-autogen-rule
policies: [policy.yaml]
resources: [labelled.yaml]
results:
  - policy: literal-autogen-rule
    rule: autogen-require-team
    resources: [apps/labelled]
    kind: ConfigMap
    result: pass
EOF
if kyverno_passes "a rule the policy itself names autogen-*" "${root}" fixture; then
  expect_pass "a rule the policy itself names autogen-*" "${root}" fixture
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
