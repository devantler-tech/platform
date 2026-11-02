#!/usr/bin/env bash
set -euo pipefail
umask 077

# Requires an inherited, dedicated single-file KUBECONFIG. Every read selects
# the caller's context; this script never changes context or kubeconfig contents.
# --context NAME --domain PUBLIC_DOMAIN --image-digest sha256:...
# --runtime-digest sha256:... (repeat) --chart-digest sha256:...
# --probe-runtime-digest sha256:... (repeat, resolved from the declared probe image)
# --apps-digest sha256:... --apps-verify-file /absolute/public-policy.json [--timeout SECONDS]
# Expected readiness activation comes from this checkout's Helm declaration.
# Browser authentication remains a separate acceptance check. This helper reads no Secrets or authenticated URLs.
context='' domain='' image_digest='' chart_digest='' apps_digest='' apps_verify_file='' timeout_seconds=300
runtime_digests=() scratch='' active='' watchdog='' changed='' started_at=$SECONDS
probe_runtime_digests=()
phase=''
readonly repository='ghcr.io/devantler-tech/khoist'
readonly namespace='data-product-controller'
expected_helm_file="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/k8s/bases/apps/data-product-controller/helm-release.yaml"
readonly expected_helm_file
expected_publisher_file="$(dirname "$expected_helm_file")/deployment-publisher-review.yaml"
readonly expected_publisher_file
expected_chart_file="$(dirname "$expected_helm_file")/oci-repository.yaml"
readonly expected_chart_file
expected_probe_file="$(dirname "$expected_helm_file")/deployment-contract-probe.yaml"
readonly expected_probe_file
# fail emits a sanitized refusal with any observed change names, then exits.
fail() {
  local detail='' role='' reason='' extra='' http_code='' curl_code=''
  # A completed public failure remains useful when the shared deadline subsequently expires.
  # Keep the deadline refusal, while retaining only the validated role and cause already observed.
  if [[ "$1" == public_contract_incomplete || ("$1" == deadline_exceeded && -f "$scratch/public-failure") ]]; then
    role=public_contract
    reason=read_incomplete
    if [[ -f "$scratch/public-failure" ]]; then
      local saved_role='' saved_reason='' saved_http='' saved_curl=''
      read -r saved_role saved_reason saved_http saved_curl extra <"$scratch/public-failure" || true
      # Only fixed identifiers may enter a public receipt, never response data.
      if [[ -z "$extra" ]]; then
        case "$saved_role" in
          sample_health | sample_observations | sample_openapi | release_index | release_openapi | publisher_health | publisher_root | publisher_index | publisher_script | publisher_style | publisher_library | descriptor_library | metadata_library)
            case "$saved_reason" in
              dns_failed | tls_failed | request_timeout | transport_failed | http_status | body_invalid | security_headers | asset_invalid)
                role=$saved_role
                reason=$saved_reason
                if [[ "$saved_http" =~ ^(000|[1-5][0-9]{2})$ ]]; then http_code=$((10#$saved_http)); fi
                if [[ "$saved_curl" =~ ^(0|[1-9][0-9]{0,2})$ ]] && ((saved_curl <= 255)); then curl_code=$saved_curl; fi
                ;;
            esac
            ;;
        esac
      fi
    fi
    detail=$(printf ',"phase":"public_contract","role":"%s","reason":"%s"' "$role" "$reason")
    if [[ -n "$http_code" ]]; then detail+=$(printf ',"httpStatus":%d' "$http_code"); fi
    if [[ -n "$curl_code" ]]; then detail+=$(printf ',"curlExit":%d' "$curl_code"); fi
  elif [[ ("$1" == deadline_exceeded || "$1" == read_incomplete) &&
    ("$phase" == initial_readiness || "$phase" == rollout_stability) ]]; then
    role=snapshot
    reason=$1
    local failure_file="$scratch/snapshot-failure"
    if [[ "$1" == read_incomplete || ! -f "$failure_file" ]]; then failure_file="$scratch/read-failure"; fi
    if [[ -f "$failure_file" ]]; then
      local saved_role='' saved_reason=''
      read -r saved_role saved_reason extra <"$failure_file" || true
      if [[ -z "$extra" ]]; then
        case "$saved_role" in
          apps | root | chart | helm | product | release-product | release-flags | replicasets | pods | endpoints | readiness | controller-flags | workloads | backends | routes | gateway | service-harbour | service-publisher-review | deployment-controller | deployment-harbour | deployment-publisher-review | deployment-probe | probe-account | observer-role | observer-binding | probe-policy | route-registry | route-harbour | route-publisher-review | snapshot)
            case "$saved_reason" in
              read_incomplete | predicate_not_ready | observation_invalid)
                role=$saved_role
                reason=$saved_reason
                if [[ "$1" == deadline_exceeded && "$failure_file" == "$scratch/read-failure" ]]; then reason=deadline_exceeded; fi
                ;;
            esac
            ;;
        esac
      fi
    fi
    detail=$(printf ',"phase":"%s","role":"%s","reason":"%s"' "$phase" "$role" "$reason")
  fi
  # Once a snapshot pair has differed, every later refusal still names what moved.
  if [[ -n "$changed" ]]; then
    printf '{"complete":false,"failure":"%s"%s,"changed":%s}\n' "$1" "$detail" "$changed"
  else
    printf '{"complete":false,"failure":"%s"%s}\n' "$1" "$detail"
  fi
  exit "${2:-1}"
}
# cleanup stops owned read processes and removes the private snapshot directory.
cleanup() {
  for pid in "$active" "$watchdog"; do
    if [[ -n "$pid" ]]; then
      kill -KILL -- "-$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  [[ -z "$scratch" ]] || rm -rf "$scratch"
}
trap cleanup EXIT
trap 'fail interrupted' HUP INT TERM
while (($#)); do
  [[ $# -ge 2 ]] || fail invalid_arguments 2
  case "$1" in
    --context) context=$2 ;;
    --domain) domain=$2 ;;
    --image-digest) image_digest=$2 ;;
    --runtime-digest) runtime_digests+=("$2") ;;
    --probe-runtime-digest) probe_runtime_digests+=("$2") ;;
    --chart-digest) chart_digest=$2 ;;
    --apps-digest) apps_digest=$2 ;;
    --apps-verify-file) apps_verify_file=$2 ;;
    --timeout) timeout_seconds=$2 ;;
    *) fail invalid_arguments 2 ;;
  esac
  shift 2
