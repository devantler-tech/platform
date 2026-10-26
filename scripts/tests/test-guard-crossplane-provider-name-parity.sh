#!/usr/bin/env bash
#
# Pins the Crossplane provider-name parity guard's verdict in all THREE directions.
#
#   exit 0  every pkg.crossplane.io Provider carries the name Crossplane derives from
#           its package (or is explicitly dispositioned) AND its generated ClusterRole
#           regex appears in the Kubescape secret-reader exception
#   exit 1  a name drifted from its derived name, or an exception entry is orphaned
#   exit 2  the guard could not check — missing root, unreadable/empty exception list,
#           or NO pkg.crossplane.io Provider found at all
#
# Two of these are anti-vacuity or scoping cases rather than behaviours:
#
#   * A tree with no Crossplane Provider must be exit 2, NOT a clean exit 0. A
#     selector that matched nothing looks exactly like a healthy tree, which is the
#     failure class the guard exists to prevent.
#   * `notification.toolkit.fluxcd.io` ALSO has a `Provider` kind (the Flux alert
#     provider). It has no package and no generated ClusterRole, so matching on
#     `kind: Provider` alone would report a false failure on a correct tree. The
#     mixed fixture asserts the reported count excludes it.
#
# Every fixture carries its OWN provider name, so an assertion can only be satisfied
# by the case it belongs to.

set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
guard="$repo_root/scripts/guard-crossplane-provider-name-parity.sh"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

failures=0
assertions=0

run_guard() { # <root>
  # Capture the status with `if` rather than toggling `set -e`: this file runs under
  # `set -uo pipefail` with NO errexit, so `set -e` here would enable a mode the
  # script never had.
  if GUARD_OUT="$("$guard" "$1" 2>&1)"; then
    GUARD_RC=0
  else
    GUARD_RC=$?
  fi
}

assert_rc() { # <label> <expected-rc> <actual-rc>
  assertions=$((assertions + 1))
  if [ "$2" = "$3" ]; then
    printf '  ok   %s (exit %s)\n' "$1" "$3"
  else
    printf '  FAIL %s: expected exit %s, got %s\n' "$1" "$2" "$3"
    printf '%s\n' "$GUARD_OUT" | sed 's/^/       | /'
    failures=$((failures + 1))
  fi
}

assert_contains() { # <label> <needle>
  assertions=$((assertions + 1))
  # A here-string, NOT a pipe: `grep -q` exits on the first match, and the SIGPIPE
  # that gives the upstream `printf` becomes the pipeline status under `pipefail`.
  if grep -qF -- "$2" <<<"$GUARD_OUT"; then
    printf '  ok   %s\n' "$1"
  else
    printf '  FAIL %s: output did not contain %s\n' "$1" "$2"
    printf '%s\n' "$GUARD_OUT" | sed 's/^/       | /'
    failures=$((failures + 1))
  fi
}

assert_not_contains() { # <label> <needle>
  assertions=$((assertions + 1))
  if grep -qF -- "$2" <<<"$GUARD_OUT"; then
    printf '  FAIL %s: output unexpectedly contained %s\n' "$1" "$2"
    printf '%s\n' "$GUARD_OUT" | sed 's/^/       | /'
    failures=$((failures + 1))
  else
    printf '  ok   %s\n' "$1"
  fi
}

# new_tree <name> -> echoes a root with the exception list present but EMPTY of
# crossplane entries; callers add providers and the matching exception lines.
new_tree() {
  local root="$scratch/$1"
  mkdir -p "$root/bases/infrastructure/cluster-security-exceptions" \
    "$root/providers/hetzner/infrastructure/crossplane"
  printf 'apiVersion: spdx.softwarecomposition.kubescape.io/v1beta1\nkind: ClusterSecurityException\nspec:\n  patterns: |\n' \
    >"$root/bases/infrastructure/cluster-security-exceptions/secret-reader-rbac.yaml"
  printf '%s\n' "$root"
}

add_provider() { # <root> <name> <package>
  cat >"$1/providers/hetzner/infrastructure/crossplane/$2.yaml" <<YAML
apiVersion: pkg.crossplane.io/v1
kind: Provider
metadata:
  name: $2
spec:
  package: $3
  runtimeConfigRef:
    name: $2
YAML
}

