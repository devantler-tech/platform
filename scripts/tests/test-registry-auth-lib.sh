#!/usr/bin/env bash
# Hermetic coverage for scripts/registry-auth-lib.sh.
#
# The subject is the lookup that decides whether the signature inventory probes a registry
# ANONYMOUSLY or with the cluster's own pull credential. It is pure file parsing — no registry, no
# network, no credential of ours is involved — because the fail-open being guarded is a lookup that
# returns *something* for a malformed or absent config. A probe that believes it is authenticated
# when it is not reads a private package's 401 as a signature verdict, which is the exact
# unreadable-means-unsigned confusion #3371's UNKNOWN discipline exists to prevent.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/registry-auth-lib.sh
source "${repo_root}/scripts/registry-auth-lib.sh"

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

failures=0
check() { # label expected actual
  local label="$1" want="$2" got="$3"
  if [ "$want" = "$got" ]; then
    echo "ok    ${label}"
  else
    echo "FAIL  ${label}: expected [${want}], got [${got}]"
    failures=$((failures + 1))
  fi
}

# Basic auth for user "u", password "p" — "dTpw" — written the long way so the fixture is not
# merely the implementation's own output echoed back at it.
readonly UP_B64="dTpw"

# 🔴 DOCKER_CONFIG must be EXPORTED INTO THE CALL, not prefixed onto `check`.
# `DOCKER_CONFIG=dir check "..." "$(registry_credential_b64 ghcr.io)"` looks right and is not: the
# command substitution is evaluated by the parent shell BEFORE the prefix applies, so the function
# runs with the ambient DOCKER_CONFIG and silently falls back to ~/.docker/config.json. Caught here
# by every case returning the HOST's real credential — including the ones asserting emptiness, so
# the suite would have "passed" nothing and printed a live token. Route every call through this.
cred() { # config_dir registry
  (
    export DOCKER_CONFIG="$1"
    shift
    registry_credential_b64 "$@"
  )
}

# Belt and braces: prove the fixtures are what is being read, by making the fallback path unusable.
# If a future edit reintroduces the ambient-environment bug, these tests fail instead of leaking.
export HOME="${work}/no-such-home"

mkcfg() { # dir json
  mkdir -p "$1"
  printf '%s' "$2" >"$1/config.json"
}

# --- a credential that is present and well-formed --------------------------
mkcfg "${work}/explicit" '{"auths":{"ghcr.io":{"username":"u","password":"p"}}}'
check "explicit username/password is encoded" "$UP_B64" "$(cred "${work}/explicit" ghcr.io)"

mkcfg "${work}/encoded" '{"auths":{"ghcr.io":{"auth":"dTpw"}}}'
check "pre-encoded auth is used verbatim" "$UP_B64" "$(cred "${work}/encoded" ghcr.io)"

# https:// prefixed keys are the legacy docker form and name the same registry.
mkcfg "${work}/legacy" '{"auths":{"https://ghcr.io/v1/":{"auth":"dTpw"}}}'
check "legacy https:// host key matches" "$UP_B64" "$(cred "${work}/legacy" ghcr.io)"

# --- every shape that must yield NOTHING ------------------------------------
# Each of these is a case where returning a credential would make the probe claim an authority it
# does not have. Empty is the only safe answer, and the caller then stays anonymous.
check "absent config yields nothing" "" "$(cred "${work}/does-not-exist" ghcr.io)"

mkcfg "${work}/other" '{"auths":{"registry.example.com":{"auth":"dTpw"}}}'
check "a DIFFERENT registry never matches" "" "$(cred "${work}/other" ghcr.io)"

mkcfg "${work}/empty" '{"auths":{}}'
check "no auths entry yields nothing" "" "$(cred "${work}/empty" ghcr.io)"

mkcfg "${work}/malformed" 'this is not json{'
check "malformed json yields nothing" "" "$(cred "${work}/malformed" ghcr.io)"

mkcfg "${work}/blank" '{"auths":{"ghcr.io":{"username":"","password":""}}}'
check "blank username/password yields nothing" "" "$(cred "${work}/blank" ghcr.io)"

mkcfg "${work}/halfempty" '{"auths":{"ghcr.io":{"username":"u"}}}'
check "username with no password yields nothing" "" "$(cred "${work}/halfempty" ghcr.io)"

mkcfg "${work}/emptyauth" '{"auths":{"ghcr.io":{"auth":""}}}'
check "empty auth string yields nothing" "" "$(cred "${work}/emptyauth" ghcr.io)"

# A credentials-store entry carries no inline secret: docker resolves it through a helper binary we
# deliberately do not invoke. Returning "" keeps the probe anonymous and therefore honest.
mkcfg "${work}/store" '{"auths":{"ghcr.io":{}},"credsStore":"osxkeychain"}'
check "credsStore entry with no inline secret yields nothing" "" "$(cred "${work}/store" ghcr.io)"

# A non-empty `auth` that does not decode to user:secret is the one shape jq cannot filter:
# it is present and well-formed as a string, and only decoding reveals it is not a credential.
mkcfg "${work}/notapair" '{"auths":{"ghcr.io":{"auth":"bm90LWEtcGFpcg=="}}}'
check "auth that decodes without a colon yields nothing" "" "$(cred "${work}/notapair" ghcr.io)"

mkcfg "${work}/nouser" '{"auths":{"ghcr.io":{"auth":"OnBhc3N3b3Jk"}}}'
check "auth with an empty username yields nothing" "" "$(cred "${work}/nouser" ghcr.io)"

mkcfg "${work}/nopass" '{"auths":{"ghcr.io":{"auth":"dXNlcjo="}}}'
check "auth with an empty secret yields nothing" "" "$(cred "${work}/nopass" ghcr.io)"

# --- the registry argument itself must be required --------------------------
mkcfg "${work}/explicit2" '{"auths":{"ghcr.io":{"auth":"dTpw"}}}'
check "no registry argument yields nothing" "" "$(cred "${work}/explicit2" || true)"

if [ "$failures" -ne 0 ]; then
  echo "registry-auth-lib.test: ${failures} failure(s)" >&2
  exit 1
fi
echo "registry-auth-lib.test: registry credentials resolve only when genuinely present"
