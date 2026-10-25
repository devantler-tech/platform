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

# copy <name> <fixture|--all>: keep fixture paths and all their policy inputs,
# but do not rerun unrelated fixtures for each single-fixture mutation. The
# committed-tree and path-with-spaces controls still exercise the full census.
copy() {
  [ "$#" -eq 2 ] || return 2
  case "$2" in
    --all) ;;
    '' | . | .. | */*) return 2 ;;
    *) [ -f "${repo_root}/tests/$2/kyverno-test.yaml" ] || return 2 ;;
  esac
  mkdir -p "${work}/$1/tests" || return 2
  cp -R "${repo_root}/k8s" "${work}/$1/" || return 2
  if [ "$2" = --all ]; then
    cp -R "${repo_root}/tests/." "${work}/$1/tests/" || return 2
  else
    cp -R "${repo_root}/tests/$2" "${work}/$1/tests/" || return 2
  fi
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
  # A here-string, not a pipe: grep -q stops at the first match, and under pipefail the
  # writer's broken pipe would then read as "missing" for a fragment early in long output.
  if ! grep -qF -- "$4" <<<"${output}"; then
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

# A misspelled scope must never turn a mutation control into an unrelated pass.
rc=0
copy missing-fixture no-such-fixture >"${work}/missing-selection" 2>&1 || rc=$?
if [ "${rc}" -ne 2 ]; then
  fail "a missing selected fixture: expected exit 2, got ${rc}"
else
  echo "ok: a missing selected fixture"
fi

# #3145: kubectl's shorthand kind makes every rule match nothing.
root="$(copy shorthand-kinds restrict-github-team-management)"
sed -i.bak -E 's#team\.github\.m\.upbound\.io/\*/(Team|TeamMembership|TeamRepository)$#\1.team.github.m.upbound.io#' \
  "${root}/${policies}/restrict-github-team-management.yaml"
if kyverno_passes "shorthand kinds" "${root}"; then
  expect_fail "shorthand kinds" "${root}" 1 "declares result: fail (kyverno reported it Excluded)"
fi

# #3152: a deleted rule leaves its rows Excluded.
root="$(copy deleted-rule restrict-tenant-network-policies)"
yq -i 'del(.spec.rules[] | select(.name == "cnp-ingress-sources-must-be-constrained"))' \
  "${root}/${policies}/restrict-tenant-network-policies.yaml"
if kyverno_passes "deleted rule" "${root}"; then
  expect_fail "deleted rule" "${root}" 1 "names rule cnp-ingress-sources-must-be-constrained of policy restrict-tenant-network-policies, which the policies it loads do not define"
fi

# #3152: a renamed rule is the same break under another name.
root="$(copy renamed-rule restrict-tenant-network-policies)"
yq -i '(.spec.rules[] | select(.name == "cnp-ingress-sources-must-be-constrained") | .name) = "cnp-ingress-sources"' \
  "${root}/${policies}/restrict-tenant-network-policies.yaml"
if kyverno_passes "renamed rule" "${root}"; then
  expect_fail "renamed rule" "${root}" 1 "names rule cnp-ingress-sources-must-be-constrained"
fi

# A rule named only by `skip` rows passes the Excluded check, so only the
# existence check can catch its removal.
root="$(copy skip-only restrict-tenant-issuer-refs)"
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
root="$(copy renamed-resource restrict-tenant-network-policies)"
yq -i '(.results[].resources[] | select(. == "tenant-ns/attack-toservices-foreign-named")) = "tenant-ns/attack-toservices-foreign-renamed"' \
  "${root}/tests/restrict-tenant-network-policies/kyverno-test.yaml"
if kyverno_passes "renamed resource" "${root}"; then
  expect_fail "renamed resource" "${root}" 1 "tenant-ns/attack-toservices-foreign-renamed, but kyverno ran no assertion for it"
fi

# A generate row names the object it generates, which need not share its trigger's name.
# Renaming the generated VPA, and the expected object with it, must still pass.
root="$(copy generated-name auto-vpa-generating-policy-deployment)"
sed -i.orig 's/string(object.metadata.name) + "-deployment"/string(object.metadata.name) + "-vpa"/' \
  "${root}/${policies}/auto-vpa-deployment.yaml"
