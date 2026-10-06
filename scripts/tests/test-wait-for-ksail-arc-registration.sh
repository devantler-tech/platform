#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
mkdir -p "$scratch/bin"
cat >"$scratch/bin/timeout" <<'SH'
#!/usr/bin/env bash
shift
exec "$@"
SH
cat >"$scratch/bin/sleep" <<'SH'
#!/usr/bin/env bash
exit 124
SH
cat >"$scratch/bin/kubectl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[[ "$*" == *'--context admin@prod'* ]] || exit 92
case "$*" in
  *'get kustomization '*)
    jq -cn --arg revision "latest@$GROUP_DIGEST" '{metadata:{generation:1},status:{observedGeneration:1,lastAppliedRevision:$revision,conditions:[{type:"Ready",status:"True"}]}}' ;;
  *'get runnergroups.actions.github.m.upbound.io platform '*)
    [[ "$GROUP_CASE" != unreadable-group ]] || exit 1
    jq -cn --arg scenario "$GROUP_CASE" '{
      apiVersion:"actions.github.m.upbound.io/v1alpha1",kind:"RunnerGroup",
      metadata:{name:"platform",namespace:"arc-runners",uid:"00000000-0000-0000-0000-000000000002",generation:2,annotations:{"crossplane.io/external-name":"27"}},
      spec:{providerConfigRef:{name:"runtime-app",kind:"ProviderConfig"},forProvider:{name:"platform",visibility:"selected",allowsPublicRepositories:true,restrictedToWorkflows:false,selectedRepositoryIds:[737584922],selectedWorkflows:[]}},
      status:{atProvider:{id:"27",name:"platform",default:false,inherited:false,visibility:"selected",allowsPublicRepositories:true,restrictedToWorkflows:false,selectedRepositoryIds:[737584922],selectedWorkflows:[],
        runnersUrl:"https://api.github.com/orgs/devantler-tech/actions/runner-groups/27/runners",
        selectedRepositoriesUrl:"https://api.github.com/orgs/devantler-tech/actions/runner-groups/27/repositories"},
        conditions:[{type:"Ready",status:"True",observedGeneration:2},{type:"Synced",status:"True",observedGeneration:2}]}
      } |
      if $scenario=="all-repositories" then .status.atProvider.visibility="all"
      elif $scenario=="extra-repository" then .status.atProvider.selectedRepositoryIds+=[1]
      elif $scenario=="default-group" then .status.atProvider.default=true
      elif $scenario=="inherited-group" then .status.atProvider.inherited=true
      elif $scenario=="wrong-remote-org" then .status.atProvider.runnersUrl="https://api.github.com/orgs/other/actions/runner-groups/27/runners"
      elif $scenario=="stale-observation" then .status.conditions[1].observedGeneration=1
      elif $scenario=="missing-observation" then del(.status.conditions[1].observedGeneration)
      elif $scenario=="unbound-id" then .metadata.annotations["crossplane.io/external-name"]="1"
      else . end' ;;
  *'get autoscalingrunnerset.actions.github.com platform-linux '*)
    jq -cn --arg scenario "$GROUP_CASE" '{metadata:{generation:1,annotations:{"runner-scale-set-id":"12","actions.github.com/runner-group-name":"platform","actions.github.com/runner-scale-set-name":"platform-linux"}},
      spec:{githubConfigUrl:"https://github.com/devantler-tech",runnerGroup:"platform",runnerScaleSetName:"platform-linux"},
      status:{phase:"Running",observedGeneration:1}} |
      if $scenario=="wrong-registered-group" then .metadata.annotations["actions.github.com/runner-group-name"]="Default" else . end' ;;
  *) exit 93 ;;
esac
SH
chmod 700 "$scratch/bin/"*
for scenario in normal all-repositories extra-repository default-group inherited-group wrong-remote-org \
  stale-observation missing-observation unbound-id unreadable-group wrong-registered-group; do
  if PATH="$scratch/bin:$PATH" GROUP_DIGEST="sha256:$(printf 'a%.0s' {1..64})" GROUP_CASE="$scenario" \
    GITHUB_ACTIONS=true GITHUB_REPOSITORY=devantler-tech/platform \
    bash "$root/scripts/wait-for-ksail-arc-registration.sh" "sha256:$(printf 'a%.0s' {1..64})" \
    >"$scratch/out" 2>"$scratch/err"; then
    [[ "$scenario" == normal ]] || { echo "accepted $scenario"; exit 1; }
  else [[ "$scenario" != normal ]] || { cat "$scratch/err"; exit 1; }; fi
done
printf 'PASS: organization registration requires an observed KSail-only group\n'
