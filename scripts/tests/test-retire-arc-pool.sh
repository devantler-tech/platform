#!/usr/bin/env bash
# Exercise the real native orchestrator and Go planner against an isolated fake
# API. No local kubeconfig, network, Secret value or production mutation is used.
set -euo pipefail
repo_root=$(cd "$(dirname "$0")/../.." && pwd)
cd "$repo_root"
scratch=$(mktemp -d)
cleanup() {
  local code=$?
  if [[ "$code" != 0 && -n "${ARC_TEST_STATE:-}" ]]; then
    printf 'Native fixture failed: %s (exit %s)\n' "${ARC_TEST_STATE##*/}" "$code" >&2
    if [[ -f "$ARC_TEST_STATE/error" ]]; then cat "$ARC_TEST_STATE/error" >&2; fi
  fi
  rm -rf "$scratch"
}
trap cleanup EXIT
mkdir "$scratch/bin"
go build -o "$scratch/planner" ./scripts/retire-arc-pool
export ARC_TEST_PLANNER="$scratch/planner" ARC_TEST_REPO="$repo_root"
cat >"$scratch/bin/go" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
[[ "$#" == 4 && "$1" == build && "$2" == -o && "$4" == ./scripts/retire-arc-pool ]]
cp "$ARC_TEST_PLANNER" "$3"
MOCK
cat >"$scratch/bin/kubectl" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == --context && "$2" == admin@prod ]]
shift 2
printf '%s\n' "$*" >>"$ARC_TEST_STATE/commands"
ns=''; if [[ "$1" == -n ]]; then ns=$2; shift 2; fi
command=$1; shift
if [[ "$command" == auth ]]; then
  [[ "$1" == whoami ]]
  jq -n --argjson admin "$ARC_TEST_ADMIN" '{kind:"SelfSubjectReview",status:{userInfo:{groups:(if $admin then ["system:masters"] else ["readers"] end)}}}'
  exit
fi
if [[ "$command" == proxy ]]; then
  printf 'Starting to serve on 127.0.0.1:19001\n'
  printf '%s\n' "$$" >"$ARC_TEST_STATE/proxy-pid"
  exec sleep 300
