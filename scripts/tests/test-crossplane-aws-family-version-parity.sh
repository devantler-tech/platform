#!/usr/bin/env bash
# Pins the Upbound AWS Crossplane family to one release.
#
# provider-aws-iam depends on provider-family-aws, which ships the ProviderConfig
# CRD every family member uses, and every member must run the family provider's
# release. Renovate groups their bumps into one PR, but grouping does not force
# equal versions: if the registry offers different latest releases, the grouped
# PR moves them apart. This fails any production render where a family member's
# package tag differs from the family provider's.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
readonly overlay="${repo_root}/k8s/providers/hetzner/infrastructure"
readonly family='xpkg.upbound.io/upbound/provider-family-aws'
readonly member_prefix='xpkg.upbound.io/upbound/provider-aws-'

workdir="$(mktemp -d)"
trap 'rm -rf "${workdir}"' EXIT

fail() {
  printf '::error::%s\n' "$1" >&2
  exit 1
}

# Prints the verdict for a rendered stream and returns non-zero on a violation.
check() {
  local rendered="$1" packages family_tag member tag mismatched=()
  packages="$(yq eval -N 'select(.kind == "Provider" and .apiVersion == "pkg.crossplane.io/v1") | .metadata.name + " " + .spec.package' "${rendered}")"

  family_tag="$(awk -v p="${family}:" 'index($2, p) == 1 { print substr($2, length(p) + 1) }' <<<"${packages}")"
  if [[ -z "${family_tag}" || "${family_tag}" == *$'\n'* ]]; then
    echo "expected exactly one ${family} Provider, found: ${family_tag:-none}"
    return 1
  fi

  local members=0
  while read -r member package; do
    [[ -n "${member}" && "${package}" == "${member_prefix}"* ]] || continue
    members=$((members + 1))
    tag="${package##*:}"
    [[ "${tag}" == "${family_tag}" ]] || mismatched+=("${member}=${tag}")
  done <<<"${packages}"

  if ((members == 0)); then
    echo "no ${member_prefix}* Provider found beside ${family}"
    return 1
  fi
  if ((${#mismatched[@]} > 0)); then
    echo "these AWS family members are not on the family provider's release ${family_tag}: ${mismatched[*]}"
    return 1
  fi
  echo "all ${members} AWS family member(s) run ${family_tag}"
}

provider() {
  printf -- '---\napiVersion: pkg.crossplane.io/v1\nkind: Provider\nmetadata: {name: %s}\nspec: {package: "%s"}\n' "$1" "$2"
}

# --- Self-test: each verdict on a fixture, so a check that stopped matching
# cannot pass the real render vacuously.
{ provider upbound-provider-family-aws "${family}:v2.8.1"
  provider provider-aws-iam "${member_prefix}iam:v2.8.1"; } >"${workdir}/equal.yaml"
{ provider upbound-provider-family-aws "${family}:v2.8.1"
  provider provider-aws-iam "${member_prefix}iam:v2.8.0"; } >"${workdir}/skewed.yaml"
provider provider-aws-iam "${member_prefix}iam:v2.8.1" >"${workdir}/no-family.yaml"
provider upbound-provider-family-aws "${family}:v2.8.1" >"${workdir}/no-member.yaml"

check "${workdir}/equal.yaml" >/dev/null ||
  fail 'self-test: a family on one release must pass'
for fixture in skewed no-family no-member; do
  if check "${workdir}/${fixture}.yaml" >/dev/null; then
    fail "self-test: the ${fixture} fixture must fail"
  fi
done

# --- The production render ----------------------------------------------------
kubectl kustomize "${overlay}" >"${workdir}/render.yaml" 2>"${workdir}/render.err" ||
  fail "k8s/providers/hetzner/infrastructure failed to build: $(tail -5 "${workdir}/render.err")"

if ! verdict="$(check "${workdir}/render.yaml")"; then
  fail "${verdict}. Pin every provider-aws-* Provider to the same tag as provider-family-aws in k8s/providers/hetzner/infrastructure/crossplane/"
fi
printf 'PASS: %s\n' "${verdict}"
