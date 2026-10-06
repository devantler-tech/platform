#!/usr/bin/env bash
set -euo pipefail
umask 077

# Requires an inherited, dedicated single-file KUBECONFIG. Every read selects
# the caller's context; this script never changes context or kubeconfig contents.
# --context NAME --domain PUBLIC_DOMAIN --image-digest sha256:...
# --runtime-digest sha256:... (repeat) --chart-digest sha256:...
# --apps-digest sha256:... --apps-verify-file /absolute/public-policy.json [--timeout SECONDS]
# Browser authentication and embedding handshakes remain separate acceptance
# checks. This helper reads no Secrets or authenticated URLs.
context='' domain='' image_digest='' chart_digest='' apps_digest='' apps_verify_file='' timeout_seconds=300
runtime_digests=() scratch='' active='' watchdog='' changed='' started_at=$SECONDS
readonly repository='ghcr.io/devantler-tech/data-product-controller'
readonly namespace='data-product-controller'
fail() {
  # Once a snapshot pair has differed, every later refusal still names what moved.
  if [[ -n "$changed" ]]; then
    printf '{"complete":false,"failure":"%s","changed":%s}\n' "$1" "$changed"
  else
    printf '{"complete":false,"failure":"%s"}\n' "$1"
  fi
  exit "${2:-1}"
}
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
for value in "$image_digest" "$chart_digest" "$apps_digest" "${runtime_digests[@]}"; do
  [[ "$value" =~ ^sha256:[a-f0-9]{64}$ ]] || fail invalid_arguments 2
