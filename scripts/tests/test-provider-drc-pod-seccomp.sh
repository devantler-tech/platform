#!/usr/bin/env bash
#
# Every Crossplane provider DeploymentRuntimeConfig must set seccompProfile at
# the POD level of its deployment template.
#
# validate-pod-security/require-seccomp-profile asserts the pod-level path, and
# Kyverno auto-generates that rule for pod controllers — so the Deployment that
# Crossplane's package manager builds from a DRC is validated against it. The
# add-security-context mutate policy cannot supply the value: its match/exclude
# carry pod selectors, so Kyverno generates no pod-controller rules for it and
# it never sees a Deployment. A DRC that sets seccompProfile only on the
# container is therefore denied by the fail-closed webhook at create time.
#
# The failure mode is silent and delayed: a running provider Deployment is not
# re-admitted, so the denial only surfaces when a revision bump forces a
# recreate — at which point the whole provider fleet goes to zero pods
# (platform#3470). This gate keeps that from regressing.
#
# It also pins the rest of the pod securityContext. Crossplane applies its own
# pod-level defaults ONLY when the DRC supplies no securityContext at all, so a
# DRC that declares seccompProfile alone silently drops runAsNonRoot and the
# uid/gid the provider images run as. The block has to stay complete.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly drc_dir="${root_dir}/k8s/providers/hetzner/infrastructure/crossplane"
readonly pod_path='.spec.deploymentTemplate.spec.template.spec.securityContext.seccompProfile.type'

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v yq >/dev/null 2>&1 || fail 'yq is required but not installed'
[ -d "${drc_dir}" ] || fail "provider directory not found: ${drc_dir}"

# Enumerate first and check the enumeration, so a failed find cannot render as
# "nothing to check" and pass vacuously.
files=()
while IFS= read -r f; do
  files+=("$f")
done < <(find "${drc_dir}" -maxdepth 1 -type f -name '*.yaml' | sort)
[ "${#files[@]}" -gt 0 ] || fail "no YAML files found under ${drc_dir}"

checked=0
for f in "${files[@]}"; do
  kind="$(yq -r '.kind // ""' "$f" 2>/dev/null || true)"
  [ "${kind}" = 'DeploymentRuntimeConfig' ] || continue

  name="$(yq -r '.metadata.name // ""' "$f")"
  [ -n "${name}" ] || fail "$(basename "$f"): DeploymentRuntimeConfig has no metadata.name"

  pod_value="$(yq -r "${pod_path} // \"\"" "$f")"
  case "${pod_value}" in
    RuntimeDefault | Localhost) ;;
    '')
      fail "${name} ($(basename "$f")): no pod-level seccompProfile.type at ${pod_path}
       Kyverno's require-seccomp-profile asserts the POD path, and no mutate
       policy reaches a Deployment, so this provider's Deployment is denied on
       its next revision bump. Add:
           template:
             spec:
               securityContext:
                 seccompProfile:
                   type: RuntimeDefault"
      ;;
    *)
      fail "${name} ($(basename "$f")): pod-level seccompProfile.type is '${pod_value}'; must be RuntimeDefault or Localhost"
      ;;
  esac

  # Crossplane's package-manager defaults are applied only when the DRC gives no
  # securityContext at all, so declaring one makes these our responsibility.
  for field in runAsNonRoot:true runAsUser:2000 runAsGroup:2000; do
    key="${field%%:*}"
    want="${field#*:}"
    got="$(yq -r ".spec.deploymentTemplate.spec.template.spec.securityContext.${key} // \"\"" "$f")"
    [ "${got}" = "${want}" ] || fail "${name} ($(basename "$f")): pod-level ${key} is '${got}', expected '${want}'
       Declaring a pod securityContext suppresses Crossplane's own defaults, so
       the DRC must restate them or the provider silently changes uid."
  done

  checked=$((checked + 1))
done

[ "${checked}" -gt 0 ] || fail "no DeploymentRuntimeConfig found under ${drc_dir} — the gate asserted nothing"

printf 'PASS: %d provider DeploymentRuntimeConfig(s) declare a complete pod securityContext.\n' "${checked}"
