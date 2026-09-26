#!/usr/bin/env bash
#
# Pins the unverified-provider digest-pin guard (#4189) in all three directions:
#
#   exit 0  every Provider whose publisher has no signature check is digest-pinned
#   exit 1  an unverified Provider package is referenced by tag only
#   exit 2  the guard could not check — missing root, or no Crossplane Provider
#           at all (a selector that matched nothing must never read as healthy)
#
# Scoping cases:
#   * Verified publishers (ghcr.io/devantler-tech/, xpkg.upbound.io/upbound/)
#     pass by tag: their signature check is what binds them.
#   * A Flux notification `Provider` has no package and must be ignored.
#   * A JSON-patch file (a top-level YAML list) must not make the guard fail.
#   * A package from a registry nobody verifies fails by tag — the verified set
#     is an allow-list, so a new publisher is unverified until checked.

set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
guard="$repo_root/scripts/guard-unverified-provider-digest-pin.sh"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

failures=0
assertions=0
digest='sha256:b342e334a0f0acbd52049206a0eb51b16eb6697e26a0a9745afdf8c8e4528ea2'

run_guard() { # <root>
  if GUARD_OUT="$("$guard" "$1" 2>&1)"; then
    GUARD_RC=0
  else
    GUARD_RC=$?
  fi
}

check() { # <label> <expected-rc> <root> [needle]
  assertions=$((assertions + 1))
  run_guard "$3"
  if [ "$GUARD_RC" != "$2" ]; then
    printf '  FAIL %s: expected exit %s, got %s\n' "$1" "$2" "$GUARD_RC"
    printf '%s\n' "$GUARD_OUT" | sed 's/^/       | /'
    failures=$((failures + 1))
    return
  fi
  if [ -n "${4:-}" ] && ! printf '%s' "$GUARD_OUT" | grep -qF -- "$4"; then
    printf '  FAIL %s: output lacks %s\n' "$1" "$4"
    printf '%s\n' "$GUARD_OUT" | sed 's/^/       | /'
    failures=$((failures + 1))
    return
  fi
  printf '  ok   %s (exit %s)\n' "$1" "$GUARD_RC"
}

provider() { # <dir> <name> <package>
  mkdir -p "$1"
  cat >"$1/$2.yaml" <<EOF
---
apiVersion: pkg.crossplane.io/v1
kind: Provider
metadata:
  name: $2
spec:
  package: $3
EOF
}

# Verified publishers by tag, an unverified one by digest, plus the two shapes
# the guard must step over.
good="$scratch/good"
provider "$good" first-party ghcr.io/devantler-tech/provider-upjet-unifi:v0.1.0
provider "$good" upbound xpkg.upbound.io/upbound/provider-family-aws:v2.6.1
provider "$good" contrib "ghcr.io/crossplane-contrib/provider-upjet-github:v0.20.0@$digest"
cat >"$good/alert-provider.yaml" <<'EOF'
apiVersion: notification.toolkit.fluxcd.io/v1beta3
kind: Provider
metadata:
  name: slack
spec:
  type: slack
EOF
cat >"$good/json-patch.yaml" <<'EOF'
- op: add
  path: /spec/replicas
  value: 2
EOF
check 'verified publishers by tag and unverified by digest pass' 0 "$good" '3 Provider(s) checked; 1 unverified'

tag_only="$scratch/tag-only"
provider "$tag_only" contrib ghcr.io/crossplane-contrib/provider-upjet-github:v0.20.0
check 'unverified publisher referenced by tag only fails' 1 "$tag_only" 'Provider contrib uses'

new_registry="$scratch/new-registry"
provider "$new_registry" other xpkg.crossplane.io/somebody/provider-x:v1.0.0
check 'unknown registry is unverified and fails by tag' 1 "$new_registry" 'Provider other uses'

lookalike="$scratch/lookalike"
provider "$lookalike" lookalike ghcr.io/devantler-tech-evil/provider-x:v1.0.0
check 'a prefix look-alike of a verified publisher is not verified' 1 "$lookalike" 'Provider lookalike uses'

short_digest="$scratch/short-digest"
provider "$short_digest" short "ghcr.io/crossplane-contrib/provider-upjet-github:v0.20.0@sha256:b342e334"
check 'a truncated digest is not a digest pin' 1 "$short_digest" 'Provider short uses'

no_package="$scratch/no-package"
mkdir -p "$no_package"
cat >"$no_package/p.yaml" <<'EOF'
apiVersion: pkg.crossplane.io/v1
kind: Provider
metadata:
  name: empty
spec: {}
EOF
check 'a Provider with no package fails' 1 "$no_package" 'with no spec.package'

empty="$scratch/empty"
mkdir -p "$empty"
cp "$good/alert-provider.yaml" "$empty/"
check 'a tree with no Crossplane Provider is UNKNOWN, not clean' 2 "$empty" 'refusing to pass vacuously'

check 'a missing root is UNKNOWN' 2 "$scratch/does-not-exist" 'is not a directory'

check 'the live tree passes' 0 "$repo_root/k8s" 'all digest-pinned'

printf '\n%s assertion(s), %s failure(s)\n' "$assertions" "$failures"
[ "$failures" -eq 0 ]