done
[[ "$timeout_seconds" =~ ^[1-9][0-9]{0,2}$ && "$timeout_seconds" -le 600 ]] || fail invalid_arguments 2
for tool in kubectl jq curl; do command -v "$tool" >/dev/null || fail missing_dependency 2; done
scratch=$(mktemp -d) || fail private_storage_unavailable
chmod 700 "$scratch"
readonly deadline=$((started_at + timeout_seconds))
set -m
remaining() {
  left=$((deadline - SECONDS))
  [[ $left -gt 0 ]] || fail deadline_exceeded
}
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
def conditions: [.[]? | {type,status,observedGeneration}];
def containers: [.[]? | {name,image,ports:[.ports[]? | {name,containerPort,protocol:(.protocol//"TCP")}],
  command,args,envCount:(.env//[]|length),envFromCount:(.envFrom//[]|length),volumeMountCount:(.volumeMounts//[]|length),
  env:[.env[]? | select(.name=="CONTRACT_PROBE_URL" or .name=="CONTRACT_READINESS_ENABLED" or .name=="CONNECTOR_READINESS_ENABLED") |
    {name,value,indirect:(.valueFrom!=null)}],readinessProbe,livenessProbe,securityContext}];
def workload: {apiVersion,kind,metadata:(.metadata|metadata),
  spec:{replicas:.spec.replicas,selector:.spec.selector,labels:.spec.template.metadata.labels,containers:(.spec.template.spec.containers|containers),
    serviceAccountName:.spec.template.spec.serviceAccountName,automountServiceAccountToken:.spec.template.spec.automountServiceAccountToken,
    volumeCount:(.spec.template.spec.volumes//[]|length),initContainerCount:(.spec.template.spec.initContainers//[]|length)},
  status:(.status|{observedGeneration,replicas,updatedReplicas,readyReplicas,availableReplicas})};
def pod: {apiVersion,kind,metadata:(.metadata|metadata),spec:{containers:(.spec.containers|containers)},
  status:{phase:.status.phase,podIP:.status.podIP,podIPs:[.status.podIPs[]? | .ip],conditions:(.status.conditions|conditions),
    containers:[.status.containerStatuses[]? | {name,ready,imageID,
      running:(.state.running|type=="object"),waiting:(.state.waiting!=null),terminated:(.state.terminated!=null)}]}};
if length != 1 then error("incomplete response") else .[0] end |
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
elif $role == "product" then {apiVersion,kind,metadata:(.metadata|metadata),
  spec:{connector:.spec.connector,contractChecks:(.spec.contractChecks//[]),ui:{url:.spec.ui.url,contract:(.spec.ui.contract|{apiVersion,hostOrigins,capabilities})}},
  status:{observedGeneration:.status.observedGeneration,conditions:(.status.conditions|conditions)}}
elif $role|startswith("service-") then {apiVersion,kind,metadata:(.metadata|metadata),
  spec:{type:.spec.type,selector:.spec.selector,ports:[.spec.ports[]? | {name,port,targetPort,protocol:(.protocol//"TCP")}]}}
elif $role == "helm" then {apiVersion,kind,metadata:(.metadata|metadata),
  spec:{suspend:.spec.suspend,chartRef:.spec.chartRef,values:{image:(.spec.values.image|{repository,tag,digest}),
    uiContract:(.spec.values.uiContract|{enabled,additionalHostOrigins}),uiAppearance:(.spec.values.uiAppearance|{enabled}),
    controller:{replicas:.spec.values.controller.replicas},demoProduct:(.spec.values.demoProduct|{enabled,replicas,publicBaseURL}),route:(.spec.values.route|{enabled,host}),
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
elif $role == "probe-policy" then {apiVersion,kind,metadata:(.metadata|metadata),spec}
else error("unknown response") end)}
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
  ($containers|length)==1 and ($containers[0].image|image($tag));
def runtime: . as $id | any($runtime_digests[]; . as $digest |
  $id==$digest or $id==("containerd://"+$digest) or $id==("docker://"+$digest) or
  $id==("cri-o://"+$digest) or $id==($repository+"@"+$digest) or $id==("docker-pullable://"+$repository+"@"+$digest));
def inventory($kind;$version): (.items|type=="array" and length<=4096) and
  all(.items[]; .kind==$kind and .apiVersion==$version and .metadata.namespace==$namespace and
    (.metadata.name|type=="string" and length>0) and (.metadata.uid|type=="string" and length>0)) and
  ([.items[].metadata.uid]|length)==([.items[].metadata.uid]|unique|length) and
  ([.items[].metadata.name]|length)==([.items[].metadata.name]|unique|length);
def pod_ready($rs;$container;$tag):
  owned("ReplicaSet";$rs.metadata.name;$rs.metadata.uid) and .metadata.deletionTimestamp==null and
  .status.phase=="Running" and ([.status.conditions[]? | select(.type=="Ready")]|length)==1 and
  any(.status.conditions[]?;.type=="Ready" and .status=="True") and named_image($container;$tag) and
  ([.status.containers[]? | select(.name==$container)] as $containers | ($containers|length)==1 and
    $containers[0].ready==true and $containers[0].running==true and $containers[0].waiting==false and
    $containers[0].terminated==false and ($containers[0].imageID|runtime));
def workload($deployment;$container;$tag;$sets;$pods):
  ($deployment|identity("Deployment";"apps/v1";$deployment.metadata.name;$namespace)) and
  ($deployment.metadata.revision|type=="string" and test("^[1-9][0-9]*$")) and
  $deployment.spec.replicas==2 and $deployment.status.observedGeneration==$deployment.metadata.generation and
  all([$deployment.status.replicas,$deployment.status.updatedReplicas,$deployment.status.readyReplicas,$deployment.status.availableReplicas][];.==2) and
  ($deployment|named_image($container;$tag)) and
  ([$sets[] | select(owned("Deployment";$deployment.metadata.name;$deployment.metadata.uid))] as $owned |
    [$owned[] | select(.metadata.revision==$deployment.metadata.revision)] as $current |
    ($current|length)==1 and
    ($current[0]|identity("ReplicaSet";"apps/v1";.metadata.name;$namespace)) and
    $current[0].spec.replicas==2 and $current[0].status.observedGeneration==$current[0].metadata.generation and
    all([$current[0].status.replicas,$current[0].status.readyReplicas,$current[0].status.availableReplicas][];.==2) and
    ($current[0]|named_image($container;$tag)) and
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
def probe_configuration($tag):
  .spec.serviceAccountName=="data-product-controller-contract-probe" and .spec.automountServiceAccountToken==false and .spec.volumeCount==0 and .spec.initContainerCount==0 and
  .spec.selector.matchLabels=={"app.kubernetes.io/name":"data-product-controller","app.kubernetes.io/instance":"data-product-controller","app.kubernetes.io/component":"contract-probe"} and
  .spec.labels==.spec.selector.matchLabels and (.spec.containers|length)==1 and named_image("contract-probe";$tag) and
  (.spec.containers[0] | .command==["/contract-probe"] and (.args//[])==[] and .envCount==2 and .envFromCount==0 and .volumeMountCount==0 and
    (.env|sort_by(.name))==([{name:"CONTRACT_PROBE_URL",value:("https://harbour-data."+$domain+"/openapi.json"),indirect:false},
      {name:"CONTRACT_READINESS_ENABLED",value:"false",indirect:false}]|sort_by(.name)) and
    .ports==[{name:"management",containerPort:8081,protocol:"TCP"}] and
    .readinessProbe.httpGet=={path:"/readyz",port:"management",scheme:"HTTP"} and .readinessProbe.timeoutSeconds==7 and
    .readinessProbe.periodSeconds==30 and .readinessProbe.failureThreshold==1 and
    .livenessProbe.httpGet=={path:"/healthz",port:"management",scheme:"HTTP"} and
    .securityContext.runAsNonRoot==true and .securityContext.readOnlyRootFilesystem==true and
    .securityContext.allowPrivilegeEscalation==false and .securityContext.capabilities.drop==["ALL"]);
def readiness_scaffold($snapshot;$tag):
  ($snapshot["deployment-probe"]|dormant_probe($snapshot.replicasets.items;$snapshot.pods.items) and probe_configuration($tag)) and
  ($snapshot["probe-account"]|object_identity("ServiceAccount";"v1";"data-product-controller-contract-probe";$namespace) and .automountServiceAccountToken==false) and
  ($snapshot["observer-role"]|object_identity("Role";"rbac.authorization.k8s.io/v1";"data-product-readiness-observer";$namespace) and
    (.rules|map(.resourceNames|=sort))==[{apiGroups:["apps"],resources:["deployments"],resourceNames:["data-product-controller-contract-probe","data-product-controller-harbour"],verbs:["get"]}]) and
  ($snapshot["observer-binding"]|object_identity("RoleBinding";"rbac.authorization.k8s.io/v1";"data-product-readiness-observer";$namespace) and
    .roleRef=={apiGroup:"rbac.authorization.k8s.io",kind:"Role",name:"data-product-readiness-observer"} and
    .subjects==[{kind:"ServiceAccount",name:"data-product-controller",namespace:$namespace}]) and
  ($snapshot["probe-policy"]|object_identity("CiliumNetworkPolicy";"cilium.io/v2";"allow-data-product-controller-contract-probe";$namespace) and
    .spec=={endpointSelector:{matchLabels:{"k8s:app.kubernetes.io/name":"data-product-controller","k8s:app.kubernetes.io/instance":"data-product-controller","k8s:app.kubernetes.io/component":"contract-probe"}},
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
(.apps|identity("Kustomization";"kustomize.toolkit.fluxcd.io/v1";"apps";"flux-system") and
  (.spec.suspend//false)==false and .status.observedGeneration==.metadata.generation and ready(["Ready"]) and
  .spec.sourceRef.kind=="OCIRepository" and .spec.sourceRef.name=="flux-system" and (.spec.sourceRef.namespace//"flux-system")=="flux-system" and
  (.status.lastAppliedRevision|revision($apps_digest))) and
(.root|identity("OCIRepository";"source.toolkit.fluxcd.io/v1";"flux-system";"flux-system") and
  (.spec.suspend//false)==false and .status.observedGeneration==.metadata.generation and ready(["Ready","SourceVerified"]) and
  .spec.verify==$apps_verify and (.status.artifact.revision|revision($apps_digest)) and (.status.artifact.digest|digest)) and
(.chart|identity("OCIRepository";"source.toolkit.fluxcd.io/v1";"data-product-controller";$namespace) and
  (.spec.suspend//false)==false and .status.observedGeneration==.metadata.generation and ready(["Ready","SourceVerified"]) and
  .spec.url=="oci://ghcr.io/devantler-tech/charts/data-product-controller" and .spec.ref.digest==$chart_digest and
  .spec.verify=={provider:"cosign",matchOIDCIdentity:[{issuer:"^https://token\\.actions\\.githubusercontent\\.com$",subject:"^https://github\\.com/devantler-tech/data-product-controller/\\.github/workflows/publish-chart\\.yaml@refs/tags/v[0-9]+\\.[0-9]+\\.[0-9]+$"}]} and
  # The cached file digest differs from the upstream OCI manifest revision.
  (.status.artifact.revision|revision($chart_digest)) and (.status.artifact.digest|digest)) and
(.helm|identity("HelmRelease";"helm.toolkit.fluxcd.io/v2";"data-product-controller";$namespace) and
  (.spec.suspend//false)==false and .status.observedGeneration==.metadata.generation and ready(["Ready"]) and
  .spec.chartRef.kind=="OCIRepository" and .spec.chartRef.name=="data-product-controller" and (.spec.chartRef.namespace//$namespace)==$namespace and
  .spec.values.image.repository==$repository and .spec.values.image.digest==$image_digest and
  ($tag|type=="string" and test("^[0-9]+\\.[0-9]+\\.[0-9]+$")) and
  .spec.values.uiContract.enabled==true and .spec.values.uiContract.additionalHostOrigins==["https://product-ui."+$domain] and
  .spec.values.uiAppearance.enabled==true and .spec.values.controller.replicas==2 and .spec.values.demoProduct.enabled==true and
  .spec.values.observationFlags==[false,false,false] and
  .spec.values.demoProduct.replicas==2 and .spec.values.demoProduct.publicBaseURL==("https://harbour-data."+$domain) and
  .spec.values.route.enabled==true and .spec.values.route.host==("data-products."+$domain) and
  .status.lastAttemptedRevisionDigest==$chart_digest and .status.lastAttemptedRevision==($tag+"+"+$chart_digest[7:19]) and
  (.status.lastAttemptedConfigDigest|digest) and .status.history[0].status=="deployed" and (.status.history[0].version|positive) and
  .status.history[0].name=="data-product-controller" and .status.history[0].namespace==$namespace and .status.history[0].chartName=="data-product-controller" and
  .status.history[0].chartVersion==.status.lastAttemptedRevision and .status.history[0].ociDigest==$chart_digest and .status.history[0].configDigest==.status.lastAttemptedConfigDigest) and
(.product|identity("DataProduct";"data.devantler.tech/v1alpha1";"harbour-observations";$namespace) and
  .status.observedGeneration==.metadata.generation and ready(["Ready"]) and .spec.ui.url==("https://harbour-data."+$domain+"/ui") and
  .spec.connector==null and .spec.contractChecks==[] and
  all(.status.conditions[]?;.type!="ConnectorReady" and .type!="ContractsReady") and
  .spec.ui.contract.apiVersion=="data-product-ui/v2" and
  (.spec.ui.contract.hostOrigins|sort)==(["https://data-products."+$domain,"https://product-ui."+$domain]|sort) and
  (.spec.ui.contract.capabilities|sort)==["appearance","resize","status"]) and
(.replicasets|inventory("ReplicaSet";"apps/v1")) and (.pods|inventory("Pod";"v1")) and
(.endpoints|inventory("EndpointSlice";"discovery.k8s.io/v1")) and
readiness_scaffold($snapshot;$tag) and
([."deployment-controller".spec.containers[] | select(.name=="controller") | .env[]] | sort_by(.name))==
  [{name:"CONNECTOR_READINESS_ENABLED",value:"false",indirect:false},{name:"CONTRACT_READINESS_ENABLED",value:"false",indirect:false}] and
all([["deployment-controller","data-product-controller","controller"],["deployment-harbour","data-product-controller-harbour","product"],["deployment-ui-kit","data-product-controller-ui-kit","ui-kit"]][];
  . as $id | $snapshot[$id[0]] as $deployment |
  $deployment.metadata.name==$id[1] and workload($deployment;$id[2];$tag;$snapshot.replicasets.items;$snapshot.pods.items)) and
all([["harbour","product"],["ui-kit","ui-kit"]][];. as $id |
  backend($snapshot["service-"+$id[0]];$snapshot["deployment-"+$id[0]];$id[1];$snapshot.replicasets.items;$snapshot.pods.items;$snapshot.endpoints.items)) and
all([["route-registry","data-product-controller-registry","data-products",{name:"oauth2-proxy",namespace:"oauth2-proxy",port:80},false],
     ["route-harbour","data-product-controller-harbour","harbour-data",{name:"data-product-controller-harbour",namespace:$namespace,port:80},true],
     ["route-ui-kit","data-product-controller-ui-kit","product-ui",{name:"data-product-controller-ui-kit",namespace:$namespace,port:80},true]][];
  . as $id | $snapshot[$id[0]] | route($id[1];$id[2];$id[3];$id[4])) and
(.gateway|identity("Gateway";"gateway.networking.k8s.io/v1";"platform";"kube-system") and .spec.gatewayClassName=="cilium" and
  ready(["Accepted","Programmed"]) and
  ([.spec.listeners[] | select(.name=="https")]|map({name,protocol,port,tls}))==[{name:"https",protocol:"HTTPS",port:443,tls:{mode:"Terminate"}}] and
  (.metadata.generation as $generation | [.status.listeners[]? | select(.name=="https")] as $listeners | ($listeners|length)==1 and
    all(["Accepted","ResolvedRefs","Programmed"][]; . as $type | [$listeners[0].conditions[]? | select(.type==$type)] as $conditions |
      ($conditions|length)==1 and $conditions[0].status=="True" and $conditions[0].observedGeneration==$generation)))
JQ

project_read() {
  local role=$1 destination=$2
  shift 2
  kubectl "$@" 2>/dev/null | jq -se --arg role "$role" --arg namespace "$namespace" -f "$scratch/project.jq" >"$destination" 2>/dev/null
}
read_resource() {
  local directory=$1 role=$2 ns=$3 resource=$4 name=${5:-} request_seconds
  remaining
  request_seconds=$left
  ((request_seconds <= 5)) || request_seconds=5
  local args=(--kubeconfig "$KUBECONFIG" --context "$context" --namespace "$ns" --request-timeout "${request_seconds}s" get "$resource")
  [[ -z "$name" ]] || args+=("$name")
  args+=(-o json)
  bounded project_read "$role" "$directory/$role.json" "${args[@]}" || fail read_incomplete
}
collect() {
  local directory=$1
  mkdir -p "$directory"
  read_resource "$directory" apps flux-system kustomizations.kustomize.toolkit.fluxcd.io apps
  read_resource "$directory" root flux-system ocirepositories.source.toolkit.fluxcd.io flux-system
  read_resource "$directory" chart "$namespace" ocirepositories.source.toolkit.fluxcd.io data-product-controller
  read_resource "$directory" helm "$namespace" helmreleases.helm.toolkit.fluxcd.io data-product-controller
  read_resource "$directory" product "$namespace" dataproducts.data.devantler.tech harbour-observations
  read_resource "$directory" service-harbour "$namespace" services data-product-controller-harbour
  read_resource "$directory" service-ui-kit "$namespace" services data-product-controller-ui-kit
  read_resource "$directory" endpoints "$namespace" endpointslices.discovery.k8s.io
  read_resource "$directory" deployment-controller "$namespace" deployments.apps data-product-controller
  read_resource "$directory" deployment-harbour "$namespace" deployments.apps data-product-controller-harbour
  read_resource "$directory" deployment-ui-kit "$namespace" deployments.apps data-product-controller-ui-kit
  read_resource "$directory" deployment-probe "$namespace" deployments.apps data-product-controller-contract-probe
  read_resource "$directory" probe-account "$namespace" serviceaccounts data-product-controller-contract-probe
  read_resource "$directory" observer-role "$namespace" roles.rbac.authorization.k8s.io data-product-readiness-observer
  read_resource "$directory" observer-binding "$namespace" rolebindings.rbac.authorization.k8s.io data-product-readiness-observer
  read_resource "$directory" probe-policy "$namespace" ciliumnetworkpolicies.cilium.io allow-data-product-controller-contract-probe
  read_resource "$directory" replicasets "$namespace" replicasets.apps
  read_resource "$directory" pods "$namespace" pods
  read_resource "$directory" route-registry "$namespace" httproutes.gateway.networking.k8s.io data-product-controller-registry
  read_resource "$directory" route-harbour "$namespace" httproutes.gateway.networking.k8s.io data-product-controller-harbour
  read_resource "$directory" route-ui-kit "$namespace" httproutes.gateway.networking.k8s.io data-product-controller-ui-kit
  read_resource "$directory" gateway kube-system gateways.gateway.networking.k8s.io platform
  bounded aggregate "$directory" || fail read_incomplete
}
aggregate() {
  jq -s '
    def owner_in($kind;$uids): any(.metadata.owners[]?;
      .kind==$kind and (.uid as $uid | $uids | index($uid))!=null);
    from_entries |
    [."deployment-controller".metadata.uid,."deployment-harbour".metadata.uid,."deployment-ui-kit".metadata.uid,."deployment-probe".metadata.uid] as $deployment_uids |
    .replicasets.items |= (map(select(owner_in("Deployment";$deployment_uids))) | sort_by(.metadata.uid)) |
    [.replicasets.items[].metadata.uid] as $rs_uids |
    .pods.items |= (map(select(owner_in("ReplicaSet";$rs_uids))) | sort_by(.metadata.uid)) |
    [."service-harbour",."service-ui-kit"] as $services |
    .endpoints.items |= (map(select(. as $slice | any($services[];. as $service |
      $slice.metadata.labels["kubernetes.io/service-name"]==$service.metadata.name or any($slice.metadata.owners[]?;.uid==$service.metadata.uid)))) | sort_by(.metadata.uid))
  ' "$1/"*.json >"$1/snapshot" 2>/dev/null
}
make_runtime_json() { printf '%s\n' "${runtime_digests[@]}" | jq -Rsc 'split("\n")[:-1]|unique' >"$scratch/runtime-digests.json"; }
bounded make_runtime_json || fail read_incomplete
runtime_json=$(<"$scratch/runtime-digests.json")
check() {
  jq -e --arg repository "$repository" --arg namespace "$namespace" --arg domain "$domain" --arg image_digest "$image_digest" \
    --arg chart_digest "$chart_digest" --arg apps_digest "$apps_digest" --argjson apps_verify "$apps_verify" --argjson runtime_digests "$runtime_json" \
    -f "$scratch/check.jq" "$1/snapshot" >/dev/null 2>&1
}

fetch() {
  local url=$1 result
  remaining
  local connect_seconds=$left
  ((connect_seconds <= 5)) || connect_seconds=5
  result=$(curl --disable --silent --show-error --proxy '' --connect-timeout "$connect_seconds" --max-time "$left" \
    --max-filesize 262144 --proto '=https' --header 'Cookie:' --header 'Authorization:' \
    --dump-header "$scratch/headers" --output "$scratch/body" --write-out '%{http_code}' --url "$url" 2>/dev/null) || return 1
  [[ "$result" == 200 ]]
}
header_is() {
  awk -v name="$1" -v expected="$2" '
    {sub(/\r$/, "")} index(tolower($0),tolower(name) ":")==1 {
      value=substr($0,length(name)+2); sub(/^[ \t]+/,"",value); count++; if(value!=expected) wrong=1
    } END {exit(count!=1 || wrong)}' "$scratch/headers"
}
public_checks() {
  local sample="https://harbour-data.$domain" kit="https://product-ui.$domain" path
  fetch "$sample/healthz" || return 1
  fetch "$sample/api/observations" || return 1
  jq -se 'length==1 and (.[0].items|type=="array" and length==2 and all(.[];
    (.station|type=="string" and length>0) and (.temperatureCelsius|type=="number") and
    (.salinityPsu|type=="number") and (.observedAt|type=="string" and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T"))))' "$scratch/body" >/dev/null || return 1
  fetch "$sample/openapi.json" || return 1
  jq -se 'length==1 and .[0].openapi=="3.1.0" and .[0].info.title=="Harbour observations" and .[0].paths["/api/observations"].get.responses["200"]!=null' "$scratch/body" >/dev/null || return 1
  fetch "$sample/ui-contract-config" || return 1
  jq -se --arg registry "https://data-products.$domain" --arg kit "$kit" \
    'length==1 and .[0].appearanceEnabled==true and (.[0].hostOrigins|sort)==([$registry,$kit]|sort)' "$scratch/body" >/dev/null || return 1
  fetch "$kit/healthz" || return 1
  [[ $(cat "$scratch/body") == ok && $(wc -c <"$scratch/body") -eq 3 ]] || return 1
  local csp="default-src 'self'; connect-src 'none'; frame-src https:; frame-ancestors 'none'; object-src 'none'; base-uri 'none'"
  for path in / /ui-contract.js /kit.js /kit.css; do
    fetch "$kit$path" || return 1
    header_is Content-Security-Policy "$csp" || return 1
    header_is Referrer-Policy no-referrer || return 1
    header_is X-Content-Type-Options nosniff || return 1
    case "$path" in
    /) grep -Fq '<body data-appearance-enabled="true">' "$scratch/body" &&
      grep -Fq 'id="manifest-form"' "$scratch/body" && grep -Fq 'src="ui-contract.js"' "$scratch/body" &&
      grep -Fq 'src="kit.js"' "$scratch/body" && grep -Fq 'href="kit.css"' "$scratch/body" || return 1 ;;
    /ui-contract.js) grep -Fq DataProductUI "$scratch/body" || return 1 ;;
    /kit.js) grep -Fq DataProductUI.mount "$scratch/body" || return 1 ;;
    /kit.css) grep -Fq color-scheme: "$scratch/body" || return 1 ;;
    esac
  done
}
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
  ' >"$scratch/changed" 2>/dev/null
}
# The public checks only count when the rollout they ran against did not move.
# An ordinary reconcile can start between the two snapshots, so a pair that
# differs is retaken from the start, public checks included, at most three
# times. A rollout that never holds still is refused and the changes are named.
# Public checks that fail while the rollout moves are retaken with it; they
# refuse the deploy once the rollout they failed against is known to be still.
attempt=0
while :; do
  attempt=$((attempt + 1))
  while :; do
    collect "$scratch/before"
    if bounded check "$scratch/before"; then break; fi
    remaining
    sleep 0.2
  done
  public_ok=1
  bounded quiet_public_checks || public_ok=0
  ((public_ok || deadline - SECONDS > 0)) || fail public_contract_incomplete
  collect "$scratch/after"
  if bounded check "$scratch/after" && bounded cmp -s "$scratch/before/snapshot" "$scratch/after/snapshot"; then
    ((public_ok)) || fail public_contract_incomplete
    break
  fi
  bounded name_changes || fail read_incomplete
  changed=$(<"$scratch/changed")
  if ((attempt >= 3)); then
    ((public_ok)) || fail public_contract_incomplete
    fail rollout_changed
  fi
  remaining
  sleep 0.2
done
remaining
printf '{"complete":true,"deployments":4,"pods":6,"routes":3,"publicChecks":9,"readinessState":"dormant"}\n'