done
[[ -n "$context" && ${#context} -le 253 && "$context" != *$'\n'* && "$context" != *$'\r'* ]] || fail invalid_arguments 2
[[ ${KUBECONFIG:-} == /* && "$KUBECONFIG" != *:* && -f "$KUBECONFIG" && -r "$KUBECONFIG" ]] || fail invalid_arguments 2
[[ "$apps_verify_file" == /* && -f "$apps_verify_file" && -r "$apps_verify_file" ]] || fail invalid_arguments 2
[[ ${#domain} -le 253 && "$domain" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$ && ! "$domain" =~ ^[0-9.]+$ ]] || fail invalid_arguments 2
case "$domain" in *.local | *.localhost | *.lan | *.invalid | *.test) fail invalid_arguments 2 ;; esac
[[ ${#runtime_digests[@]} -gt 0 && ${#runtime_digests[@]} -le 8 ]] || fail invalid_arguments 2
[[ ${#probe_runtime_digests[@]} -gt 0 && ${#probe_runtime_digests[@]} -le 8 ]] || fail invalid_arguments 2
for value in "$image_digest" "$chart_digest" "$apps_digest" "${runtime_digests[@]}" "${probe_runtime_digests[@]}"; do
  [[ "$value" =~ ^sha256:[a-f0-9]{64}$ ]] || fail invalid_arguments 2
done
[[ "$timeout_seconds" =~ ^[1-9][0-9]{0,2}$ && "$timeout_seconds" -le 600 ]] || fail invalid_arguments 2
for tool in kubectl jq curl yq; do command -v "$tool" >/dev/null || fail missing_dependency 2; done
scratch=$(mktemp -d) || fail private_storage_unavailable
chmod 700 "$scratch"
readonly deadline=$((started_at + timeout_seconds))
set -m
# remaining enforces the shared deadline and records the time left for reads.
remaining() {
  left=$((deadline - SECONDS))
  [[ $left -gt 0 ]] || fail deadline_exceeded
}
# bounded runs one read within the remaining budget and reaps its process group.
bounded() {
  local result=0
  remaining
  "$@" &
  active=$!
  (
    sleep "$left"
    kill -KILL -- "-$active" 2>/dev/null || true
  ) &
  watchdog=$!
  wait "$active" 2>/dev/null || result=$?
  # Credential plugins can return while a child still holds this read's group.
  kill -KILL -- "-$active" 2>/dev/null || true
  active=''
  kill -KILL -- "-$watchdog" 2>/dev/null || true
  wait "$watchdog" 2>/dev/null || true
  watchdog=''
  remaining
  return "$result"
}
# read_expected_state accepts only the declared dormant or active flag profile.
read_expected_state() {
  yq -o=json '.spec.values | [.connectorReadiness.enabled,.contractReadiness.enabled,.contractProbe.enabled]' "$expected_helm_file" 2>/dev/null |
    jq -ers 'if length!=1 then error("ambiguous expected flags") elif .[0]==[false,false,false] then "dormant"
      elif .[0]==[true,true,false] then "active" else error("unsupported expected flags") end' >"$scratch/readiness-state" 2>/dev/null
}
bounded read_expected_state || fail invalid_expected_state 2
expected_state=$(<"$scratch/readiness-state")
# read_expected_releases binds source activation to this checkout, including rollback.
read_expected_releases() {
  yq -o=json '.spec.values.demoProduct.ksailReleases.enabled // false' "$expected_helm_file" 2>/dev/null |
    jq -ers 'if length==1 and (.[0]|type=="boolean") then .[0]|tostring else error("invalid release flag") end' >"$scratch/release-state" 2>/dev/null
}
bounded read_expected_releases || fail invalid_expected_state 2
expected_releases=$(<"$scratch/release-state")
# read_expected_release binds the caller's digests to the exact checked-out image and chart.
# A deploy that passes other digests, or a live release at another tag, is refused.
read_expected_release() {
  yq -o=json '.spec.values.image | {"repository": .repository, "tag": .tag, "digest": .digest}' "$expected_helm_file" >"$scratch/expected-image.json" 2>/dev/null || return 1
  yq -o=json '.spec | {"url": .url, "digest": .ref.digest}' "$expected_chart_file" >"$scratch/expected-chart.json" 2>/dev/null || return 1
  jq -ner --arg repository "$repository" --arg image_digest "$image_digest" --arg chart_digest "$chart_digest" \
    --slurpfile image "$scratch/expected-image.json" --slurpfile chart "$scratch/expected-chart.json" '
    if ($image|length)!=1 or ($chart|length)!=1 or ($image[0]|type)!="object" or ($chart[0]|type)!="object" or
      $image[0].repository!=$repository or $image[0].digest!=$image_digest or
      $chart[0].url!="oci://ghcr.io/devantler-tech/charts/khoist" or $chart[0].digest!=$chart_digest or
      ($image[0].tag|type)!="string" or ($image[0].tag|test("^[0-9]+\\.[0-9]+\\.[0-9]+$")|not)
    then error("unbound release declaration") else $image[0].tag end' >"$scratch/expected-tag" 2>/dev/null
}
bounded read_expected_release || fail invalid_expected_state 2
expected_tag=$(<"$scratch/expected-tag")
# read_expected_publisher binds the literal opt-in to this checkout, never to live flags.
read_expected_publisher() {
  yq -o=json '.spec.template.spec.containers' "$expected_publisher_file" 2>/dev/null |
    jq -ers '
      if length!=1 or (.[0]|type)!="array" then error("invalid publisher declaration") else .[0] end |
      [.[] | select(.name=="publisher-review")] |
      if length!=1 or (.[0].env|type)!="array" or (.[0].envFrom!=null and .[0].envFrom!=[]) then error("invalid publisher declaration") else .[0].env end |
      [.[] | select(.name=="PUBLISHER_PREFLIGHT_ENABLED")] |
      if all(.[];.valueFrom==null and (.value|type)=="string") then [.[].value] else error("invalid publisher declaration") end |
      if .==[] or .==["false"] then false elif .==["true"] then true else error("invalid publisher declaration") end |
      tostring' >"$scratch/publisher-state" 2>/dev/null
}
bounded read_expected_publisher || fail invalid_expected_state 2
expected_publisher=$(<"$scratch/publisher-state")
# read_expected_probe binds the standalone probe's exact declared immutable
# image separately from the installed chart and controller workloads.
read_expected_probe() {
  yq -o=json '.spec.template.spec.containers' "$expected_probe_file" 2>/dev/null |
    jq -ers 'if length==1 and (.[0]|type=="array" and length==1) and .[0][0].name=="contract-probe" and
      (.[0][0].image|type=="string" and test("^ghcr.io/devantler-tech/(data-product-controller|khoist)(:[0-9]+\\.[0-9]+\\.[0-9]+)?@sha256:[0-9a-f]{64}$"))
      then .[0][0].image else error("invalid probe declaration") end' >"$scratch/probe-image" 2>/dev/null
}
bounded read_expected_probe || fail invalid_expected_state 2
probe_image=$(<"$scratch/probe-image")
probe_repository=${probe_image%@*}
probe_repository=${probe_repository%%:*}
# read_verify_policy validates and copies the single public artifact identity policy.
read_verify_policy() {
  [[ $(wc -c <"$apps_verify_file") -le 16384 ]] || return 1
  jq -se 'length==1 and (.[0]|type=="object" and keys==["matchOIDCIdentity","provider"] and
    .provider=="cosign" and (.matchOIDCIdentity|type=="array" and length==1 and
      all(.[]; type=="object" and keys==["issuer","subject"] and
        (.issuer|type=="string" and length>0) and (.subject|type=="string" and length>0))))' "$apps_verify_file" >/dev/null || return 1
  jq -c '.' "$apps_verify_file" >"$scratch/apps-verify.json"
}
bounded read_verify_policy || fail invalid_verification_policy 2
apps_verify=$(<"$scratch/apps-verify.json")

cat >"$scratch/project.jq" <<'JQ'
def metadata: {name,namespace,uid,generation,deletionTimestamp,labels,
  revision:.annotations["deployment.kubernetes.io/revision"],
  owners:[.ownerReferences[]? | {apiVersion,kind,name,uid,controller}]};
def conditions: [.[]? | {type,status,observedGeneration,reason}];
def metrics_annotations: (.//{} | with_entries(select(.key|startswith("coroot.com/"))));
def containers: [.[]? | {name,image,ports:[.ports[]? | {name,containerPort,protocol:(.protocol//"TCP")}],
  command,args,envCount:(.env//[]|length),envFromCount:(.envFrom//[]|length),volumeMountCount:(.volumeMounts//[]|length),
  env:[.env[]? | select(.name=="CONTRACT_PROBE_URL" or .name=="CONTRACT_READINESS_ENABLED" or .name=="CONNECTOR_READINESS_ENABLED" or .name=="KSAIL_RELEASES_ENABLED") |
    {name,value,indirect:(.valueFrom!=null)}],
  publisherFlags:[.env[]? | select(.name=="PUBLISHER_PREFLIGHT_ENABLED") |
    {name,value,indirect:(.valueFrom!=null)}],
  retiredSettings:[.env[]? | select(.name=="UI_CONTRACT_ENABLED" or .name=="UI_APPEARANCE_ENABLED" or .name=="UI_HOST_ORIGINS" or .name=="REGISTRY_UI_ENABLED" or .name=="KSAIL_RELEASES_ENABLED") | .name],environmentLiteral:(.envFrom==null or .envFrom==[]),readinessProbe,livenessProbe,securityContext}];
def workload: {apiVersion,kind,metadata:(.metadata|metadata),
  spec:{replicas:.spec.replicas,selector:.spec.selector,labels:.spec.template.metadata.labels,
    metricsAnnotations:(.spec.template.metadata.annotations|metrics_annotations),containers:(.spec.template.spec.containers|containers),
    serviceAccountName:.spec.template.spec.serviceAccountName,automountServiceAccountToken:.spec.template.spec.automountServiceAccountToken,
    volumeCount:(.spec.template.spec.volumes//[]|length),initContainerCount:(.spec.template.spec.initContainers//[]|length)},
  status:(.status|{observedGeneration,replicas,updatedReplicas,readyReplicas,availableReplicas})};
def pod: {apiVersion,kind,metadata:(.metadata|metadata),spec:{containers:(.spec.containers|containers),
  metricsAnnotations:(.metadata.annotations|metrics_annotations),
  serviceAccountName:.spec.serviceAccountName,automountServiceAccountToken:.spec.automountServiceAccountToken,
  volumeCount:(.spec.volumes//[]|length),initContainerCount:(.spec.initContainers//[]|length)},
  status:{phase:.status.phase,podIP:.status.podIP,podIPs:[.status.podIPs[]? | .ip],conditions:(.status.conditions|conditions),
    containers:[.status.containerStatuses[]? | {name,ready,imageID,
      running:(.state.running|type=="object"),waiting:(.state.waiting!=null),terminated:(.state.terminated!=null)}]}};
if $allow_absent and length == 0 then {key:$role,value:null} else
if length != 1 or (.[0]|type)!="object" then error("incomplete response") else .[0] end |
{key:$role,value:(
if $role == "pods" or $role == "replicasets" or $role == "endpoints" then
  (if $role == "pods" then "Pod" elif $role == "endpoints" then "EndpointSlice" else "ReplicaSet" end) as $kind |
  (if $role == "pods" then "v1" elif $role == "endpoints" then "discovery.k8s.io/v1" else "apps/v1" end) as $version |
  if ((.kind == ($kind+"List") and .apiVersion == $version) or (.kind == "List" and .apiVersion == "v1")) and
    (.items|type=="array" and length<=4096) and (.metadata.continue//"")=="" and
    all(.items[]; .kind==$kind and .apiVersion==$version and .metadata.namespace==$namespace and
      (.metadata.name|type=="string" and length>0) and (.metadata.uid|type=="string" and length>0)) and
    ([.items[].metadata.uid]|length)==([.items[].metadata.uid]|unique|length) and
    ([.items[].metadata.name]|length)==([.items[].metadata.name]|unique|length)
  then {items:[.items[] | if $role == "pods" then pod elif $role == "endpoints" then
    {apiVersion,kind,metadata:(.metadata|metadata),addressType,ports:[.ports[]? | {name,port,protocol:(.protocol//"TCP")}],
      endpoints:[.endpoints[]? | {addresses,conditions:{ready:.conditions.ready,serving:.conditions.serving,terminating:.conditions.terminating},
        # The EndpointSlice controller leaves the core Pod reference version unset.
        targetRef:(.targetRef|{apiVersion:(if .apiVersion==null or .apiVersion=="" then "v1" else .apiVersion end),kind,name,namespace,uid})}]} else workload end]}
  else error("incomplete inventory") end
elif $role|startswith("deployment-") then workload
elif $role == "apps" then {apiVersion,kind,metadata:(.metadata|metadata),spec:{suspend:.spec.suspend,sourceRef:.spec.sourceRef},
  status:{observedGeneration:.status.observedGeneration,conditions:(.status.conditions|conditions),lastAppliedRevision:.status.lastAppliedRevision}}
elif $role == "chart" or $role == "root" then {apiVersion,kind,metadata:(.metadata|metadata),
  spec:{suspend:.spec.suspend,url:.spec.url,ref:{digest:.spec.ref.digest},verify:.spec.verify},
  status:{observedGeneration:.status.observedGeneration,conditions:(.status.conditions|conditions),artifact:(.status.artifact|{revision,digest})}}
elif $role == "release-product" then {apiVersion,kind,metadata:(.metadata|metadata),
  spec:{id:.spec.id,name:.spec.name,version:.spec.version,owner:(.spec.owner|{name,url}),connector:.spec.connector,contractChecks:(.spec.contractChecks//[]),
    outputs:[.spec.outputs[]? | {name,protocol,url,contractUrl,mediaType}],
    declaresUI:(.spec|has("ui"))},
  status:{observedGeneration:.status.observedGeneration,conditions:(.status.conditions|conditions)}}
elif $role == "product" then {apiVersion,kind,metadata:(.metadata|metadata),
  spec:{connector:.spec.connector,contractChecks:(.spec.contractChecks//[]),outputs:[.spec.outputs[]? | {name,url,contractUrl}],
    declaresUI:(.spec|has("ui"))},
  status:{observedGeneration:.status.observedGeneration,conditions:(.status.conditions|conditions)}}
elif $role|startswith("service-") then {apiVersion,kind,metadata:(.metadata|metadata),
  spec:{type:.spec.type,selector:.spec.selector,ports:[.spec.ports[]? | {name,port,targetPort,protocol:(.protocol//"TCP")}]}}
elif $role == "helm" then {apiVersion,kind,metadata:(.metadata|metadata),
  spec:{suspend:.spec.suspend,chartRef:.spec.chartRef,upgradeChartNameChangeStrategy:.spec.upgrade.chartNameChangeStrategy,values:{image:(.spec.values.image|{repository,tag,digest}),
    controller:{replicas:.spec.values.controller.replicas},demoProduct:(.spec.values.demoProduct|{enabled,replicas,publicBaseURL,ksailReleasesEnabled:(.ksailReleases.enabled//false)}),route:(.spec.values.route|{enabled,host}),
    observationFlags:[.spec.values | .connectorReadiness.enabled,.contractReadiness.enabled,.contractProbe.enabled]}},
  status:{observedGeneration:.status.observedGeneration,conditions:(.status.conditions|conditions),
    lastAttemptedRevision:.status.lastAttemptedRevision,lastAttemptedRevisionDigest:.status.lastAttemptedRevisionDigest,
    lastAttemptedConfigDigest:.status.lastAttemptedConfigDigest,history:[.status.history[]? | {name,namespace,chartName,chartVersion,ociDigest,configDigest,status,version}]}}
elif $role|startswith("route-") then {apiVersion,kind,metadata:(.metadata|metadata),
  spec:{parentRefs:.spec.parentRefs,hostnames:.spec.hostnames,rules:[.spec.rules[]? | {matches,backendRefs,
    filters:[.filters[]? | {type,remove:.requestHeaderModifier.remove}]}]},
  status:{parents:[.status.parents[]? | {parentRef,controllerName,conditions:(.conditions|conditions)}]}}
elif $role == "gateway" then {apiVersion,kind,metadata:(.metadata|metadata),
  spec:{gatewayClassName:.spec.gatewayClassName,listeners:[.spec.listeners[]? | {name,protocol,port,tls:{mode:.tls.mode}}]},
  status:{conditions:(.status.conditions|conditions),listeners:[.status.listeners[]? | {name,conditions:(.conditions|conditions)}]}}
elif $role == "probe-account" then {apiVersion,kind,metadata:(.metadata|metadata),automountServiceAccountToken}
elif $role == "observer-role" then {apiVersion,kind,metadata:(.metadata|metadata),rules}
elif $role == "observer-binding" then {apiVersion,kind,metadata:(.metadata|metadata),roleRef,subjects}
elif $role == "probe-policy" then {apiVersion,kind,metadata:(.metadata|metadata),spec,specs}
else error("unknown response") end)}
end
JQ
cat >"$scratch/check.jq" <<'JQ'
def positive: type=="number" and .==floor and .>0;
def digest: type=="string" and test("^sha256:[a-f0-9]{64}$");
def revision($expected): .==$expected or
  (type=="string" and (split("@") as $parts | ($parts|length)==2 and ($parts[0]|length)>0 and $parts[1]==$expected));
def object_identity($kind;$version;$name;$ns):
  .kind==$kind and .apiVersion==$version and .metadata.name==$name and .metadata.namespace==$ns and
  (.metadata.uid|type=="string" and length>0) and .metadata.deletionTimestamp==null;
def identity($kind;$version;$name;$ns): object_identity($kind;$version;$name;$ns) and (.metadata.generation|positive);
def ready($types): . as $resource |
  all($types[]; . as $type | [$resource.status.conditions[]? | select(.type==$type)] as $conditions |
    ($conditions|length)==1 and $conditions[0].status=="True" and $conditions[0].observedGeneration==$resource.metadata.generation) and
  all(.status.conditions[]?; (.type!="Reconciling" and .type!="Stalled") or .status!="True");
def owned($kind;$name;$uid): [.metadata.owners[]? | select(.controller==true)] as $owners |
  ($owners|length)==1 and $owners[0].apiVersion=="apps/v1" and $owners[0].kind==$kind and $owners[0].name==$name and $owners[0].uid==$uid;
def image($tag): .==($repository+"@"+$image_digest) or .==($repository+":"+$tag+"@"+$image_digest);
def named_image($container;$tag): [.spec.containers[]? | select(.name==$container)] as $containers |
  ($containers|length)==1 and (if $container=="contract-probe" then $containers[0].image==$probe_image else $containers[0].image|image($tag) end);
def no_retired_settings: all(.spec.containers[]?;.retiredSettings==[]);
def publisher_flag_ready($container):
  if $container!="publisher-review" then true else
    [.spec.containers[]? | select(.name==$container)] as $containers |
    ($containers|length)==1 and $containers[0].environmentLiteral==true and ($containers[0].publisherFlags as $flags |
      ($flags|type=="array" and all(.[];.indirect==false and (.value|type)=="string")) and
      (if $publisher_enabled then [$flags[].value]==["true"] else $flags==[] or [$flags[].value]==["false"] end))
  end;
def runtime($container): . as $id |
  (if $container=="contract-probe" then $probe_runtime_digests else $runtime_digests end) as $digests |
  (if $container=="contract-probe" then $probe_repository else $repository end) as $runtime_repository |
  any($digests[]; . as $digest |
  $id==$digest or $id==("containerd://"+$digest) or $id==("docker://"+$digest) or
  $id==("cri-o://"+$digest) or $id==($runtime_repository+"@"+$digest) or $id==("docker-pullable://"+$runtime_repository+"@"+$digest));
def inventory($kind;$version): (.items|type=="array" and length<=4096) and
  all(.items[]; .kind==$kind and .apiVersion==$version and .metadata.namespace==$namespace and
    (.metadata.name|type=="string" and length>0) and (.metadata.uid|type=="string" and length>0)) and
  ([.items[].metadata.uid]|length)==([.items[].metadata.uid]|unique|length) and
  ([.items[].metadata.name]|length)==([.items[].metadata.name]|unique|length);
def pod_ready($rs;$container;$tag):
  owned("ReplicaSet";$rs.metadata.name;$rs.metadata.uid) and .metadata.deletionTimestamp==null and
  .status.phase=="Running" and ([.status.conditions[]? | select(.type=="Ready")]|length)==1 and
  any(.status.conditions[]?;.type=="Ready" and .status=="True") and named_image($container;$tag) and publisher_flag_ready($container) and no_retired_settings and
  ([.status.containers[]? | select(.name==$container)] as $containers | ($containers|length)==1 and
    $containers[0].ready==true and $containers[0].running==true and $containers[0].waiting==false and
    $containers[0].terminated==false and ($containers[0].imageID|runtime($container)));
def workload($deployment;$container;$tag;$sets;$pods):
  ($deployment|identity("Deployment";"apps/v1";$deployment.metadata.name;$namespace)) and
  ($deployment.metadata.revision|type=="string" and test("^[1-9][0-9]*$")) and
  $deployment.spec.replicas==2 and $deployment.status.observedGeneration==$deployment.metadata.generation and
  all([$deployment.status.replicas,$deployment.status.updatedReplicas,$deployment.status.readyReplicas,$deployment.status.availableReplicas][];.==2) and
  ($deployment|named_image($container;$tag) and publisher_flag_ready($container) and no_retired_settings) and
  ([$sets[] | select(owned("Deployment";$deployment.metadata.name;$deployment.metadata.uid))] as $owned |
    [$owned[] | select(.metadata.revision==$deployment.metadata.revision)] as $current |
    ($current|length)==1 and
    ($current[0]|identity("ReplicaSet";"apps/v1";.metadata.name;$namespace)) and
    $current[0].spec.replicas==2 and $current[0].status.observedGeneration==$current[0].metadata.generation and
    all([$current[0].status.replicas,$current[0].status.readyReplicas,$current[0].status.availableReplicas][];.==2) and
    ($current[0]|named_image($container;$tag) and publisher_flag_ready($container) and no_retired_settings) and
    all($owned[] | select(.metadata.uid!=$current[0].metadata.uid); .spec.replicas==0 and (.status.replicas//0)==0) and
    ([$pods[] | . as $pod | select(any($owned[]; . as $rs | $pod | owned("ReplicaSet";$rs.metadata.name;$rs.metadata.uid)))] as $owned_pods |
      ($owned_pods|length)==2 and all($owned_pods[];pod_ready($current[0];$container;$tag))));
def dormant_probe($sets;$pods):
  identity("Deployment";"apps/v1";"data-product-controller-contract-probe";$namespace) and
  .spec.replicas==0 and .status.observedGeneration==.metadata.generation and
  all([.status.replicas,.status.updatedReplicas,.status.readyReplicas,.status.availableReplicas][];(.//0)==0) and
  (.metadata.uid as $uid | [$sets[] | select(owned("Deployment";"data-product-controller-contract-probe";$uid))] as $owned |
    all($owned[];.spec.replicas==0 and (.status.replicas//0)==0) and
    all($pods[];. as $pod | all($owned[];. as $rs | $pod | owned("ReplicaSet";$rs.metadata.name;$rs.metadata.uid)|not)));
def private_probe_spec($tag;$enabled):
  .spec.metricsAnnotations=={"coroot.com/scrape-metrics":"true","coroot.com/metrics-port":"8081","coroot.com/metrics-path":"/metrics","coroot.com/metrics-scheme":"http"} and
  .spec.serviceAccountName=="data-product-controller-contract-probe" and .spec.automountServiceAccountToken==false and .spec.volumeCount==0 and .spec.initContainerCount==0 and
  (.spec.containers|length)==1 and named_image("contract-probe";$tag) and
  (.spec.containers[0] | .command==["/contract-probe"] and (.args//[])==[] and .envCount==2 and .envFromCount==0 and .volumeMountCount==0 and
    (.env|sort_by(.name))==([{name:"CONTRACT_PROBE_URL",value:("https://harbour-data."+$domain+"/openapi.json"),indirect:false},
      {name:"CONTRACT_READINESS_ENABLED",value:$enabled,indirect:false}]|sort_by(.name)) and
    .ports==[{name:"management",containerPort:8081,protocol:"TCP"}] and
    .readinessProbe.httpGet=={path:"/readyz",port:"management",scheme:"HTTP"} and .readinessProbe.timeoutSeconds==7 and
    .readinessProbe.periodSeconds==30 and .readinessProbe.failureThreshold==1 and
    .livenessProbe.httpGet=={path:"/healthz",port:"management",scheme:"HTTP"} and
    .securityContext.runAsNonRoot==true and .securityContext.readOnlyRootFilesystem==true and
    .securityContext.allowPrivilegeEscalation==false and .securityContext.capabilities.drop==["ALL"]);
def probe_configuration($tag;$enabled):
  .spec.selector.matchLabels=={"app.kubernetes.io/name":"data-product-controller","app.kubernetes.io/instance":"data-product-controller","app.kubernetes.io/component":"contract-probe"} and
  .spec.labels==.spec.selector.matchLabels and private_probe_spec($tag;$enabled);
def reference($name): (.namespace//"")=="" and (del(.namespace))==
  {apiVersion:"apps/v1",kind:"Deployment",name:$name};
def product_observation:
  . as $product |
  if $readiness_state=="dormant" then .spec.connector==null and .spec.contractChecks==[] and
    all(.status.conditions[]?;.type!="ConnectorReady" and .type!="ContractsReady")
  else .spec.connector.adapter=="deployment/v1" and (.spec.connector|keys)==["adapter","resourceRef"] and
    (.spec.connector.resourceRef|reference("data-product-controller-harbour")) and
    (.spec.contractChecks|length)==1 and (.spec.contractChecks[0]|keys)==["output","resourceRef"] and
    .spec.contractChecks[0].output=="observations" and
    (.spec.contractChecks[0].resourceRef|reference("data-product-controller-contract-probe")) and
    ready(["ConnectorReady","ContractsReady","Ready"]) and
    all([["ConnectorReady","ConnectorReady"],["ContractsReady","ContractsReady"],["Ready","DependenciesReady"]][];
      . as $expected | any($product.status.conditions[]?; .type==$expected[0] and .reason==$expected[1]))
  end;
def readiness_observation($snapshot;$tag):
  ($snapshot["deployment-probe"] as $probe |
    ($probe|probe_configuration($tag;($readiness_state=="active"|tostring))) and
    if $readiness_state=="dormant" then $probe|dormant_probe($snapshot.replicasets.items;$snapshot.pods.items)
    else ($probe|workload(.;"contract-probe";$tag;$snapshot.replicasets.items;$snapshot.pods.items)) and
      ($probe.metadata.labels//{}|has("platform.devantler.tech/replica-floor")|not) and
      all($snapshot.replicasets.items[] | select(owned("Deployment";$probe.metadata.name;$probe.metadata.uid) and .metadata.revision==$probe.metadata.revision);
        private_probe_spec($tag;"true")) and
      ([$snapshot.replicasets.items[] | select(owned("Deployment";$probe.metadata.name;$probe.metadata.uid)) | .metadata.uid] as $probe_sets |
        all($snapshot.pods.items[] | select(any(.metadata.owners[]?; .uid as $uid | $probe_sets|index($uid)!=null));
          . as $pod | private_probe_spec($tag;"true") and
          all($probe.spec.labels|to_entries[]; . as $label | $pod.metadata.labels[$label.key]==$label.value)))
    end) and
  ($snapshot["probe-account"]|object_identity("ServiceAccount";"v1";"data-product-controller-contract-probe";$namespace) and .automountServiceAccountToken==false) and
  ($snapshot["observer-role"]|object_identity("Role";"rbac.authorization.k8s.io/v1";"data-product-readiness-observer";$namespace) and
    (.rules|map(.resourceNames|=sort))==[{apiGroups:["apps"],resources:["deployments"],resourceNames:["data-product-controller-contract-probe","data-product-controller-harbour"],verbs:["get"]}]) and
  ($snapshot["observer-binding"]|object_identity("RoleBinding";"rbac.authorization.k8s.io/v1";"data-product-readiness-observer";$namespace) and
    .roleRef=={apiGroup:"rbac.authorization.k8s.io",kind:"Role",name:"data-product-readiness-observer"} and
    .subjects==[{kind:"ServiceAccount",name:"data-product-controller",namespace:$namespace}]) and
  ($snapshot["probe-policy"]|object_identity("CiliumNetworkPolicy";"cilium.io/v2";"allow-data-product-controller-contract-probe";$namespace) and
    (.specs==null or .specs==[]) and .spec=={endpointSelector:{matchLabels:{"k8s:app.kubernetes.io/name":"data-product-controller","k8s:app.kubernetes.io/instance":"data-product-controller","k8s:app.kubernetes.io/component":"contract-probe"}},
      ingress:[{fromEndpoints:[{matchLabels:{"k8s:io.kubernetes.pod.namespace":"observability","app.kubernetes.io/managed-by":"coroot-operator","app.kubernetes.io/part-of":"coroot","app.kubernetes.io/component":"coroot-cluster-agent"}}],toPorts:[{ports:[{port:"8081",protocol:"TCP"}]}]}],
      egress:[{toFQDNs:[{matchName:("harbour-data."+$domain)}],toPorts:[{ports:[{port:"443",protocol:"TCP"}]}]},
        {toEndpoints:[{matchLabels:{"k8s:io.kubernetes.pod.namespace":"kube-system","k8s-app":"kube-dns"}}],
         toPorts:[{ports:[{port:"53",protocol:"UDP"},{port:"53",protocol:"TCP"}],rules:{dns:[{matchName:("harbour-data."+$domain)}]}}]}]});
def family: if contains(":") then "IPv6" else "IPv4" end;
def backend($service;$deployment;$container;$sets;$pods;$slices):
  $deployment.spec.selector.matchLabels as $selector |
  [$sets[] | select(owned("Deployment";$deployment.metadata.name;$deployment.metadata.uid)) | .metadata.uid] as $rs_uids |
  [$pods[] | select(any(.metadata.owners[]?; .kind=="ReplicaSet" and (.uid as $uid | $rs_uids|index($uid))!=null))] as $current_pods |
  [$current_pods[].metadata.uid]|sort as $pod_uids |
  [$slices[] | select(.metadata.labels["kubernetes.io/service-name"]==$service.metadata.name or
    any(.metadata.owners[]?;.uid==$service.metadata.uid))] as $owned_slices |
  ($service|object_identity("Service";"v1";$deployment.metadata.name;$namespace)) and $service.spec.type=="ClusterIP" and
  ($selector|type=="object" and length>0) and $service.spec.selector==$selector and
  all($selector|to_entries[];. as $label | $deployment.spec.labels[$label.key]==$label.value) and
  $service.spec.ports==[{name:"http",port:80,targetPort:"http",protocol:"TCP"}] and
  ([$deployment.spec.containers[] | select(.name==$container) | .ports[] | select(.name=="http")]==[{name:"http",containerPort:8080,protocol:"TCP"}]) and
  ($current_pods|length)==2 and all($current_pods[]; . as $pod |
    all($selector|to_entries[];. as $label | $pod.metadata.labels[$label.key]==$label.value) and
    ([.spec.containers[] | select(.name==$container) | .ports[] | select(.name=="http")]==[{name:"http",containerPort:8080,protocol:"TCP"}]) and
    (.status.podIPs|type=="array" and length>0 and length==(unique|length) and all(.[];type=="string" and length>0)) and
    (.status.podIP as $primary|.status.podIPs|index($primary)!=null)) and
  ($owned_slices|length)>0 and all($owned_slices[]; . as $slice |
    object_identity("EndpointSlice";"discovery.k8s.io/v1";.metadata.name;$namespace) and
    .metadata.labels["kubernetes.io/service-name"]==$service.metadata.name and
    ([.metadata.owners[]? | select(.controller==true)]==[{apiVersion:"v1",kind:"Service",name:$service.metadata.name,uid:$service.metadata.uid,controller:true}]) and
    (.addressType=="IPv4" or .addressType=="IPv6") and .ports==[{name:"http",port:8080,protocol:"TCP"}] and
    (.endpoints|type=="array") and all(.endpoints[]; . as $endpoint |
      .conditions.ready==true and (.conditions.terminating//false)==false and (.conditions.serving//true)==true and
      .targetRef.apiVersion=="v1" and .targetRef.kind=="Pod" and .targetRef.namespace==$namespace and
      ([$current_pods[] | select(.metadata.uid==$endpoint.targetRef.uid and .metadata.name==$endpoint.targetRef.name)] as $target |
        ($target|length)==1 and (.addresses|type=="array" and length>0 and length==(unique|length)) and
        (.addresses|sort)==([$target[0].status.podIPs[] | select(family==$slice.addressType)]|sort)))) and
  ([$owned_slices[].endpoints[].targetRef.uid]|unique|sort)==$pod_uids and
  ([$current_pods[].status.podIPs[]|family]|unique|sort)==([$owned_slices[].addressType]|unique|sort) and
  all([$owned_slices[].addressType]|unique|.[];. as $family |
    ([$owned_slices[]|select(.addressType==$family)|.endpoints[].targetRef.uid]|unique|sort)==$pod_uids);
def parent($ns): {group:(.group//"gateway.networking.k8s.io"),kind:(.kind//"Gateway"),namespace:(.namespace//$ns),name,sectionName,port:(.port//null)};
def route($name;$host;$backend;$public):
  .metadata.generation as $resource_generation |
  identity("HTTPRoute";"gateway.networking.k8s.io/v1";$name;$namespace) and
  (.spec.parentRefs|map(parent($namespace)))==[{group:"gateway.networking.k8s.io",kind:"Gateway",namespace:"kube-system",name:"platform",sectionName:"https",port:null}] and
  .spec.hostnames==[$host+"."+$domain] and (.spec.rules|length)==1 and
  .spec.rules[0].matches==[{path:{type:"PathPrefix",value:"/"}}] and
  (.spec.rules[0].backendRefs|map({name,namespace:(.namespace//$namespace),port,weight:(.weight//1),group:(.group//""),kind:(.kind//"Service")}))==[$backend+{weight:1,group:"",kind:"Service"}] and
  (if $public then [.spec.rules[0].filters[]? | select(.type=="RequestHeaderModifier") | .remove] == [["Cookie","Authorization"]] else true end) and
  ([.status.parents[]? | select(.controllerName=="io.cilium/gateway-controller" and
    (.parentRef|parent($namespace))=={group:"gateway.networking.k8s.io",kind:"Gateway",namespace:"kube-system",name:"platform",sectionName:"https",port:null})] as $parents |
    ($parents|length)==1 and all(["Accepted","ResolvedRefs"][]; . as $type |
      [$parents[0].conditions[]? | select(.type==$type)] as $conditions |
      ($conditions|length)==1 and $conditions[0].status=="True" and $conditions[0].observedGeneration==$resource_generation));
. as $snapshot |
.helm.spec.values.image.tag as $tag |
def demand($role): if . then true else error({role:$role,reason:"predicate_not_ready"}) end;
try (
((.apps|identity("Kustomization";"kustomize.toolkit.fluxcd.io/v1";"apps";"flux-system") and
  (.spec.suspend//false)==false and .status.observedGeneration==.metadata.generation and ready(["Ready"]) and
  .spec.sourceRef.kind=="OCIRepository" and .spec.sourceRef.name=="flux-system" and (.spec.sourceRef.namespace//"flux-system")=="flux-system" and
  (.status.lastAppliedRevision|revision($apps_digest))) | demand("apps")) and
((.root|identity("OCIRepository";"source.toolkit.fluxcd.io/v1";"flux-system";"flux-system") and
  (.spec.suspend//false)==false and .status.observedGeneration==.metadata.generation and ready(["Ready","SourceVerified"]) and
  .spec.verify==$apps_verify and (.status.artifact.revision|revision($apps_digest)) and (.status.artifact.digest|digest)) | demand("root")) and
((.chart|identity("OCIRepository";"source.toolkit.fluxcd.io/v1";"data-product-controller";$namespace) and
  (.spec.suspend//false)==false and .status.observedGeneration==.metadata.generation and ready(["Ready","SourceVerified"]) and
  .spec.url=="oci://ghcr.io/devantler-tech/charts/khoist" and .spec.ref.digest==$chart_digest and
  .spec.verify=={provider:"cosign",matchOIDCIdentity:[{issuer:"^https://token\\.actions\\.githubusercontent\\.com$",subject:"^https://github\\.com/devantler-tech/khoist/\\.github/workflows/publish-chart\\.yaml@refs/tags/v[0-9]+\\.[0-9]+\\.[0-9]+$"}]} and
  # The cached file digest differs from the upstream OCI manifest revision.
  (.status.artifact.revision|revision($chart_digest)) and (.status.artifact.digest|digest)) | demand("chart")) and
((.helm|identity("HelmRelease";"helm.toolkit.fluxcd.io/v2";"data-product-controller";$namespace) and
  (.spec.suspend//false)==false and .status.observedGeneration==.metadata.generation and ready(["Ready"]) and
  .spec.upgradeChartNameChangeStrategy=="InPlaceUpdate" and
  .spec.chartRef.kind=="OCIRepository" and .spec.chartRef.name=="data-product-controller" and (.spec.chartRef.namespace//$namespace)==$namespace and
  .spec.values.image.repository==$repository and .spec.values.image.digest==$image_digest and
  $tag==$expected_tag and
  .spec.values.controller.replicas==2 and .spec.values.demoProduct.enabled==true and
  .spec.values.observationFlags==[$readiness_state=="active",$readiness_state=="active",false] and
  .spec.values.demoProduct.replicas==2 and .spec.values.demoProduct.publicBaseURL==("https://harbour-data."+$domain) and
  .spec.values.demoProduct.ksailReleasesEnabled==$ksail_enabled and
  .spec.values.route.enabled==true and .spec.values.route.host==("data-products."+$domain) and
  .status.lastAttemptedRevisionDigest==$chart_digest and .status.lastAttemptedRevision==($tag+"+"+$chart_digest[7:19]) and
  (.status.lastAttemptedConfigDigest|digest) and .status.history[0].status=="deployed" and (.status.history[0].version|positive) and
  .status.history[0].name=="data-product-controller" and .status.history[0].namespace==$namespace and .status.history[0].chartName=="khoist" and
  .status.history[0].chartVersion==.status.lastAttemptedRevision and .status.history[0].ociDigest==$chart_digest and .status.history[0].configDigest==.status.lastAttemptedConfigDigest) | demand("helm")) and
((.product|identity("DataProduct";"data.devantler.tech/v1alpha1";"harbour-observations";$namespace) and
  .status.observedGeneration==.metadata.generation and ready(["Ready"]) and .spec.declaresUI==false and
  .spec.outputs==[{name:"observations",url:("https://harbour-data."+$domain+"/api/observations"),contractUrl:("https://harbour-data."+$domain+"/openapi.json")}] and
  product_observation) | demand("product")) and
((if $ksail_enabled then
  (."release-product"|identity("DataProduct";"data.devantler.tech/v1alpha1";"ksail-releases";$namespace) and
    .status.observedGeneration==.metadata.generation and ready(["Ready"]) and
    .spec.id=="https://github.com/devantler-tech/ksail/releases" and .spec.name=="KSail releases" and .spec.version=="v1.0.0" and
    .spec.owner=={name:"devantler-tech",url:"https://github.com/devantler-tech/ksail"} and
    .spec.connector==null and .spec.contractChecks==[] and
    .spec.outputs==[{name:"releases",protocol:"OpenAPI",url:"https://api.github.com/repos/devantler-tech/ksail/releases",
      contractUrl:("https://harbour-data."+$domain+"/ksail-releases/openapi.json"),mediaType:"application/json"}] and
    .spec.declaresUI==false)
  else ."release-product"==null end) | demand("release-product")) and
((."deployment-harbour" as $harbour |
  [$harbour] + [.replicasets.items[]? | select(owned("Deployment";$harbour.metadata.name;$harbour.metadata.uid) and .metadata.revision==$harbour.metadata.revision)] +
    [.pods.items[]? | select(.metadata.labels["app.kubernetes.io/component"]=="harbour-product")] |
  all(.[]; [.spec.containers[]? | select(.name=="product")] as $products | ($products|length)==1 and $products[0].envFromCount==0 and
    ([$products[0].env[]? | select(.name=="KSAIL_RELEASES_ENABLED")]==[]))) | demand("release-flags")) and
((.replicasets|inventory("ReplicaSet";"apps/v1")) | demand("replicasets")) and
((.pods|inventory("Pod";"v1")) | demand("pods")) and
((.endpoints|inventory("EndpointSlice";"discovery.k8s.io/v1")) | demand("endpoints")) and
(readiness_observation($snapshot;$tag) | demand("readiness")) and
(([."deployment-controller".spec.containers[] | select(.name=="controller") | .env[]] | sort_by(.name))==
  [{name:"CONNECTOR_READINESS_ENABLED",value:($readiness_state=="active"|tostring),indirect:false},
    {name:"CONTRACT_READINESS_ENABLED",value:($readiness_state=="active"|tostring),indirect:false}] | demand("controller-flags")) and
(all([["deployment-controller","data-product-controller","controller"],["deployment-harbour","data-product-controller-harbour","product"],["deployment-publisher-review","data-product-controller-publisher-review","publisher-review"]][];
  . as $id | $snapshot[$id[0]] as $deployment |
  $deployment.metadata.name==$id[1] and workload($deployment;$id[2];$tag;$snapshot.replicasets.items;$snapshot.pods.items)) | demand("workloads")) and
(all([["harbour","product"],["publisher-review","publisher-review"]][];. as $id |
  backend($snapshot["service-"+$id[0]];$snapshot["deployment-"+$id[0]];$id[1];$snapshot.replicasets.items;$snapshot.pods.items;$snapshot.endpoints.items)) | demand("backends")) and
(all([["route-registry","data-product-controller-registry","data-products",{name:"oauth2-proxy",namespace:"oauth2-proxy",port:80},false],
     ["route-harbour","data-product-controller-harbour","harbour-data",{name:"data-product-controller-harbour",namespace:$namespace,port:80},true],
     ["route-publisher-review","data-product-controller-publisher-review","publisher-review",{name:"data-product-controller-publisher-review",namespace:$namespace,port:80},true]][];
  . as $id | $snapshot[$id[0]] | route($id[1];$id[2];$id[3];$id[4])) | demand("routes")) and
((.gateway|identity("Gateway";"gateway.networking.k8s.io/v1";"platform";"kube-system") and .spec.gatewayClassName=="cilium" and
  ready(["Accepted","Programmed"]) and
  ([.spec.listeners[] | select(.name=="https")]|map({name,protocol,port,tls}))==[{name:"https",protocol:"HTTPS",port:443,tls:{mode:"Terminate"}}] and
  (.metadata.generation as $generation | [.status.listeners[]? | select(.name=="https")] as $listeners | ($listeners|length)==1 and
    all(["Accepted","ResolvedRefs","Programmed"][]; . as $type | [$listeners[0].conditions[]? | select(.type==$type)] as $conditions |
      ($conditions|length)==1 and $conditions[0].status=="True" and $conditions[0].observedGeneration==$generation))) | demand("gateway"))
) catch (
  if type=="object" and (.role|type)=="string" and .reason=="predicate_not_ready" then
    {role:.role,reason:.reason}
  else {role:"snapshot",reason:"observation_invalid"} end | ., false
)
JQ

# project_read stores only the reviewed fields from one resource response.
project_read() {
  local role=$1 destination=$2 allow_absent=$3
  shift 3
  kubectl "$@" 2>/dev/null | jq -se --arg role "$role" --arg namespace "$namespace" --argjson allow_absent "$allow_absent" -f "$scratch/project.jq" >"$destination" 2>/dev/null
}
# read_resource fetches and projects one fixed target within the shared deadline.
read_resource() {
  local directory=$1 role=$2 ns=$3 resource=$4 name=${5:-} request_seconds allow_absent=false
  remaining
  request_seconds=$left
  ((request_seconds <= 5)) || request_seconds=5
  local args=(--kubeconfig "$KUBECONFIG" --context "$context" --namespace "$ns" --request-timeout "${request_seconds}s" get "$resource")
  [[ -z "$name" ]] || args+=("$name")
  # Only a disabled release product may be absent. kubectl still fails on
  # Forbidden, partial responses and transport errors; pipefail preserves that.
  if [[ "$role" == release-product && "$expected_releases" == false ]]; then
    allow_absent=true
    args+=(--ignore-not-found)
  fi
  args+=(-o json)
  printf '%s read_incomplete\n' "$role" >"$scratch/read-failure"
  # One failed read proves nothing about the rollout, so it is read once more
  # inside the shared deadline. A second failure still refuses, and the snapshot
  # pair still has to agree, so a retried read accepts nothing the first would not.
  if ! bounded project_read "$role" "$directory/$role.json" "$allow_absent" "${args[@]}"; then
    printf '%s\n' "$role" >>"$scratch/read-retries"
    bounded project_read "$role" "$directory/$role.json" "$allow_absent" "${args[@]}" || fail read_incomplete
  fi
}
# collect gathers every resource needed to judge a complete rollout snapshot.
collect() {
  local directory=$1
  mkdir -p "$directory"
  read_resource "$directory" apps flux-system kustomizations.kustomize.toolkit.fluxcd.io apps
  read_resource "$directory" root flux-system ocirepositories.source.toolkit.fluxcd.io flux-system
  read_resource "$directory" chart "$namespace" ocirepositories.source.toolkit.fluxcd.io data-product-controller
  read_resource "$directory" helm "$namespace" helmreleases.helm.toolkit.fluxcd.io data-product-controller
  read_resource "$directory" product "$namespace" dataproducts.data.devantler.tech harbour-observations
  read_resource "$directory" release-product "$namespace" dataproducts.data.devantler.tech ksail-releases
  read_resource "$directory" service-harbour "$namespace" services data-product-controller-harbour
  read_resource "$directory" service-publisher-review "$namespace" services data-product-controller-publisher-review
  read_resource "$directory" endpoints "$namespace" endpointslices.discovery.k8s.io
  read_resource "$directory" deployment-controller "$namespace" deployments.apps data-product-controller
  read_resource "$directory" deployment-harbour "$namespace" deployments.apps data-product-controller-harbour
  read_resource "$directory" deployment-publisher-review "$namespace" deployments.apps data-product-controller-publisher-review
  read_resource "$directory" deployment-probe "$namespace" deployments.apps data-product-controller-contract-probe
  read_resource "$directory" probe-account "$namespace" serviceaccounts data-product-controller-contract-probe
  read_resource "$directory" observer-role "$namespace" roles.rbac.authorization.k8s.io data-product-readiness-observer
  read_resource "$directory" observer-binding "$namespace" rolebindings.rbac.authorization.k8s.io data-product-readiness-observer
  read_resource "$directory" probe-policy "$namespace" ciliumnetworkpolicies.cilium.io allow-data-product-controller-contract-probe
  read_resource "$directory" replicasets "$namespace" replicasets.apps
  read_resource "$directory" pods "$namespace" pods
  read_resource "$directory" route-registry "$namespace" httproutes.gateway.networking.k8s.io data-product-controller-registry
  read_resource "$directory" route-harbour "$namespace" httproutes.gateway.networking.k8s.io data-product-controller-harbour
  read_resource "$directory" route-publisher-review "$namespace" httproutes.gateway.networking.k8s.io data-product-controller-publisher-review
  read_resource "$directory" gateway kube-system gateways.gateway.networking.k8s.io platform
  printf 'snapshot observation_invalid\n' >"$scratch/read-failure"
  bounded aggregate "$directory" || fail read_incomplete
}
# aggregate keeps only owned rollout descendants and relevant endpoint slices.
aggregate() {
  jq -s '
    def owner_in($kind;$uids): any(.metadata.owners[]?;
      .kind==$kind and (.uid as $uid | $uids | index($uid))!=null);
    from_entries |
    [."deployment-controller".metadata.uid,."deployment-harbour".metadata.uid,."deployment-publisher-review".metadata.uid,."deployment-probe".metadata.uid] as $deployment_uids |
    .replicasets.items |= (map(select(owner_in("Deployment";$deployment_uids))) | sort_by(.metadata.uid)) |
    [.replicasets.items[].metadata.uid] as $rs_uids |
    .pods.items |= (map(select(owner_in("ReplicaSet";$rs_uids))) | sort_by(.metadata.uid)) |
    [."service-harbour",."service-publisher-review"] as $services |
    .endpoints.items |= (map(select(. as $slice | any($services[];. as $service |
      $slice.metadata.labels["kubernetes.io/service-name"]==$service.metadata.name or any($slice.metadata.owners[]?;.uid==$service.metadata.uid)))) | sort_by(.metadata.uid))
  ' "$1/"*.json >"$1/snapshot" 2>/dev/null
}
# make_runtime_json records the unique allowed runtime digests for the predicates.
make_runtime_json() { printf '%s\n' "${runtime_digests[@]}" | jq -Rsc 'split("\n")[:-1]|unique' >"$scratch/runtime-digests.json"; }
bounded make_runtime_json || fail read_incomplete
runtime_json=$(<"$scratch/runtime-digests.json")
# make_probe_runtime_json keeps the probe's resolved runtimes independent.
make_probe_runtime_json() { printf '%s\n' "${probe_runtime_digests[@]}" | jq -Rsc 'split("\n")[:-1]|unique' >"$scratch/probe-runtime-digests.json"; }
bounded make_probe_runtime_json || fail read_incomplete
probe_runtime_json=$(<"$scratch/probe-runtime-digests.json")
# check requires the snapshot to satisfy the declared identities and readiness state.
check() {
  local result=0
  jq -ce --arg repository "$repository" --arg namespace "$namespace" --arg domain "$domain" --arg image_digest "$image_digest" --arg expected_tag "$expected_tag" --arg readiness_state "$expected_state" --argjson ksail_enabled "$expected_releases" --argjson publisher_enabled "$expected_publisher" \
    --arg chart_digest "$chart_digest" --arg apps_digest "$apps_digest" --argjson apps_verify "$apps_verify" --argjson runtime_digests "$runtime_json" \
    --arg probe_image "$probe_image" --arg probe_repository "$probe_repository" --argjson probe_runtime_digests "$probe_runtime_json" \
    -f "$scratch/check.jq" "$1/snapshot" >"$scratch/check-result" 2>/dev/null || result=$?
  if ((result == 0)); then
    rm -f "$scratch/snapshot-failure"
    return 0
  fi
  # Only the structured refusal emitted by our fixed predicates is retained.
  # Native jq errors and incomplete output cannot become diagnostic values.
  if jq -ers 'if length==2 and .[1]==false and (.[0]|type)=="object" and
    (.[0]|keys)==["reason","role"] and (.[0].role|type)=="string" and
    (.[0].reason=="predicate_not_ready" or .[0].reason=="observation_invalid")
    then .[0].role+" "+.[0].reason else error("incomplete diagnostic") end' \
    "$scratch/check-result" >"$scratch/snapshot-failure.next" 2>/dev/null; then
    mv "$scratch/snapshot-failure.next" "$scratch/snapshot-failure"
  else
    printf 'snapshot observation_invalid\n' >"$scratch/snapshot-failure"
  fi
  return "$result"
}

# refuse_public records fixed identifiers and optional numeric diagnostics.
# Normalize curl's output before writing the single-line private record: an
# unexpected write-out value must never become response data in a public receipt.
refuse_public() {
  local http_code=${3:--}
  [[ "$http_code" =~ ^(000|[1-5][0-9]{2})$ ]] || http_code=-
  printf '%s %s %s %s\n' "$1" "$2" "$http_code" "${4:--}" >"$scratch/public-failure"
}

# fetch retrieves one bounded HTTPS response without browser credentials.
# A dropped connection (refused, empty reply or reset) is retried once inside the
# shared deadline. Its cause is recorded first, so a retry the deadline forbids
# still names it; every other failure refuses on its first attempt.
fetch() {
  local url=$1 role=$2 result curl_exit reason=transport_failed retried=0
  while :; do
    remaining
    # Leave one second for curl's refusal to reach the parent before its watchdog fires.
    # An exhausted budget still refuses; it never grants extra transfer time.
    ((left > 1)) || fail deadline_exceeded
    local transfer_seconds=$((left - 1)) connect_seconds=$((left - 1))
    ((connect_seconds <= 5)) || connect_seconds=5
    curl_exit=0
    result=$(curl --disable --silent --show-error --proxy '' --connect-timeout "$connect_seconds" --max-time "$transfer_seconds" \
      --max-filesize 262144 --proto '=https' --header 'Cookie:' --header 'Authorization:' \
      --dump-header "$scratch/headers" --output "$scratch/body" --write-out '%{http_code}' --url "$url" 2>/dev/null) || curl_exit=$?
    case "$curl_exit" in
      7 | 52 | 56) ;;
      *) break ;;
    esac
    ((retried == 0)) || break
    retried=1
    refuse_public "$role" "$reason" "$result" "$curl_exit"
    printf '%s\n' "$curl_exit" >>"$scratch/public-retries"
  done
  # A retry that succeeded must not leave its first cause on a later deadline refusal.
  rm -f "$scratch/public-failure"
  if ((curl_exit)); then
    case "$curl_exit" in
      6) reason=dns_failed ;;
      35 | 60) reason=tls_failed ;;
      28) reason=request_timeout ;;
    esac
    refuse_public "$role" "$reason" "$result" "$curl_exit"
    return 1
  fi
  [[ "$result" == "${3:-200}" ]] || {
    refuse_public "$role" http_status "$result" "$curl_exit"
    return 1
  }
}
# header_is requires exactly one response header with the expected value.
header_is() {
  awk -v name="$1" -v expected="$2" '
    {sub(/\r$/, "")} index(tolower($0),tolower(name) ":")==1 {
      value=substr($0,length(name)+2); sub(/^[ \t]+/,"",value); count++; if(value!=expected) wrong=1
    } END {exit(count!=1 || wrong)}' "$scratch/headers"
}
# public_checks verifies the fixed public contracts, assets and response policies.
public_checks() {
  local sample="https://harbour-data.$domain" publisher="https://publisher-review.$domain" path role
  rm -f "$scratch/public-failure"
  fetch "$sample/healthz" sample_health || return 1
  fetch "$sample/api/observations" sample_observations || return 1
  jq -se 'length==1 and (.[0].items|type=="array" and length==2 and all(.[];
    (.station|type=="string" and length>0) and (.temperatureCelsius|type=="number") and
    (.salinityPsu|type=="number") and (.observedAt|type=="string" and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T"))))' "$scratch/body" >/dev/null || {
    refuse_public sample_observations body_invalid
    return 1
  }
  fetch "$sample/openapi.json" sample_openapi || return 1
  jq -se 'length==1 and .[0].openapi=="3.1.0" and .[0].info.title=="Harbour observations" and .[0].paths["/api/observations"].get.responses["200"]!=null' "$scratch/body" >/dev/null || {
    refuse_public sample_openapi body_invalid
    return 1
  }
  if [[ "$expected_releases" == true ]]; then
    fetch "$sample/ksail-releases/ui" release_index || return 1
    if ! header_is Content-Security-Policy "default-src 'self'; script-src 'self'; style-src 'self'; connect-src https://api.github.com/repos/devantler-tech/ksail/releases; frame-ancestors https:; object-src 'none'; base-uri 'none'" ||
      ! header_is Referrer-Policy no-referrer || ! header_is X-Content-Type-Options nosniff; then
      refuse_public release_index security_headers
      return 1
    fi
    grep -Fq '<meta name="product-kind" content="ksail-releases"' "$scratch/body" || {
      refuse_public release_index asset_invalid
      return 1
    }
    fetch "$sample/ksail-releases/openapi.json" release_openapi || return 1
    jq -se 'length==1 and .[0].openapi=="3.1.0" and .[0].info.title=="KSail release history" and
      .[0].servers==[{url:"https://api.github.com"}] and (.[0].paths|keys)==["/repos/devantler-tech/ksail/releases"] and
      .[0].paths["/repos/devantler-tech/ksail/releases"].get.responses["200"]!=null' "$scratch/body" >/dev/null || {
      refuse_public release_openapi body_invalid
      return 1
    }
  fi
  fetch "$publisher/healthz" publisher_health || return 1
  [[ $(cat "$scratch/body") == ok && $(wc -c <"$scratch/body") -eq 3 ]] || {
    refuse_public publisher_health body_invalid
    return 1
  }
  local csp="default-src 'self'; connect-src 'none'; frame-src 'none'; frame-ancestors 'none'; object-src 'none'; base-uri 'none'"
  if [[ "$expected_publisher" == true ]]; then
    fetch "$publisher/" publisher_root 307 || return 1
    header_is Location /publisher-review || {
      refuse_public publisher_root asset_invalid
      return 1
    }
  else
    fetch "$publisher/" publisher_root 404 || return 1
  fi
  for path in /publisher-review /publisher.js /publisher.css /preflight-report.js /descriptor.js /metadata.js; do
    case "$path" in
      /publisher-review) role=publisher_index ;;
      /publisher.js) role=publisher_script ;;
      /publisher.css) role=publisher_style ;;
      /preflight-report.js) role=publisher_library ;;
      /descriptor.js) role=descriptor_library ;;
      /metadata.js) role=metadata_library ;;
    esac
    if [[ "$expected_publisher" != true ]]; then
      fetch "$publisher$path" "$role" 404 || return 1
      continue
    fi
    fetch "$publisher$path" "$role" || return 1
    if ! header_is Content-Security-Policy "$csp" || ! header_is Cache-Control no-store ||
      ! header_is Referrer-Policy no-referrer || ! header_is X-Content-Type-Options nosniff; then
      refuse_public "$role" security_headers
      return 1
    fi
    case "$path" in
      /publisher-review)
        if ! { grep -Fq 'id="report-form"' "$scratch/body" && grep -Fq 'id="report-file"' "$scratch/body" &&
          grep -Fq 'id="report-text"' "$scratch/body" && grep -Fq 'id="report-status"' "$scratch/body" &&
          grep -Fq 'src="/metadata.js"' "$scratch/body" && grep -Fq 'src="/descriptor.js"' "$scratch/body" &&
          grep -Fq 'src="/preflight-report.js"' "$scratch/body" && grep -Fq 'src="/publisher.js"' "$scratch/body" &&
          grep -Fq 'href="publisher.css"' "$scratch/body"; }; then
          refuse_public "$role" asset_invalid
          return 1
        fi
        ;;
      /publisher.js) grep -Fq DataProductPreflightReport.parse "$scratch/body" || {
        refuse_public "$role" asset_invalid
        return 1
      } ;;
      /publisher.css) grep -Fq color-scheme: "$scratch/body" || {
        refuse_public "$role" asset_invalid
        return 1
      } ;;
      /preflight-report.js) grep -Fq DataProductPreflightReport "$scratch/body" || {
        refuse_public "$role" asset_invalid
        return 1
      } ;;
      /descriptor.js) grep -Fq DataProductDescriptor "$scratch/body" || {
        refuse_public "$role" asset_invalid
        return 1
      } ;;
      /metadata.js) grep -Fq data-product-ui/v2 "$scratch/body" || {
        refuse_public "$role" asset_invalid
        return 1
      } ;;
    esac
  done
}
# quiet_public_checks prevents response details from reaching the rollout receipt.
quiet_public_checks() { public_checks >/dev/null 2>&1; }
# name_changes: which parts of the rollout differ between the two snapshots, as
# the helper's own role and field names only. A name that is not plain letters
# is reduced to its role, so no value read from the cluster reaches the output.
name_changes() {
  jq -cn --slurpfile before "$scratch/before/snapshot" --slurpfile after "$scratch/after/snapshot" '
    def leaves: [paths(type != "object" and type != "array") as $p | {p:$p,v:getpath($p)}];
    ($before[0]|leaves) as $b | ($after[0]|leaves) as $a |
    [(($b-$a)+($a-$b))[] | .p | map(select(type=="string")) |
      if (.[1:3]|all(.[];test("^[A-Za-z]+$"))) then .[:3] else .[:1] end | join(".")] | unique | .[:16]
  ' >"$scratch/changed" 2>/dev/null || return 1
  # Every retaken pair's names, kept for the report of a rollout that then holds
  # still. Written whole and moved into place, so a read never sees half of it.
  jq -cn --slurpfile seen "$scratch/settled" --slurpfile now "$scratch/changed" \
    '($seen[0]+$now[0])|unique|.[:16]' >"$scratch/settled.next" 2>/dev/null || return 1
  mv "$scratch/settled.next" "$scratch/settled"
}
printf '[]\n' >"$scratch/settled"
# The public checks only count when the rollout they ran against did not move.
# An ordinary reconcile can start between the two snapshots, so a pair that
# differs is retaken from the start, public checks included, at most three
# times. A rollout that never holds still is refused and the changes are named.
# Public checks that fail while the rollout moves are retaken with it; they
# refuse the deploy once the rollout they failed against is known to be still.
attempt=0
while :; do
  attempt=$((attempt + 1))
  if ((attempt == 1)); then phase=initial_readiness; else phase=rollout_stability; fi
  while :; do
    collect "$scratch/before"
    if bounded check "$scratch/before"; then break; fi
    remaining
    sleep 0.2
  done
  public_ok=1
  phase=public_contract
  bounded quiet_public_checks || public_ok=0
  ((public_ok || deadline - SECONDS > 0)) || fail public_contract_incomplete
  phase=rollout_stability
  collect "$scratch/after"
  if bounded check "$scratch/after" && bounded cmp -s "$scratch/before/snapshot" "$scratch/after/snapshot"; then
    ((public_ok)) || fail public_contract_incomplete
    break
  fi
  bounded name_changes || fail read_incomplete
  changed=$(<"$scratch/changed")
  # No moving pair validates its public result, even on the last attempt.
  # Discard it before either retrying or reporting that the rollout never settled.
  rm -f "$scratch/public-failure"
  if ((attempt >= 3)); then
    fail rollout_changed
  fi
  remaining
  sleep 0.2
done
remaining
if [[ "$expected_state" == active ]]; then expected_pods=8; else expected_pods=6; fi
if [[ "$expected_releases" == true ]]; then public_count=13; else public_count=11; fi
# An accepted receipt still counts the dropped connections and failed reads it
# absorbed, so a retry cannot hide a dependency that keeps dropping them.
retries_field=''
if [[ -s "$scratch/public-retries" ]]; then retries_field=$(printf ',"publicRetries":%d' "$(wc -l <"$scratch/public-retries")"); fi
if [[ -s "$scratch/read-retries" ]]; then retries_field+=$(printf ',"readRetries":%d' "$(wc -l <"$scratch/read-retries")"); fi
# Keep the accepted state and the names of changes that settled in one receipt.
# A retake reports only role/field names; no cluster values leave the verifier.
if ((attempt > 1)); then
  printf '{"complete":true,"deployments":4,"pods":%s,"routes":3,"publicChecks":%d,"readinessState":"%s","ksailReleasesEnabled":%s,"publisherReviewEnabled":%s,"retaken":%d,"settled":%s%s}\n' \
    "$expected_pods" "$public_count" "$expected_state" "$expected_releases" "$expected_publisher" "$((attempt - 1))" "$(<"$scratch/settled")" "$retries_field"
else
  printf '{"complete":true,"deployments":4,"pods":%s,"routes":3,"publicChecks":%d,"readinessState":"%s","ksailReleasesEnabled":%s,"publisherReviewEnabled":%s%s}\n' "$expected_pods" "$public_count" "$expected_state" "$expected_releases" "$expected_publisher" "$retries_field"
fi
