#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
mkdir -p "$scratch/bin"
cat >"$scratch/bin/kubectl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  *'get crd runnergroups.actions.github.m.upbound.io'*)
    [[ "$GROUP_CASE" != missing-kind ]] || exit 1
    jq -n --arg scenario "$GROUP_CASE" '{spec:{group:"actions.github.m.upbound.io",scope:"Namespaced",names:{plural:"runnergroups"},versions:[{name:"v1alpha1",served:true}]},status:{conditions:[{type:"Established",status:(if $scenario=="stale-kind" then "False" else "True" end)},{type:"NamesAccepted",status:"True"}]}}' ;;
  *'get role github-config-managed-resources'*)
    printf '{"rules":[{"apiGroups":["actions.github.m.upbound.io"],"resources":["repositorypermissions","runnergroups"],"verbs":["get","list","watch","create","update","patch","delete"]}]}' ;;
  *'auth can-i create runnergroups.actions.github.m.upbound.io'*|*'auth can-i patch runnergroups.actions.github.m.upbound.io'*)
    [[ "$GROUP_CASE" != no-tenant-grant ]] || { printf 'no\n'; exit 1; }
    printf 'yes\n' ;;
  *'apply --dry-run=server'*)
    [[ "$*" == *'--as=system:serviceaccount:github-config:github-config'* ]] || exit 91
    input=''; while (( $# )); do if [[ "$1" == -f ]]; then input="$2"; break; fi; shift; done
    [[ -n "$input" && -f "$input" ]] || exit 92
    if jq -e '.spec.forProvider.visibility=="selected" and .spec.forProvider.selectedRepositoryIds==[737584922] and (.metadata.annotations["crossplane.io/external-name"] // "")==""' "$input" >/dev/null; then exit 0; fi
    [[ "$GROUP_CASE" != waived-policy ]] || exit 0
    if [[ "$GROUP_CASE" == unrelated-denial ]]; then echo 'forbidden by unrelated policy' >&2; exit 1; fi
    if jq -e '(.metadata.annotations["crossplane.io/external-name"] // "")!=""' "$input" >/dev/null; then
      echo 'blocked by restrict-github-runner-group/tenant-cannot-rebind-group' >&2
    else echo 'blocked by restrict-github-runner-group/ksail-only-runner-group' >&2; fi
    exit 1 ;;
  *) exit 93 ;;
esac
SH
chmod 700 "$scratch/bin/kubectl"
for scenario in normal missing-kind stale-kind no-tenant-grant waived-policy unrelated-denial; do
  if PATH="$scratch/bin:$PATH" GROUP_CASE="$scenario" GITHUB_ACTIONS=true GITHUB_REPOSITORY=devantler-tech/platform \
    bash "$root/scripts/verify-ksail-runner-group-capability.sh" >"$scratch/out" 2>"$scratch/err"; then
    [[ "$scenario" == normal ]] || { echo "accepted $scenario"; exit 1; }
    grep -q '^PASS: KSail-only runner-group' "$scratch/out"
  else [[ "$scenario" != normal ]] || { cat "$scratch/err"; exit 1; }; fi
done
printf 'PASS: live runner-group capability acceptance controls\n'