fi
kind=${1:-}; name=${2:-}
if [[ "$command" == get ]]; then
  current_phase=$(jq -r '.metadata.annotations."platform.devantler.tech/arc-retirement" // "{}"|fromjson|.phase//""' "$ARC_TEST_STATE/ns.json")
  if [[ "$kind" == helmrelease && "${ARC_TEST_LATE:-}" == hr && "$current_phase" == fenced && -s "$ARC_TEST_STATE/late-hr.json" ]]; then
    mv "$ARC_TEST_STATE/late-hr.json" "$ARC_TEST_STATE/hr.json"
  fi
  if [[ "$kind" == externalsecret && "${ARC_TEST_LATE:-}" == eso && "$current_phase" == absent && -s "$ARC_TEST_STATE/late-eso.json" ]]; then
    mv "$ARC_TEST_STATE/late-eso.json" "$ARC_TEST_STATE/eso.json"
  fi
  if [[ "$kind" == autoscalingrunnerset && "${ARC_TEST_LATE:-}" == ars && "$current_phase" == uninstalling && ! -s "$ARC_TEST_STATE/hr.json" && -s "$ARC_TEST_STATE/late-ars.json" ]]; then
    mv "$ARC_TEST_STATE/late-ars.json" "$ARC_TEST_STATE/ars.json"
  fi
  if [[ "$kind" == autoscalingrunnerset && "$ARC_TEST_REPLACE" == ars && "$current_phase" == uninstalling ]]; then
    jq '.metadata.uid="replacement-ars"' "$ARC_TEST_STATE/ars.json" >"$ARC_TEST_STATE/next.json"; mv "$ARC_TEST_STATE/next.json" "$ARC_TEST_STATE/ars.json"
  fi
  if [[ "$kind" == helmrelease && "$ARC_TEST_REPLACE" == hr && "$(jq -r '.metadata.annotations."platform.devantler.tech/arc-retirement" // "{}"|fromjson|.phase//""' "$ARC_TEST_STATE/ns.json")" == uninstalling ]]; then
    jq '.metadata.uid="replacement-hr"' "$ARC_TEST_STATE/hr.json" >"$ARC_TEST_STATE/next.json"; mv "$ARC_TEST_STATE/next.json" "$ARC_TEST_STATE/hr.json"
  fi
  if [[ "$kind" == externalsecret && "$ARC_TEST_REPLACE" == eso && "$(jq -r '.metadata.annotations."platform.devantler.tech/arc-retirement" // "{}"|fromjson|.phase//""' "$ARC_TEST_STATE/ns.json")" == credential-removing ]]; then
    jq '.metadata.uid="replacement-eso"' "$ARC_TEST_STATE/eso.json" >"$ARC_TEST_STATE/next.json"; mv "$ARC_TEST_STATE/next.json" "$ARC_TEST_STATE/eso.json"
  fi
  case "$kind" in
    namespace) cat "$ARC_TEST_STATE/ns.json" ;;
    helmrelease) [[ "$ARC_TEST_READ_FAILURE" != true ]] || exit 1; [[ -s "$ARC_TEST_STATE/hr.json" ]] && cat "$ARC_TEST_STATE/hr.json" ;;
    externalsecret) [[ -s "$ARC_TEST_STATE/eso.json" ]] && cat "$ARC_TEST_STATE/eso.json" ;;
    autoscalingrunnerset) [[ -s "$ARC_TEST_STATE/ars.json" ]] && cat "$ARC_TEST_STATE/ars.json" ;;
    ephemeralrunnerset) [[ -s "$ARC_TEST_STATE/ers.json" ]] && cat "$ARC_TEST_STATE/ers.json" ;;
    autoscalinglisteners) jq -n --slurpfile items "$ARC_TEST_STATE/listeners.json" '{kind:"AutoscalingListenerList",metadata:{},items:$items[0]}' ;;
    deployment) cat "$ARC_TEST_STATE/controller.json" ;;
    replicasets) jq -n --slurpfile items "$ARC_TEST_STATE/replicasets.json" '{items:$items[0]}' ;;
    pods) [[ "$ns" == arc-systems ]]; if [[ "$*" == *app.kubernetes.io/instance=arc-controller* ]]; then jq -c '.[]' "$ARC_TEST_STATE/controller-pods.json"; else jq -c '.[]' "$ARC_TEST_STATE/listener-pods.json"; fi ;;
    clusterpolicy) yq -o=json '.' "$ARC_TEST_REPO/k8s/bases/infrastructure/cluster-policies/best-practices/restrict-arc-retirement.yaml" | jq '.spec.admission=true | .spec.emitWarning=false | .spec.validationFailureAction="Audit" | .spec.rules |= map(.skipBackgroundRequests=true)' ;;
    ocirepository) cat "$ARC_TEST_STATE/source.json" ;;
    kustomizations) touch "$ARC_TEST_STATE/source-proof-read";jq -n --slurpfile layers "$ARC_TEST_STATE/layers.json" '{items:$layers[0]}' ;;
    kustomization) jq --arg name "$name" '.[]|select(.metadata.name==$name)' "$ARC_TEST_STATE/layers.json" ;;
    *) exit 2 ;;
  esac
  exit 0
