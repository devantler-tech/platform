#!/usr/bin/env bash
set -euo pipefail

# `kyverno test` reports a rule whose namespaceSelector matches nothing as
# "Excluded", and an Excluded result satisfies an expected `skip`. That makes the
# fixture pass VACUOUSLY if the namespace labels in values.yaml ever go missing:
# measured 2026-08-29, stripping the labels left the suite reporting 6 passed /
# 0 failed while the opt-in rules never fired once. The default-off control is
# exactly the assertion that trap disguises, so it cannot be the only gate.
#
# This gate reads the mutated resources themselves and asserts all three states
# positively, so a rule that never fires fails here even when `kyverno test` is
# green.

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
policy="${repo_root}/k8s/bases/infrastructure/cluster-policies/best-practices/add-security-context.yaml"
fixtures="${repo_root}/tests/add-baseline-context/resources.yaml"
values="${repo_root}/tests/add-baseline-context/values.yaml"
out_dir="$(mktemp -d)"
trap 'rm -rf "${out_dir}"' EXIT

if ! kyverno apply "${policy}" \
  --resource "${fixtures}" \
  --values-file "${values}" \
  --output "${out_dir}" \
  --remove-color >"${out_dir}/apply.log" 2>&1; then
  echo "::error::kyverno apply failed for add-security-context"
  sed -n '1,60p' "${out_dir}/apply.log"
  exit 1
fi

fail=0
check() {
  local file="$1" expr="$2" want="$3" what="$4" got
  if [ ! -f "${out_dir}/${file}" ]; then
    echo "::error::${file} was not emitted; the fixture did not render"
    fail=1
    return
  fi
  got="$(yq "select(document_index == 0) | ${expr}" "${out_dir}/${file}")"
  if [ "${got}" != "${want}" ]; then
    echo "::error::${what}: expected '${want}', got '${got}'"
    fail=1
  fi
}

# ON state — an opted-in namespace receives both fields, pod level and on every
# container and initContainer. These are the assertions the vacuity trap hides.
check plain-mutated.yaml '.spec.securityContext.fsGroupChangePolicy' \
  OnRootMismatch "opted-in pod did not receive fsGroupChangePolicy"
check plain-mutated.yaml '.spec.containers[0].securityContext.seLinuxOptions.level' \
  s0 "opted-in container did not receive seLinuxOptions"
check plain-mutated.yaml '.spec.initContainers[0].securityContext.seLinuxOptions.level' \
  s0 "opted-in initContainer did not receive seLinuxOptions"

# A pod with no initContainers at all is the common shape; the foreach over
# `spec.initContainers[]` must tolerate the field being absent rather than
# erroring, which would drop the container mutation with it.
check no-init-mutated.yaml '.spec.securityContext.fsGroupChangePolicy' \
  OnRootMismatch "pod without initContainers did not receive fsGroupChangePolicy"
check no-init-mutated.yaml '.spec.containers[0].securityContext.seLinuxOptions.level' \
  s0 "pod without initContainers did not receive seLinuxOptions"

# Conditional anchors — a workload that sets its own values keeps them.
check preset-mutated.yaml '.spec.securityContext.fsGroupChangePolicy' \
  Always "preset fsGroupChangePolicy was overwritten"
check preset-mutated.yaml '.spec.containers[0].securityContext.seLinuxOptions.level' \
  s9 "preset container seLinuxOptions was overwritten"
check preset-mutated.yaml '.spec.initContainers[0].securityContext.seLinuxOptions.level' \
  s9 "preset initContainer seLinuxOptions was overwritten"

# A container that already carries a PARTIAL seLinuxOptions object — a sibling
# field set, `level` absent. Anchoring the whole object would see it present and
# skip, leaving `level` unset while the rule still reports as applied; anchoring
# the leaf fills the gap and preserves the sibling. Measured 2026-08-29: with the
# object-level anchor this came back `{user: system_u}` with no level at all.
check partial-mutated.yaml '.spec.containers[0].securityContext.seLinuxOptions.level' \
  s0 "partial container seLinuxOptions did not receive the missing level"
check partial-mutated.yaml '.spec.containers[0].securityContext.seLinuxOptions.user' \
  system_u "partial container seLinuxOptions lost its pre-existing user"
check partial-mutated.yaml '.spec.initContainers[0].securityContext.seLinuxOptions.level' \
  s0 "partial initContainer seLinuxOptions did not receive the missing level"
check partial-mutated.yaml '.spec.initContainers[0].securityContext.seLinuxOptions.user' \
  system_u "partial initContainer seLinuxOptions lost its pre-existing user"

# OFF state — a namespace without the opt-in label is untouched. The rule ships
# default-off; if this regresses, merging it silently mutates twelve namespaces.
check unlabelled-mutated.yaml '.spec.securityContext.fsGroupChangePolicy' \
  null "unlabelled namespace was mutated at the pod level"
check unlabelled-mutated.yaml '.spec.containers[0].securityContext.seLinuxOptions' \
  null "unlabelled namespace had seLinuxOptions injected"

# A pod whose POD-level securityContext already declares SELinux options, with a
# container carrying none. A container-level seLinuxOptions REPLACES the pod-level
# struct wholesale, so injecting `level` would override the operator's level AND
# drop the sibling `user` for that container. Measured 2026-08-29 without the
# foreach precondition: the pod declared `{user: system_u, level: s9}` and the
# container came back `{level: s0}`. The first assertion below is the regression;
# the fsGroupChangePolicy one proves the fixture is processed at all, so a rule
# that silently stopped matching cannot make the other three pass vacuously.
check podlevel-mutated.yaml '.spec.containers[0].securityContext.seLinuxOptions' \
  null "pod-level SELinux options were overridden by a container-level injection"
check podlevel-mutated.yaml '.spec.securityContext.seLinuxOptions.level' \
  s9 "pod-level seLinuxOptions.level was not preserved"
check podlevel-mutated.yaml '.spec.securityContext.seLinuxOptions.user' \
  system_u "pod-level seLinuxOptions.user was not preserved"
check podlevel-mutated.yaml '.spec.securityContext.fsGroupChangePolicy' \
  OnRootMismatch "pod-level rule did not fire on the podlevel fixture"

if [ "${fail}" -ne 0 ]; then
  exit 1
fi

echo "Baseline-context opt-in mutation: injected when labelled, preserved when preset, inert when not."
