#!/usr/bin/env bash
# Evaluate the actual direct Namespace lookup in the pinned admission engine.
set -euo pipefail
repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$repo_root"
policy=k8s/bases/infrastructure/cluster-policies/best-practices/restrict-arc-retirement.yaml
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
fail() { printf 'ARC retirement admission: FAIL %s\n' "$1" >&2; exit 1; }
[[ -f "$policy" ]] || fail missing-policy
jq -n '{apiVersion:"helm.toolkit.fluxcd.io/v2",kind:"HelmRelease",
  metadata:{name:"platform-runners",namespace:"arc-runners"},
  spec:{suspend:false,values:{minRunners:0,maxRunners:0}}}' >"$scratch/base.json"
cases=0
run_case() {
  local name=$1 operation=$2 flag=$3 change=$4 expected=$5
  local rule=${6:-fence-pool-retirement} old=${7:-'{}'} groups=${8:-'["system:serviceaccounts:flux-system"]'}
  if [[ "$flag" == owned ]]; then flag='{"phase":"fenced"}'; fi
  local dir="$scratch/$name"
  mkdir "$dir"
  cp "$policy" "$dir/policy.yaml"
  jq "$change" "$scratch/base.json" >"$dir/resource.json"
  jq -n --arg op "$operation" --argjson old "$old" '{apiVersion:"cli.kyverno.io/v1alpha1",kind:"Values",
    metadata:{name:"arc-retirement"},globalValues:{"request.operation":$op,"request.oldObject":$old}}' >"$dir/values.json"
  jq -n --argjson groups "$groups" '{apiVersion:"cli.kyverno.io/v1alpha1",kind:"UserInfo",metadata:{name:"arc-retirement"},
    userInfo:{username:"fixture",groups:$groups}}' >"$dir/userinfo.json"
  local code=200
  case "$flag" in 403|404|500) code=$flag ;; esac
  jq -n --slurpfile resource "$dir/resource.json" --arg rule "$rule" --arg flag "$flag" --arg op "$operation" --arg expected "$expected" --argjson code "$code" '{
    apiVersion:"cli.kyverno.io/v1alpha1",kind:"Test",metadata:{name:"arc-retirement"},
    policies:["policy.yaml"],resources:["resource.json"],variables:"values.json",userinfo:"userinfo.json",
    apiCallResponses:[{urlPath:"/api/v1/namespaces/arc-runners",method:"GET",
      response:{statusCode:$code,body:{apiVersion:"v1",kind:"Namespace",metadata:{
        name:"arc-runners",annotations:(if $flag=="missing" then {} else {"platform.devantler.tech/arc-retirement":$flag} end)}}}}],
    results:[{kind:$resource[0].kind,policy:"restrict-arc-retirement",rule:$rule,
      resources:[(($resource[0].metadata.namespace // "") + "/" + $resource[0].metadata.name | ltrimstr("/"))],operation:$op,result:$expected}]
    }' >"$dir/kyverno-test.yaml"
  cases=$((cases+1))
}
run_case recreate-zero CREATE owned '.' fail
run_case recreate-active CREATE owned '.spec.values.maxRunners=1' fail
run_case empty-journal CREATE '' '.' fail
run_case malformed-journal CREATE '{not-json' '.' fail
run_case reopen-admission UPDATE owned '.spec.values.maxRunners=1' fail
run_case unchanged-existing-violation UPDATE owned '.spec.values.maxRunners=1' fail fence-pool-retirement '{"spec":{"suspend":false,"values":{"minRunners":0,"maxRunners":1}}}'
run_case omitted-maximum UPDATE owned 'del(.spec.values.maxRunners)' fail
run_case omitted-minimum UPDATE owned 'del(.spec.values.minRunners)' fail
run_case raised-minimum UPDATE owned '.spec.values.minRunners=1' fail
run_case suspend-current UPDATE owned '.spec.suspend=true | .spec.values.maxRunners=1' pass
run_case drain-reconciling UPDATE owned '.' pass
run_case ordinary-source CREATE missing '.spec.values.maxRunners=1' skip
run_case forbidden-lookup CREATE 403 '.' error
run_case missing-namespace CREATE 404 '.' error
run_case failed-lookup CREATE 500 '.' error
run_case other-release CREATE owned '.metadata.name="other-release"' skip
run_case other-namespace CREATE owned '.metadata.namespace="other-namespace"' skip
ars='{apiVersion:"actions.github.com/v1alpha1",kind:"AutoscalingRunnerSet",metadata:{name:"platform-linux",namespace:"arc-runners"},spec:{minRunners:0,maxRunners:0}}'
run_case recreate-scale-set CREATE owned "$ars" fail fence-scale-set-retirement
run_case revert-scale-set UPDATE owned "$ars | .spec.maxRunners=1" fail fence-scale-set-retirement
run_case drain-scale-set UPDATE owned "$ars" pass fence-scale-set-retirement
run_case omitted-scale-bound UPDATE owned "$ars | del(.spec.maxRunners)" fail fence-scale-set-retirement
run_case ordinary-scale-set CREATE missing "$ars | .spec.maxRunners=1" skip fence-scale-set-retirement
listener='{apiVersion:"actions.github.com/v1alpha1",kind:"AutoscalingListener",metadata:{name:"platform-linux-listener",namespace:"arc-systems"},spec:{autoscalingRunnerSetNamespace:"arc-runners",autoscalingRunnerSetName:"platform-linux"}}'
run_case create-zero-listener CREATE owned "$listener" pass fence-listener-retirement
run_case create-active-listener CREATE owned "$listener | .spec.maxRunners=1" fail fence-listener-retirement
run_case reopen-listener UPDATE owned "$listener | .spec.maxRunners=1" fail fence-listener-retirement
run_case neighboring-listener CREATE owned "$listener | .spec.autoscalingRunnerSetName=\"other-pool\" | .spec.maxRunners=1" skip fence-listener-retirement
old_listener='{"apiVersion":"actions.github.com/v1alpha1","kind":"AutoscalingListener","metadata":{"name":"platform-linux-listener","namespace":"arc-systems","deletionTimestamp":"2026-10-07T00:00:00Z"},"spec":{"autoscalingRunnerSetNamespace":"arc-runners","autoscalingRunnerSetName":"platform-linux","maxRunners":1}}'
run_case deleting-listener-cleanup UPDATE owned "$listener | .metadata.deletionTimestamp=\"2026-10-07T00:00:00Z\" | .spec.maxRunners=1" pass fence-listener-retirement "$old_listener"
run_case deleting-listener-reopen UPDATE owned "$listener | .metadata.deletionTimestamp=\"2026-10-07T00:00:00Z\" | .spec.maxRunners=2" fail fence-listener-retirement "$old_listener"
listener_pod='{apiVersion:"v1",kind:"Pod",metadata:{name:"listener",namespace:"arc-systems",labels:{"actions.github.com/scale-set-name":"platform-linux","actions.github.com/scale-set-namespace":"arc-runners","app.kubernetes.io/component":"runner-scale-set-listener"}}}'
for phase in uninstalled credential-removing absent restored; do
  run_case "terminal-listener-$phase" CREATE "{\"phase\":\"$phase\"}" "$listener" fail fence-listener-retirement
  run_case "terminal-listener-pod-$phase" CREATE "{\"phase\":\"$phase\"}" "$listener_pod" fail fence-retired-listener-pod
done
run_case drain-listener-pod CREATE '{"phase":"quiescing"}' "$listener_pod" pass fence-retired-listener-pod
run_case neighbor-pool-listener-pod CREATE '{"phase":"restored"}' "$listener_pod | .metadata.labels[\"actions.github.com/scale-set-name\"]=\"neighbor\"" skip fence-retired-listener-pod
run_case legacy-listener-pod CREATE '{"phase":"restored"}' "$listener_pod | .metadata.labels[\"actions.github.com/scale-set-namespace\"]=\"arc-ksail-analysis\"" skip fence-retired-listener-pod
run_case runner-job-pod CREATE '{"phase":"restored"}' "$listener_pod | .metadata.namespace=\"arc-runners\" | .metadata.labels[\"app.kubernetes.io/component\"]=\"runner\"" skip fence-retired-listener-pod
eso='{apiVersion:"external-secrets.io/v1",kind:"ExternalSecret",metadata:{name:"arc-github-app",namespace:"arc-runners"},spec:{}}'
run_case recreate-credential CREATE owned "$eso" fail fence-credential-recreation
run_case ordinary-credential CREATE missing "$eso" skip fence-credential-recreation
run_case update-credential UPDATE owned "$eso" pass fence-credential-recreation '{"spec":{}}'
run_case rewrite-credential UPDATE owned "$eso | .spec.target={name:\"other\"}" fail fence-credential-recreation '{"spec":{"target":{"name":"original"}}}'
run_case other-credential CREATE owned "$eso | .metadata.name=\"other-credential\"" skip fence-credential-recreation
secret='{apiVersion:"v1",kind:"Secret",metadata:{name:"arc-github-app",namespace:"arc-runners"}}'
for phase in fenced drained quiescing quiesced uninstalling uninstalled; do
  run_case "finalizer-credential-$phase" CREATE "{\"phase\":\"$phase\"}" "$secret" pass fence-retired-secret
done
for phase in credential-removing absent restored unknown; do
  run_case "late-credential-$phase" CREATE "{\"phase\":\"$phase\"}" "$secret" fail fence-retired-secret
done
run_case ordinary-secret CREATE missing "$secret" pass fence-retired-secret
run_case empty-secret-journal CREATE '' "$secret" fail fence-retired-secret
run_case incomplete-secret-journal CREATE '{}' "$secret" fail fence-retired-secret
run_case malformed-secret-journal CREATE '{broken' "$secret" error fence-retired-secret
run_case secret-update UPDATE '{"phase":"absent"}' "$secret" skip fence-retired-secret
run_case neighboring-secret CREATE '{"phase":"absent"}' "$secret | .metadata.name=\"other-secret\"" skip fence-retired-secret
ns='{apiVersion:"v1",kind:"Namespace",metadata:{name:"arc-runners",annotations:{"platform.devantler.tech/arc-retirement":""}}}'
old='{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"arc-runners","annotations":{"platform.devantler.tech/arc-retirement":""}}}'
run_case preserve-empty-journal UPDATE owned "$ns" pass preserve-retirement-journal "$old"
run_case remove-empty-journal UPDATE owned "$ns | .metadata.annotations={}" fail preserve-retirement-journal "$old"
run_case change-journal UPDATE owned "$ns | .metadata.annotations[\"platform.devantler.tech/arc-retirement\"]=\"changed\"" fail preserve-retirement-journal "$old"
run_case plant-journal UPDATE owned "$ns" fail preserve-retirement-journal '{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"arc-runners","annotations":null}}'
run_case admin-clear-journal UPDATE owned "$ns | .metadata.annotations={}" pass preserve-retirement-journal "$old" '["system:masters"]'
run_case neighboring-namespace UPDATE owned "$ns | .metadata.name=\"other-namespace\" | .metadata.annotations={}" skip preserve-retirement-journal "$old"
run_case delete-retiring-namespace DELETE owned "$ns" fail preserve-retiring-namespace "$old"
run_case delete-unfenced-namespace DELETE missing "$ns | .metadata.annotations={}" pass preserve-retiring-namespace '{}'
bash scripts/validate-kyverno-fixture-evaluation.sh "$scratch"
printf 'ARC retirement admission: %s evaluated engine cases passed.\n' "$cases"
