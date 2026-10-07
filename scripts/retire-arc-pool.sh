#!/usr/bin/env bash
# Native production-only retirement. No runner job, node, App key or JIT data
# is deleted/read. All responses and patch plans stay in a private directory.
set +x
set -Eeuo pipefail
umask 077
GITHUB_ACTIONS=${GITHUB_ACTIONS:-}
GITHUB_REPOSITORY=${GITHUB_REPOSITORY:-}
GITHUB_REF=${GITHUB_REF:-}
GITHUB_RUN_ID=${GITHUB_RUN_ID:-}
GITHUB_RUN_ATTEMPT=${GITHUB_RUN_ATTEMPT:-}
GITHUB_SHA=${GITHUB_SHA:-}
[[ "$GITHUB_ACTIONS" == true && "$GITHUB_REPOSITORY" == devantler-tech/platform &&
   "$GITHUB_RUN_ID" =~ ^[1-9][0-9]{0,19}$ && "$GITHUB_RUN_ATTEMPT" =~ ^[1-9][0-9]{0,4}$ &&
   "$GITHUB_SHA" =~ ^[0-9a-f]{40}$ &&
   ( "$GITHUB_REF" == refs/heads/main || "$GITHUB_REF" == refs/heads/gh-readonly-queue/main/* ) ]] ||
   { printf 'ARC retirement requires the protected production workflow.\n' >&2; exit 1; }
[[ "$#" == 1 && ( "$1" == before-publish || "$1" == after-reconcile ) ]] || exit 2
mode=$1
repo_root=$(cd "$(dirname "$0")/.." && pwd)
cd "$repo_root"
controller=k8s/bases/infrastructure/controllers/actions-runner-controller/helm-release.yaml
[[ "$(yq -r '.metadata.annotations."platform.devantler.tech/arc-recovery" // ""' "$controller")" == drain-only ]] ||
  { printf 'ARC retirement is not armed by this declaration.\n'; exit; }
[[ "$(yq -r '.spec.suspend' "$controller")" == false &&
   "$(yq -r '.spec.values.flags.watchSingleNamespace' "$controller")" == arc-runners ]] || exit 1
work=$(mktemp -d)
proxy_pid=''
stage=initialization
cleanup() {
  if [[ -n "$proxy_pid" ]]; then kill "$proxy_pid" 2>/dev/null || true; wait "$proxy_pid" 2>/dev/null || true; fi
  rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 143' INT TERM
trap 'printf "ARC retirement refused at %s.\n" "$stage" >&2' ERR
kc() { kubectl --context admin@prod "$@" 2>"$work/kubectl-error"; }
fail() { printf 'ARC retirement refused at %s.\n' "$1" >&2; exit 1; }
stage=administrator-identity
kc auth whoami -o json >"$work/identity.json"
jq -e '.kind=="SelfSubjectReview" and (.status.userInfo.groups | index("system:masters")!=null)' "$work/identity.json" >/dev/null
go build -o "$work/planner" ./scripts/retire-arc-pool
check_json() { "$work/planner" check-json <"$1" >"$work/proof-output" 2>"$work/proof-error" || fail ambiguous-api-response; }
optional_object() {
  local kind=$1 name=$2 namespace=$3 file=$4
  kc -n "$namespace" get "$kind" "$name" --ignore-not-found -o json >"$file" || fail object-read
  if [[ ! -s "$file" ]]; then printf 'null\n' >"$file"; fi
  check_json "$file"
  jq -e 'type=="object" or .==null' "$file" >/dev/null || fail object-response
}
refresh_objects() {
  stage=resource-identities
  kc get namespace arc-runners --ignore-not-found -o json >"$work/ns.json" || fail namespace-read
  [[ -s "$work/ns.json" ]] || fail missing-namespace
  check_json "$work/ns.json"
  jq -e '.apiVersion=="v1" and .kind=="Namespace" and .metadata.name=="arc-runners" and
    (.metadata.uid|type=="string" and length>0) and (.metadata.resourceVersion|type=="string" and length>0) and
    (.metadata.deletionTimestamp==null)' "$work/ns.json" >/dev/null || fail namespace-response
  optional_object helmrelease platform-runners arc-runners "$work/hr.json"
  optional_object externalsecret arc-github-app arc-runners "$work/eso.json"
  printf 'null\n' >"$work/ars.json"
  if jq -e '.metadata.annotations."platform.devantler.tech/arc-retirement" | fromjson | has("baseline")' "$work/ns.json" >"$work/proof-output" 2>"$work/proof-error"; then
    optional_object autoscalingrunnerset platform-linux arc-runners "$work/ars.json"
  fi
}
snapshot() {
  jq -n --slurpfile ns "$work/ns.json" --slurpfile hr "$work/hr.json" --slurpfile eso "$work/eso.json" \
    --slurpfile ars "$work/ars.json" --slurpfile controller "$work/controller.json" --slurpfile pods "$work/controller-pods.json" --slurpfile receipt "$work/receipt.json" \
    --arg run "$GITHUB_RUN_ID" --arg attempt "$GITHUB_RUN_ATTEMPT" --arg sha "$GITHUB_SHA" \
    --argjson drained "$drained" --argjson children "$children" --argjson nodes "$nodes" \
    --argjson secret "$secret" --argjson source "$source" --argjson replaced "$replaced" '
    def project: if .==null then null else {Name:.metadata.name,Namespace:(.metadata.namespace//""),
      UID:.metadata.uid,RV:.metadata.resourceVersion,Annotations:(.metadata.annotations//{}),Spec:(.spec//null)} end;
    {State:{Namespace:($ns[0]|project),Release:($hr[0]|project),Credential:($eso[0]|project),ScaleSet:($ars[0]|project),
      Owner:{run:$run,attempt:$attempt,sha:$sha},DrainProven:$drained,ChildrenAbsent:$children,
      NodesAbsent:$nodes,SecretAbsent:$secret,SourceProven:$source,
      Controller:($controller[0]|project),ControllerPodUIDs:($pods[0]|map(.uid)),ControllerReplaced:$replaced},Receipt:$receipt[0]}' >"$work/input.json"
}
plan() {
  snapshot
  "$work/planner" "$1" <"$work/input.json" >"$work/patch.json"
}
apply_ns() {
  kc patch namespace arc-runners --type=json --patch-file "$work/patch.json" >"$work/mutation.json"
  refresh_objects
}
metadata_proxy() {
  [[ -z "$proxy_pid" ]] || return
  stage=metadata-proxy
  kubectl --context admin@prod proxy --address=127.0.0.1 --port=0 \
    '--accept-hosts=^127\.0\.0\.1$' \
    '--accept-paths=^(/api/v1/nodes|/api/v1/namespaces/(arc-runners|arc-systems)/pods|/api/v1/namespaces/arc-runners/secrets/arc-github-app|/apis/actions\.github\.com/v1alpha1/namespaces/(arc-runners|arc-systems)/(autoscalingrunnersets|autoscalinglisteners|ephemeralrunnersets|ephemeralrunners))$' \
    '--reject-methods=^(G|GE|GET.+|[^G].*|G[^E].*|GE[^T].*)$' \
    >"$work/proxy.log" 2>"$work/proxy-error" &
  proxy_pid=$!
  local tries
  for ((tries=0; tries<100; tries++)); do
    if [[ -s "$work/proxy.log" ]]; then break; fi
    kill -0 "$proxy_pid" 2>/dev/null || fail metadata-proxy-start
    sleep 0.1
  done
  proxy_port=$(sed -nE 's/^Starting to serve on 127\.0\.0\.1:([0-9]+)$/\1/p' "$work/proxy.log")
  [[ "$proxy_port" =~ ^[0-9]{1,5}$ && "$proxy_port" -gt 0 && "$proxy_port" -le 65535 ]] || fail metadata-proxy-address
}
empty_metadata() {
  local path=$1
  metadata_proxy
  curl --silent --show-error --fail --max-time 10 --noproxy '*' \
    -H 'Accept: application/json;as=PartialObjectMetadataList;g=meta.k8s.io;v=v1' \
    "http://127.0.0.1:$proxy_port$path" >"$work/metadata.json" 2>"$work/curl-error" || fail metadata-list-read
  check_json "$work/metadata.json"
  jq -e '.apiVersion=="meta.k8s.io/v1" and .kind=="PartialObjectMetadataList" and
    (.metadata.resourceVersion|type=="string" and length>0) and (.metadata.continue//"")=="" and
    (.metadata.remainingItemCount//0)==0 and has("items") and
    ((.items|type)=="array" or .items==null)' "$work/metadata.json" >/dev/null || fail metadata-list-response
  jq -e '(.items//[]|length)==0' "$work/metadata.json" >/dev/null
}
secret_absent() {
  metadata_proxy
  local code
  code=$(curl --silent --show-error --max-time 10 --noproxy '*' \
    -H 'Accept: application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1' \
    --output "$work/secret-metadata.json" --write-out '%{http_code}' \
    "http://127.0.0.1:$proxy_port/api/v1/namespaces/arc-runners/secrets/arc-github-app" 2>"$work/curl-error") || fail secret-metadata-transport
  check_json "$work/secret-metadata.json"
  case "$code" in
    404) jq -e '.kind=="Status" and .reason=="NotFound" and .code==404' "$work/secret-metadata.json" >/dev/null || fail secret-metadata-not-found ;;
    200) jq -e '.apiVersion=="meta.k8s.io/v1" and .kind=="PartialObjectMetadata" and
      .metadata.name=="arc-github-app" and .metadata.namespace=="arc-runners" and
      (.metadata.uid|type=="string" and length>0)' "$work/secret-metadata.json" >/dev/null || fail secret-metadata-response; return 1 ;;
    *) fail secret-metadata-read ;;
  esac
}
absence_receipts() {
  children=true; nodes=true; secret=true
  local kind
  for kind in autoscalingrunnersets autoscalinglisteners ephemeralrunnersets ephemeralrunners; do
    if ! empty_metadata "/apis/actions.github.com/v1alpha1/namespaces/arc-runners/$kind"; then children=false; fi
  done
  if ! empty_metadata '/apis/actions.github.com/v1alpha1/namespaces/arc-systems/autoscalinglisteners'; then children=false; fi
  if ! empty_metadata '/api/v1/namespaces/arc-runners/pods'; then children=false; fi
  if ! empty_metadata '/api/v1/namespaces/arc-systems/pods?labelSelector=platform.devantler.tech%2Farc-role%3Dlistener'; then children=false; fi
  if ! empty_metadata '/api/v1/nodes?labelSelector=platform.devantler.tech%2Fci-runner%3Denabled'; then nodes=false; fi
  if ! secret_absent; then secret=false; fi
}
own_resource() {
  local file=$1 kind=$2 name=$3
  local tag
  tag=$(jq -r '.metadata.uid' "$work/ns.json")
  local baseline=false
  if jq -e '.baseline!=null' "$work/journal.json" >/dev/null; then baseline=true; fi
  jq -e --arg tag "retirement:$tag" --argjson baseline "$baseline" '((.metadata.annotations//{} | has("platform.devantler.tech/arc-retirement") | not) and
    (.metadata.annotations//{} | has("kustomize.toolkit.fluxcd.io/reconcile") | not)) or
    (.metadata.annotations."platform.devantler.tech/arc-retirement"==$tag and
     .metadata.annotations."kustomize.toolkit.fluxcd.io/reconcile"=="disabled") or
    ($baseline and (.metadata.annotations//{} | has("platform.devantler.tech/arc-retirement") | not) and
     .metadata.annotations."kustomize.toolkit.fluxcd.io/reconcile"=="disabled")' "$file" >/dev/null || fail foreign-resource-fence
  jq --arg tag "retirement:$tag" '[
    {op:"test",path:"/metadata/uid",value:.metadata.uid},
    {op:"test",path:"/metadata/resourceVersion",value:.metadata.resourceVersion},
    {op:"add",path:"/metadata/annotations",value:((.metadata.annotations//{})+
      {"platform.devantler.tech/arc-retirement":$tag,"kustomize.toolkit.fluxcd.io/reconcile":"disabled"})}
    ] + (if .kind=="HelmRelease" then [
      {op:"add",path:"/spec/suspend",value:false},
      {op:"add",path:"/spec/values/minRunners",value:0},
      {op:"add",path:"/spec/values/maxRunners",value:0}]
      elif .kind=="AutoscalingRunnerSet" then [
      {op:"add",path:"/spec/minRunners",value:0},{op:"add",path:"/spec/maxRunners",value:0}] else [] end)' "$file" >"$work/resource-patch.json"
  kc -n arc-runners patch "$kind" "$name" --type=json --patch-file "$work/resource-patch.json" >"$work/mutation.json"
}
delete_owned() {
  local file=$1 path=$2
  # Bind the destructive request to the journal, not merely to whichever UID
  # a fresh GET returned. Replacements or a changed owner must survive untouched.
  refresh_objects
  bind_current
  case "$phase" in
    uninstalling) drain_receipts || fail changed-drain-proof; controller_replaced || fail changed-controller-proof ;;
    credential-removing) absence_receipts ;;
    *) fail unexpected-delete-phase ;;
  esac
  snapshot
  "$work/planner" verify <"$work/input.json" >"$work/verified-journal.json" || fail changed-delete-owner
  local uid_key
  case "$path" in
    /apis/helm.toolkit.fluxcd.io/v2/namespaces/arc-runners/helmreleases/platform-runners) uid_key=hrUID ;;
    /apis/external-secrets.io/v1/namespaces/arc-runners/externalsecrets/arc-github-app) uid_key=esoUID ;;
    /apis/actions.github.com/v1alpha1/namespaces/arc-runners/autoscalingrunnersets/platform-linux) uid_key=arsUID ;;
    *) fail unexpected-delete-target ;;
  esac
  jq -e --arg key "$uid_key" --slurpfile journal "$work/verified-journal.json" \
    '.metadata.uid==(if $key=="arsUID" then $journal[0].baseline.arsUID else $journal[0][$key] end) and
     (.metadata.uid|type=="string" and length>0)' "$file" >/dev/null || fail replacement-delete-target
  jq '{apiVersion:"v1",kind:"DeleteOptions",propagationPolicy:"Foreground",
    preconditions:{uid:.metadata.uid,resourceVersion:.metadata.resourceVersion}}' "$file" >"$work/delete.json"
  kc delete --raw "$path" -f "$work/delete.json" >"$work/delete-response.json"
}
drained=false; children=false; nodes=false; secret=false; source=false; replaced=false; claimed=false
printf 'null\n' >"$work/controller.json"
printf '[]\n' >"$work/controller-pods.json"
printf 'null\n' >"$work/receipt.json"
refresh_objects
if jq -e '.==null' "$work/hr.json" >/dev/null &&
   jq -e '.==null' "$work/eso.json" >/dev/null &&
   jq -e '.metadata.annotations | has("platform.devantler.tech/arc-retirement") | not' "$work/ns.json" >/dev/null; then
  printf 'ARC retirement has no installed release or credential-sync object; the unchanged absence guard follows.\n'
  exit
fi
remaining=${ARC_RETIREMENT_REMAINING:-1200}
[[ "$remaining" =~ ^[0-9]{1,4}$ && "$remaining" -gt 0 && "$remaining" -le 1200 ]] || fail retirement-deadline
deadline=$((SECONDS+remaining))
nonce=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
[[ "$nonce" =~ ^[0-9a-f]{32}$ ]] || fail request-nonce
ticket="native-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT-$nonce"
await() {
  local stage_name=$1; shift
  stage=$stage_name
  until "$@"; do
    ((SECONDS<deadline)) || fail "$stage_name-timeout"
    sleep 5
  done
}
pods_projection() {
  local namespace=$1 selector=$2 file=$3
  # Only the named status/ownership fields are serialized. Runner Pod specs,
  # credentials, logs and managedFields are never requested by this helper.
  kc -n "$namespace" get pods -l "$selector" -o 'jsonpath={range .items[*]}{"{\"uid\":\""}{.metadata.uid}{"\",\"ownerUID\":\""}{.metadata.ownerReferences[?(@.controller==true)].uid}{"\",\"ownerKind\":\""}{.metadata.ownerReferences[?(@.controller==true)].kind}{"\",\"phase\":\""}{.status.phase}{"\",\"ready\":\""}{.status.conditions[?(@.type=="Ready")].status}{"\",\"deleting\":\""}{.metadata.deletionTimestamp}{"\"}\n"}{end}' >"$work/projected-pods.ndjson" || fail pod-status-read
  jq -s '.' "$work/projected-pods.ndjson" >"$file" || fail pod-status-response
}
controller_receipts() {
  kc -n arc-systems get deployment arc-controller -o json >"$work/controller.json" || fail controller-read
  pods_projection arc-systems 'app.kubernetes.io/instance=arc-controller' "$work/controller-pods.json"
}
inspect() {
  snapshot
  "$work/planner" inspect <"$work/input.json" >"$work/journal.json" || fail journal-read
  phase=$(jq -er '.phase' "$work/journal.json") || fail journal-phase
}
bind_current() {
  # Admission can finish a previously accepted zero CREATE after the close CAS.
  # Enroll that first exact UID before touching it, and invalidate every old
  # completion receipt. At most three first identities can restart this attempt.
  [[ "$claimed" == true ]] || return 0
  jq -e '.baseline!=null' "$work/journal.json" >/dev/null || return 0
  plan bind
  if jq -e --slurpfile ns "$work/ns.json" '.[-1].value."platform.devantler.tech/arc-retirement" != $ns[0].metadata.annotations."platform.devantler.tech/arc-retirement"' "$work/patch.json" >/dev/null; then
    apply_ns; inspect
    if [[ "$phase" == fenced ]]; then
      [[ "$mode" == before-publish ]] || fail late-baseline-after-publication
      local restarts=${ARC_RETIREMENT_RESTARTS:-0}
      [[ "$restarts" =~ ^[0-3]$ && "$restarts" -lt 3 ]] || fail late-baseline-restart-bound
      export ARC_RETIREMENT_RESTARTS=$((restarts+1))
      export ARC_RETIREMENT_REMAINING=$((deadline-SECONDS))
      cleanup
      exec bash scripts/retire-arc-pool.sh "$mode"
    fi
  fi
}
installed_policy() {
  stage=installed-retirement-policy
  kc get clusterpolicy restrict-arc-retirement -o json >"$work/policy.json" || fail missing-retirement-policy
  yq -o=json '.spec' k8s/bases/infrastructure/cluster-policies/best-practices/restrict-arc-retirement.yaml >"$work/declared-policy.json"
  jq -e '.spec.admission!=false and .spec.background==false and
    .spec.webhookConfiguration.failurePolicy=="Fail" and
    all(.spec.rules[]; .validate.failureAction=="Enforce" and .validate.allowExistingViolations==false)' "$work/policy.json" >/dev/null || fail retirement-policy-enforcement
  jq '.spec | del(.admission,.emitWarning,.validationFailureAction) | .rules |= map(del(.skipBackgroundRequests))' "$work/policy.json" >"$work/live-policy.json"
  jq -es '.[0]==.[1]' "$work/declared-policy.json" "$work/live-policy.json" >/dev/null || fail retirement-policy-drift
}
denied_create() {
  local rule=$1 file=$2
  if kc create --dry-run=server -f "$file" >"$work/probe-output"; then fail inert-retirement-admission; fi
  if ! grep -Fq 'restrict-arc-retirement' "$work/kubectl-error" || ! grep -Fq "$rule" "$work/kubectl-error"; then
    fail unproven-retirement-denial
  fi
}
denied_patch() {
  local kind=$1 name=$2 rule=$3 file=$4
  if kc -n arc-runners patch "$kind" "$name" --dry-run=server --type=json --patch-file "$file" >"$work/probe-output"; then fail inert-retirement-update; fi
  if ! grep -Fq 'restrict-arc-retirement' "$work/kubectl-error" || ! grep -Fq "$rule" "$work/kubectl-error"; then fail unproven-retirement-update; fi
}
admission_receipts() {
  stage=retirement-admission
  # All probes are server dry-runs. A generic error, stale webhook, schema
  # rejection or AlreadyExists is UNKNOWN and cannot establish this fence.
  jq -n '{apiVersion:"helm.toolkit.fluxcd.io/v2",kind:"HelmRelease",metadata:{name:"platform-runners",namespace:"arc-runners"},
    spec:{interval:"10m",suspend:false,chartRef:{kind:"OCIRepository",name:"platform-runners"},values:{minRunners:0,maxRunners:0}}}' >"$work/hr-probe.json"
  denied_create fence-pool-retirement "$work/hr-probe.json"
  yq -o=json '.spec.values' k8s/bases/infrastructure/actions-runners/helm-release.yaml |
    jq '{apiVersion:"actions.github.com/v1alpha1",kind:"AutoscalingRunnerSet",metadata:{name:"platform-linux",namespace:"arc-runners"},
      spec:(del(.controllerServiceAccount,.listenerConfig) | .minRunners=0 | .maxRunners=0)}' >"$work/ars-probe.json"
  denied_create fence-scale-set-retirement "$work/ars-probe.json"
  yq -o=json '.' k8s/bases/infrastructure/actions-runners/external-secret.yaml >"$work/eso-probe.json"
  denied_create fence-credential-recreation "$work/eso-probe.json"
  jq -n '{apiVersion:"actions.github.com/v1alpha1",kind:"AutoscalingListener",metadata:{name:"arc-retirement-proof",namespace:"arc-systems"},
    spec:{autoscalingRunnerSetName:"platform-linux",autoscalingRunnerSetNamespace:"arc-runners",minRunners:0,maxRunners:1}}' >"$work/listener-probe.json"
  denied_create fence-listener-retirement "$work/listener-probe.json"
  if jq -e '.!=null' "$work/hr.json" >/dev/null; then
    jq '[{op:"test",path:"/metadata/uid",value:.metadata.uid},{op:"test",path:"/metadata/resourceVersion",value:.metadata.resourceVersion},
      {op:"add",path:"/spec/suspend",value:false},{op:"add",path:"/spec/values/minRunners",value:0},
      {op:"add",path:"/spec/values/maxRunners",value:0}]' "$work/hr.json" >"$work/zero-probe.json"
    kc -n arc-runners patch helmrelease platform-runners --dry-run=server --type=json --patch-file "$work/zero-probe.json" >"$work/probe-output" || fail zero-admission-control
    jq '.[-1].value=1' "$work/zero-probe.json" >"$work/active-probe.json"
    denied_patch helmrelease platform-runners fence-pool-retirement "$work/active-probe.json"
  fi
  if jq -e '.!=null' "$work/eso.json" >/dev/null; then
    jq '[{op:"test",path:"/metadata/uid",value:.metadata.uid},{op:"test",path:"/metadata/resourceVersion",value:.metadata.resourceVersion},
      {op:"add",path:"/spec/target/name",value:"arc-retirement-proof"}]' "$work/eso.json" >"$work/eso-update-probe.json"
    denied_patch externalsecret arc-github-app fence-credential-recreation "$work/eso-update-probe.json"
  fi
  case "$phase" in uninstalled|credential-removing|absent|restored)
    jq --arg namespace arc-systems '{apiVersion:"v1",kind:"Pod",metadata:{name:"arc-retirement-listener-proof",namespace:$namespace,
      labels:{"actions.github.com/scale-set-name":"platform-linux","actions.github.com/scale-set-namespace":"arc-runners","app.kubernetes.io/component":"runner-scale-set-listener"}},spec:.spec.template.spec}' "$work/controller.json" >"$work/pod-probe.json"
    denied_create fence-retired-listener-pod "$work/pod-probe.json"
    jq '.spec.maxRunners=0' "$work/listener-probe.json" >"$work/zero-listener-probe.json"
    denied_create fence-listener-retirement "$work/zero-listener-probe.json"
    ;;
  esac
  case "$phase" in credential-removing|absent|restored)
    jq -n '{apiVersion:"v1",kind:"Secret",metadata:{name:"arc-github-app",namespace:"arc-runners"},type:"Opaque"}' >"$work/secret-probe.json"
    denied_create fence-retired-secret "$work/secret-probe.json"
    ;;
  esac
}
drain_receipts() {
  drained=false
  refresh_objects
  bind_current
  optional_object autoscalingrunnerset platform-linux arc-runners "$work/ars.json"
  optional_object ephemeralrunnerset platform-linux arc-runners "$work/ers.json"
  kc -n arc-systems get autoscalinglisteners -o json >"$work/listeners.json" || fail listener-read
  jq -e '.kind=="AutoscalingListenerList" and (.metadata.continue//"")=="" and (.items|type)=="array"' "$work/listeners.json" >/dev/null || fail listener-list-response
  jq '.items' "$work/listeners.json" >"$work/listener-items.json"
  pods_projection arc-systems 'actions.github.com/scale-set-name=platform-linux,actions.github.com/scale-set-namespace=arc-runners' "$work/listener-pods.json"
  local idle=true capacity=true
  if ! empty_metadata '/apis/actions.github.com/v1alpha1/namespaces/arc-runners/ephemeralrunners'; then idle=false; fi
  if ! empty_metadata '/api/v1/namespaces/arc-runners/pods'; then idle=false; fi
  if ! empty_metadata '/api/v1/nodes?labelSelector=platform.devantler.tech%2Fci-runner%3Denabled'; then capacity=false; fi
  jq -n --slurpfile hr "$work/hr.json" --slurpfile ars "$work/ars.json" --slurpfile ers "$work/ers.json" \
    --slurpfile listeners "$work/listener-items.json" --slurpfile pods "$work/listener-pods.json" \
    --argjson idle "$idle" --argjson capacity "$capacity" \
    '{HR:$hr[0],ARS:$ars[0],ERS:$ers[0],Listeners:$listeners[0],ListenerPods:$pods[0],ChildrenAbsent:$idle,NodesAbsent:$capacity}' >"$work/drain.json" || fail drain-projection
  if "$work/planner" check-drain <"$work/drain.json" >"$work/proof-output" 2>"$work/proof-error"; then drained=true; return 0; fi
  return 1
}
controller_replaced() {
  replaced=false
  controller_receipts
  kc -n arc-systems get replicasets -l app.kubernetes.io/instance=arc-controller -o json >"$work/replicasets.json" || fail controller-owners-read
  jq -n --slurpfile dep "$work/controller.json" --slurpfile pods "$work/controller-pods.json" \
    --slurpfile sets "$work/replicasets.json" --slurpfile journal "$work/journal.json" \
    '{Deployment:$dep[0],Pods:$pods[0],ReplicaSets:$sets[0].items,Journal:$journal[0],Ticket:$journal[0].controllerTicket}' >"$work/controller-proof.json" || fail controller-projection
  if "$work/planner" check-controller <"$work/controller-proof.json" >"$work/proof-output" 2>"$work/proof-error"; then replaced=true; return 0; fi
  return 1
}
no_pool() {
  refresh_objects
  bind_current
  if jq -e '.baseline!=null' "$work/journal.json" >/dev/null &&
     jq -e '.==null' "$work/hr.json" >/dev/null && jq -e '.!=null' "$work/ars.json" >/dev/null; then
    # A chart CREATE accepted before uninstall can leave its scale set behind.
    # Only the journal-bound orphan is removed, after the unchanged drain and
    # controller retirement proofs; ARC finalizers retire children naturally.
    if ! drain_receipts || ! controller_replaced; then return 1; fi
    if jq -e '.metadata.deletionTimestamp==null' "$work/ars.json" >/dev/null; then
      delete_owned "$work/ars.json" /apis/actions.github.com/v1alpha1/namespaces/arc-runners/autoscalingrunnersets/platform-linux
    fi
    refresh_objects
    bind_current
  fi
  absence_receipts
  jq -e '.==null' "$work/hr.json" >/dev/null && [[ "$children" == true && "$nodes" == true ]]
}
no_credential() {
  refresh_objects
  bind_current
  absence_receipts
  jq -e '.==null' "$work/eso.json" >/dev/null && [[ "$children" == true && "$nodes" == true && "$secret" == true ]]
}
fresh_source_request() {
  local name
  for name in flux-system infrastructure-controllers infrastructure; do
    kc -n flux-system get kustomization "$name" -o json >"$work/layer.json"
    jq --arg ticket "$ticket" '[{op:"test",path:"/metadata/uid",value:.metadata.uid},
      {op:"test",path:"/metadata/resourceVersion",value:.metadata.resourceVersion},
      {op:"add",path:"/metadata/annotations",value:((.metadata.annotations//{})+{"reconcile.fluxcd.io/requestedAt":$ticket})}]' "$work/layer.json" >"$work/request.json"
    kc -n flux-system patch kustomization "$name" --type=json --patch-file "$work/request.json" >"$work/mutation.json"
  done
}
inactive_source() {
  source=false
  kc -n flux-system get ocirepository flux-system -o json >"$work/source.json" || fail source-read
  kc -n flux-system get kustomizations flux-system infrastructure-controllers infrastructure -o json >"$work/layers.json" || fail source-layers-read
  jq -n --slurpfile src "$work/source.json" --slurpfile layers "$work/layers.json" --arg digest "${PLATFORM_MANIFEST_DIGEST:?published digest required}" --arg ticket "$ticket" \
    '{OCI:$src[0],Kustomizations:$layers[0].items,Digest:$digest,Ticket:$ticket}' >"$work/source-proof.json" || fail source-projection
  if "$work/planner" check-source <"$work/source-proof.json" >"$work/proof-output" 2>"$work/proof-error"; then source=true; return 0; fi
  return 1
}
source_writer_quiesced() {
  # Fresh acknowledgements at all three source layers finish pending applies
  # while the exact HR/ESO are excluded. This proof concerns the currently
  # signed writer, independently of the inactive digest published afterward.
  local PLATFORM_MANIFEST_DIGEST
  kc -n flux-system get ocirepository flux-system -o json >"$work/writer-source.json" || fail writer-source-read
  check_json "$work/writer-source.json"
  PLATFORM_MANIFEST_DIGEST=$(jq -er '.status.artifact.digest' "$work/writer-source.json") || fail writer-source-digest
  inactive_source
}

# The declaration itself must keep the fence in every later publication. The
# first policy-only deployment has no installed ARC objects and exits above.
installed_policy
controller_receipts
if jq -e '.metadata.annotations|has("platform.devantler.tech/arc-retirement")' "$work/ns.json" >/dev/null; then
  inspect
  pending_binding=false
  if jq -e --slurpfile hr "$work/hr.json" --slurpfile eso "$work/eso.json" --slurpfile ars "$work/ars.json" '.baseline!=null and
    ((.hrUID=="" and $hr[0]!=null) or (.esoUID=="" and $eso[0]!=null) or (.baseline.arsUID=="" and $ars[0]!=null))' "$work/journal.json" >/dev/null; then pending_binding=true; fi
  if [[ "$pending_binding" == false ]]; then
    case "$phase" in drained|quiescing|quiesced) await current-drain drain_receipts ;; uninstalling) if jq -e '.!=null' "$work/hr.json" >/dev/null; then await current-drain drain_receipts; fi ;; esac
    case "$phase" in quiesced|uninstalling) await current-controller controller_replaced ;; esac
    case "$phase" in uninstalled|credential-removing|absent|restored) absence_receipts ;; esac
  fi
  prior_run=$(jq -er '.owner.run' "$work/journal.json"); prior_attempt=$(jq -er '.owner.attempt' "$work/journal.json")
  if [[ "$prior_run" != "$GITHUB_RUN_ID" || "$prior_attempt" != "$GITHUB_RUN_ATTEMPT" ]]; then
    [[ "$prior_run" =~ ^[1-9][0-9]{0,19}$ && "$prior_attempt" =~ ^[1-9][0-9]{0,4}$ ]] || fail previous-owner-identity
    GH_TOKEN=${GH_TOKEN:?workflow token required} gh api "repos/devantler-tech/platform/actions/runs/$prior_run/attempts/$prior_attempt" >"$work/receipt.json" 2>"$work/github-error" || fail previous-attempt-read
  fi
fi
plan claim; apply_ns; inspect
claimed=true
bind_current
admission_receipts
if [[ "$mode" == after-reconcile ]]; then
  [[ "$phase" == absent || "$phase" == restored ]] || fail incomplete-retirement
  fresh_source_request
  await inactive-source inactive_source
  absence_receipts
  refresh_objects; bind_current
  plan restored; apply_ns
  printf 'ARC retirement restored the exact inactive artifact with a durable closed fence.\n'
  exit
fi
if [[ "$phase" == fenced ]]; then
  if jq -e '.!=null' "$work/hr.json" >/dev/null; then
    jq -e '.kind=="HelmRelease" and .spec.chartRef.kind=="OCIRepository" and .spec.chartRef.name=="platform-runners" and
      .spec.values.runnerScaleSetName=="platform-linux" and .spec.values.githubConfigSecret=="arc-github-app" and
      .spec.values.githubConfigUrl=="https://github.com/devantler-tech" and .spec.values.runnerGroup=="platform"' "$work/hr.json" >/dev/null || fail unexpected-release
    own_resource "$work/hr.json" helmrelease platform-runners
  fi
  if jq -e '.!=null' "$work/eso.json" >/dev/null; then
    jq -e '.kind=="ExternalSecret" and .spec.target.name=="arc-github-app" and .spec.target.creationPolicy=="Owner" and
      .spec.secretStoreRef.name=="openbao" and .spec.secretStoreRef.kind=="SecretStore"' "$work/eso.json" >/dev/null || fail unexpected-credential-sync
    own_resource "$work/eso.json" externalsecret arc-github-app
  fi
  if jq -e '.baseline!=null' "$work/journal.json" >/dev/null && jq -e '.!=null' "$work/ars.json" >/dev/null; then
    own_resource "$work/ars.json" autoscalingrunnerset platform-linux
  fi
  if jq -e '.baseline!=null' "$work/journal.json" >/dev/null; then
    fresh_source_request
    await quiesced-source-writer source_writer_quiesced
    refresh_objects; bind_current
  fi
  await natural-drain drain_receipts
  plan drained; apply_ns; inspect
fi
if [[ "$phase" == drained ]]; then
  controller_receipts
  plan quiescing; apply_ns; inspect
fi
if [[ "$phase" == quiescing ]]; then
  controller_receipts
  controller_ticket=$(jq -er '.controllerTicket' "$work/journal.json")
  jq --arg ticket "$controller_ticket" '[{op:"test",path:"/metadata/uid",value:.metadata.uid},
    {op:"test",path:"/metadata/resourceVersion",value:.metadata.resourceVersion},
    {op:"add",path:"/spec/template/metadata/annotations",value:((.spec.template.metadata.annotations//{})+
      {"platform.devantler.tech/arc-retirement-restart":$ticket})}]' "$work/controller.json" >"$work/controller-patch.json"
  kc -n arc-systems patch deployment arc-controller --type=json --patch-file "$work/controller-patch.json" >"$work/mutation.json"
  await retired-controller-processes controller_replaced
  await stable-zero-listener drain_receipts
  plan quiesced; apply_ns; inspect
fi
if [[ "$phase" == quiesced ]]; then plan uninstalling; apply_ns; inspect; fi
if [[ "$phase" == uninstalling ]]; then
  refresh_objects
  bind_current
  if jq -e '.!=null' "$work/hr.json" >/dev/null && jq -e '.metadata.deletionTimestamp==null' "$work/hr.json" >/dev/null; then
    delete_owned "$work/hr.json" /apis/helm.toolkit.fluxcd.io/v2/namespaces/arc-runners/helmreleases/platform-runners
  fi
  await chart-uninstall no_pool
  plan uninstalled; apply_ns; inspect
fi
if [[ "$phase" == uninstalled ]]; then
  admission_receipts
  plan credential-removing; apply_ns; inspect
fi
if [[ "$phase" == credential-removing ]]; then
  admission_receipts
  refresh_objects
  bind_current
  if jq -e '.!=null' "$work/eso.json" >/dev/null && jq -e '.metadata.deletionTimestamp==null' "$work/eso.json" >/dev/null; then
    delete_owned "$work/eso.json" /apis/external-secrets.io/v1/namespaces/arc-runners/externalsecrets/arc-github-app
  fi
  await credential-finalization no_credential
  plan absent; apply_ns; inspect
fi
[[ "$phase" == absent || "$phase" == restored ]] || fail incomplete-retirement
absence_receipts
refresh_objects; bind_current
[[ "$children" == true && "$nodes" == true && "$secret" == true ]] || fail retirement-regressed
printf 'ARC retirement finalized the pool and credential before publication; its closed fence remains.\n'
