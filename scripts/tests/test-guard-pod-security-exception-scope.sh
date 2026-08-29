#!/usr/bin/env bash
#
# Pins guard-pod-security-exception-scope.sh in all THREE directions.
#
#   exit 0  the exception covers exactly the namespaces the mutation does not reach
#   exit 1  the two have drifted, in either direction
#   exit 2  the guard could not check — a missing file, a rule that moved, or any
#           list that came back empty
#
# The assertions that matter are not the happy path. Three of these exist because the
# obvious implementation gets them wrong:
#
#   * THE IMAGEUID CARVE-OUT. add-pod-security-context excludes fifteen namespaces,
#     and three of them (cert-manager, keda, opencost) ARE reached, by the by-name
#     add-imageuid-pod-security-context rule. A guard that reads only the exclusion
#     list reports three false differences on a correct tree.
#   * A MISSING RULE IS NOT AN EMPTY SET. If add-imageuid-pod-security-context is
#     renamed away, subtracting nothing leaves all fifteen "unreached" — a plausible
#     number that silently changes the invariant. That must be exit 2, not exit 1.
#   * A VACUOUS PASS. An empty NotIn compared against anything must not read as
#     agreement, and an empty-vs-empty comparison must never be reported as OK.
#
# Every fixture is derived from the REAL files and mutates exactly one thing, so a
# failure can only be caused by the case it belongs to.

set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
guard="$repo_root/scripts/guard-pod-security-exception-scope.sh"
policy_src="$repo_root/k8s/bases/infrastructure/cluster-policies/best-practices/add-security-context.yaml"
exception_src="$repo_root/k8s/bases/infrastructure/cluster-security-exceptions/pod-security-mutations.yaml"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

failures=0
assertions=0

run_guard() { # <policy> <exception>
  if GUARD_OUT="$("$guard" "$1" "$2" 2>&1)"; then
    GUARD_RC=0
  else
    GUARD_RC=$?
  fi
}

assert_rc() { # <label> <expected> <actual>
  assertions=$((assertions + 1))
  if [ "$2" = "$3" ]; then
    printf '  ok   %s (exit %s)\n' "$1" "$3"
  else
    printf '  FAIL %s: expected exit %s, got %s\n' "$1" "$2" "$3"
    printf '%s\n' "$GUARD_OUT" | sed 's/^/       | /'
    failures=$((failures + 1))
  fi
}

assert_mentions() { # <label> <needle>
  assertions=$((assertions + 1))
  if printf '%s\n' "$GUARD_OUT" | grep -Fq -- "$2"; then
    printf '  ok   %s (names %s)\n' "$1" "$2"
  else
    printf '  FAIL %s: output does not name %s\n' "$1" "$2"
    printf '%s\n' "$GUARD_OUT" | sed 's/^/       | /'
    failures=$((failures + 1))
  fi
}

# A fixture that failed to BUILD is the trap that makes an ablation look clean: the
# file is unchanged, the guard passes, and the assertion reads as robustness. Every
# fixture below is therefore diffed against its source and must actually differ.
assert_fixture_changed() { # <label> <src> <fixture>
  assertions=$((assertions + 1))
  if cmp -s "$2" "$3"; then
    printf '  FAIL %s: fixture is byte-identical to its source, so this case never ran\n' "$1"
    failures=$((failures + 1))
    return 1
  fi
  printf '  ok   %s (fixture built)\n' "$1"
  return 0
}

printf 'guard-pod-security-exception-scope\n'

# --------------------------------------------------------------- the real tree --
run_guard "$policy_src" "$exception_src"
assert_rc 'the committed tree agrees' 0 "$GUARD_RC"
assert_mentions 'reports the unreached count' '12 namespace(s) unreached'

# cert-manager/keda/opencost are in the exclusion list and MUST NOT be reported. This
# is the imageuid carve-out, asserted on the real files rather than a fixture.
for ns in cert-manager keda opencost; do
  assertions=$((assertions + 1))
  if printf '%s\n' "$GUARD_OUT" | grep -Fq -- "$ns"; then
    printf '  FAIL imageuid carve-out: %s was reported as a difference\n' "$ns"
    failures=$((failures + 1))
  else
    printf '  ok   imageuid carve-out: %s not reported\n' "$ns"
  fi
done

# ------------------------------------------------- drift: mutation side gains a ns --
fixture="$scratch/policy-extra-exclusion.yaml"
yq '(.spec.rules[] | select(.name == "add-pod-security-context") | .exclude.any[0].resources.namespaces) += ["guard-fixture-added-to-mutation"] |
    (.spec.rules[] | select(.name == "add-container-security-context") | .exclude.any[0].resources.namespaces) += ["guard-fixture-added-to-mutation"]' \
  "$policy_src" >"$fixture"
