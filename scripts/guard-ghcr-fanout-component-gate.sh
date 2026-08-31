#!/usr/bin/env bash
#
# Asserts that the GHCR fan-out requirement follows the apps-base component gate.
#
# An app that pulls private first-party images declares its own ghcr-auth
# ExternalSecret, and scripts/refresh-flux-ghcr-auth.sh lists its namespace in
# FANOUT_NAMESPACES so the post-reconcile reassertion proves that credential
# reached the consumer. Those are two halves of ONE switch and are edited by
# hand in two files, so they can drift apart silently:
#
#   * an app enabled in the base but MISSING from FANOUT_NAMESPACES ships a
#     running consumer whose pull credential nothing proves;
#   * an app COMMENTED OUT of the base but still listed makes an ExternalSecret
#     mandatory that production can never reconcile. The pre-publish staging
#     invocation exempts it (--record-runtime-proof), but the post-reconcile
#     reassertion in .github/actions/deploy-prod runs with --reuse-runtime-proof,
#     where the exemption does not apply, so it marks the fan-out incomplete and
#     FAILS EVERY OTHERWISE-SUCCESSFUL DEPLOY until the app is enabled.
#
# The second is not hypothetical: it is exactly what staging the
# data-product-controller off introduced (platform#3476).
#
# The invariant, scoped to apps only:
#
#   an app under k8s/bases/apps/ that DECLARES a ghcr-auth ExternalSecret is
#   present in FANOUT_NAMESPACES if and only if it is enabled (uncommented) in
#   k8s/bases/apps/kustomization.yaml
#
# FANOUT_NAMESPACES also carries infrastructure namespaces that are not apps at
# all (kyverno's ghcr-auth comes from
# k8s/bases/infrastructure/external-secrets/, which is unconditionally enabled).
# Those are listed in NON_APP_FANOUT_NAMESPACES below rather than being skipped
# by pattern, so that a TYPO in an app name — which is also "not an app
# directory" — is reported instead of silently falling out of scope.
#
# Exit codes:
#   0  parity holds
#   1  drift: an enabled ghcr-auth app is unlisted, a disabled one is listed, or
#      a listed namespace is neither a ghcr-auth app nor a declared non-app
#   2  could not check: missing root or inputs, or a selector that matched
#      NOTHING (no ghcr-auth app, or an empty FANOUT_NAMESPACES). An empty
#      result from a filtered read is a claim about the filter, and a guard that
#      checked nothing must never look like a guard that passed.

set -uo pipefail

# Namespaces that are legitimately in FANOUT_NAMESPACES without being an app
# under k8s/bases/apps/. Keep the reason with the entry.
#   kyverno — ghcr-auth is declared in k8s/bases/infrastructure/external-secrets/
#             and the infrastructure layer is never gated off.
readonly -a NON_APP_FANOUT_NAMESPACES=("kyverno")

repo_root="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"

die() { printf '%s\n' "$1" >&2; exit 2; }

apps_dir="$repo_root/k8s/bases/apps"
apps_kustomization="$apps_dir/kustomization.yaml"
fanout_script="$repo_root/scripts/refresh-flux-ghcr-auth.sh"

[ -d "$apps_dir" ] || die "guard: no apps base at $apps_dir"
[ -r "$apps_kustomization" ] || die "guard: unreadable $apps_kustomization"
[ -r "$fanout_script" ] || die "guard: unreadable $fanout_script"
command -v yq >/dev/null 2>&1 || die "guard: yq is required"