fi
if [[ "$command" == create ]]; then
  [[ "$kind" == --dry-run=server && "$name" == -f ]]
  body=$3; probe_kind=$(jq -r '.kind' "$body")
  [[ "$ARC_TEST_INERT_ADMISSION" != true ]] || { printf 'AlreadyExists\n' >&2; exit 1; }
  current_phase=$(jq -r '.metadata.annotations."platform.devantler.tech/arc-retirement"|fromjson|.phase' "$ARC_TEST_STATE/ns.json")
  if [[ "$probe_kind" == Secret && "$current_phase" != credential-removing && "$current_phase" != absent && "$current_phase" != restored ]]; then
    if [[ -s "$ARC_TEST_STATE/eso.json" ]]; then printf 'AlreadyExists\n' >&2; exit 1; fi
    printf '{}\n'; exit
  fi
  if [[ "$probe_kind" == Pod && "$current_phase" != uninstalled && "$current_phase" != credential-removing && "$current_phase" != absent && "$current_phase" != restored ]]; then printf '{}\n'; exit; fi
  if [[ "$probe_kind" == AutoscalingListener ]] && jq -e '.spec.maxRunners==0' "$body" >/dev/null &&
    [[ "$current_phase" != uninstalled && "$current_phase" != credential-removing && "$current_phase" != absent && "$current_phase" != restored ]]; then printf '{}\n'; exit; fi
  case "$probe_kind" in HelmRelease) rule=fence-pool-retirement ;; AutoscalingRunnerSet) rule=fence-scale-set-retirement ;;
    ExternalSecret) rule=fence-credential-recreation ;; AutoscalingListener) rule=fence-listener-retirement ;;
    Secret) rule=fence-retired-secret ;; Pod) rule=fence-retired-listener-pod ;; *) exit 2 ;; esac
  printf 'restrict-arc-retirement/%s denied\n' "$rule" >&2; exit 1
fi
if [[ "$command" == patch ]]; then
  if [[ "$*" == *--dry-run=server* ]]; then
    while [[ "$#" -gt 0 ]]; do if [[ "$1" == --patch-file ]]; then patch=$2; break; fi; shift; done
    if [[ "$kind" == externalsecret ]]; then printf 'restrict-arc-retirement/fence-credential-recreation denied\n' >&2; exit 1; fi
    if jq -e '.[-1].value==1' "$patch" >/dev/null; then printf 'restrict-arc-retirement/fence-pool-retirement denied\n' >&2; exit 1; fi
    printf '{}\n'; exit
  fi
  patch=''; while [[ "$#" -gt 0 ]]; do if [[ "$1" == --patch-file ]]; then patch=$2; break; fi; shift; done
  [[ -n "$patch" ]]
  case "$kind" in
    namespace) file=ns.json ;;
    helmrelease) file=hr.json ;;
    externalsecret) file=eso.json ;;
    autoscalingrunnerset) file=ars.json ;;
    deployment) file=controller.json ;;
    kustomization) file=layer.json; jq --arg name "$name" '.[]|select(.metadata.name==$name)' "$ARC_TEST_STATE/layers.json" >"$ARC_TEST_STATE/$file" ;;
    *) exit 2 ;;
  esac
  jq --slurpfile patch "$patch" '
    reduce $patch[0][] as $p (.;
      ($p.path | split("/")[1:] | map(gsub("~1";"/")|gsub("~0";"~"))) as $path |
      if $p.op=="test" then if getpath($path)==$p.value then . else error("CAS failed") end
      elif $p.op=="add" or $p.op=="replace" then setpath($path;$p.value)
      else error("unsupported mutation") end)' "$ARC_TEST_STATE/$file" >"$ARC_TEST_STATE/next.json"
  mv "$ARC_TEST_STATE/next.json" "$ARC_TEST_STATE/$file"
  case "$kind" in
    deployment)
      counter=$(cat "$ARC_TEST_STATE/controller-count" 2>/dev/null || printf 0);counter=$((counter+1))
      printf '%s\n' "$counter" >"$ARC_TEST_STATE/controller-count"
      jq --arg uid "new-$counter" 'map(.uid=$uid)' "$ARC_TEST_STATE/new-controller-pods.json" >"$ARC_TEST_STATE/controller-pods.json" ;;
    kustomization) if [[ "$ARC_TEST_NO_ACK" != true ]]; then jq '.status.lastHandledReconcileAt=.metadata.annotations."reconcile.fluxcd.io/requestedAt"' "$ARC_TEST_STATE/$file" >"$ARC_TEST_STATE/next.json"; mv "$ARC_TEST_STATE/next.json" "$ARC_TEST_STATE/$file"; fi; jq --arg name "$name" --slurpfile layer "$ARC_TEST_STATE/$file" 'map(if .metadata.name==$name then $layer[0] else . end)' "$ARC_TEST_STATE/layers.json" >"$ARC_TEST_STATE/next.json"; mv "$ARC_TEST_STATE/next.json" "$ARC_TEST_STATE/layers.json" ;;
  esac
  printf '{}\n'; exit
