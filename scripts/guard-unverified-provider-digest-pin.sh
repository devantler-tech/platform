#!/usr/bin/env bash
#
# guard-unverified-provider-digest-pin.sh — every Crossplane provider package
# whose signature nothing verifies must be pinned by digest (#4189).
#
# Two publishers are verified before merge: ghcr.io/devantler-tech/ against this
# organisation's signers (check-first-party-package-signatures.sh) and
# xpkg.upbound.io/upbound/ against Upbound's build identity
# (check-upbound-package-signatures.sh). Any other package has no signature
# check, so the only thing that makes the reviewed bytes the ones that run is an
# immutable digest. A tag alone can be moved or re-pushed upstream.
#
# The verified publishers are an allow-list on purpose: a package from a new
# registry is unverified until someone adds a check for it, so it must be
# digest-pinned from the start instead of slipping through unnoticed.
#
# Usage: guard-unverified-provider-digest-pin.sh <root>
#   exit 0  every unverified Provider package carries @sha256:<64 hex>
#   exit 1  an unverified Provider package is not digest-pinned
#   exit 2  the guard could not check (missing root, unreadable manifest, or no
#           pkg.crossplane.io Provider found at all — never a vacuous pass)

set -euo pipefail

root="${1:-}"

die() {
  printf 'guard-unverified-provider-digest-pin: %s\n' "$1" >&2
  exit 2
}

[ -n "$root" ] || die 'usage: guard-unverified-provider-digest-pin.sh <root>'
[ -d "$root" ] || die "root '$root' is not a directory"

verified_prefixes='ghcr.io/devantler-tech/
xpkg.upbound.io/upbound/'

list="$(mktemp)"
trap 'rm -f "$list"' EXIT

# Emit "<file>\t<name>\t<package>" for every pkg.crossplane.io Provider document.
while IFS= read -r -d '' file; do
  # shellcheck disable=SC2016 # the yq expression is data, not shell.
  if ! yq ea -r 'select(tag == "!!map") | select(.apiVersion == "pkg.crossplane.io/*" and .kind == "Provider") | [.metadata.name // "", .spec.package // ""] | @tsv' \
    "$file" >"$list.one" 2>/dev/null; then
    rm -f "$list.one"
    die "could not parse '$file' — refusing to pass over a manifest this guard cannot read"
  fi
  while IFS="$(printf '\t')" read -r name pkg; do
    [ -n "$name$pkg" ] || continue
    printf '%s\t%s\t%s\n' "$file" "$name" "$pkg" >>"$list"
  done <"$list.one"
  rm -f "$list.one"
done < <(find "$root" -type f \( -name '*.yaml' -o -name '*.yml' \) -print0)

[ -s "$list" ] || die "found no pkg.crossplane.io Provider manifests under '$root' — refusing to pass vacuously"

status=0
checked=0
unverified=0
while IFS="$(printf '\t')" read -r file name pkg; do
  checked=$((checked + 1))
  if [ -z "$pkg" ]; then
    printf 'guard-unverified-provider-digest-pin: %s declares Provider %s with no spec.package\n' "$file" "${name:-<unnamed>}" >&2
    status=1
    continue
  fi
  verified=0
  while IFS= read -r prefix; do
    case "$pkg" in "$prefix"*) verified=1 ;; esac
  done <<<"$verified_prefixes"
  [ "$verified" -eq 1 ] && continue
  unverified=$((unverified + 1))
  if ! printf '%s\n' "$pkg" | grep -Eq '@sha256:[0-9a-f]{64}$'; then
    printf 'guard-unverified-provider-digest-pin: %s: Provider %s uses %s\n' "$file" "$name" "$pkg" >&2
    printf '  No signature check covers this publisher, so the package must be pinned by digest\n' >&2
    printf '  (<repository>:<tag>@sha256:<digest>) for the reviewed bytes to be the ones that run.\n' >&2
    printf '  Resolve it with: crane digest %s  (or add a signature check for this publisher)\n' "${pkg%@*}" >&2
    status=1
  fi
done <"$list"

if [ "$status" -eq 0 ]; then
  printf 'guard-unverified-provider-digest-pin: OK — %s Provider(s) checked; %s unverified, all digest-pinned\n' \
    "$checked" "$unverified"
fi
exit "$status"
