#!/usr/bin/env bash
#
# Every Crossplane provider must end up with a complete POD-level
# securityContext on the Deployment its package manager generates.
#
# validate-pod-security/require-seccomp-profile asserts the pod-level path, and
# Kyverno auto-generates that rule for pod controllers, so the generated
# Deployment is validated against it. The add-security-context mutate policy
# cannot supply the value: its match/exclude carry pod selectors, so Kyverno
# generates no pod-controller rules for it and it never sees a Deployment.
#
# Two ways to get this wrong, and both end in the same place:
#
#   * a DeploymentRuntimeConfig that sets seccompProfile only on the container,
#     which does not satisfy a pod-level pattern; or
#   * a Provider with no runtimeConfigRef at all, since Crossplane's own pod
#     defaults are runAsNonRoot/runAsUser/runAsGroup and carry no profile.
#
# The failure is silent and delayed. A running provider Deployment is not
# re-admitted, so the denial only surfaces when a revision bump forces a
# recreate — at which point the whole provider fleet goes to zero pods
# (platform#3470).
#
# The gate also pins the rest of the pod securityContext, because Crossplane
# applies its defaults ONLY when the DRC supplies no securityContext at all: a
# DRC declaring seccompProfile alone silently drops runAsNonRoot and the
# uid/gid the provider images run as.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly drc_dir="${root_dir}/k8s/providers/hetzner/infrastructure/crossplane"
readonly sc_path='.spec.deploymentTemplate.spec.template.spec.securityContext'

# Crossplane's documented pod-level defaults, which declaring a securityContext
# suppresses, plus the profile the policy actually asserts.
readonly -a required_fields=(
  'runAsNonRoot:true'
  'runAsUser:2000'
  'runAsGroup:2000'
  'seccompProfile.type:RuntimeDefault'
)

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v yq >/dev/null 2>&1 || fail 'yq is required but not installed'
[ -d "${drc_dir}" ] || fail "provider directory not found: ${drc_dir}"

# Enumerate first and check the enumeration, so a failed find renders as an
# error rather than as "nothing to check".
files=()
while IFS= read -r f; do
  files+=("$f")
done < <(find "${drc_dir}" -maxdepth 1 -type f -name '*.yaml' | sort)
[ "${#files[@]}" -gt 0 ] || fail "no YAML files found under ${drc_dir}"

# Index every DeploymentRuntimeConfig by name, across every document of every
# file — a DRC sharing a file with other resources must not be skipped.
drc_files=''
for f in "${files[@]}"; do
  while IFS= read -r drc_name; do
    [ -n "${drc_name}" ] || continue
    drc_files="${drc_files}${drc_name} ${f}"$'\n'
  done < <(yq -r 'select(.kind == "DeploymentRuntimeConfig") | .metadata.name // ""' "$f")
done

drc_file_for() {
  printf '%s' "${drc_files}" | awk -v want="$1" '$1 == want { print $2; exit }'
}

check_drc() {
  local drc_name="$1" provider="$2" file
  file="$(drc_file_for "${drc_name}")"
  [ -n "${file}" ] || fail "provider ${provider}: runtimeConfigRef names '${drc_name}', but no DeploymentRuntimeConfig with that name exists under ${drc_dir}"

  local spec key want got
  for spec in "${required_fields[@]}"; do
    key="${spec%%:*}"
    want="${spec#*:}"
    got="$(yq -r "select(.kind == \"DeploymentRuntimeConfig\" and .metadata.name == \"${drc_name}\") | ${sc_path}.${key} // \"\"" "${file}")"
    [ "${got}" = "${want}" ] || fail "provider ${provider} via ${drc_name} ($(basename "${file}")): pod-level ${key} is '${got}', expected '${want}'
       Kyverno's require-seccomp-profile asserts the POD path and no mutate policy
       reaches a Deployment, so an incomplete block is denied on the next revision
       bump. Declaring a pod securityContext also suppresses Crossplane's own
       defaults, so all of these must be stated together:
           securityContext:
             runAsNonRoot: true
             runAsUser: 2000
             runAsGroup: 2000
             seccompProfile:
               type: RuntimeDefault"
  done
}

checked=0
for f in "${files[@]}"; do
  while IFS= read -r provider; do
    [ -n "${provider}" ] || continue
    ref="$(yq -r "select(.kind == \"Provider\" and .metadata.name == \"${provider}\") | .spec.runtimeConfigRef.name // \"\"" "$f")"
    [ -n "${ref}" ] || fail "provider ${provider} ($(basename "$f")): no spec.runtimeConfigRef
       Without one, Crossplane's pod defaults apply and they carry no
       seccompProfile, so the generated Deployment is denied by the fail-closed
       webhook. Add a DeploymentRuntimeConfig and reference it."
    check_drc "${ref}" "${provider}"
    checked=$((checked + 1))
  done < <(yq -r 'select(.kind == "Provider") | .metadata.name // ""' "$f")
done

[ "${checked}" -gt 0 ] || fail "no Provider found under ${drc_dir} — the gate asserted nothing"

printf 'PASS: %d Crossplane provider(s) resolve to a DeploymentRuntimeConfig with a complete pod securityContext.\n' "${checked}"