fi
if [[ "$command" == delete ]]; then
  [[ "$kind" == --raw ]]; path=$name; shift 2; [[ "$1" == -f ]]; body=$2
  case "$path" in
    /apis/helm.toolkit.fluxcd.io/v2/namespaces/arc-runners/helmreleases/platform-runners) file=hr.json ;;
    /apis/external-secrets.io/v1/namespaces/arc-runners/externalsecrets/arc-github-app) file=eso.json ;;
    /apis/actions.github.com/v1alpha1/namespaces/arc-runners/autoscalingrunnersets/platform-linux) file=ars.json ;;
    *) exit 2 ;;
  esac
  jq -e --slurpfile body "$body" '.metadata.uid==$body[0].preconditions.uid and .metadata.resourceVersion==$body[0].preconditions.resourceVersion and $body[0].propagationPolicy=="Foreground"' "$ARC_TEST_STATE/$file" >/dev/null
  if [[ "$file" == hr.json ]]; then
    : >"$ARC_TEST_STATE/hr.json"; : >"$ARC_TEST_STATE/ars.json"; : >"$ARC_TEST_STATE/ers.json"
    printf '[]\n' >"$ARC_TEST_STATE/listeners.json"; printf '[]\n' >"$ARC_TEST_STATE/listener-pods.json"
  elif [[ "$file" == ars.json ]]; then
    : >"$ARC_TEST_STATE/ars.json"; : >"$ARC_TEST_STATE/ers.json"
    printf '[]\n' >"$ARC_TEST_STATE/listeners.json"; printf '[]\n' >"$ARC_TEST_STATE/listener-pods.json"
  else : >"$ARC_TEST_STATE/eso.json"; fi
  printf '{}\n'; exit
fi
exit 2
MOCK
cat >"$scratch/bin/curl" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
[[ "$ARC_TEST_METADATA_FAILURE" != true ]] || exit 22
output=''; url=''
while [[ "$#" -gt 0 ]]; do
  case "$1" in --output) output=$2; shift 2 ;; http://127.0.0.1:19001/*) url=$1; shift ;; *) shift ;; esac
done
[[ -n "$url" ]]
printf '%s\n' "$url" >>"$ARC_TEST_STATE/metadata-paths"
if [[ "$url" == */secrets/arc-github-app ]]; then
  if [[ -s "$ARC_TEST_STATE/eso.json" ]]; then
    jq -n '{apiVersion:"meta.k8s.io/v1",kind:"PartialObjectMetadata",metadata:{name:"arc-github-app",namespace:"arc-runners",uid:"secret"}}' >"$output"; printf 200
  else jq -n '{kind:"Status",reason:"NotFound",code:404}' >"$output"; printf 404; fi
else
  jq -n '{apiVersion:"meta.k8s.io/v1",kind:"PartialObjectMetadataList",metadata:{resourceVersion:"9"},items:null}'