rm -f "${root}/${policies}/auto-vpa-deployment.yaml.orig"
grep -Fq 'string(object.metadata.name) + "-vpa"' "${root}/${policies}/auto-vpa-deployment.yaml" ||
  fail "generated object named apart from its trigger: the rename did not apply"
yq -i '.metadata.name = "shared-vpa"' "${root}/tests/auto-vpa-generating-policy-deployment/generated-vpa.yaml"
if kyverno_passes "generated object named apart from its trigger" "${root}"; then
  expect_pass "generated object named apart from its trigger" "${root}"
fi

# #5130: a GeneratingPolicy has no spec.rules; kyverno names its one rule after the policy.
# A row naming any other rule must still fail check 1, and a row expecting an object the
# policy never generates must still fail.
root="$(copy generating-policy-rule auto-vpa-generating-policy-deployment)"
yq -i '.results[0].rule = "generate-vpa-for-deployment"' \
  "${root}/tests/auto-vpa-generating-policy-deployment/kyverno-test.yaml"
expect_fail "GeneratingPolicy row naming a rule it does not define" "${root}" 1 \
  "names rule generate-vpa-for-deployment of policy auto-vpa-deployment"
root="$(copy generating-policy-missing auto-vpa-generating-policy-deployment)"
yq -i '.metadata.namespace = "other-ns"' "${root}/tests/auto-vpa-generating-policy-deployment/resources.yaml"
yq -i '.namespaceSelector[0].name = "other-ns" | .namespaceSelector[0].labels."kubernetes.io/metadata.name" = "other-ns"' \
  "${root}/tests/auto-vpa-generating-policy-deployment/values.yaml"
yq -i '.metadata.name = "absent-deployment"' "${root}/tests/auto-vpa-generating-policy-deployment/generated-vpa.yaml"
expect_fail "GeneratingPolicy expecting an object it never generates" "${root}" 1 "kyverno test"

# A generate row names only the object it generates, so the triggers of one entry that all
# generate the same fixed-name object share its name, and kyverno drops a misspelled one
# without a row or a failure.
root="${work}/generated-fixed-name"
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
root="$(copy no-results restrict-tenant-issuer-refs)"
yq -i '.results = []' "${root}/tests/restrict-tenant-issuer-refs/kyverno-test.yaml"
if kyverno_passes "no results" "${root}"; then
  expect_fail "no results" "${root}" 1 "declares no results"
fi

# An autogen rule Kyverno never generates (the policy sets autogen-controllers: none) reads
# as Excluded, so a skip row naming one passes kyverno and the Excluded check alike.
root="$(copy autogen-disabled prioritise-hcloud-volume-pods)"
yq -i '.results += [{"policy": "prioritise-hcloud-volume-pods", "rule": "autogen-prioritise-hcloud-claims", "kind": "Pod", "resources": ["observability/no-volumes"], "result": "skip"}]' \
  "${root}/tests/prioritise-hcloud-volume-pods/kyverno-test.yaml"
if kyverno_passes "autogen rule the policy never generates" "${root}"; then
  expect_fail "autogen rule the policy never generates" "${root}" 1 "prioritise-hcloud-volume-pods generates no such rule"
fi

# Kyverno generates no autogen rule for any rule of a policy where one rule filters by
# selector or mutates with a JSON patch, as add-security-context does both, even for a base
# rule that matches plain Pods.
root="$(copy autogen-policy-filtered add-baseline-context)"
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

# #4530: a policy kyverno loads but refuses to evaluate has every row skipped as
# "Invalid Policy" and counted as passing. A bare JSON literal is not a variable kyverno accepts.
root="$(copy invalid-policy restrict-github-team-management)"
# shellcheck disable=SC2016 # the braces and backticks are kyverno syntax, not shell
yq -i '(.spec.rules[] | select(.name == "teams-allow-listed") | .validate.deny.conditions.all[0].key) = "{{ `true` }}"' \
  "${root}/${policies}/restrict-github-team-management.yaml"