# --- the apps that declare a ghcr-auth ExternalSecret -----------------------
# Match the DECLARATION (kind + metadata.name), never a bare `name: ghcr-auth`:
# a ServiceAccount imagePullSecret and an OCIRepository secretRef both REFERENCE
# the secret without declaring the ExternalSecret, so a plain grep would report
# apps whose credential is declared elsewhere.
ghcr_apps=()
for app_path in "$apps_dir"/*/; do
  [ -d "$app_path" ] || continue
  app="$(basename "$app_path")"
  declares=""
  while IFS= read -r manifest; do
    [ -n "$manifest" ] || continue
    if hit="$(yq -N -r 'select(.kind == "ExternalSecret" and .metadata.name == "ghcr-auth") | .metadata.name' \
      "$manifest" 2>/dev/null)" && [ -n "$hit" ]; then
      declares=yes
      break
    fi
  done < <(find "$app_path" -type f \( -name '*.yaml' -o -name '*.yml' \) 2>/dev/null)
  [ -n "$declares" ] && ghcr_apps+=("$app")
done

((${#ghcr_apps[@]} > 0)) ||
  die "guard: found NO app declaring a ghcr-auth ExternalSecret under $apps_dir — refusing to report parity on an empty set"

# --- which of those are enabled in the apps base ----------------------------
# Read the resource list structurally: a commented-out entry is absent from the
# parsed document, which is exactly the gate's semantics.
enabled_list="$(yq -N -r '.resources[]' "$apps_kustomization" 2>/dev/null)" ||
  die "guard: could not parse .resources from $apps_kustomization"
[ -n "$enabled_list" ] ||
  die "guard: $apps_kustomization declares no resources — refusing to report parity"

is_enabled() { # <app>
  printf '%s\n' "$enabled_list" | grep -qxF -- "$1/"
}

# --- the declared fan-out namespaces ----------------------------------------
# Take only the FANOUT_NAMESPACES array body, dropping comment lines so a
# commented-out entry reads as absent (the same semantics as the base gate).
fanout_list="$(
  sed -n '/^readonly -a FANOUT_NAMESPACES=(/,/^)/p' "$fanout_script" |
    sed '1d;$d' |
    sed 's/#.*$//' |
    grep -oE '"[^"]+"' |
    tr -d '"'
)"
[ -n "$fanout_list" ] ||
  die "guard: FANOUT_NAMESPACES in $fanout_script parsed as EMPTY — refusing to report parity"

is_listed() { # <namespace>
  printf '%s\n' "$fanout_list" | grep -qxF -- "$1"
}

is_non_app() { # <namespace>
  local n
  for n in "${NON_APP_FANOUT_NAMESPACES[@]}"; do [ "$n" = "$1" ] && return 0; done
  return 1
}

drift=0
printf 'ghcr-auth apps: %s\n' "${ghcr_apps[*]}"
printf 'fan-out namespaces: %s\n' "$(printf '%s' "$fanout_list" | tr '\n' ' ')"

for app in "${ghcr_apps[@]}"; do
  if is_enabled "$app"; then
    if is_listed "$app"; then
      printf '  ok   %s — enabled in the apps base and listed in FANOUT_NAMESPACES\n' "$app"
    else
      printf '  FAIL %s is ENABLED in %s but MISSING from FANOUT_NAMESPACES in %s; its running consumer pull credential would never be proven.\n' \
        "$app" "k8s/bases/apps/kustomization.yaml" "scripts/refresh-flux-ghcr-auth.sh"
      drift=1
    fi
  else
    if is_listed "$app"; then
      printf '  FAIL %s is COMMENTED OUT of %s but still listed in FANOUT_NAMESPACES in %s; the post-reconcile reassertion cannot ever find its ExternalSecret and every prod deploy would fail.\n' \
        "$app" "k8s/bases/apps/kustomization.yaml" "scripts/refresh-flux-ghcr-auth.sh"
      drift=1
    else
      printf '  ok   %s — staged off in the apps base and absent from FANOUT_NAMESPACES\n' "$app"
    fi
  fi
done

# Anything listed that is neither a ghcr-auth app nor a declared non-app entry
# is an orphan — most often a typo in an app name.
while IFS= read -r ns; do
  [ -n "$ns" ] || continue
  is_non_app "$ns" && { printf '  ok   %s — declared non-app fan-out namespace\n' "$ns"; continue; }
  listed_is_app=0
  for app in "${ghcr_apps[@]}"; do [ "$app" = "$ns" ] && listed_is_app=1 && break; done
  ((listed_is_app)) && continue
  printf '  FAIL %s is in FANOUT_NAMESPACES but declares no ghcr-auth ExternalSecret under k8s/bases/apps/ and is not a declared non-app namespace.\n' "$ns"
  drift=1
done <<<"$fanout_list"

if ((drift)); then
  printf '\nThe apps-base component gate and the GHCR fan-out requirement disagree.\n' >&2
  exit 1
fi
printf '\nThe GHCR fan-out requirement follows the apps-base component gate.\n'
