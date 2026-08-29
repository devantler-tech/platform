#!/usr/bin/env bash
#
# Fail when a Crossplane Provider's NAME stops agreeing with the two places that
# depend on it: the name Crossplane derives from its package, and the Kubescape
# secret-reader exceptions that match its generated ClusterRole by regex.
#
# WHY THE NAME MATTERS (platform#3454, a live prod outage).
#
# Crossplane's dependency manager resolves a declared dependency by looking for a
# Provider with the name it DERIVES from the package reference (`<org>-<repo>`),
# and creates one when that name is absent. The Lock, however, is keyed by package
# SOURCE. Declare a dependency package under a shortened alias and the two disagree:
# the name lookup misses, Crossplane creates a second Provider for a source the Lock
# already holds, and building the dependency graph then fails with
#
#     cannot initialize dependency graph from the packages in the lock:
#     node <source> already exists
#
# That failure is not scoped to the duplicated package — it marks EVERY provider in
# the cluster Unhealthy and stops all Crossplane reconciliation. It went live on the
# Crossplane 2.3.4 -> 2.4.0 bump: the duplicate appeared 21 seconds after the new core
# rolled out, and the fleet stayed dead for 15 hours because nothing in GitHub can see
# it.
#
# WHY THE EXCEPTIONS MATTER (the same rename breaks them, silently).
#
# Crossplane names a provider's generated RBAC after the provider REVISION —
# `crossplane:provider:<provider-name>-<package-hash>:system` — so the provider name
# is a prefix of a cluster-scoped ClusterRole name. Two Kubescape exception lists
# match that ClusterRole by anchored regex to suppress C-0015 ("List Kubernetes
# secrets") for providers that read credential Secrets by design. Renaming a provider
# without updating both regexes does not fail anything: the manifests still build, the
# provider still runs, and the only symptom is a C-0015 finding reappearing days later
# with no obvious cause. The two lists are kept in sync BY HAND (their own headers say
# so), which is exactly the arrangement that drifts.
#
# 🔴 FAIL CLOSED. Every input this guard cannot read is exit 2, never a quiet pass.
# An empty provider set compared against empty exception files is the failure mode
# that makes a guard look green forever, so the provider set and both exception files
# are required to be non-empty before any comparison happens.
#
# 🔴 SCOPE BY apiVersion, NOT BY `kind: Provider`. `notification.toolkit.fluxcd.io`
# also has a `Provider` kind (the Flux alert Provider, e.g. `slack`), which has no
# package, no derived name and no generated ClusterRole. Matching on kind alone pulls
# it in and reports a false failure on a correct tree.
#
# Exit codes:
#   0  every pkg.crossplane.io Provider is canonically named (or explicitly
#      dispositioned below) and carries its ClusterRole regex in both exception lists
#   1  a drift was found
#   2  an input could not be read, or a required set was empty

set -euo pipefail

root="${1:-k8s}"

secret_reader="$root/bases/infrastructure/cluster-security-exceptions/secret-reader-rbac.yaml"
headlamp_mirror="$root/bases/infrastructure/controllers/kubescape/config-map-headlamp-exceptions.yaml"

die() {
  printf 'guard-crossplane-provider-name-parity: %s\n' "$1" >&2
  exit 2
}

[ -d "$root" ] || die "root '$root' is not a directory"
[ -r "$secret_reader" ] || die "cannot read the secret-reader exception '$secret_reader'"
[ -r "$headlamp_mirror" ] || die "cannot read the Headlamp exception mirror '$headlamp_mirror'"
[ -s "$secret_reader" ] || die "the secret-reader exception '$secret_reader' is empty"
[ -s "$headlamp_mirror" ] || die "the Headlamp exception mirror '$headlamp_mirror' is empty"

# Providers deliberately NOT carrying their derived name. Each is safe ONLY while no
# installed package declares it as a dependency: nothing resolves them by name today,
# so no duplicate is created. Adding a package that depends on one of these re-creates
# platform#3454 — rename it here rather than extending this list. Draining this list is
# tracked separately; a NEW provider must be canonically named.
noncanonical_disposition="
provider-aws-iam
provider-upjet-github
provider-upjet-unifi
"