if kyverno_passes "a policy kyverno refuses to evaluate" "${root}"; then
  expect_fail "a policy kyverno refuses to evaluate" "${root}" 1 "of restrict-github-team-management (result: Skip, reason: Invalid Policy) and still counted them as passing"
fi

# The control: the same edit with an expression kyverno accepts is evaluated, and the
# rows judge it (the allow-list now refuses the approved teams as well).
root="$(copy valid-constant-policy restrict-github-team-management)"
# shellcheck disable=SC2016 # the braces and backticks are kyverno syntax, not shell
yq -i '(.spec.rules[] | select(.name == "teams-allow-listed") | .validate.deny.conditions.all[0].key) = "{{ request.object.metadata.name == `\"\"` }}"' \
  "${root}/${policies}/restrict-github-team-management.yaml"
expect_fail "an evaluated policy with the same edit" "${root}" 1 "Want pass, got fail"

# #5133: the host-access baseline must reject a permission broadening in each
# check (0 host paths, 1 host namespaces, 2 hostUsers). Each check in turn is
# replaced with one that allows everything, so this proves that the fixture's
# denial cases are evaluated against that check, not merely present.
for host_check in 0 1 2; do
  root="$(copy "weaken-host-check-${host_check}" validate-host-restrictions)"
  HOST_CHECK="${host_check}" yq -i \
    '.spec.validations[env(HOST_CHECK)].expression = "true"' \
    "${root}/${policies}/validate-host-restrictions.yaml"
  expect_fail "weakened host check ${host_check}" "${root}" 1 "kyverno test tests failed"
done

# #5143: the same proof for the pod-security baseline, one check at a time (0 container
# security, 1 pod seccomp profile, 2 container seccomp override). The copy must pass
# before the edit, and the failure after it must be a denial the weakened check no
# longer produces, so no unrelated failing row can satisfy the assertion.
for pss_check in 0 1 2; do
  root="$(copy "weaken-pss-check-${pss_check}" validate-pod-security)"
  expect_pass "unweakened pss check ${pss_check}" "${root}"
  PSS_CHECK="${pss_check}" yq -i \
    '.spec.validations[env(PSS_CHECK)].expression = "true"' \
    "${root}/${policies}/validate-pod-security.yaml"
  expect_fail "weakened pss check ${pss_check}" "${root}" 1 "Want fail, got pass"
done

# A fixture whose expectation is wrong still fails through kyverno itself.
root="$(copy wrong-expectation restrict-tenant-issuer-refs)"
yq -i '(.results[] | select(.result == "fail") | .result) = "pass"' \
  "${root}/tests/restrict-tenant-issuer-refs/kyverno-test.yaml"
expect_fail "a failing kyverno test" "${root}" 1 "kyverno test tests failed"

expect_fail "a missing tests directory" "${repo_root}" 2 "tests directory not found" no-such-dir

mkdir -p "${work}/empty-fixtures/tests" "${work}/listing-bin"
expect_fail "an empty fixture census" "${work}/empty-fixtures" 2 "no kyverno-test.yaml found"
cat >"${work}/listing-bin/find" <<'EOF'
#!/usr/bin/env bash
exit 45
EOF
chmod +x "${work}/listing-bin/find"
PATH="${work}/listing-bin:${PATH}" expect_fail "an unreadable fixture listing" "${repo_root}" 2 "could not list kyverno fixtures"
rm "${work}/listing-bin/find"
cat >"${work}/listing-bin/sort" <<'EOF'
#!/usr/bin/env bash
exit 46
EOF
chmod +x "${work}/listing-bin/sort"
PATH="${work}/listing-bin:${PATH}" expect_fail "a failed fixture sort" "${repo_root}" 2 "could not sort kyverno fixtures"
root="$(copy 'paths with spaces' --all)"
expect_pass "fixture paths containing spaces" "${root}"

if [ "${failures}" -gt 0 ]; then
  echo "${failures} case(s) failed"
  exit 1
fi
echo "All kyverno fixture evaluation cases passed."