fi
MOCK
cat >"$scratch/bin/gh" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$ARC_TEST_STATE/github-commands"
printf '{"id":456,"run_attempt":1,"status":"in_progress","head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","repository":{"full_name":"devantler-tech/platform"}}\n'
MOCK
chmod +x "$scratch/bin/go" "$scratch/bin/kubectl" "$scratch/bin/curl" "$scratch/bin/gh"
export PATH="$scratch/bin:$PATH"
export GITHUB_ACTIONS=true GITHUB_REPOSITORY=devantler-tech/platform GITHUB_REF=refs/heads/main
export GITHUB_RUN_ID=123 GITHUB_RUN_ATTEMPT=1 GITHUB_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
export GH_TOKEN=fixture PLATFORM_MANIFEST_DIGEST=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
fixture() {
  export ARC_TEST_STATE="$scratch/state-$1" ARC_TEST_ADMIN=true ARC_TEST_READ_FAILURE=false ARC_TEST_METADATA_FAILURE=false ARC_TEST_REPLACE='' ARC_TEST_LATE='' ARC_TEST_INERT_ADMISSION=false ARC_TEST_NO_ACK=false
  mkdir "$ARC_TEST_STATE"
  jq -n '{apiVersion:"v1",kind:"Namespace",metadata:{name:"arc-runners",uid:"ns-1",resourceVersion:"1",annotations:{foreign:"preserve"}}}' >"$ARC_TEST_STATE/ns.json"
  jq -n '{apiVersion:"helm.toolkit.fluxcd.io/v2",kind:"HelmRelease",metadata:{name:"platform-runners",namespace:"arc-runners",uid:"hr-1",resourceVersion:"2"},spec:{suspend:false,chartRef:{kind:"OCIRepository",name:"platform-runners"},values:{runnerScaleSetName:"platform-linux",githubConfigUrl:"https://github.com/devantler-tech",githubConfigSecret:"arc-github-app",runnerGroup:"platform",minRunners:0,maxRunners:1}}}' >"$ARC_TEST_STATE/hr.json"
  jq -n '{apiVersion:"external-secrets.io/v1",kind:"ExternalSecret",metadata:{name:"arc-github-app",namespace:"arc-runners",uid:"eso-1",resourceVersion:"3"},spec:{target:{name:"arc-github-app",creationPolicy:"Owner"},secretStoreRef:{name:"openbao",kind:"SecretStore"}}}' >"$ARC_TEST_STATE/eso.json"
  : >"$ARC_TEST_STATE/ars.json"; : >"$ARC_TEST_STATE/ers.json"
  printf '[]\n' >"$ARC_TEST_STATE/listeners.json"; printf '[]\n' >"$ARC_TEST_STATE/listener-pods.json"
  jq -n '{metadata:{name:"arc-controller",namespace:"arc-systems",uid:"deployment",resourceVersion:"4",generation:3},spec:{replicas:1,template:{metadata:{},spec:{containers:[{name:"manager",command:["/manager"],args:["--watch-single-namespace=arc-runners","--auto-scaling-runner-set-only"]}]} }},status:{observedGeneration:3,replicas:1,updatedReplicas:1,readyReplicas:1,availableReplicas:1}}' >"$ARC_TEST_STATE/controller.json"
  jq -n '[{uid:"old-1",ownerUID:"old-rs",ownerKind:"ReplicaSet",phase:"Running",ready:"True",deleting:""}]' >"$ARC_TEST_STATE/controller-pods.json"
  jq -n '[{uid:"new-1",ownerUID:"new-rs",ownerKind:"ReplicaSet",phase:"Running",ready:"True",deleting:""}]' >"$ARC_TEST_STATE/new-controller-pods.json"
  jq -n '[{metadata:{uid:"new-rs",ownerReferences:[{kind:"Deployment",uid:"deployment",controller:true}]}}]' >"$ARC_TEST_STATE/replicasets.json"
  jq -n --arg digest "$PLATFORM_MANIFEST_DIGEST" '{metadata:{name:"flux-system",namespace:"flux-system",uid:"source",generation:2},spec:{verify:{provider:"cosign"}},status:{observedGeneration:2,artifact:{digest:$digest},conditions:[{type:"Ready",status:"True",observedGeneration:2},{type:"SourceVerified",status:"True",observedGeneration:2}]}}' >"$ARC_TEST_STATE/source.json"
  jq -n --arg digest "$PLATFORM_MANIFEST_DIGEST" '["flux-system","infrastructure-controllers","infrastructure"]|map({metadata:{name:.,namespace:"flux-system",uid:.,resourceVersion:"6",generation:2},spec:{sourceRef:{kind:"OCIRepository",name:"flux-system"}},status:{observedGeneration:2,lastAppliedRevision:("latest@"+$digest),lastAttemptedRevision:("latest@"+$digest),conditions:[{type:"Ready",status:"True",observedGeneration:2}]}})' >"$ARC_TEST_STATE/layers.json"
}
baseline_fixture() {
  fixture "$1"
  jq '.spec.values.maxRunners=0' "$ARC_TEST_STATE/hr.json" >"$ARC_TEST_STATE/next.json";mv "$ARC_TEST_STATE/next.json" "$ARC_TEST_STATE/hr.json"
  jq -n '{apiVersion:"actions.github.com/v1alpha1",kind:"AutoscalingRunnerSet",metadata:{name:"platform-linux",namespace:"arc-runners",uid:"ars-1",resourceVersion:"8",generation:2,annotations:{"meta.helm.sh/release-name":"platform-runners","meta.helm.sh/release-namespace":"arc-runners"}},spec:{runnerScaleSetName:"platform-linux",githubConfigUrl:"https://github.com/devantler-tech",githubConfigSecret:"arc-github-app",runnerGroup:"platform",minRunners:0,maxRunners:0},status:{observedGeneration:2}}' >"$ARC_TEST_STATE/ars.json"
  jq --slurpfile hr "$ARC_TEST_STATE/hr.json" --slurpfile eso "$ARC_TEST_STATE/eso.json" --slurpfile ars "$ARC_TEST_STATE/ars.json" '
    .metadata.annotations."platform.devantler.tech/arc-retirement"=({version:1,owner:{run:"123",attempt:"1",sha:("a"*40)},namespaceUID:"ns-1",hrUID:"",esoUID:"",phase:"baseline",controllerUID:"",controllerPodUIDs:[],controllerTicket:"",baseline:{version:1,sourceSHA:("b"*40),digest:("sha256:"+("c"*64)),maximum:0,hrSpec0:$hr[0].spec,arsSpec0:$ars[0].spec,esoSpec:$eso[0].spec,hrUID:"",arsUID:"",esoUID:""}}|tojson)' "$ARC_TEST_STATE/ns.json" >"$ARC_TEST_STATE/next.json";mv "$ARC_TEST_STATE/next.json" "$ARC_TEST_STATE/ns.json"
}
run_native() { bash scripts/retire-arc-pool.sh "$1" >"$ARC_TEST_STATE/output" 2>"$ARC_TEST_STATE/error"; }
refused() {
  if run_native before-publish; then printf 'Native retirement wrongly accepted %s\n' "$1" >&2; exit 1; fi
  if [[ -s "$ARC_TEST_STATE/proxy-pid" ]]; then
    ! kill -0 "$(cat "$ARC_TEST_STATE/proxy-pid")" 2>/dev/null || { printf 'Owned proxy survived failure.\n' >&2; exit 1; }
  fi
}
fixture outside; GITHUB_ACTIONS=false; refused outside; GITHUB_ACTIONS=true
[[ ! -f "$ARC_TEST_STATE/commands" ]]
fixture reader; ARC_TEST_ADMIN=false; refused reader
fixture denied-read; ARC_TEST_READ_FAILURE=true; refused denied-read
[[ ! -f "$ARC_TEST_STATE/metadata-paths" ]]
fixture empty; : >"$ARC_TEST_STATE/hr.json"; : >"$ARC_TEST_STATE/eso.json"; run_native before-publish
[[ ! -f "$ARC_TEST_STATE/proxy-pid" ]]
fixture failed-metadata; ARC_TEST_METADATA_FAILURE=true; refused failed-metadata
[[ -s "$ARC_TEST_STATE/eso.json" && -s "$ARC_TEST_STATE/hr.json" ]]
fixture inert-admission; ARC_TEST_INERT_ADMISSION=true; refused inert-admission
if grep -q '^delete ' "$ARC_TEST_STATE/commands"; then printf 'Inert admission allowed deletion.\n' >&2; exit 1; fi
fixture replaced-release; ARC_TEST_REPLACE=hr; refused replaced-release
if grep -q '^delete ' "$ARC_TEST_STATE/commands"; then printf 'Replaced release was deleted.\n' >&2; exit 1; fi
jq -e '.metadata.uid=="replacement-hr"' "$ARC_TEST_STATE/hr.json" >/dev/null
fixture replaced-credential; ARC_TEST_REPLACE=eso; refused replaced-credential
if grep -q 'delete.*externalsecrets' "$ARC_TEST_STATE/commands"; then printf 'Replaced credential sync was deleted.\n' >&2; exit 1; fi
jq -e '.metadata.uid=="replacement-eso"' "$ARC_TEST_STATE/eso.json" >/dev/null
fixture full
run_native before-publish
jq -e '.metadata.annotations.foreign=="preserve" and (.metadata.annotations."platform.devantler.tech/arc-retirement"|fromjson|.phase=="absent" and .controllerPodUIDs==["old-1"])' "$ARC_TEST_STATE/ns.json" >/dev/null
[[ ! -s "$ARC_TEST_STATE/hr.json" && ! -s "$ARC_TEST_STATE/eso.json" ]]
if kill -0 "$(cat "$ARC_TEST_STATE/proxy-pid")" 2>/dev/null; then printf 'Owned proxy survived success.\n' >&2; exit 1; fi
run_native after-reconcile
jq -e '.metadata.annotations."platform.devantler.tech/arc-retirement"|fromjson|.phase=="restored"' "$ARC_TEST_STATE/ns.json" >/dev/null
first_ticket=$(jq -r '.[0].status.lastHandledReconcileAt' "$ARC_TEST_STATE/layers.json")
run_native after-reconcile
[[ "$(jq -r '.[0].status.lastHandledReconcileAt' "$ARC_TEST_STATE/layers.json")" != "$first_ticket" ]]
ARC_TEST_NO_ACK=true
rm -f "$ARC_TEST_STATE/source-proof-read"
if timeout 15s bash scripts/retire-arc-pool.sh after-reconcile >"$ARC_TEST_STATE/output" 2>"$ARC_TEST_STATE/error"; then
  printf 'Unacknowledged fresh request passed.\n' >&2; exit 1