# Emit "<name>\t<package>\t<file>" for every pkg.crossplane.io Provider document.
# shellcheck disable=SC2016  # the awk program is data; the shell must not expand it.
providers="$(
  find "$root" -type f -name '*.yaml' -print0 |
    xargs -0 awk '
      function flush(  where) {
        if (is_xp && is_provider) {
          where = (srcfile == "" ? FILENAME : srcfile)
          if (name != "" && pkg != "")
            printf "%s\t%s\t%s\n", name, pkg, where
          else
            # A Provider this line-oriented parser cannot read is NOT skipped: a
            # silently omitted document is exactly how an aliased provider would
            # slip past a guard that still exits 0 on its neighbours.
            printf "!unparseable\t%s\t%s\n", (name == "" ? "metadata.name" : "spec.package"), where
        }
        is_xp = 0; is_provider = 0; name = ""; pkg = ""; srcfile = ""
      }
      FNR == 1 { flush() }
      /^---[[:space:]]*$/ { flush(); next }
      /^apiVersion:[[:space:]]*pkg\.crossplane\.io\// { is_xp = 1; srcfile = FILENAME }
      /^kind:[[:space:]]*Provider[[:space:]]*$/ { is_provider = 1; srcfile = FILENAME }
      /^  name:[[:space:]]*[^[:space:]]/ { if (name == "") { name = $2; if (srcfile == "") srcfile = FILENAME } }
      /^  package:[[:space:]]*[^[:space:]]/ { if (pkg == "") { pkg = $2; if (srcfile == "") srcfile = FILENAME } }
      END { flush() }
    '
)" || die 'failed to enumerate Provider manifests'

[ -n "$providers" ] || die "found no pkg.crossplane.io Provider manifests under '$root' — refusing to pass vacuously"

# Derive the name Crossplane generates from a package reference:
#   xpkg.upbound.io/upbound/provider-family-aws:v2.6.1 -> upbound-provider-family-aws
# Strips any digest and any tag on the final path segment, drops the registry host
# (the first segment, when it looks like a host), and joins the rest with '-'.
derive_name() {
  local ref="$1" path host rest
  ref="${ref%%@*}"
  path="${ref%/*}"
  local last="${ref##*/}"
  last="${last%%:*}"
  if [ "$path" = "$ref" ]; then
    printf '%s\n' "$last"
    return
  fi
  host="${path%%/*}"
  rest="${path#*/}"
  case "$host" in
    *.* | *:* | localhost) ;;
    *) rest="$path" ;;
  esac
  if [ "$rest" = "$path" ] && [ "$host" = "$path" ]; then
    printf '%s\n' "$last"
    return
  fi
  printf '%s\n' "$(printf '%s/%s' "$rest" "$last" | tr '/' '-')"
}

status=0
checked=0

while IFS="$(printf '\t')" read -r name pkg file; do
  [ -n "$name" ] || continue

  if [ "$name" = "!unparseable" ]; then
    die "$(printf '%s declares a pkg.crossplane.io Provider whose %s this guard could not read.\n  Refusing to pass: an unreadable Provider is indistinguishable from an aliased one, and skipping it is how platform#3454 would recur.\n  Write it in block style (the shape the rest of the tree uses), or teach this parser the shape.' "$file" "$pkg")"
  fi

  checked=$((checked + 1))

  derived="$(derive_name "$pkg")"
  if [ "$name" != "$derived" ]; then
    if printf '%s\n' "$noncanonical_disposition" | grep -qxF -- "$name"; then
      printf 'guard-crossplane-provider-name-parity: NOTE %s is not canonically named (derived: %s) — dispositioned; safe only while nothing depends on it\n' \
        "$name" "$derived"
    else
      printf 'guard-crossplane-provider-name-parity: %s (%s) is named "%s" but Crossplane derives "%s" from its package.\n' \
        "${file}" "$pkg" "$name" "$derived" >&2
      printf '  A dependent package would make Crossplane create a SECOND Provider for this source, poisoning the lock dependency graph and marking every provider Unhealthy (platform#3454).\n' >&2
      printf '  Rename it to "%s", or add it to this guard'"'"'s reviewed disposition list with the reason.\n' "$derived" >&2
      status=1
    fi
  fi

  regex="^crossplane:provider:${name}-[0-9a-f]+:system\$"
  for f in "$secret_reader" "$headlamp_mirror"; do
    if ! grep -qF -- "$regex" "$f"; then
      printf 'guard-crossplane-provider-name-parity: %s has no secret-reader exception entry in %s\n' "$name" "$f" >&2
      printf '  Expected the anchored ClusterRole pattern: %s\n' "$regex" >&2
      printf '  Crossplane names provider RBAC after the revision, so renaming a provider silently orphans its exception and C-0015 reappears with no failing check.\n' >&2
      status=1
    fi
  done
done <<EOF
$providers
EOF

[ "$checked" -gt 0 ] || die 'parsed no providers out of a non-empty manifest set — the parser is broken, not the tree'

if [ "$status" -ne 0 ]; then
  printf 'guard-crossplane-provider-name-parity: provider naming and its dependents have drifted\n' >&2
  exit 1
fi

printf 'guard-crossplane-provider-name-parity: OK — %d Crossplane Provider(s) checked; names agree with their derived names and both exception lists\n' "$checked"
