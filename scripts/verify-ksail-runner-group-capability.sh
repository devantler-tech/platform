#!/usr/bin/env bash
# Protected, metadata-only readback and server dry-run admission acceptance.
set -euo pipefail
umask 077
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
policy="$root/k8s/providers/hetzner/infrastructure/crossplane/managed-resource-activation-policy.yaml"
fail() { printf 'Runner-group capability: FAIL at %s\n' "$1" >&2; exit 1; }
activation=$(yq -o=json '.spec.activate' "$policy" 2>/dev/null) || fail activation-source
enabled=$(jq -er 'if type=="array" and all(.[]; type=="string") then
  map(select(.=="runnergroups.actions.github.m.upbound.io"))|length
  else error("invalid activation policy") end' <<<"$activation" 2>/dev/null) || fail activation-source
[[ "$enabled" == 0 || "$enabled" == 1 ]] || fail activation-source
if [[ "$enabled" == 0 ]]; then
  printf 'Runner-group capability inactive; no runtime access\n'
  exit 0
fi
[[ "${GITHUB_ACTIONS:-}" == true && "${GITHUB_REPOSITORY:-}" == devantler-tech/platform ]] || exit 1
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
kc() { timeout 35s kubectl --context admin@prod --request-timeout=30s "$@"; }
kc get crd runnergroups.actions.github.m.upbound.io -o json >"$scratch/crd.json" 2>"$scratch/error" || fail kind-read
jq -e '.spec.group=="actions.github.m.upbound.io" and .spec.scope=="Namespaced" and .spec.names.plural=="runnergroups" and
  any(.spec.versions[]; .name=="v1alpha1" and .served==true) and
  any(.status.conditions[]; .type=="Established" and .status=="True") and
  any(.status.conditions[]; .type=="NamesAccepted" and .status=="True")' "$scratch/crd.json" >/dev/null || fail kind-established
kc -n github-config get role github-config-managed-resources -o json >"$scratch/role.json" 2>"$scratch/error" || fail role-read
jq -e 'any(.rules[]; .apiGroups==["actions.github.m.upbound.io"] and
  (.resources|index("runnergroups"))!=null and (.verbs|index("create"))!=null and (.verbs|index("patch"))!=null) and
  all(.rules[]; (.resources|index("*"))==null)' "$scratch/role.json" >/dev/null || fail role-bounds
readonly caller=system:serviceaccount:github-config:github-config
for verb in create patch; do
  answer=$(kc --as="$caller" -n github-config auth can-i "$verb" runnergroups.actions.github.m.upbound.io 2>"$scratch/error") || fail tenant-grant
  [[ "$answer" == yes ]] || fail tenant-grant
done
# Server dry-run evaluates the actual CRD, tenant RBAC and installed admission
# policy, including when the separately reconciled group already exists.
yq -o=json '.' "$root/tests/restrict-github-runner-group/ksail-only/resources.yaml" >"$scratch/group.json"
kc --as="$caller" -n github-config apply --dry-run=server --server-side --field-manager=ksail-group-acceptance \
  --force-conflicts -f "$scratch/group.json" >/dev/null 2>"$scratch/error" || fail admitted-ksail-group
for scenario in all-repositories wildcard-external-name; do
  yq -o=json '.' "$root/tests/restrict-github-runner-group/$scenario/resources.yaml" >"$scratch/group.json"
  if kc --as="$caller" -n github-config apply --dry-run=server --server-side --field-manager=ksail-group-acceptance \
    --force-conflicts -f "$scratch/group.json" >/dev/null 2>"$scratch/error"; then fail intercepted-denial; fi
  rule=ksail-only-runner-group
  [[ "$scenario" != wildcard-external-name ]] || rule=tenant-cannot-rebind-group
  if ! grep -Fq 'restrict-github-runner-group' "$scratch/error" || ! grep -Fq "$rule" "$scratch/error"; then
    fail authenticated-denial
  fi
done
printf 'PASS: KSail-only runner-group CRD, tenant RBAC and intercepted admission denials\n'
