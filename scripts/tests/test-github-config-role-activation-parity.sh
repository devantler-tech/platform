#!/usr/bin/env bash
set -euo pipefail

# The github-config tenant Role must grant exactly the managed-resource kinds
# the ManagedResourceActivationPolicy turns into CRDs.
#
# Activating a kind without granting it leaves the tenant's Flux Kustomization
# failing at apply time, inside a separately published OCI artifact, with an
# RBAC error that names the ServiceAccount rather than this file. Granting a
# kind that is not activated re-widens the tenant's authority past the
# namespaced surface the Role exists to bound. Both directions are checked.
#
# The bare `github.m.upbound.io` group is deliberately out of scope: it carries
# the ProviderConfig, which is not a managed resource and never appears in the
# activation policy. The `*.github.m.upbound.io` suffix match excludes it.

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
role="${repo_root}/k8s/bases/apps/github-config/role.yaml"
policy="${repo_root}/k8s/providers/hetzner/infrastructure/crossplane/managed-resource-activation-policy.yaml"

for f in "${role}" "${policy}"; do
  if [[ ! -f "${f}" ]]; then
    echo "::error::missing input for the github-config RBAC parity check: ${f}"
    exit 1
  fi
done

# `<resource>.<group>` for every activated GitHub managed resource.
activated="$(
  yq --no-doc -r \
    '.spec.activate[] | select(. == "*.github.m.upbound.io")' \
    "${policy}" | sort -u
)"

# `<resource>.<group>` for every managed-resource kind the Role grants.
# shellcheck disable=SC2016  # $g is a yq variable; the shell must not expand it.
granted="$(
  yq --no-doc -r \
    '.rules[]
       | select(.apiGroups[] == "*.github.m.upbound.io")
       | .apiGroups[] as $g
       | .resources[] + "." + $g' \
    "${role}" | sort -u
)"

# Fail closed on a vacuous read. A path typo, a renamed field, or a yq upgrade
# that changes the expression's meaning would otherwise make BOTH sides empty
# and the comparison below pass while testing nothing.
activated_count="$(printf '%s\n' "${activated}" | grep -c . || true)"
granted_count="$(printf '%s\n' "${granted}" | grep -c . || true)"

if ((activated_count < 5)); then
  echo "::error::read ${activated_count} activated GitHub managed resources from ${policy}; expected at least 5 — the query matched nothing meaningful"
  exit 1
fi

if ((granted_count < 5)); then
  echo "::error::read ${granted_count} granted GitHub managed resources from ${role}; expected at least 5 — the query matched nothing meaningful"
  exit 1
fi

# A wildcard would satisfy every parity comparison below while granting the
# whole group, so reject it explicitly rather than relying on the set diff.
if printf '%s\n' "${granted}" | grep -q '^\*\.'; then
  echo "::error::${role} grants a resources wildcard on a GitHub managed-resource group; enumerate the activated kinds instead"
  exit 1
fi

missing="$(comm -23 <(printf '%s\n' "${activated}") <(printf '%s\n' "${granted}"))"
extra="$(comm -13 <(printf '%s\n' "${activated}") <(printf '%s\n' "${granted}"))"

status=0

if [[ -n "${missing}" ]]; then
  status=1
  while IFS= read -r kind; do
    [[ -n "${kind}" ]] || continue
    echo "::error::${kind} is activated but the github-config Role does not grant it; add its resource name to the matching apiGroup rule in k8s/bases/apps/github-config/role.yaml"
  done <<<"${missing}"
fi

if [[ -n "${extra}" ]]; then
  status=1
  while IFS= read -r kind; do
    [[ -n "${kind}" ]] || continue
    echo "::error::the github-config Role grants ${kind}, which no ManagedResourceActivationPolicy activates; remove it or activate the kind in k8s/providers/hetzner/infrastructure/crossplane/managed-resource-activation-policy.yaml"
  done <<<"${extra}"
fi

if ((status != 0)); then
  exit "${status}"
fi

echo "github-config Role grants exactly the ${activated_count} activated GitHub managed-resource kinds."