else
  code=$?;[[ "$code" == 124 ]] || { printf 'Fresh-request timeout was not exercised (exit %s).\n' "$code" >&2;exit 1; }
fi
[[ -s "$ARC_TEST_STATE/source-proof-read" || -f "$ARC_TEST_STATE/source-proof-read" ]]
if kill -0 "$(cat "$ARC_TEST_STATE/proxy-pid")" 2>/dev/null; then printf 'Retry proxy survived termination.\n' >&2; exit 1; fi
ARC_TEST_NO_ACK=false
run_native before-publish
jq -e '.metadata.annotations."platform.devantler.tech/arc-retirement"|fromjson|.phase=="restored"' "$ARC_TEST_STATE/ns.json" >/dev/null
if grep -Eq 'delete.*(pod|node|ephemeralrunner)|(^| )get secrets?( |$)' "$ARC_TEST_STATE/commands"; then printf 'Recovery touched jobs, capacity or a full Secret.\n' >&2; exit 1; fi
fixture foreign
jq '.metadata.annotations."platform.devantler.tech/arc-retirement"="{\"version\":1,\"owner\":{\"run\":\"456\",\"attempt\":\"1\",\"sha\":\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\"},\"namespaceUID\":\"ns-1\",\"hrUID\":\"hr-1\",\"esoUID\":\"eso-1\",\"phase\":\"fenced\",\"controllerUID\":\"\",\"controllerPodUIDs\":[],\"controllerTicket\":\"\"}"' "$ARC_TEST_STATE/ns.json" >"$ARC_TEST_STATE/next.json"; mv "$ARC_TEST_STATE/next.json" "$ARC_TEST_STATE/ns.json"
refused live-foreign-attempt
if grep -q '^patch ' "$ARC_TEST_STATE/commands"; then printf 'Foreign live attempt was mutated.\n' >&2; exit 1; fi
baseline_fixture interrupted-opening
run_native before-publish
jq -e '.metadata.annotations."platform.devantler.tech/arc-retirement"|fromjson|.phase=="absent" and .baseline.hrUID=="hr-1" and .baseline.arsUID=="ars-1" and .baseline.esoUID=="eso-1" and .baseline.maximum==0' "$ARC_TEST_STATE/ns.json" >/dev/null
for late in hr eso ars; do
  baseline_fixture "late-$late";ARC_TEST_LATE=$late
  mv "$ARC_TEST_STATE/$late.json" "$ARC_TEST_STATE/late-$late.json"
  timeout 40s bash scripts/retire-arc-pool.sh before-publish >"$ARC_TEST_STATE/output" 2>"$ARC_TEST_STATE/error"
  jq -e '.metadata.annotations."platform.devantler.tech/arc-retirement"|fromjson|.phase=="absent" and .hrUID=="hr-1" and .esoUID=="eso-1" and .baseline.arsUID=="ars-1"' "$ARC_TEST_STATE/ns.json" >/dev/null
  [[ ! -s "$ARC_TEST_STATE/hr.json" && ! -s "$ARC_TEST_STATE/eso.json" && ! -s "$ARC_TEST_STATE/ars.json" ]]
  if [[ "$late" == ars ]] && ! grep -q 'delete.*autoscalingrunnersets/platform-linux' "$ARC_TEST_STATE/commands"; then printf 'Bound orphan was not finalized.\n' >&2;exit 1;fi