add_exception() { # <root> <provider-name>
  local line="^crossplane:provider:$2-[0-9a-f]+:system\$"
  printf '        name: %s\n' "$line" \
    >>"$1/bases/infrastructure/cluster-security-exceptions/secret-reader-rbac.yaml"
}

printf 'test-guard-crossplane-provider-name-parity\n'

# --- 1. clean: canonical name, the exception list carries it ------------------
t="$(new_tree clean)"
add_provider "$t" upbound-provider-family-aws xpkg.upbound.io/upbound/provider-family-aws:v2.6.1
add_exception "$t" upbound-provider-family-aws
run_guard "$t"
assert_rc 'canonical name with its exception passes' 0 "$GUARD_RC"
assert_contains 'reports the number checked' '1 Crossplane Provider(s) checked'

# --- 2. the platform#3454 drift: a shortened alias, exceptions consistent ------
# This is the pre-fix tree: the name is internally consistent everywhere, so nothing
# else in the repo fails. Only the derived-name comparison catches it.
t="$(new_tree alias)"
add_provider "$t" provider-family-aws xpkg.upbound.io/upbound/provider-family-aws:v2.6.1
add_exception "$t" provider-family-aws
run_guard "$t"
assert_rc 'a shortened alias is refused' 1 "$GUARD_RC"
assert_contains 'names the derived name it expected' 'upbound-provider-family-aws'
assert_contains 'explains the lock-poisoning consequence' 'poisoning the lock dependency graph'

# --- 3. orphaned exception ----------------------------------------------------
t="$(new_tree orphan-secret-reader)"
add_provider "$t" upbound-provider-orphan-a xpkg.upbound.io/upbound/provider-orphan-a:v1.0.0
run_guard "$t"
assert_rc 'a missing secret-reader entry is refused' 1 "$GUARD_RC"
assert_contains 'names the secret-reader list' 'secret-reader-rbac.yaml'

# --- 4. the Flux Provider kind is not a Crossplane Provider --------------------
t="$(new_tree flux-mixed)"
add_provider "$t" upbound-provider-family-aws xpkg.upbound.io/upbound/provider-family-aws:v2.6.1
add_exception "$t" upbound-provider-family-aws
mkdir -p "$t/providers/hetzner/infrastructure/flux-notifications"
cat >"$t/providers/hetzner/infrastructure/flux-notifications/provider.yaml" <<'YAML'
apiVersion: notification.toolkit.fluxcd.io/v1beta3
kind: Provider
metadata:
  name: slack
spec:
  type: slack
YAML
run_guard "$t"
assert_rc 'a Flux alert Provider does not make the tree fail' 0 "$GUARD_RC"
assert_contains 'and is not counted as a Crossplane provider' '1 Crossplane Provider(s) checked'

# --- 5. anti-vacuity: no Crossplane Provider at all ----------------------------
t="$(new_tree empty)"
run_guard "$t"
assert_rc 'a tree with no Crossplane Provider is exit 2, never a clean pass' 2 "$GUARD_RC"
assert_contains 'says it refused to pass vacuously' 'refusing to pass vacuously'

# --- 6. fail closed on an unreadable exception list ----------------------------
t="$(new_tree unreadable)"
add_provider "$t" upbound-provider-family-aws xpkg.upbound.io/upbound/provider-family-aws:v2.6.1
add_exception "$t" upbound-provider-family-aws
rm -f "$t/bases/infrastructure/cluster-security-exceptions/secret-reader-rbac.yaml"
run_guard "$t"
assert_rc 'a missing exception list is exit 2, not a drift verdict' 2 "$GUARD_RC"

# --- 7. fail closed on a root that does not exist ------------------------------
run_guard "$scratch/does-not-exist"
assert_rc 'a missing root is exit 2' 2 "$GUARD_RC"

# --- 8. an empty exception list is not silently "no entries to check" ----------
t="$(new_tree empty-exception)"
add_provider "$t" upbound-provider-family-aws xpkg.upbound.io/upbound/provider-family-aws:v2.6.1
: >"$t/bases/infrastructure/cluster-security-exceptions/secret-reader-rbac.yaml"
run_guard "$t"
assert_rc 'an empty exception list is exit 2' 2 "$GUARD_RC"