if assert_fixture_changed 'mutation-gains-a-namespace' "$policy_src" "$fixture"; then
  run_guard "$fixture" "$exception_src"
  assert_rc 'a namespace excluded by the mutation but absent from the exception fails' 1 "$GUARD_RC"
  assert_mentions 'names the added namespace' 'guard-fixture-added-to-mutation'
fi

# ------------------------------------------ drift: mutation drops a ns, exception keeps it --
fixture="$scratch/policy-dropped-exclusion.yaml"
yq '(.spec.rules[] | select(.name == "add-pod-security-context") | .exclude.any[0].resources.namespaces) |= map(select(. != "velero")) |
    (.spec.rules[] | select(.name == "add-container-security-context") | .exclude.any[0].resources.namespaces) |= map(select(. != "velero"))' \
  "$policy_src" >"$fixture"
if assert_fixture_changed 'mutation-drops-a-namespace' "$policy_src" "$fixture"; then
  run_guard "$fixture" "$exception_src"
  assert_rc 'a namespace the mutation now reaches but the exception still excludes fails' 1 "$GUARD_RC"
  assert_mentions 'names the dropped namespace' 'velero'
fi

# ------------------------------------------------- drift between the two mutation rules --
fixture="$scratch/policy-rules-disagree.yaml"
yq '(.spec.rules[] | select(.name == "add-container-security-context") | .exclude.any[0].resources.namespaces) |= map(select(. != "velero"))' \
  "$policy_src" >"$fixture"
if assert_fixture_changed 'mutation-rules-disagree' "$policy_src" "$fixture"; then
  run_guard "$fixture" "$exception_src"
  assert_rc 'the two mutation rules excluding different sets fails' 1 "$GUARD_RC"
  assert_mentions 'names the rule divergence' 'no longer exclude the same namespaces'
fi

# ------------------------------------------------------ fail-closed: the rule moved --
fixture="$scratch/policy-imageuid-renamed.yaml"
yq '(.spec.rules[] | select(.name == "add-imageuid-pod-security-context") | .name) = "add-imageuid-pod-security-context-renamed"' \
  "$policy_src" >"$fixture"
if assert_fixture_changed 'imageuid-rule-renamed' "$policy_src" "$fixture"; then
  run_guard "$fixture" "$exception_src"
  assert_rc 'a renamed imageuid rule is UNCHECKABLE, not a 15-namespace difference' 2 "$GUARD_RC"
  assert_mentions 'names the list it could not resolve' 'add-imageuid-pod-security-context matched namespaces'
fi

# ---------------------------------------------------- fail-closed: empty NotIn set --
fixture="$scratch/exception-empty-notin.yaml"
yq '(.spec.match.namespaceSelector.matchExpressions[] | select(.key == "kubernetes.io/metadata.name" and .operator == "NotIn") | .values) = []' \
  "$exception_src" >"$fixture"
if assert_fixture_changed 'exception-empty-notin' "$exception_src" "$fixture"; then
  run_guard "$policy_src" "$fixture"
  assert_rc 'an empty NotIn is UNCHECKABLE, never a vacuous pass' 2 "$GUARD_RC"
fi

# -------------------------------------------- fail-closed: the NotIn expression moved --
fixture="$scratch/exception-operator-changed.yaml"
yq '(.spec.match.namespaceSelector.matchExpressions[] | select(.operator == "NotIn") | .operator) = "In"' \
  "$exception_src" >"$fixture"
if assert_fixture_changed 'exception-operator-changed' "$exception_src" "$fixture"; then
  run_guard "$policy_src" "$fixture"
  assert_rc 'a NotIn that became In is UNCHECKABLE' 2 "$GUARD_RC"
fi

# ------------------------------------------------------- fail-closed: unreadable input --
printf 'this: [is: not: valid: yaml\n' >"$scratch/broken.yaml"
run_guard "$scratch/broken.yaml" "$exception_src"
assert_rc 'unparseable policy is exit 2' 2 "$GUARD_RC"

run_guard "$scratch/does-not-exist.yaml" "$exception_src"
assert_rc 'a missing policy file is exit 2' 2 "$GUARD_RC"
assert_mentions 'says the file is missing' 'not found'

run_guard "$policy_src" "$scratch/does-not-exist.yaml"
assert_rc 'a missing exception file is exit 2' 2 "$GUARD_RC"

printf '\n%d assertion(s), %d failure(s)\n' "$assertions" "$failures"
[ "$failures" -eq 0 ]