done
baseline_fixture replaced-scale-set;ARC_TEST_REPLACE=ars;refused replaced-scale-set
if grep -q 'delete.*autoscalingrunnersets' "$ARC_TEST_STATE/commands";then printf 'Replaced scale set was deleted.\n' >&2;exit 1;fi
jq -e '.metadata.uid=="replacement-ars"' "$ARC_TEST_STATE/ars.json" >/dev/null
baseline_fixture unacknowledged-writer;ARC_TEST_NO_ACK=true
if timeout 15s bash scripts/retire-arc-pool.sh before-publish >"$ARC_TEST_STATE/output" 2>"$ARC_TEST_STATE/error";then
  printf 'Unacknowledged source writer allowed retirement.\n' >&2;exit 1
else
  code=$?;[[ "$code" == 124 ]] || { printf 'Writer timeout was not exercised (exit %s).\n' "$code" >&2;exit 1; }
fi
[[ -f "$ARC_TEST_STATE/source-proof-read" ]]
if grep -q '^delete ' "$ARC_TEST_STATE/commands";then printf 'Unacknowledged writer allowed deletion.\n' >&2;exit 1;fi
if [[ -s "$ARC_TEST_STATE/proxy-pid" ]] && kill -0 "$(cat "$ARC_TEST_STATE/proxy-pid")" 2>/dev/null;then printf 'Writer timeout retained its proxy.\n' >&2;exit 1;fi
printf 'Native ARC retirement: protected context, full partial-install retirement, durable restoration, foreign-attempt refusal, read failures and proxy cleanup passed.\n'