# --- 9. a dispositioned non-canonical name passes, but says so -----------------
t="$(new_tree dispositioned)"
add_provider "$t" provider-upjet-github ghcr.io/crossplane-contrib/provider-upjet-github:v0.19.1
add_exception "$t" provider-upjet-github
run_guard "$t"
assert_rc 'a reviewed non-canonical name passes' 0 "$GUARD_RC"
assert_contains 'but is reported rather than hidden' 'is not canonically named'

# --- 10. the failure names the file the provider is IN --------------------------
# Regression: the parser flushes a completed document at FNR==1 of the NEXT file, by
# which time awk's FILENAME has already advanced — so the message blamed whichever
# file happened to be read next. That misdirects whoever has to fix it, and on the
# real tree it pointed at deployment-runtime-config.yaml instead of the provider.
t="$(new_tree filename-attribution)"
add_provider "$t" provider-family-aws xpkg.upbound.io/upbound/provider-family-aws:v2.6.1
add_exception "$t" provider-family-aws
add_provider "$t" upbound-provider-zzz-later xpkg.upbound.io/upbound/provider-zzz-later:v1.0.0
add_exception "$t" upbound-provider-zzz-later
run_guard "$t"
assert_rc 'still refuses the aliased provider alongside a correct one' 1 "$GUARD_RC"
assert_contains 'blames the file the aliased provider is in' 'crossplane/provider-family-aws.yaml'
assert_not_contains 'does not blame the innocent later file' 'crossplane/upbound-provider-zzz-later.yaml ('

# --- 11. a Provider this parser cannot read is exit 2, never a silent skip ------
# Regression: flush() emitted a record only when BOTH name and package matched its
# narrow block-style patterns, and dropped the document otherwise. A flow-style
# Provider therefore vanished while its neighbours still parsed, so `checked` stayed
# non-zero, the anti-vacuity backstop never fired, and the guard exited 0 over
# exactly the aliased shape it exists to catch — the platform#3454 outage, re-armed.
t="$(new_tree unparseable)"
add_provider "$t" upbound-provider-family-aws xpkg.upbound.io/upbound/provider-family-aws:v2.6.1
add_exception "$t" upbound-provider-family-aws
run_guard "$t"
# Negative control FIRST: without the unreadable document this very tree passes, so
# the assertion below can only be satisfied by the document the case is about.
assert_rc 'control: the same tree without the unreadable document passes' 0 "$GUARD_RC"
cat >"$t/providers/hetzner/infrastructure/crossplane/flow-style.yaml" <<'YAML'
apiVersion: pkg.crossplane.io/v1
kind: Provider
metadata: {}
spec: { package: xpkg.upbound.io/upbound/provider-family-aws:v2.6.1 }
YAML
run_guard "$t"
assert_rc 'an unreadable Provider is exit 2, not a silent skip' 2 "$GUARD_RC"
assert_contains 'blames the file it could not read' 'flow-style.yaml'
assert_contains 'names the field it could not read' 'metadata.name'

# Semantic flow-style inputs are readable. Keep the unreadable-input assertions
# above for a genuinely absent name, and prove a readable alias is actual drift.
for shape in flow whole-flow alias merge; do
  t="$(new_tree "semantic-$shape")"
  add_exception "$t" upbound-provider-family-aws
  case "$shape" in
    flow)
      printf '%s\n' 'apiVersion: pkg.crossplane.io/v1' 'kind: Provider' \
        'metadata: { name: upbound-provider-family-aws }' \
        'spec: { package: xpkg.upbound.io/upbound/provider-family-aws:v2.6.1 }' \
        >"$t/providers/canonical.yaml" ;;
    whole-flow)
      printf '%s\n' '{apiVersion: pkg.crossplane.io/v1, kind: Provider, metadata: {name: upbound-provider-family-aws}, spec: {package: "xpkg.upbound.io/upbound/provider-family-aws:v2.6.1"}}' \
        >"$t/providers/canonical.yml" ;;
    alias)
      printf '%s\n' 'apiVersion: pkg.crossplane.io/v1' 'kind: Provider' \
        'identity: &metadata {name: upbound-provider-family-aws}' \
        'packageRef: &package {package: "xpkg.upbound.io/upbound/provider-family-aws:v2.6.1"}' \
        'metadata: *metadata' 'spec: *package' >"$t/providers/canonical.yaml" ;;
    merge)
      printf '%s\n' 'apiVersion: pkg.crossplane.io/v1' 'kind: Provider' \
        'defaults: &defaults {name: shortened}' \
        'metadata: {name: upbound-provider-family-aws, <<: *defaults}' \
        'spec: {package: "xpkg.upbound.io/upbound/provider-family-aws:v2.6.1"}' \
        >"$t/providers/canonical.yaml" ;;
  esac
  run_guard "$t"
  assert_rc "$shape canonical Provider is checked" 0 "$GUARD_RC"
  assert_contains "$shape canonical Provider has a nonzero checked count" 'Crossplane Provider(s) checked'
done

t="$(new_tree flow-alias-drift)"
add_exception "$t" shortened
cat >"$t/providers/shortened.yaml" <<'YAML'
apiVersion: pkg.crossplane.io/v1
kind: Provider
metadata: { name: shortened }
spec: { package: xpkg.upbound.io/upbound/provider-family-aws:v2.6.1 }
YAML
run_guard "$t"
assert_rc 'a readable flow alias is drift rather than unreadable' 1 "$GUARD_RC"
assert_contains 'flow drift names the derived name' 'upbound-provider-family-aws'

for field in name package; do
  t="$(new_tree "typed-$field")"
  add_provider "$t" upbound-provider-family-aws xpkg.upbound.io/upbound/provider-family-aws:v2.6.1
  add_exception "$t" upbound-provider-family-aws
  if [ "$field" = name ]; then
    yq -i '.metadata.name = ["invalid"]' "$t/providers/hetzner/infrastructure/crossplane/upbound-provider-family-aws.yaml"
    expected_field=metadata.name
  else
    yq -i '.spec.package = false' "$t/providers/hetzner/infrastructure/crossplane/upbound-provider-family-aws.yaml"
    expected_field=spec.package
  fi
  run_guard "$t"
  assert_rc "a nonstring $field is unreadable" 2 "$GUARD_RC"
  assert_contains "the nonstring $field refusal identifies the field" "$expected_field"
done

t="$(new_tree invalid-yaml)"
add_provider "$t" upbound-provider-family-aws xpkg.upbound.io/upbound/provider-family-aws:v2.6.1
add_exception "$t" upbound-provider-family-aws
printf '%s\n' 'apiVersion: pkg.crossplane.io/v1' 'kind: Provider' 'metadata: {' >"$t/providers/broken.yaml"
run_guard "$t"
assert_rc 'a malformed Provider is unreadable beside a valid one' 2 "$GUARD_RC"
assert_contains 'the malformed input identifies its file' 'broken.yaml'

t="$(new_tree unsupported-yq)"
add_provider "$t" upbound-provider-family-aws xpkg.upbound.io/upbound/provider-family-aws:v2.6.1
add_exception "$t" upbound-provider-family-aws
mkdir -p "$scratch/old-yq-bin"
cat >"$scratch/old-yq-bin/yq" <<'SH'
#!/usr/bin/env bash
echo 'unknown flag: --yaml-fix-merge-anchor-to-spec' >&2
exit 1
SH
chmod +x "$scratch/old-yq-bin/yq"
if GUARD_OUT="$(PATH="$scratch/old-yq-bin:$PATH" "$guard" "$t" 2>&1)"; then
  GUARD_RC=0
else
  GUARD_RC=$?
fi
assert_rc 'an incompatible YAML parser is unreadable' 2 "$GUARD_RC"
assert_contains 'tool incompatibility identifies the required parser capability' 'yq with --yaml-fix-merge-anchor-to-spec support is required'
assert_not_contains 'tool incompatibility is not reported as malformed input' 'cannot parse YAML input'

printf '\n%d assertion(s), %d failure(s)\n' "$assertions" "$failures"
[ "$failures" -eq 0 ]
