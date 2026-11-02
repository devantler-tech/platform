#!/usr/bin/env bash
set -euo pipefail
umask 077
root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly script="${root_dir}/scripts/verify-data-product-rollout.sh"
# shellcheck source=scripts/tests/lib/fixture-workers.lib.sh
source "${root_dir}/scripts/tests/lib/fixture-workers.lib.sh"
scratch="$(mktemp -d)"
fixture_pool_init "$scratch" 2
# cleanup joins pending workers before deleting shared scratch and preserves the original failure.
cleanup() {
  local result=$? workers_result=0
  trap - EXIT
  fixture_pool_finish >&2 || workers_result=$?
  rm -rf "${scratch}"
  if [[ "$result" == 0 ]]; then result=$workers_result; fi
  exit "$result"
}
trap cleanup EXIT
trap fixture_pool_interrupt HUP INT TERM
# fail reports the violated fixture invariant and stops the regression suite.
fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}
mkdir "${scratch}/bin" "${scratch}/healthy"
printf 'synthetic dedicated kubeconfig\n' >"${scratch}/kubeconfig"
index="sha256:$(printf 'a%.0s' {1..64})"
child="sha256:$(printf 'b%.0s' {1..64})"
chart="sha256:$(printf 'c%.0s' {1..64})"
apps="sha256:$(printf 'd%.0s' {1..64})"
readonly index child chart apps
real_jq="$(command -v jq)"
readonly real_jq

# Exercise setup consuming the total deadline separately from cleanup of a
# plugin that really started. A one-second whole-script budget can expire in
# preflight before the first kubectl call on a loaded CI runner.
cat >"${scratch}/bin/jq" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${MODE:-}" == hang || "${MODE:-}" == preflight-timeout ]]; then
  if [[ ! -e "$FIXTURE/slow-preflight" ]]; then
    : >"$FIXTURE/slow-preflight"
    sleep 2
  fi
fi
# A timeout alone cannot prove a mutated snapshot was rejected on a busy host.
# Record actual projection/predicate rejection before accepting that control.
for argument in "$@"; do
  if [[ "$argument" == */project.jq || "$argument" == */check.jq ]]; then
    if "$REAL_JQ" "$@"; then
      exit 0
    else
      result=$?
      printf '%s\n' "$result" >>"$FIXTURE/rejections"
      exit "$result"
    fi
  fi
done
exec "$REAL_JQ" "$@"
SH
chmod +x "${scratch}/bin/jq"

# Production derives the expected state from the checked-out Helm declaration,
# never from the live object being judged. Keep that read explicit in fixtures.
cat >"${scratch}/bin/yq" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[[ $# == 3 && "$1" == -o=json ]] || exit 91
if [[ "$3" == "$EXPECTED_PROBE_FILE" ]]; then
  [[ "$2" == '.spec.template.spec.containers' ]] || exit 91
  cat "$FIXTURE/expected-probe.json"
  exit 0
fi
if [[ "$3" == "$EXPECTED_PUBLISHER_FILE" ]]; then
  [[ "$2" == '.spec.template.spec.containers' ]] || exit 91
  cat "$FIXTURE/expected-publisher.json"
  exit 0
fi
if [[ "$3" == "$EXPECTED_CHART_FILE" ]]; then
  [[ "$2" == '.spec | {"url": .url, "digest": .ref.digest}' ]] || exit 91
  cat "$FIXTURE/expected-chart.json"
  exit 0
fi
[[ "$3" == "$EXPECTED_HELM_FILE" ]] || exit 91
case "$2" in
  '.spec.values.image | {"repository": .repository, "tag": .tag, "digest": .digest}') cat "$FIXTURE/expected-image.json" ;;
  '.spec.values | [.connectorReadiness.enabled,.contractReadiness.enabled,.contractProbe.enabled]') cat "$FIXTURE/expected-flags.json" ;;
  '.spec.values.demoProduct.ksailReleases.enabled // false') cat "$FIXTURE/expected-ksail.json" ;;
  *) exit 91 ;;
esac
SH
chmod +x "${scratch}/bin/yq"

cat >"${scratch}/fixtures.jq" <<'JQ'
def meta($name;$ns): {name:$name,namespace:$ns,uid:($name+"-uid"),generation:3};
def labels($name): {"app.kubernetes.io/name":"data-product-controller","app.kubernetes.io/instance":"data-product-controller","app.kubernetes.io/component":(if $name|endswith("-harbour") then "harbour-product" elif $name|endswith("-publisher-review") then "publisher-review" else "controller" end)};
def ports: [{name:"http",containerPort:8080,protocol:"TCP"}];
def chart_verify: {provider:"cosign",matchOIDCIdentity:[{issuer:"^https://token\\.actions\\.githubusercontent\\.com$",subject:"^https://github\\.com/devantler-tech/khoist/\\.github/workflows/publish-chart\\.yaml@refs/tags/v[0-9]+\\.[0-9]+\\.[0-9]+$"}]};
def root_verify: {provider:"cosign",matchOIDCIdentity:[{issuer:"^https://token\\.actions\\.githubusercontent\\.com$",subject:"^https://github\\.com/devantler-tech/platform/approved-synthetic-publication$"}]};
def condition($type): {type:$type,status:"True",observedGeneration:3};
def probe_labels: {"app.kubernetes.io/name":"data-product-controller","app.kubernetes.io/instance":"data-product-controller","app.kubernetes.io/component":"contract-probe"};
def probe_container: {name:"contract-probe",image:($repo+"@"+$index),command:["/contract-probe"],args:[],
  env:[{name:"CONTRACT_PROBE_URL",value:"https://harbour-data.example.com/openapi.json"},{name:"CONTRACT_READINESS_ENABLED",value:"false"}],
  ports:[{name:"management",containerPort:8081,protocol:"TCP"}],
  readinessProbe:{httpGet:{path:"/readyz",port:"management",scheme:"HTTP"},timeoutSeconds:7,periodSeconds:30,failureThreshold:1},
  livenessProbe:{httpGet:{path:"/healthz",port:"management",scheme:"HTTP"},timeoutSeconds:2,periodSeconds:10,failureThreshold:3},
  securityContext:{runAsNonRoot:true,readOnlyRootFilesystem:true,allowPrivilegeEscalation:false,capabilities:{drop:["ALL"]}}};
def owner($kind;$name): {apiVersion:"apps/v1",kind:$kind,name:$name,uid:($name+"-uid"),controller:true};
def deployment($name;$container): {apiVersion:"apps/v1",kind:"Deployment",metadata:(meta($name;"data-product-controller") + {annotations:{"deployment.kubernetes.io/revision":"7"}}),
  spec:{replicas:2,selector:{matchLabels:labels($name)},template:{metadata:{labels:labels($name)},spec:{containers:[{name:$container,image:($repo+"@"+$index),ports:ports,env:[]}]}}},
  status:{observedGeneration:3,replicas:2,updatedReplicas:2,readyReplicas:2,availableReplicas:2}};
def rs($name;$container): {apiVersion:"apps/v1",kind:"ReplicaSet",metadata:(meta($name+"-new";"data-product-controller") + {annotations:{"deployment.kubernetes.io/revision":"7"},ownerReferences:[owner("Deployment";$name)]}),
  spec:{replicas:2,template:{spec:{containers:[{name:$container,image:($repo+"@"+$index),env:[]}]}}},status:{observedGeneration:3,replicas:2,readyReplicas:2,availableReplicas:2}};
def pod($name;$container;$ordinal): {apiVersion:"v1",kind:"Pod",metadata:(meta($name+"-pod-"+($ordinal|tostring);"data-product-controller") + {labels:labels($name),ownerReferences:[owner("ReplicaSet";$name+"-new")]}),
  spec:{containers:[{name:$container,image:($repo+"@"+$index),ports:ports,env:[]}]},status:{phase:"Running",podIP:("10.0.0."+($ordinal|tostring)),podIPs:[{ip:("10.0.0."+($ordinal|tostring))}],conditions:[{type:"Ready",status:"True"}],containerStatuses:[{name:$container,ready:true,imageID:($repo+"@"+$child),state:{running:{startedAt:"2026-10-03T00:00:00Z"}}}]}};
def service($name): {apiVersion:"v1",kind:"Service",metadata:meta($name;"data-product-controller"),spec:{type:"ClusterIP",selector:labels($name),ports:[{name:"http",port:80,targetPort:"http",protocol:"TCP"}]}};
def slice($name): {apiVersion:"discovery.k8s.io/v1",kind:"EndpointSlice",metadata:(meta($name+"-slice";"data-product-controller") + {labels:{"kubernetes.io/service-name":$name},ownerReferences:[{apiVersion:"v1",kind:"Service",name:$name,uid:($name+"-uid"),controller:true}]}),addressType:"IPv4",ports:[{name:"http",port:8080,protocol:"TCP"}],endpoints:[range(1;3) as $ordinal | {addresses:[("10.0.0."+($ordinal|tostring))],conditions:{ready:true,serving:true,terminating:false},targetRef:{kind:"Pod",namespace:"data-product-controller",name:($name+"-pod-"+($ordinal|tostring)),uid:($name+"-pod-"+($ordinal|tostring)+"-uid")}}]};
def route($suffix;$host;$backend): {apiVersion:"gateway.networking.k8s.io/v1",kind:"HTTPRoute",metadata:meta("data-product-controller-"+$suffix;"data-product-controller"),
  spec:{parentRefs:[{name:"platform",namespace:"kube-system",sectionName:"https"}],hostnames:[$host+".example.com"],rules:[{matches:[{path:{type:"PathPrefix",value:"/"}}],backendRefs:[$backend],filters:(if $suffix == "registry" then [] else [{type:"RequestHeaderModifier",requestHeaderModifier:{remove:["Cookie","Authorization"]}}] end)}]},
  status:{parents:[{parentRef:{group:"gateway.networking.k8s.io",kind:"Gateway",name:"platform",namespace:"kube-system",sectionName:"https"},controllerName:"io.cilium/gateway-controller",conditions:[condition("Accepted"),condition("ResolvedRefs")]}]}};
{apps:{apiVersion:"kustomize.toolkit.fluxcd.io/v1",kind:"Kustomization",metadata:meta("apps";"flux-system"),spec:{suspend:false,sourceRef:{kind:"OCIRepository",name:"flux-system"}},status:{observedGeneration:3,conditions:[condition("Ready")],lastAppliedRevision:("latest@"+$apps)}},
 root:{apiVersion:"source.toolkit.fluxcd.io/v1",kind:"OCIRepository",metadata:meta("flux-system";"flux-system"),spec:{suspend:false,verify:root_verify},status:{observedGeneration:3,conditions:[condition("Ready"),condition("SourceVerified")],artifact:{revision:("latest@"+$apps),digest:("sha256:"+([range(64)|"e"]|join("")))}}},
 chart:{apiVersion:"source.toolkit.fluxcd.io/v1",kind:"OCIRepository",metadata:meta("data-product-controller";"data-product-controller"),spec:{suspend:false,url:"oci://ghcr.io/devantler-tech/charts/khoist",ref:{digest:$chart},verify:chart_verify},status:{observedGeneration:3,conditions:[condition("Ready"),condition("SourceVerified")],artifact:{revision:$chart,digest:("sha256:"+([range(64)|"e"]|join("")))}}},
 product:{apiVersion:"data.devantler.tech/v1alpha1",kind:"DataProduct",metadata:meta("harbour-observations";"data-product-controller"),spec:{outputs:[{name:"observations",url:"https://harbour-data.example.com/api/observations",contractUrl:"https://harbour-data.example.com/openapi.json"}]},status:{observedGeneration:3,conditions:[condition("Ready")]}},
 helm:{apiVersion:"helm.toolkit.fluxcd.io/v2",kind:"HelmRelease",metadata:meta("data-product-controller";"data-product-controller"),spec:{suspend:false,chartRef:{kind:"OCIRepository",name:"data-product-controller"},upgrade:{chartNameChangeStrategy:"InPlaceUpdate"},values:{image:{repository:$repo,tag:"1.20.0",digest:$index},controller:{replicas:2},demoProduct:{enabled:true,replicas:2,publicBaseURL:"https://harbour-data.example.com"},route:{enabled:true,host:"data-products.example.com"},connectorReadiness:{enabled:false},contractReadiness:{enabled:false},contractProbe:{enabled:false}}},status:{observedGeneration:3,conditions:[condition("Ready")],lastAttemptedRevision:("1.20.0+"+$chart[7:19]),lastAttemptedRevisionDigest:$chart,lastAttemptedConfigDigest:("sha256:"+([range(64)|"f"]|join(""))),history:[{name:"data-product-controller",namespace:"data-product-controller",chartName:"khoist",chartVersion:("1.20.0+"+$chart[7:19]),ociDigest:$chart,configDigest:("sha256:"+([range(64)|"f"]|join(""))),status:"deployed",version:10}]}},
 "deployment-controller":(deployment("data-product-controller";"controller") | .spec.template.spec.containers[0].env=[
   {name:"CONNECTOR_READINESS_ENABLED",value:"false"},{name:"CONTRACT_READINESS_ENABLED",value:"false"}]),
 "deployment-harbour":deployment("data-product-controller-harbour";"product"),
 "deployment-publisher-review":deployment("data-product-controller-publisher-review";"publisher-review"),
 "deployment-probe":{apiVersion:"apps/v1",kind:"Deployment",metadata:meta("data-product-controller-contract-probe";"data-product-controller"),
   spec:{replicas:0,selector:{matchLabels:probe_labels},template:{metadata:{labels:probe_labels,annotations:{"coroot.com/scrape-metrics":"true","coroot.com/metrics-port":"8081","coroot.com/metrics-path":"/metrics","coroot.com/metrics-scheme":"http"}},
     spec:{serviceAccountName:"data-product-controller-contract-probe",automountServiceAccountToken:false,containers:[probe_container]}}},
   status:{observedGeneration:3,replicas:0,updatedReplicas:0,readyReplicas:0,availableReplicas:0}},
 "probe-account":{apiVersion:"v1",kind:"ServiceAccount",metadata:meta("data-product-controller-contract-probe";"data-product-controller"),automountServiceAccountToken:false},
 "observer-role":{apiVersion:"rbac.authorization.k8s.io/v1",kind:"Role",metadata:meta("data-product-readiness-observer";"data-product-controller"),
   rules:[{apiGroups:["apps"],resources:["deployments"],resourceNames:["data-product-controller-harbour","data-product-controller-contract-probe"],verbs:["get"]}]},
 "observer-binding":{apiVersion:"rbac.authorization.k8s.io/v1",kind:"RoleBinding",metadata:meta("data-product-readiness-observer";"data-product-controller"),
   roleRef:{apiGroup:"rbac.authorization.k8s.io",kind:"Role",name:"data-product-readiness-observer"},
   subjects:[{kind:"ServiceAccount",name:"data-product-controller",namespace:"data-product-controller"}]},
 "probe-policy":{apiVersion:"cilium.io/v2",kind:"CiliumNetworkPolicy",metadata:meta("allow-data-product-controller-contract-probe";"data-product-controller"),
   spec:{endpointSelector:{matchLabels:{"k8s:app.kubernetes.io/name":"data-product-controller","k8s:app.kubernetes.io/instance":"data-product-controller","k8s:app.kubernetes.io/component":"contract-probe"}},
     ingress:[{fromEndpoints:[{matchLabels:{"k8s:io.kubernetes.pod.namespace":"observability","app.kubernetes.io/managed-by":"coroot-operator","app.kubernetes.io/part-of":"coroot","app.kubernetes.io/component":"coroot-cluster-agent"}}],toPorts:[{ports:[{port:"8081",protocol:"TCP"}]}]}],
     egress:[{toFQDNs:[{matchName:"harbour-data.example.com"}],toPorts:[{ports:[{port:"443",protocol:"TCP"}]}]},
       {toEndpoints:[{matchLabels:{"k8s:io.kubernetes.pod.namespace":"kube-system","k8s-app":"kube-dns"}}],
        toPorts:[{ports:[{port:"53",protocol:"UDP"},{port:"53",protocol:"TCP"}],rules:{dns:[{matchName:"harbour-data.example.com"}]}}]}]}},
 replicasets:{apiVersion:"apps/v1",kind:"ReplicaSetList",items:[rs("data-product-controller";"controller"),rs("data-product-controller-harbour";"product"),rs("data-product-controller-publisher-review";"publisher-review"),
   (rs("data-product-controller-contract-probe";"contract-probe") | .metadata.name="dormant-probe-rs" | .metadata.uid="dormant-probe-rs-uid" | .spec.replicas=0 | .status.replicas=0 | .status.readyReplicas=0 | .status.availableReplicas=0)]},
 pods:{apiVersion:"v1",kind:"PodList",items:[pod("data-product-controller";"controller";1),pod("data-product-controller";"controller";2),pod("data-product-controller-harbour";"product";1),pod("data-product-controller-harbour";"product";2),pod("data-product-controller-publisher-review";"publisher-review";1),pod("data-product-controller-publisher-review";"publisher-review";2)]},
 "service-harbour":service("data-product-controller-harbour"),"service-publisher-review":service("data-product-controller-publisher-review"),
 "all-products":{apiVersion:"v1",kind:"List",items:[{metadata:{name:"INVENTORY-NAME-CANARY",namespace:"tenant-canary"},spec:{}},{metadata:{name:"harbour-observations",namespace:"data-product-controller"},spec:{}}]},
 endpoints:{apiVersion:"discovery.k8s.io/v1",kind:"EndpointSliceList",metadata:{},items:[slice("data-product-controller-harbour"),slice("data-product-controller-publisher-review")]},
 "route-registry":route("registry";"data-products";{name:"oauth2-proxy",namespace:"oauth2-proxy",port:80}),
 "route-harbour":route("harbour";"harbour-data";{name:"data-product-controller-harbour",port:80}),
 "route-publisher-review":route("publisher-review";"publisher-review";{name:"data-product-controller-publisher-review",port:80}),
 gateway:{apiVersion:"gateway.networking.k8s.io/v1",kind:"Gateway",metadata:meta("platform";"kube-system"),spec:{gatewayClassName:"cilium",listeners:[{name:"https",protocol:"HTTPS",port:443,tls:{mode:"Terminate"}}]},status:{conditions:[condition("Accepted"),condition("Programmed")],listeners:[{name:"https",conditions:[condition("Accepted"),condition("ResolvedRefs"),condition("Programmed")]}]}}}
JQ
jq -n --arg index "$index" --arg child "$child" --arg chart "$chart" --arg apps "$apps" \
  --arg repo 'ghcr.io/devantler-tech/khoist' -f "${scratch}/fixtures.jq" >"${scratch}/all.json"
while IFS= read -r key; do jq --arg key "$key" '.[$key]' "${scratch}/all.json" >"${scratch}/healthy/${key}.json"; done < <(jq -r 'keys[]' "${scratch}/all.json")
printf '[false,false,false]\n' >"${scratch}/healthy/expected-flags.json"
printf 'false\n' >"${scratch}/healthy/expected-ksail.json"
jq '[."deployment-publisher-review".spec.template.spec.containers[] | select(.name=="publisher-review")]' "${scratch}/all.json" >"${scratch}/healthy/expected-publisher.json"
jq '.helm.spec.values.image' "${scratch}/all.json" >"${scratch}/healthy/expected-image.json"
jq '{url:.chart.spec.url,digest:.chart.spec.ref.digest}' "${scratch}/all.json" >"${scratch}/healthy/expected-chart.json"
jq '."deployment-probe".spec.template.spec.containers' "${scratch}/all.json" >"${scratch}/healthy/expected-probe.json"
jq '.root.spec.verify' "${scratch}/all.json" >"${scratch}/apps-verify.json"

cat >"${scratch}/bin/kubectl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == --kubeconfig && "$2" == "$KUBECONFIG" && "$3 $4 $5" == '--context synthetic-ci --namespace' ]] || exit 91
namespace=$6
[[ "$7" == --request-timeout && "$8" =~ ^[1-5]s$ && "$9" == get ]] || exit 92
shift 9
resource=$1 name=${2:-}
key=''
case "$namespace:$resource:$name" in
  flux-system:kustomizations.kustomize.toolkit.fluxcd.io:apps) key=apps ;;
  flux-system:ocirepositories.source.toolkit.fluxcd.io:flux-system) key=root ;;
  data-product-controller:ocirepositories.source.toolkit.fluxcd.io:data-product-controller) key=chart ;;
  data-product-controller:helmreleases.helm.toolkit.fluxcd.io:data-product-controller) key=helm ;;
  data-product-controller:dataproducts.data.devantler.tech:harbour-observations) key=product ;;
  data-product-controller:dataproducts.data.devantler.tech:ksail-releases) key=release-product ;;
  data-product-controller:services:data-product-controller-harbour) key=service-harbour ;;
  data-product-controller:services:data-product-controller-publisher-review) key=service-publisher-review ;;
  data-product-controller:endpointslices.discovery.k8s.io:-o) key=endpoints ;;
  data-product-controller:deployments.apps:data-product-controller) key=deployment-controller ;;
  data-product-controller:deployments.apps:data-product-controller-harbour) key=deployment-harbour ;;
  data-product-controller:deployments.apps:data-product-controller-publisher-review) key=deployment-publisher-review ;;
  data-product-controller:deployments.apps:data-product-controller-contract-probe) key=deployment-probe ;;
  data-product-controller:serviceaccounts:data-product-controller-contract-probe) key=probe-account ;;
  data-product-controller:roles.rbac.authorization.k8s.io:data-product-readiness-observer) key=observer-role ;;
  data-product-controller:rolebindings.rbac.authorization.k8s.io:data-product-readiness-observer) key=observer-binding ;;
  data-product-controller:ciliumnetworkpolicies.cilium.io:allow-data-product-controller-contract-probe) key=probe-policy ;;
  data-product-controller:dataproducts.data.devantler.tech:--all-namespaces) key=all-products ;;
  data-product-controller:httproutes.gateway.networking.k8s.io:data-product-controller-registry) key=route-registry ;;
  data-product-controller:httproutes.gateway.networking.k8s.io:data-product-controller-harbour) key=route-harbour ;;
  data-product-controller:httproutes.gateway.networking.k8s.io:data-product-controller-publisher-review) key=route-publisher-review ;;
  kube-system:gateways.gateway.networking.k8s.io:platform) key=gateway ;;
  data-product-controller:replicasets.apps:-o) key=replicasets ;;
  data-product-controller:pods:-o) key=pods ;;
  *) exit 93 ;;
esac
if [[ "$key" == pods || "$key" == replicasets || "$key" == endpoints ]]; then
  [[ "$*" == "$resource -o json" ]] || exit 94
else
  if [[ "$key" == release-product && "$(cat "$FIXTURE/expected-ksail.json")" == false ]]; then
    [[ "$*" == "$resource $name --ignore-not-found -o json" ]] || exit 95
  else
    [[ "$*" == "$resource $name -o json" ]] || exit 95
  fi
fi
printf '%s\n' "$key" >>"$FIXTURE/reads"
if [[ "$key" == apps && "${MODE:-}" == hang ]]; then
  (trap '' TERM; sleep 30) &
  printf '%s\n' "$!" >"$FIXTURE/child.pid"
  wait
fi
if [[ "$key" == chart && "${MODE:-}" == orphan ]]; then
  (trap '' TERM; sleep 30) >/dev/null 2>&1 &
  printf '%s\n' "$!" >>"$FIXTURE/orphan.pids"
fi
if [[ "$key" == chart ]]; then
  case "${MODE:-}" in
    forbidden) printf 'PRIVATE-ERROR-CANARY\n' >&2; exit 1 ;;
    empty) exit 0 ;;
    multi) cat "$FIXTURE/chart.json" "$FIXTURE/chart.json"; exit 0 ;;
  esac
fi
if [[ "$key" == service-harbour ]]; then
  case "${MODE:-}" in
    read-dropped-once)
      if [[ ! -f "$FIXTURE/read-dropped" ]]; then
        : >"$FIXTURE/read-dropped"
        printf 'PRIVATE-ERROR-CANARY\n' >&2
        exit 1
      fi ;;
    read-dropped-always) printf 'PRIVATE-ERROR-CANARY\n' >&2; exit 1 ;;
  esac
fi
if [[ "$key" == observer-role && "${MODE:-}" == partial-observer ]]; then
  cat "$FIXTURE/observer-role.json"; exit 1
fi
if [[ "$key" == all-products ]]; then
  case "${MODE:-}" in
    inventory-forbidden) printf 'PRIVATE-ERROR-CANARY\n' >&2; exit 1 ;;
    inventory-hang) sleep 60 ;;
    inventory-partial) cat "$FIXTURE/all-products.json"; exit 1 ;;
    inventory-empty) printf '{"items":[]}\n'; exit 0 ;;
    inventory-malformed) printf '{"items":[{"metadata":{}}]}\n'; exit 0 ;;
    inventory-ui) jq '.items[0].spec.ui={}' "$FIXTURE/all-products.json"; exit 0 ;;
  esac
fi
if [[ "$key" == release-product ]]; then
  case "${MODE:-}" in
    release-forbidden) printf 'PRIVATE-ERROR-CANARY\n' >&2; exit 1 ;;
    release-partial-forbidden) cat "$FIXTURE/product.json"; printf 'PRIVATE-ERROR-CANARY\n' >&2; exit 1 ;;
    release-null) printf 'null\n'; exit 0 ;;
    release-malformed) printf '{\n'; exit 0 ;;
  esac
fi
count=0
[[ ! -f "$FIXTURE/$key.count" ]] || count=$(cat "$FIXTURE/$key.count")
count=$((count + 1)); printf '%s\n' "$count" >"$FIXTURE/$key.count"
# The second and later reads return the changed object; in flap mode only every
# other read does, so no two snapshots in a row ever agree. An object with a
# second change returns it from the fourth read on, so the second pair differs
# from the first for another reason and only the third pair agrees.
snapshot_mode=${MODE:-}
[[ "$snapshot_mode" != flap-public-failure ]] || snapshot_mode=flap
if [[ "$snapshot_mode" != flap && -f "$FIXTURE/after2/$key.json" ]] && ((count >= 4)); then
  response="$FIXTURE/after2/$key.json"
elif [[ -f "$FIXTURE/after/$key.json" ]] && { { [[ "$snapshot_mode" == flap ]] && ((count % 2 == 0)); } || { [[ "$snapshot_mode" != flap ]] && ((count >= 2)); }; }; then
  response="$FIXTURE/after/$key.json"
else
  response="$FIXTURE/$key.json"
fi
if [[ "$key" == release-product ]] && { [[ ! -f "$response" ]] || jq -e '.==null' "$response" >/dev/null; }; then
  [[ "$*" == "$resource $name --ignore-not-found -o json" ]] || { printf 'PRIVATE-ERROR-CANARY\n' >&2; exit 1; }
  exit 0
fi
cat "$response"
SH
cat >"${scratch}/bin/curl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == --disable ]] || exit 91
shift
headers='' body='' url='' cookie=0 authorization=0 proto=0 proxy=0 transfer_seconds=0
while (($#)); do
  case "$1" in
    --silent|--show-error) shift ;;
    --proxy) [[ "$2" == '' ]] || exit 92; proxy=1; shift 2 ;;
    --connect-timeout) [[ "$2" =~ ^[1-5]$ ]] || exit 93; shift 2 ;;
    --max-time) [[ "$2" =~ ^[1-9][0-9]*$ ]] || exit 94; transfer_seconds=$2; shift 2 ;;
    --max-filesize) [[ "$2" == 262144 ]] || exit 95; shift 2 ;;
    --proto) [[ "$2" == '=https' ]] || exit 96; proto=1; shift 2 ;;
    --header) case "$2" in Cookie:) cookie=1 ;; Authorization:) authorization=1 ;; *) exit 97 ;; esac; shift 2 ;;
    --dump-header) headers=$2; shift 2 ;;
    --output) body=$2; shift 2 ;;
    --write-out) [[ "$2" == '%{http_code}' ]] || exit 98; shift 2 ;;
    --url) url=$2; shift 2 ;;
    *) exit 99 ;;
  esac
done
[[ "$cookie:$authorization:$proto:$proxy" == 1:1:1:1 && -n "$headers" && -n "$body" ]] || exit 100
printf '%s\n' "$url" >>"$FIXTURE/urls"
storage_dir=$(dirname "$body")
if [[ "${MODE:-}" == unsafe-storage ]]; then chmod 755 "$storage_dir"; fi
# GNU stat -f may emit filesystem data even when the BSD-style attempt fails.
if permission=$(stat -f '%Lp' "$storage_dir" 2>/dev/null); then
  :
else
  permission=$(stat -c '%a' "$storage_dir") || exit 101
fi
[[ "$permission" == 700 ]] || exit 101
csp="default-src 'self'; connect-src 'none'; frame-src https:; frame-ancestors 'none'; object-src 'none'; base-uri 'none'"
publisher=$(jq -r 'any(.[0].env[]; .name=="PUBLISHER_PREFLIGHT_ENABLED" and .value=="true")' "$FIXTURE/expected-publisher.json")
code=200
printf 'HTTP/2 200\r\nContent-Security-Policy: %s\r\nReferrer-Policy: no-referrer\r\nX-Content-Type-Options: nosniff\r\n\r\n' "$csp" >"$headers"
case "$url" in
  https://harbour-data.example.com/healthz) printf '' >"$body" ;;
  https://harbour-data.example.com/api/observations) printf '{"items":[{"station":"Nordhavn","temperatureCelsius":12.4,"salinityPsu":7.8,"observedAt":"2026-01-01T00:00:00Z"},{"station":"Sydhavnen","temperatureCelsius":13.1,"salinityPsu":7.2,"observedAt":"2026-01-01T00:00:00Z"}]}\n' >"$body" ;;
  https://harbour-data.example.com/openapi.json) printf '{"openapi":"3.1.0","info":{"title":"Harbour observations"},"paths":{"/api/observations":{"get":{"responses":{"200":{}}}}}}\n' >"$body" ;;
  https://harbour-data.example.com/ksail-releases/ui)
    printf '<meta name="product-kind" content="ksail-releases" />\n' >"$body"
    csp="default-src 'self'; script-src 'self'; style-src 'self'; connect-src https://api.github.com/repos/devantler-tech/ksail/releases; frame-ancestors https:; object-src 'none'; base-uri 'none'"
    printf 'HTTP/2 200\r\nContent-Security-Policy: %s\r\nReferrer-Policy: no-referrer\r\nX-Content-Type-Options: nosniff\r\n\r\n' "$csp" >"$headers" ;;
  https://harbour-data.example.com/ksail-releases/openapi.json) printf '{"openapi":"3.1.0","info":{"title":"KSail release history"},"servers":[{"url":"https://api.github.com"}],"paths":{"/repos/devantler-tech/ksail/releases":{"get":{"responses":{"200":{}}}}}}\n' >"$body" ;;
  https://publisher-review.example.com/healthz) printf 'ok\n' >"$body" ;;
  https://publisher-review.example.com/) : >"$body" ;;
  https://publisher-review.example.com/descriptor.js) printf 'window.DataProductDescriptor = {};\n' >"$body" ;;
  https://publisher-review.example.com/metadata.js) printf 'const capabilities = {"data-product-ui/v2": []};\n' >"$body" ;;
  https://publisher-review.example.com/publisher-review)
    printf '<form id="report-form"><input id="report-file"><textarea id="report-text"></textarea></form><p id="report-status"></p><script src="/metadata.js"></script><script src="/descriptor.js"></script><script src="/preflight-report.js"></script><script src="/publisher.js"></script><link href="publisher.css">\n' >"$body" ;;
  https://publisher-review.example.com/publisher.js) printf 'DataProductPreflightReport.parse({});\n' >"$body" ;;
  https://publisher-review.example.com/preflight-report.js) printf 'window.DataProductPreflightReport = {};\n' >"$body" ;;
  https://publisher-review.example.com/publisher.css) printf ':root { color-scheme: light dark; }\n' >"$body" ;;
  *) exit 102 ;;
esac
case "$url" in
  */publisher-review | */publisher.js | */publisher.css | */preflight-report.js | */descriptor.js | */metadata.js)
    if [[ "$publisher" == true ]]; then
      csp="default-src 'self'; connect-src 'none'; frame-src 'none'; frame-ancestors 'none'; object-src 'none'; base-uri 'none'"
      printf 'HTTP/2 200\r\nContent-Security-Policy: %s\r\nCache-Control: no-store\r\nReferrer-Policy: no-referrer\r\nX-Content-Type-Options: nosniff\r\n\r\n' "$csp" >"$headers"
    else code=404; fi ;;
esac
if [[ "$url" == https://publisher-review.example.com/ ]]; then
  if [[ "$publisher" == true ]]; then
    code=307
    printf 'HTTP/2 307\r\nLocation: /publisher-review\r\nContent-Security-Policy: %s\r\nCache-Control: no-store\r\n\r\n' "$csp" >"$headers"
  else code=404; fi
fi
if [[ "${MODE:-}" == flaky-public && ! -e "$FIXTURE/public-failed-once" ]]; then
  : >"$FIXTURE/public-failed-once"
  printf '503'
  exit 0
fi
case "${MODE:-}" in
  connection-refused-once | empty-reply-once | connection-reset-once)
    if [[ ! -e "$FIXTURE/public-dropped-once" ]]; then
      : >"$FIXTURE/public-dropped-once"
      printf '000'
      case "$MODE" in
        connection-refused-once) exit 7 ;;
        empty-reply-once) exit 52 ;;
        *) exit 56 ;;
      esac
    fi
    ;;
  dropped-at-deadline) sleep "$transfer_seconds"; printf '000'; exit 56 ;;
  slow-public) sleep 0.55 ;;
  connection-reset) printf '000'; printf 'PRIVATE-ERROR-CANARY: private upstream\n' >&2; exit 56 ;;
  invalid-http-status) printf '503PRIVATE-ERROR-CANARY'; exit 0 ;;
  dns-failure) printf 'PRIVATE-ERROR-CANARY: private resolver\n' >&2; exit 6 ;;
  tls-failure) printf 'PRIVATE-ERROR-CANARY: private certificate\n' >&2; exit 60 ;;
  tls-handshake-failure) printf 'PRIVATE-ERROR-CANARY: private handshake\n' >&2; exit 35 ;;
  request-timeout) printf 'PRIVATE-ERROR-CANARY: private endpoint\n' >&2; exit 28 ;;
  transfer-timeout) sleep "$transfer_seconds"; printf 'PRIVATE-ERROR-CANARY: private endpoint\n' >&2; exit 28 ;;
  wrong-release-ui) [[ "$url" != */ksail-releases/ui ]] || printf '<title>Harbour</title>\n' >"$body" ;;
  weak-release-csp) [[ "$url" != */ksail-releases/ui ]] || printf "HTTP/2 200\r\nContent-Security-Policy: default-src *\r\n\r\n" >"$headers" ;;
  wrong-release-api) [[ "$url" != */ksail-releases/openapi.json ]] || printf '{"openapi":"3.1.0","info":{"title":"KSail release history"},"servers":[{"url":"https://other.example.com"}],"paths":{"/repos/devantler-tech/ksail/releases":{"get":{"responses":{"200":{}}}}}}\n' >"$body" ;;
  http-failure | flap-public-failure) printf '503'; exit 0 ;;
  redirect) printf '302'; exit 0 ;;
  missing-csp) [[ "$url" != */publisher-review ]] || printf 'HTTP/2 200\r\n\r\n' >"$headers" ;;
  weak-csp) [[ "$url" != */publisher-review ]] || printf 'HTTP/2 200\r\nContent-Security-Policy: default-src *\r\n\r\n' >"$headers" ;;
  duplicate-csp) [[ "$url" != */publisher-review ]] || printf 'Content-Security-Policy: %s\r\n' "$csp" >>"$headers" ;;
  wrong-body) printf '{"items":[]}\n' >"$body" ;;
  wrong-asset) [[ "$url" != */metadata.js ]] || printf 'not a protocol library\n' >"$body" ;;
  publisher-unavailable) [[ "$url" != */publisher-review ]] || code=503 ;;
  publisher-missing-style) [[ "$url" != */publisher.css ]] || code=404 ;;
  publisher-wrong-script) [[ "$url" != */publisher.js ]] || printf 'wrong script\n' >"$body" ;;
  publisher-wrong-report-library) [[ "$url" != */preflight-report.js ]] || printf 'wrong library\n' >"$body" ;;
  publisher-wrong-page) [[ "$url" != */publisher-review ]] || printf '<title>Other workspace</title>\n' >"$body" ;;
  publisher-weak-csp) [[ "$url" != */publisher-review ]] || printf 'HTTP/2 200\r\nContent-Security-Policy: default-src *\r\n\r\n' >"$headers" ;;
  publisher-cacheable) [[ "$url" != */publisher-review ]] || sed -i.bak 's/no-store/public/' "$headers" ;;
  publisher-wrong-root) [[ "$url" != https://publisher-review.example.com/ ]] || sed -i.bak 's|Location: /publisher-review|Location: /other|' "$headers" ;;
  publisher-unplanned-route) [[ "$url" != */publisher-review ]] || code=200 ;;
esac
printf '%s' "$code"
SH
chmod +x "${scratch}/bin/kubectl" "${scratch}/bin/curl"

expected_urls=11
# run_case exercises one fixture and checks its verdict, cleanup and sanitized output.
# run_case drains independent mutations before exercising a foreground verifier scenario.
run_case() {
  # Foreground cases and shared-input changes never overlap the two workers.
  # The child deliberately reads its own exported flag; the parent stays unset.
  # shellcheck disable=SC2031
  if [[ ${FIXTURE_POOL_WORKER:-0} != 1 ]]; then fixture_pool_drain; fi
  local name=$1 expected=$2 mode=${3:-} result=0 timeout_seconds=${4:-15}
  # Healthy fixtures include the expanded sequential public probes and full
  # snapshots. Keep rejection/cleanup deadline controls separately bounded.
  if [[ $# -lt 4 && "$expected" == pass ]]; then timeout_seconds=30; fi
  local fixture="${scratch}/${name}"
  [[ -d "$fixture" ]] || {
    mkdir "$fixture"
    cp "${scratch}/healthy/"*.json "$fixture/"
  }
  if [[ $# -lt 4 && "$expected" != pass && "$mode" != http-failure && "$mode" != redirect && "$mode" != *csp && "$mode" != wrong-body && "$mode" != wrong-asset && "$mode" != unsafe-storage ]]; then timeout_seconds=3; fi
  local probe_runtime="$child"
  if [[ -f "$fixture/expected-probe-runtime" ]]; then probe_runtime=$(cat "$fixture/expected-probe-runtime"); fi
  PATH="${scratch}/bin:$PATH" REAL_JQ="$real_jq" FIXTURE="$fixture" MODE="$mode" KUBECONFIG="${scratch}/kubeconfig" \
    EXPECTED_HELM_FILE="${root_dir}/k8s/bases/apps/data-product-controller/helm-release.yaml" \
    EXPECTED_CHART_FILE="${root_dir}/k8s/bases/apps/data-product-controller/oci-repository.yaml" \
    EXPECTED_PUBLISHER_FILE="${root_dir}/k8s/bases/apps/data-product-controller/deployment-publisher-review.yaml" \
    EXPECTED_PROBE_FILE="${root_dir}/k8s/bases/apps/data-product-controller/deployment-contract-probe.yaml" \
    bash "$script" --context synthetic-ci --domain example.com --image-digest "$index" \
    --runtime-digest "$child" --probe-runtime-digest "$probe_runtime" --chart-digest "$chart" --apps-digest "$apps" --apps-verify-file "${scratch}/apps-verify.json" --timeout "$timeout_seconds" \
    >"$fixture/stdout" 2>"$fixture/stderr" || result=$?
  if [[ "$expected" == pass ]]; then
    local expected_state expected_pods public_count ksail publisher
    expected_state=$(jq -r 'if .==[true,true,false] then "active" else "dormant" end' "$fixture/expected-flags.json")
    if [[ "$expected_state" == active ]]; then expected_pods=8; else expected_pods=6; fi
    ksail=$(cat "$fixture/expected-ksail.json")
    publisher=$(jq -r 'any(.[0].env[]; .name=="PUBLISHER_PREFLIGHT_ENABLED" and .value=="true")' "$fixture/expected-publisher.json")
    if [[ "$ksail" == true ]]; then public_count=13; else public_count=11; fi
    if [[ "$result" != 0 ]] || ! jq -e --arg state "$expected_state" --argjson pods "$expected_pods" --argjson public_count "$public_count" --argjson ksail "$ksail" --argjson publisher "$publisher" '.complete == true and .deployments == 4 and .pods == $pods and .routes == 3 and .publicChecks == $public_count and .ksailReleasesEnabled == $ksail and .publisherReviewEnabled == $publisher and .readinessState == $state' "$fixture/stdout" >/dev/null; then
      local reason
      reason=$(jq -r '.failure // "invalid_report"' "$fixture/stdout" 2>/dev/null) || reason=invalid_report
      case "$reason" in
        invalid_arguments | missing_dependency | private_storage_unavailable | deadline_exceeded | invalid_verification_policy | invalid_expected_state | read_incomplete | public_contract_incomplete | rollout_changed | interrupted) ;;
        *) reason=invalid_report ;;
      esac
      fail "$name: healthy live-shaped rollout was not accepted (helper exit $result, $reason)"
    fi
    [[ $(wc -l <"$fixture/urls") -eq "$expected_urls" ]] || fail "$name: public checks were incomplete"
  else
    if [[ "$result" == 0 ]] || ! jq -e '.complete == false and (.failure | type == "string")' "$fixture/stdout" >/dev/null; then fail "$name: incomplete or unsafe rollout was accepted"; fi
    local public_role='' public_reason=''
    case "$mode" in
      dns-failure)
        public_role=sample_health
        public_reason=dns_failed
        ;;
      tls-failure | tls-handshake-failure)
        public_role=sample_health
        public_reason=tls_failed
        ;;
      request-timeout | transfer-timeout)
        public_role=sample_health
        public_reason=request_timeout
        ;;
      http-failure | redirect | invalid-http-status)
        public_role=sample_health
        public_reason=http_status
        ;;
      connection-reset)
        public_role=sample_health
        public_reason=transport_failed
        ;;
      missing-csp | weak-csp | duplicate-csp)
        public_role=publisher_index
        public_reason=security_headers
        ;;
      wrong-body)
        public_role=sample_observations
        public_reason=body_invalid
        ;;
      wrong-asset)
        public_role=metadata_library
        public_reason=asset_invalid
        ;;
      wrong-release-ui)
        public_role=release_index
        public_reason=asset_invalid
        ;;
      weak-release-csp)
        public_role=release_index
        public_reason=security_headers
        ;;
      wrong-release-api)
        public_role=release_openapi
        public_reason=body_invalid
        ;;
      publisher-unavailable | publisher-unplanned-route)
        public_role=publisher_index
        public_reason=http_status
        ;;
      publisher-missing-style)
        public_role=publisher_style
        public_reason=http_status
        ;;
      publisher-wrong-script)
        public_role=publisher_script
        public_reason=asset_invalid
        ;;
      publisher-wrong-report-library)
        public_role=publisher_library
        public_reason=asset_invalid
        ;;
      publisher-wrong-page)
        public_role=publisher_index
        public_reason=asset_invalid
        ;;
      publisher-weak-csp | publisher-cacheable)
        public_role=publisher_index
        public_reason=security_headers
        ;;
      publisher-wrong-root)
        public_role=publisher_root
        public_reason=asset_invalid
        ;;
    esac
    if [[ -n "$public_role" ]]; then
      jq -e --arg role "$public_role" --arg reason "$public_reason" '
        (.failure == "public_contract_incomplete" or
          ($reason == "request_timeout" and .failure == "deadline_exceeded")) and .phase == "public_contract" and
        .role == $role and .reason == $reason' "$fixture/stdout" >/dev/null ||
        fail "$name: refusal omitted the safe public phase, role or reason"
    fi
    if [[ "$mode" == unsafe-storage ]]; then
      jq -e '.failure == "public_contract_incomplete"' "$fixture/stdout" >/dev/null || fail "$name: unsafe permissions were not refused during public checks"
    fi
    if [[ "$name" == changed-* ]]; then
      [[ -f "$fixture/urls" && $(wc -l <"$fixture/urls") -eq "$expected_urls" ]] || fail "$name: the public checks were not repeated for every snapshot pair"
      jq -e --arg key "${name#changed-}" '.failure == "rollout_changed" and (.changed | type == "array" and length > 0 and any(.[]; . == $key or startswith($key + ".")))' "$fixture/stdout" >/dev/null ||
        fail "$name: the refusal did not name the object that kept changing"
    fi
  fi
  if [[ "$mode" == preflight-timeout || "$mode" == hang ]]; then
    jq -e '.failure == "deadline_exceeded"' "$fixture/stdout" >/dev/null || fail "$name: wrong timeout failure"
    if [[ "$mode" == preflight-timeout ]]; then
      [[ ! -e "$fixture/reads" && ! -e "$fixture/child.pid" ]] || fail "$name: plugin started after preflight deadline"
    else
      [[ -s "$fixture/child.pid" ]] || fail "$name: hanging plugin never started"
      local child_pid
      child_pid=$(cat "$fixture/child.pid")
      [[ "$child_pid" =~ ^[1-9][0-9]*$ ]] || fail "$name: invalid child PID"
      if kill -0 "$child_pid" 2>/dev/null; then fail "$name: deadline left a credential-plugin child running"; fi
    fi
  fi
  [[ $(cat "${scratch}/kubeconfig") == 'synthetic dedicated kubeconfig' ]] || fail "$name: kubeconfig changed"
  if grep -Eq 'PRIVATE-ERROR-CANARY|example.com|synthetic-ci|sha256:|data-product-controller|environment-canary' "$fixture/stdout" "$fixture/stderr"; then fail "$name: output was not sanitized"; fi
  printf 'PASS: %s\n' "$name"
}
# A failed public request must identify its numeric cause without exposing
# response data. Missing diagnostics make unrelated production evictions opaque.
run_case receipt-http-failure fail http-failure 15
jq -e '.httpStatus == 503 and .curlExit == 0' "${scratch}/receipt-http-failure/stdout" >/dev/null ||
  fail 'HTTP refusal lost its numeric status or curl exit'
[[ $(wc -l <"${scratch}/receipt-http-failure/urls") -eq 1 ]] || fail 'HTTP refusal was retried'
run_case receipt-connection-reset fail connection-reset 15
jq -e '.httpStatus == 0 and .curlExit == 56' "${scratch}/receipt-connection-reset/stdout" >/dev/null ||
  fail 'transport refusal lost its numeric curl exit'
[[ $(wc -l <"${scratch}/receipt-connection-reset/urls") -eq 2 ]] || fail 'a dropped connection was not retried exactly once'
# One dropped connection recovers, and the accepted receipt still counts it.
expected_urls=12
for mode in connection-refused-once empty-reply-once connection-reset-once; do
  run_case "$mode" pass "$mode"
  jq -e '.complete == true and .publicRetries == 1' "${scratch}/${mode}/stdout" >/dev/null ||
    fail "$mode: the accepted receipt lost the absorbed retry"
done
expected_urls=11
# The cluster-wide inventory reports counts only, and never reads an unread list as zero.
run_case inventory-clean pass ''
jq -e '.productsInventoried == 2 and .productsDeclaringUI == 0 and (has("productInventory") | not)' "${scratch}/inventory-clean/stdout" >/dev/null ||
  fail 'the receipt lost the product inventory'
run_case inventory-ui pass inventory-ui
jq -e '.productsInventoried == 2 and .productsDeclaringUI == 1' "${scratch}/inventory-ui/stdout" >/dev/null ||
  fail 'the inventory missed a product declaring the retired field'
for mode in inventory-forbidden inventory-partial inventory-empty inventory-malformed; do
  run_case "$mode" pass "$mode"
  jq -e '.complete == true and .productInventory == "unavailable" and (has("productsDeclaringUI") | not)' "${scratch}/${mode}/stdout" >/dev/null ||
    fail "$mode: an unread inventory was reported as a count"
done
# Reaching the shared deadline on this read must not refuse a settled rollout.
run_case inventory-deadline pass inventory-hang 8
jq -e '.complete == true and .productInventory == "unavailable"' "${scratch}/inventory-deadline/stdout" >/dev/null ||
  fail 'the inventory deadline refused a settled rollout'
# A stalled read gives up at its own short limit, well before the shared deadline.
inventory_started=$SECONDS
run_case inventory-hang pass inventory-hang 50
jq -e '.complete == true and .productInventory == "unavailable"' "${scratch}/inventory-hang/stdout" >/dev/null ||
  fail 'a stalled inventory refused a settled rollout'
((SECONDS - inventory_started < 40)) || fail 'a stalled inventory waited for the shared deadline'
for mode in inventory-clean inventory-ui inventory-forbidden; do
  if grep -Eq 'INVENTORY-NAME-CANARY|tenant-canary|PRIVATE-ERROR-CANARY' "${scratch}/${mode}/stdout"; then fail "$mode: the receipt exposed inventory values"; fi
done
# A retry never extends the deadline, and the refusal keeps the cause it could not retry.
run_case dropped-at-deadline fail dropped-at-deadline 3
# Which refusal fires first depends on the second the budget ran out in; both keep the cause.
jq -e '(.failure == "deadline_exceeded" or .failure == "public_contract_incomplete") and .curlExit == 56' "${scratch}/dropped-at-deadline/stdout" >/dev/null ||
  fail 'a retry the deadline forbids lost its curl exit'
[[ $(wc -l <"${scratch}/dropped-at-deadline/urls") -eq 1 ]] || fail 'a retry ran past the deadline'
run_case receipt-invalid-status fail invalid-http-status 15
jq -e 'has("httpStatus") == false and .curlExit == 0' "${scratch}/receipt-invalid-status/stdout" >/dev/null ||
  fail 'malformed curl output was accepted as an HTTP status'
# Initial snapshot refusals must identify a fixed role without exposing values.
mkdir "${scratch}/initial-unready"
cp "${scratch}/healthy/"*.json "${scratch}/initial-unready/"
jq '.status.observedGeneration=2 | .metadata.labels.private="PRIVATE-ERROR-CANARY"' \
  "${scratch}/healthy/apps.json" >"${scratch}/initial-unready/apps.json"
run_case initial-unready fail '' 10
[[ -s "${scratch}/initial-unready/rejections" ]] || fail 'initial-unready: predicate was not exercised'
jq -e '.failure=="deadline_exceeded" and .phase=="initial_readiness" and .role=="apps" and .reason=="predicate_not_ready"' \
  "${scratch}/initial-unready/stdout" >/dev/null || fail 'initial-unready: snapshot refusal has no safe diagnostic'

run_case initial-unreadable fail forbidden 10
jq -e '.failure=="read_incomplete" and .phase=="initial_readiness" and .role=="chart" and .reason=="read_incomplete"' \
  "${scratch}/initial-unreadable/stdout" >/dev/null || fail 'initial-unreadable: failed read has no safe diagnostic'
[[ $(grep -cx chart "${scratch}/initial-unreadable/reads") -eq 2 ]] || fail 'initial-unreadable: a failed read was not retried exactly once'

# One failed read recovers, and the accepted receipt still counts it.
run_case read-dropped-once pass read-dropped-once
jq -e '.complete == true and .readRetries == 1' "${scratch}/read-dropped-once/stdout" >/dev/null ||
  fail 'read-dropped-once: the accepted receipt lost the absorbed read'
# A read that keeps failing still refuses, naming the role it could not read.
run_case read-dropped-always fail read-dropped-always 10
jq -e '.failure=="read_incomplete" and .phase=="initial_readiness" and .role=="service-harbour" and .reason=="read_incomplete"' \
  "${scratch}/read-dropped-always/stdout" >/dev/null || fail 'read-dropped-always: a failed retry lost its diagnostic'
[[ $(grep -cx service-harbour "${scratch}/read-dropped-always/reads") -eq 2 ]] || fail 'read-dropped-always: a failed read was not retried exactly once'

mkdir "${scratch}/initial-malformed"
cp "${scratch}/healthy/"*.json "${scratch}/initial-malformed/"
jq '.spec.parentRefs="PRIVATE-ERROR-CANARY"' \
  "${scratch}/healthy/route-harbour.json" >"${scratch}/initial-malformed/route-harbour.json"
run_case initial-malformed fail '' 10
jq -e '.failure=="deadline_exceeded" and .phase=="initial_readiness" and .role=="snapshot" and .reason=="observation_invalid"' \
  "${scratch}/initial-malformed/stdout" >/dev/null || fail 'initial-malformed: invalid observation was not classified safely'

# A failed endpoint check is not attributable when every snapshot pair moves,
# including the final pair. Reach all three pairs before judging the refusal.
mkdir -p "${scratch}/final-moving-public/after"
cp "${scratch}/healthy/"*.json "${scratch}/final-moving-public/"
jq '.metadata.labels.settled="yes"' "${scratch}/healthy/apps.json" >"${scratch}/final-moving-public/after/apps.json"
run_case final-moving-public fail flap-public-failure 20
[[ $(cat "${scratch}/final-moving-public/apps.count") == 6 && $(wc -l <"${scratch}/final-moving-public/urls") -eq 3 ]] ||
  fail 'final-moving-public: did not reach three moving pairs with failed public checks'
jq -e '.failure == "rollout_changed" and .changed == ["apps.metadata.labels"] and (has("phase") or has("role") or has("reason")) == false' "${scratch}/final-moving-public/stdout" >/dev/null ||
  fail 'final-moving-public: final moving snapshot retained an unvalidated public failure'
for mode in dns-failure tls-failure tls-handshake-failure request-timeout; do
  run_case "$mode" fail "$mode" 15
  [[ $(wc -l <"${scratch}/${mode}/urls") -eq 1 ]] || fail "$mode: a non-retryable failure was retried"
done
# Spend the complete transfer budget, as a connected endpoint that never responds does.
# The refusal must retain its sanitized transport cause even at the shared deadline.
run_case transfer-timeout fail transfer-timeout 15

# mutate_case verifies that a dormant-profile mutation reaches snapshot rejection.
# mutate_case_impl copies the immutable healthy seed and checks one isolated rollout mutation.
mutate_case_impl() {
  local name=$1 key=$2 mutation=$3
  mkdir "${scratch}/$name"
  cp "${scratch}/healthy/"*.json "${scratch}/$name/"
  jq "$mutation" "${scratch}/healthy/$key.json" >"${scratch}/$name/$key.json"
  run_case "$name" fail '' "${4:-3}"
  [[ -s "${scratch}/$name/rejections" ]] || fail "$name: rejection did not reach snapshot validation"
}
fixture_pool_register mutate_case_impl
# mutate_case schedules a rollout mutation under the bounded worker and receipt contract.
mutate_case() { fixture_pool_submit mutate_case_impl "$@"; }

mutate_case publisher-unplanned-publisher deployment-publisher-review '.spec.template.spec.containers[0].env += [{name:"PUBLISHER_PREFLIGHT_ENABLED",value:"true"}]'
mutate_case publisher-pod-unplanned-publisher pods '.items[5].spec.containers[0].env += [{name:"PUBLISHER_PREFLIGHT_ENABLED",value:"true"}]'
mutate_case publisher-publisher-indirect deployment-publisher-review '.spec.template.spec.containers[0].env += [{name:"PUBLISHER_PREFLIGHT_ENABLED",value:"false",valueFrom:{configMapKeyRef:{name:"environment-canary",key:"flag"}}}]'
mutate_case publisher-publisher-malformed pods '.items[5].spec.containers[0].env += [{name:"PUBLISHER_PREFLIGHT_ENABLED",value:"FALSE"}]'
mutate_case publisher-deployment-indirect-environment deployment-publisher-review '.spec.template.spec.containers[0].envFrom=[{configMapRef:{name:"environment-canary"}}]'
mutate_case publisher-pod-indirect-environment pods '.items[4].spec.containers[0].envFrom=[{secretRef:{name:"environment-canary"}}]'
mutate_case publisher-pod-malformed-environment pods '.items[5].spec.containers[0].envFrom={}'
mutate_case publisher-replicaset-unplanned-publisher replicasets '.items |= map(if .spec.template.spec.containers[0].name=="publisher-review" then .spec.template.spec.containers[0].env += [{name:"PUBLISHER_PREFLIGHT_ENABLED",value:"true"}] else . end)'
mutate_case publisher-replicaset-indirect-environment replicasets '.items |= map(if .spec.template.spec.containers[0].name=="publisher-review" then .spec.template.spec.containers[0].envFrom=[{secretRef:{name:"environment-canary"}}] else . end)'

for publisher in true false; do
  name="declared-publisher-$publisher"
  mkdir "${scratch}/$name"
  cp "${scratch}/healthy/"*.json "${scratch}/$name/"
  jq --arg state "$publisher" '.[0].env += [{name:"PUBLISHER_PREFLIGHT_ENABLED",value:$state}]' "${scratch}/healthy/expected-publisher.json" >"${scratch}/$name/expected-publisher.json"
  jq --arg state "$publisher" '.spec.template.spec.containers[0].env += [{name:"PUBLISHER_PREFLIGHT_ENABLED",value:$state}]' "${scratch}/healthy/deployment-publisher-review.json" >"${scratch}/$name/deployment-publisher-review.json"
  jq --arg state "$publisher" '.items |= map(if .spec.containers[0].name=="publisher-review" then .spec.containers[0].env += [{name:"PUBLISHER_PREFLIGHT_ENABLED",value:$state}] else . end)' "${scratch}/healthy/pods.json" >"${scratch}/$name/pods.json"
  jq --arg state "$publisher" '.items |= map(if .spec.template.spec.containers[0].name=="publisher-review" then .spec.template.spec.containers[0].env += [{name:"PUBLISHER_PREFLIGHT_ENABLED",value:$state}] else . end)' "${scratch}/healthy/replicasets.json" >"${scratch}/$name/replicasets.json"
  run_case "$name" pass
done

# Published routes and assets must match the declared opt-in.
for mode in publisher-unavailable publisher-missing-style publisher-wrong-script publisher-wrong-report-library publisher-wrong-page publisher-weak-csp publisher-cacheable publisher-wrong-root missing-csp weak-csp duplicate-csp wrong-asset; do
  mkdir "${scratch}/$mode"
  cp "${scratch}/declared-publisher-true/"*.json "${scratch}/$mode/"
  run_case "$mode" fail "$mode" 15
done
run_case publisher-unplanned-route fail publisher-unplanned-route 15

# publisher_mutation starts from the separately declared enabled profile.
# publisher_mutation_impl checks one private copy of the publisher identity fixtures.
publisher_mutation_impl() {
  local name=$1 key=$2 mutation=$3
  mkdir "${scratch}/$name"
  cp "${scratch}/declared-publisher-true/"*.json "${scratch}/$name/"
  jq "$mutation" "${scratch}/declared-publisher-true/$key.json" >"${scratch}/$name/$key.json"
  run_case "$name" fail '' 3
  [[ -s "${scratch}/$name/rejections" ]] || fail "$name: rejection did not reach snapshot validation"
}
fixture_pool_register publisher_mutation_impl
# publisher_mutation schedules a publisher identity mutation without sharing writable fixtures.
publisher_mutation() { fixture_pool_submit publisher_mutation_impl "$@"; }
publisher_mutation declared-publisher-missing-deployment deployment-publisher-review '.spec.template.spec.containers[0].env |= map(select(.name!="PUBLISHER_PREFLIGHT_ENABLED"))'
publisher_mutation declared-publisher-missing-pod pods '.items[4].spec.containers[0].env |= map(select(.name!="PUBLISHER_PREFLIGHT_ENABLED"))'
publisher_mutation declared-publisher-missing-replicaset replicasets '.items |= map(if .spec.template.spec.containers[0].name=="publisher-review" then .spec.template.spec.containers[0].env |= map(select(.name!="PUBLISHER_PREFLIGHT_ENABLED")) else . end)'
publisher_mutation declared-publisher-disabled-pod pods '.items[5].spec.containers[0].env[0].value="false"'
publisher_mutation declared-publisher-duplicate deployment-publisher-review '.spec.template.spec.containers[0].env += [.spec.template.spec.containers[0].env[0]]'

invalid_case=0
for mutation in '.[0].env=[{name:"PUBLISHER_PREFLIGHT_ENABLED",value:true}]' '.[0].env=[{name:"PUBLISHER_PREFLIGHT_ENABLED",value:"true"},{name:"PUBLISHER_PREFLIGHT_ENABLED",value:"true"}]' '.[0].env=[{name:"PUBLISHER_PREFLIGHT_ENABLED",valueFrom:{secretKeyRef:{name:"environment-canary",key:"flag"}}}]' '. += [.[0]]' '.[0].env=[{name:"PUBLISHER_PREFLIGHT_ENABLED",value:"TRUE"}]' '.[0].envFrom=[{configMapRef:{name:"environment-canary"}}]'; do
  ((invalid_case += 1))
  name="invalid-publisher-declaration-$invalid_case"
  mkdir "${scratch}/$name"
  cp "${scratch}/healthy/"*.json "${scratch}/$name/"
  jq "$mutation" "${scratch}/healthy/expected-publisher.json" >"${scratch}/$name/expected-publisher.json"
  run_case "$name" fail
  jq -e '.failure=="invalid_expected_state"' "${scratch}/$name/stdout" >/dev/null || fail 'invalid declaration was not refused in preflight'
  [[ ! -e "${scratch}/$name/reads" ]] || fail 'invalid publisher declaration reached the cluster'
done

# Caller digests and the live release must agree with the checked-out image and chart.
invalid_case=0
for mutation in 'expected-image.json|.digest="sha256:"+("0"*64)' 'expected-image.json|.repository="ghcr.io/devantler-tech/data-product-controller"' 'expected-image.json|.tag="latest"' 'expected-chart.json|.digest="sha256:"+("0"*64)' 'expected-chart.json|.url="oci://ghcr.io/devantler-tech/charts/data-product-controller"'; do
  ((invalid_case += 1))
  name="invalid-release-declaration-$invalid_case"
  mkdir "${scratch}/$name"
  cp "${scratch}/healthy/"*.json "${scratch}/$name/"
  jq "${mutation#*|}" "${scratch}/healthy/${mutation%%|*}" >"${scratch}/$name/${mutation%%|*}"
  run_case "$name" fail
  jq -e '.failure=="invalid_expected_state"' "${scratch}/$name/stdout" >/dev/null || fail "$name: unbound release was not refused in preflight"
  [[ ! -e "${scratch}/$name/reads" ]] || fail "$name: unbound release reached the cluster"
done
# Retired settings are refused on every serving object, in each workload.
for setting in UI_CONTRACT_ENABLED UI_APPEARANCE_ENABLED UI_HOST_ORIGINS REGISTRY_UI_ENABLED; do
  for key in deployment-controller deployment-harbour deployment-publisher-review; do
    mutate_case "retired-$setting-$key" "$key" ".spec.template.spec.containers[0].env += [{name:\"$setting\",value:\"true\"}]"
  done
  mutate_case "retired-$setting-pod" pods ".items[0].spec.containers[0].env += [{name:\"$setting\",value:\"true\"}]"
  mutate_case "retired-$setting-replicaset" replicasets ".items[0].spec.template.spec.containers[0].env += [{name:\"$setting\",value:\"true\"}]"
done
mutate_case helm-reinstalls-on-rename helm 'del(.spec.upgrade.chartNameChangeStrategy)'
mutate_case helm-explicit-reinstall helm '.spec.upgrade.chartNameChangeStrategy="Reinstall"'
mutate_case helm-other-tag helm '.spec.values.image.tag="1.20.1"'
mutate_case harbour-indirect-environment deployment-harbour '.spec.template.spec.containers[0].envFrom=[{configMapRef:{name:"environment-canary"}}]'

mutate_case unexpected-live-ksail-activation helm '.spec.values.demoProduct.ksailReleases.enabled=true'

mkdir "${scratch}/withdrawn-release-still-registered"
cp "${scratch}/healthy/"*.json "${scratch}/withdrawn-release-still-registered/"
jq '.metadata.name="ksail-releases"' "${scratch}/healthy/product.json" >"${scratch}/withdrawn-release-still-registered/release-product.json"
run_case withdrawn-release-still-registered fail '' 3
[[ -s "${scratch}/withdrawn-release-still-registered/rejections" ]] || fail 'withdrawn product did not reach snapshot rejection'
for mode in release-forbidden release-partial-forbidden release-null release-malformed; do
  run_case "$mode" fail "$mode" 3
done
mkdir -p "${scratch}/withdrawal-settles/after" "${scratch}/withdrawal-settles/after2"
cp "${scratch}/healthy/"*.json "${scratch}/withdrawal-settles/"
jq '.metadata.name="ksail-releases"' "${scratch}/healthy/product.json" >"${scratch}/withdrawal-settles/after/release-product.json"
printf 'null\n' >"${scratch}/withdrawal-settles/after2/release-product.json"
expected_urls=22
run_case withdrawal-settles pass slow-public 45
expected_urls=11
jq -e '.retaken==1 and (.settled|index("release-product")!=null)' "${scratch}/withdrawal-settles/stdout" >/dev/null ||
  fail 'withdrawal changed during public checks without a retaken snapshot'

mkdir "${scratch}/declared-activation-but-live-dormant"
cp "${scratch}/healthy/"*.json "${scratch}/declared-activation-but-live-dormant/"
printf '[true,true,false]\n' >"${scratch}/declared-activation-but-live-dormant/expected-flags.json"
run_case declared-activation-but-live-dormant fail
[[ -s "${scratch}/declared-activation-but-live-dormant/rejections" ]] || fail 'declared activation mismatch did not reach snapshot validation'
mutate_case probe-supplemental-network probe-policy '.specs=[(.spec | .egress=[{toEntities:["world"]}])]'
mutate_case probe-object-network probe-policy '.specs={}'
mutate_case probe-string-network probe-policy '.specs=""'
mutate_case probe-boolean-network probe-policy '.specs=false'
mutate_case helm-missing-connector-flag helm 'del(.spec.values.connectorReadiness.enabled)'
mutate_case helm-missing-contract-flag helm 'del(.spec.values.contractReadiness.enabled)'
mutate_case helm-missing-probe-flag helm 'del(.spec.values.contractProbe.enabled)'
run_case healthy pass
mutate_case dormant-wrong-output-url product '.spec.outputs[0].url="https://other.example.com/api/observations"' 15
mutate_case dormant-missing-output-url product 'del(.spec.outputs[0].url)'
mkdir "${scratch}/active"
jq --arg child "$child" '
  .helm.spec.values.connectorReadiness.enabled=true | .helm.spec.values.contractReadiness.enabled=true |
  ."deployment-controller".spec.template.spec.containers[0].env[].value="true" |
  .product.spec.connector={adapter:"deployment/v1",resourceRef:{apiVersion:"apps/v1",kind:"Deployment",name:"data-product-controller-harbour"}} |
  .product.spec.contractChecks=[{output:"observations",resourceRef:{apiVersion:"apps/v1",kind:"Deployment",name:"data-product-controller-contract-probe"}}] |
  .product.spec.outputs=[{name:"observations",url:"https://harbour-data.example.com/api/observations",contractUrl:"https://harbour-data.example.com/openapi.json"}] |
  .product.status.conditions=[{type:"Ready",status:"True",observedGeneration:3,reason:"DependenciesReady"},
    {type:"ConnectorReady",status:"True",observedGeneration:3,reason:"ConnectorReady"},
    {type:"ContractsReady",status:"True",observedGeneration:3,reason:"ContractsReady"}] |
  ."deployment-probe".metadata.annotations={"deployment.kubernetes.io/revision":"8"} |
  ."deployment-probe".spec.replicas=2 | ."deployment-probe".status={observedGeneration:3,replicas:2,updatedReplicas:2,readyReplicas:2,availableReplicas:2} |
  ."deployment-probe".spec.template.spec.containers[0].env[1].value="true" |
  ."deployment-probe" as $probe |
  {apiVersion:"apps/v1",kind:"ReplicaSet",metadata:{name:($probe.metadata.name+"-new"),namespace:"data-product-controller",uid:($probe.metadata.name+"-new-uid"),generation:3,
    annotations:$probe.metadata.annotations,ownerReferences:[{apiVersion:"apps/v1",kind:"Deployment",name:$probe.metadata.name,uid:$probe.metadata.uid,controller:true}]},
    spec:{replicas:2,template:$probe.spec.template},status:{observedGeneration:3,replicas:2,readyReplicas:2,availableReplicas:2}} as $set |
  .replicasets.items += [$set] |
  .pods.items += [range(1;3) as $ordinal | {apiVersion:"v1",kind:"Pod",metadata:{name:($probe.metadata.name+"-pod-"+($ordinal|tostring)),namespace:"data-product-controller",
    uid:($probe.metadata.name+"-pod-"+($ordinal|tostring)+"-uid"),labels:$probe.spec.template.metadata.labels,annotations:$probe.spec.template.metadata.annotations,
    ownerReferences:[{apiVersion:"apps/v1",kind:"ReplicaSet",name:$set.metadata.name,uid:$set.metadata.uid,controller:true}]},
    spec:$probe.spec.template.spec,status:{phase:"Running",conditions:[{type:"Ready",status:"True"}],containerStatuses:[{name:"contract-probe",ready:true,imageID:("containerd://"+$child),state:{running:{startedAt:"2026-10-03T00:00:00Z"}}}]}}]
' "${scratch}/all.json" >"${scratch}/active.json"
while IFS= read -r key; do jq --arg key "$key" '.[$key]' "${scratch}/active.json" >"${scratch}/active/${key}.json"; done < <(jq -r 'keys[]' "${scratch}/active.json")
printf '[true,true,false]\n' >"${scratch}/active/expected-flags.json"
printf 'false\n' >"${scratch}/active/expected-ksail.json"
cp "${scratch}/healthy/expected-publisher.json" "${scratch}/healthy/expected-image.json" "${scratch}/healthy/expected-chart.json" "${scratch}/active/"
cp "${scratch}/healthy/expected-probe.json" "${scratch}/active/expected-probe.json"
run_case active pass

# The probe may consume a diagnostic release independently. Both its declared
# image and actual runtime must match; the controller retains its own pins.
mkdir "${scratch}/independent-probe"
cp "${scratch}/active/"*.json "${scratch}/independent-probe/"
probe_image="ghcr.io/devantler-tech/khoist:9.0.5@sha256:$(printf 'e%.0s' {1..64})"
probe_runtime="sha256:$(printf 'f%.0s' {1..64})"
printf '%s\n' "$probe_runtime" >"${scratch}/independent-probe/expected-probe-runtime"
jq --arg image "$probe_image" '.[0].image=$image' "${scratch}/active/expected-probe.json" >"${scratch}/independent-probe/expected-probe.json"
jq --arg image "$probe_image" '.spec.template.spec.containers[0].image=$image' "${scratch}/active/deployment-probe.json" >"${scratch}/independent-probe/deployment-probe.json"
jq --arg image "$probe_image" '(.items[] | select(.spec.template.spec.containers[0].name=="contract-probe") | .spec.template.spec.containers[0].image)=$image' "${scratch}/active/replicasets.json" >"${scratch}/independent-probe/replicasets.json"
jq --arg image "$probe_image" --arg runtime "$probe_runtime" '(.items[] | select(.spec.containers[0].name=="contract-probe")) |=
  (.spec.containers[0].image=$image | .status.containerStatuses[0].imageID=("containerd://"+$runtime))' "${scratch}/active/pods.json" >"${scratch}/independent-probe/pods.json"
run_case independent-probe pass
# independent_probe_mutation checks a real exact-image/runtime refusal in the
# active two-replica snapshot, without replacing any public or ownership check.
independent_probe_mutation() {
  local name=$1 key=$2 mutation=$3
  mkdir "${scratch}/$name"
  cp "${scratch}/independent-probe/"*.json "${scratch}/$name/"
  cp "${scratch}/independent-probe/expected-probe-runtime" "${scratch}/$name/"
  jq "$mutation" "${scratch}/independent-probe/$key.json" >"${scratch}/$name/$key.json"
  run_case "$name" fail '' 3
  [[ -s "${scratch}/$name/rejections" ]] || fail "$name: rejection did not reach snapshot validation"
}
independent_probe_mutation probe-wrong-deployment-image deployment-probe '.spec.template.spec.containers[0].image="ghcr.io/devantler-tech/khoist:9.0.4@sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"'
independent_probe_mutation probe-wrong-replicaset-image replicasets '(.items[] | select(.spec.template.spec.containers[0].name=="contract-probe") | .spec.template.spec.containers[0].image)="ghcr.io/devantler-tech/khoist@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"'
independent_probe_mutation probe-wrong-pod-image pods '(.items[] | select(.spec.containers[0].name=="contract-probe") | .spec.containers[0].image)="ghcr.io/devantler-tech/data-product-controller@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"'
independent_probe_mutation probe-legacy-runtime pods '(.items[] | select(.spec.containers[0].name=="contract-probe") | .status.containerStatuses[0].imageID)="containerd://sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"'
independent_probe_mutation probe-wrong-runtime-repository pods '(.items[] | select(.spec.containers[0].name=="contract-probe") | .status.containerStatuses[0].imageID)="ghcr.io/devantler-tech/data-product-controller@sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"'
independent_probe_mutation controller-cannot-use-probe-runtime pods '.items[0].status.containerStatuses[0].imageID="containerd://sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"'
independent_probe_mutation controller-cannot-use-probe-image deployment-controller '.spec.template.spec.containers[0].image="ghcr.io/devantler-tech/khoist:9.0.5@sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"'
independent_probe_mutation probe-live-declaration-mismatch expected-probe '.[0].image="ghcr.io/devantler-tech/data-product-controller@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"'
# A declared public product must exist at its exact current generation and
# actual workload setting; source data is never fetched by this cluster receipt.
mkdir "${scratch}/ksail"
cp "${scratch}/active/"*.json "${scratch}/ksail/"
printf 'true\n' >"${scratch}/ksail/expected-ksail.json"
jq '.spec.values.demoProduct.ksailReleases.enabled=true' "${scratch}/active/helm.json" >"${scratch}/ksail/helm.json"
jq '.metadata.name="ksail-releases" | .metadata.uid="release-product-uid" |
  .spec={id:"https://github.com/devantler-tech/ksail/releases",name:"KSail releases",version:"v1.0.0",owner:{name:"devantler-tech",url:"https://github.com/devantler-tech/ksail"},
    outputs:[{name:"releases",protocol:"OpenAPI",url:"https://api.github.com/repos/devantler-tech/ksail/releases",contractUrl:"https://harbour-data.example.com/ksail-releases/openapi.json",mediaType:"application/json"}]}
' "${scratch}/active/product.json" >"${scratch}/ksail/release-product.json"
expected_urls=13
run_case ksail pass
# ksail_mutation requires a real snapshot-predicate rejection for each control.
# ksail_mutation_impl checks one private copy of the KSail release-product fixtures.
ksail_mutation_impl() {
  local name=$1 key=$2 mutation=$3
  mkdir "${scratch}/$name"
  cp "${scratch}/ksail/"*.json "${scratch}/$name/"
  jq "$mutation" "${scratch}/ksail/$key.json" >"${scratch}/$name/$key.json"
  run_case "$name" fail '' 3
  [[ -s "${scratch}/$name/rejections" ]] || fail "$name: rejection did not reach snapshot validation"
}
fixture_pool_register ksail_mutation_impl
# ksail_mutation schedules an independent release-product mutation and completion receipt.
ksail_mutation() { fixture_pool_submit ksail_mutation_impl "$@"; }
ksail_mutation ksail-disabled-live helm '.spec.values.demoProduct.ksailReleases.enabled=false'
ksail_mutation ksail-stale-product release-product '.status.observedGeneration=2'
ksail_mutation ksail-failed-product release-product '.status.conditions[0].status="False"'
ksail_mutation ksail-wrong-source release-product '.spec.outputs[0].url="https://other.example.com/releases"'
ksail_mutation ksail-wrong-contract release-product '.spec.outputs[0].contractUrl="https://other.example.com/openapi.json"'
ksail_mutation ksail-declares-ui release-product '.spec.ui={url:"https://harbour-data.example.com/ksail-releases/ui"}'
ksail_mutation ksail-stale-workload-flag deployment-harbour '.spec.template.spec.containers[0].env=[{name:"KSAIL_RELEASES_ENABLED",value:"true"}]'
ksail_mutation ksail-stale-pod-flag pods '.items[2].spec.containers[0].env=[{name:"KSAIL_RELEASES_ENABLED",value:"true"}]'
mkdir "${scratch}/ksail-missing-product"
cp "${scratch}/ksail/"*.json "${scratch}/ksail-missing-product/"
printf 'null\n' >"${scratch}/ksail-missing-product/release-product.json"
run_case ksail-missing-product fail '' 3
jq -e '.failure=="read_incomplete"' "${scratch}/ksail-missing-product/stdout" >/dev/null || fail 'enabled missing product was accepted as absence'
for mode in wrong-release-ui weak-release-csp wrong-release-api; do
  mkdir "${scratch}/$mode"
  cp "${scratch}/ksail/"*.json "${scratch}/$mode/"
  run_case "$mode" fail "$mode" 15
  jq -e '.failure=="public_contract_incomplete"' "${scratch}/$mode/stdout" >/dev/null || fail "$mode: public source profile was not refused"
done
expected_urls=11
mkdir -p "${scratch}/active-settled-label/after"
cp "${scratch}/active/"*.json "${scratch}/active-settled-label/"
jq '.metadata.labels.settled="yes"' "${scratch}/active/apps.json" >"${scratch}/active-settled-label/after/apps.json"
expected_urls=22
run_case active-settled-label pass
expected_urls=11
jq -e '.retaken == 1 and .settled == ["apps.metadata.labels"]' "${scratch}/active-settled-label/stdout" >/dev/null ||
  fail 'active-settled-label: the active report did not name the change that settled'
# active_mutation verifies that an active-profile mutation reaches snapshot rejection.
# active_mutation_impl checks one private copy of the active declaration and workload fixtures.
active_mutation_impl() {
  local name=$1 key=$2 mutation=$3
  mkdir "${scratch}/$name"
  cp "${scratch}/active/"*.json "${scratch}/$name/"
  jq "$mutation" "${scratch}/active/$key.json" >"${scratch}/$name/$key.json"
  run_case "$name" fail '' "${4:-3}"
  [[ -s "${scratch}/$name/rejections" ]] || fail "$name: rejection did not reach snapshot validation"
}
fixture_pool_register active_mutation_impl
# active_mutation schedules an active-state mutation within the same strict worker bound.
active_mutation() { fixture_pool_submit active_mutation_impl "$@"; }
active_mutation active-pod-missing-metrics pods '(.items[] | select(.metadata.labels["app.kubernetes.io/component"]=="contract-probe") | .metadata.annotations) |= del(.["coroot.com/scrape-metrics"])'
active_mutation active-pod-wrong-metrics-path pods '(.items[] | select(.metadata.labels["app.kubernetes.io/component"]=="contract-probe") | .metadata.annotations["coroot.com/metrics-path"])="/readyz"'
active_mutation active-replicaset-missing-metrics replicasets '(.items[] | select(.spec.template.metadata.labels["app.kubernetes.io/component"]=="contract-probe") | .spec.template.metadata.annotations) |= del(.["coroot.com/scrape-metrics"])'
active_mutation active-stale-contract-condition product '.status.conditions[2].observedGeneration=2'
active_mutation active-false-connector product '.status.conditions[1].status="False"'
active_mutation active-missing-contract-condition product '.status.conditions |= map(select(.type!="ContractsReady"))'
active_mutation active-duplicate-condition product '.status.conditions += [.status.conditions[2]]'
active_mutation active-wrong-reason product '.status.conditions[2].reason="DependenciesReady"'
active_mutation active-wrong-connector product '.spec.connector.resourceRef.name="other-workload"'
active_mutation active-foreign-scope product '.spec.connector.resourceRef.namespace="other"'
active_mutation active-wrong-output product '.spec.contractChecks[0].output="other"'
active_mutation active-wrong-url product '.spec.outputs[0].contractUrl="https://other.example.com/openapi.json"'
active_mutation active-wrong-output-url product '.spec.outputs[0].url="https://other.example.com/api/observations"' 15
active_mutation active-missing-output-url product 'del(.spec.outputs[0].url)'
active_mutation active-wrong-probe product '.spec.contractChecks[0].resourceRef.name="data-product-controller-harbour"'
active_mutation active-disabled-controller deployment-controller '.spec.template.spec.containers[0].env[1].value="false"'
active_mutation active-missing-connector-flag helm 'del(.spec.values.connectorReadiness.enabled)'
active_mutation active-missing-contract-flag helm 'del(.spec.values.contractReadiness.enabled)'
active_mutation active-missing-probe-flag helm 'del(.spec.values.contractProbe.enabled)'
active_mutation active-disabled-helm helm '.spec.values.contractReadiness.enabled=false'
active_mutation active-disabled-probe deployment-probe '.spec.template.spec.containers[0].env[1].value="false"'
active_mutation active-zero-probe deployment-probe '.spec.replicas=0'
active_mutation active-partial-probe deployment-probe '.status.readyReplicas=1'
active_mutation active-old-probe-rs replicasets '.items[-1].metadata.annotations["deployment.kubernetes.io/revision"]="7"'
active_mutation active-unready-probe-pod pods '.items[-1].status.containerStatuses[0].ready=false'
active_mutation active-foreign-probe-image pods '.items[-1].status.containerStatuses[0].imageID="containerd://sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"'
active_mutation active-probe-pod-token pods '.items[-1].spec.automountServiceAccountToken=true'
active_mutation active-probe-pod-init pods '.items[-1].spec.initContainers=[{name:"unexpected",image:"example.invalid/other:latest"}]'
active_mutation active-probe-pod-label pods 'del(.items[-1].metadata.labels["app.kubernetes.io/component"])'
active_mutation active-probe-pod-target pods '.items[-1].spec.containers[0].env[0].value="https://other.example.com/openapi.json"'
active_mutation active-probe-pod-liveness pods '.items[-1].spec.containers[0].readinessProbe.httpGet.path="/healthz"'
active_mutation active-probe-pod-env-from pods '.items[-1].spec.containers[0].envFrom=[{configMapRef:{name:"unexpected"}}]'
active_mutation active-retained-exemption deployment-probe '.metadata.labels["platform.devantler.tech/replica-floor"]="exempt"'
active_mutation declared-dormancy-but-live-active expected-flags '.=[false,false,false]'
for state in '[true,false,false]' '[true,true,true]' '[null,null,null]' '["true",true,false]' '[]' '[true,true,false]\n[true,true,false]'; do
  name="invalid-expected-state-${RANDOM}"
  mkdir "${scratch}/$name"
  cp "${scratch}/healthy/"*.json "${scratch}/$name/"
  printf '%b\n' "$state" >"${scratch}/$name/expected-flags.json"
  run_case "$name" fail
  jq -e '.failure=="invalid_expected_state"' "${scratch}/$name/stdout" >/dev/null || fail "$name: wrong configuration failure"
  [[ ! -e "${scratch}/$name/reads" ]] || fail "$name: invalid expectation reached the cluster"
done
if [[ ${TEST_READINESS_ONLY:-false} == true ]]; then exit 0; fi
mutate_case probe-extra-init deployment-probe '.spec.template.spec.initContainers=[{name:"unexpected",image:"example.invalid/other:latest",envFrom:[{secretRef:{name:"environment-canary"}}]}]'
mutate_case helm-unplanned-observation helm '.spec.values.connectorReadiness.enabled=true'
mutate_case helm-unplanned-contracts helm '.spec.values.contractReadiness.enabled=true'
mutate_case helm-unplanned-chart-probe helm '.spec.values.contractProbe.enabled=true'
mutate_case controller-unplanned-observation deployment-controller '.spec.template.spec.containers[0].env[0].value="true"'
mutate_case dormant-running-probe deployment-probe '.spec.replicas=1'
mutate_case dormant-stale-probe deployment-probe '.status.observedGeneration=2'
mutate_case dormant-probe-pod pods '.items += [(.items[0] | .metadata.ownerReferences[0].name="dormant-probe-rs" | .metadata.ownerReferences[0].uid="dormant-probe-rs-uid" | .metadata.name="unexpected-probe-pod" | .metadata.uid="unexpected-probe-pod-uid")]'
mutate_case probe-token deployment-probe '.spec.template.spec.automountServiceAccountToken=true'
mutate_case account-token probe-account '.automountServiceAccountToken=true'
mutate_case probe-wrong-target deployment-probe '.spec.template.spec.containers[0].env[0].value="https://other.example.com/openapi.json"'
mutate_case probe-target-reference deployment-probe '.spec.template.spec.containers[0].env[0] |= (del(.value) | .valueFrom={secretKeyRef:{name:"environment-canary",key:"url"}})'
mutate_case probe-health-as-ready deployment-probe '.spec.template.spec.containers[0].readinessProbe.httpGet.path="/healthz"'
mutate_case observer-list observer-role '.rules[0].verbs += ["list"]'
mutate_case observer-secret observer-role '.rules += [{apiGroups:[""],resources:["secrets"],verbs:["get"]}]'
mutate_case observer-unbounded observer-role 'del(.rules[0].resourceNames)'
mutate_case observer-wrong-account observer-binding '.subjects[0].name="other-controller"'
mutate_case probe-broad-network probe-policy '.spec.egress += [{toEntities:["world"]}]'
mutate_case probe-missing-metrics-discovery deployment-probe 'del(.spec.template.metadata.annotations["coroot.com/scrape-metrics"])'
mutate_case probe-wrong-metrics-path deployment-probe '.spec.template.metadata.annotations["coroot.com/metrics-path"]="/readyz"'
mutate_case probe-broad-metrics-source probe-policy 'del(.spec.ingress[0].fromEndpoints[0].matchLabels["app.kubernetes.io/component"])'
mutate_case probe-wrong-metrics-port probe-policy '.spec.ingress[0].toPorts[0].ports[0].port="8082"'
mutate_case probe-extra-metrics-ingress probe-policy '.spec.ingress += [{fromEntities:["world"]}]'
mutate_case dormant-product-reference product '.spec.contractChecks=[{output:"observations",resourceRef:{apiVersion:"apps/v1",kind:"Deployment",name:"data-product-controller-contract-probe"}}]'
run_case partial-observer fail partial-observer
# A healthy snapshot pair also parses the declared feature state. Allow host scheduling
# overhead here; the one-second expiry controls below still enforce the total deadline.
run_case healthy-acceptance-bound pass "" 10
run_case orphan-plugin pass orphan
while IFS= read -r pid; do
  if kill -0 "$pid" 2>/dev/null; then fail 'successful read left a credential-plugin child running'; fi
done <"${scratch}/orphan-plugin/orphan.pids"
mkdir "${scratch}/generic-lists"
cp "${scratch}/healthy/"*.json "${scratch}/generic-lists/"
for key in pods replicasets endpoints; do jq '.kind="List" | .apiVersion="v1"' "${scratch}/healthy/$key.json" >"${scratch}/generic-lists/$key.json"; done
run_case generic-lists pass
mkdir -p "${scratch}/warming/after"
cp "${scratch}/healthy/"*.json "${scratch}/warming/"
cp "${scratch}/healthy/apps.json" "${scratch}/warming/after/apps.json"
jq '.status.conditions[0].status="False"' "${scratch}/healthy/apps.json" >"${scratch}/warming/apps.json"
run_case warming pass
mkdir "${scratch}/scaled-down-old-rs"
cp "${scratch}/healthy/"*.json "${scratch}/scaled-down-old-rs/"
jq '.items += [(.items[2] | .metadata.name="retired-rs" | .metadata.uid="retired-rs-uid" | .metadata.annotations["deployment.kubernetes.io/revision"]="6" | .spec.replicas=0 | .status.replicas=0 | .status.readyReplicas=0 | .status.availableReplicas=0)]' \
  "${scratch}/healthy/replicasets.json" >"${scratch}/scaled-down-old-rs/replicasets.json"
run_case scaled-down-old-rs pass
mkdir "${scratch}/unrelated-workload"
cp "${scratch}/healthy/"*.json "${scratch}/unrelated-workload/"
jq '.items += [(.items[5] | .metadata.name="unrelated" | .metadata.uid="unrelated-uid" | .metadata.ownerReferences[0].name="unrelated-rs" | .metadata.ownerReferences[0].uid="unrelated-rs-uid")]' \
  "${scratch}/healthy/pods.json" >"${scratch}/unrelated-workload/pods.json"
run_case unrelated-workload pass
mutate_case incomplete-page pods '.metadata.continue="next-page"' 10
mutate_case malformed-unrelated-inventory pods '.items += [(.items[5] | .metadata.namespace="foreign" | .metadata.uid=null | .metadata.ownerReferences[0].name="unrelated-rs" | .metadata.ownerReferences[0].uid="unrelated-rs-uid")]' 10
mutate_case wrong-publication apps '.status.lastAppliedRevision="latest@sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"'
mutate_case stale-apps apps '.status.observedGeneration=2'
mutate_case duplicate-ready apps '.status.conditions += [.status.conditions[0]]'
mutate_case unverified-chart chart '.status.conditions[1].status="False"'
mutate_case wrong-chart-revision chart '.status.artifact.revision="sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"'
mutate_case no-file-digest chart 'del(.status.artifact.digest)'
mutate_case helm-wrong-oci helm '.status.lastAttemptedRevisionDigest="sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"'
mutate_case helm-stale-history helm '.status.history[0].ociDigest="sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"'
mutate_case helm-wrong-profile helm '.spec.values.controller.replicas=1'
mutate_case partial-deployment deployment-publisher-review '.status.availableReplicas=1'
mutate_case stale-deployment deployment-controller '.status.observedGeneration=2'
mutate_case zero-replicas deployment-harbour '.spec.replicas=0 | .status.replicas=0 | .status.readyReplicas=0 | .status.updatedReplicas=0 | .status.availableReplicas=0'
mutate_case wrong-container deployment-publisher-review '.spec.template.spec.containers[0].name="other"'
mutate_case partial-rs replicasets '.items[2].status.readyReplicas=1'
mutate_case old-rs replicasets '.items[2].metadata.annotations["deployment.kubernetes.io/revision"]="6"'
mutate_case foreign-rs replicasets '.items[2].metadata.ownerReferences[0].uid="foreign-uid"'
mutate_case duplicate-rs replicasets '.items += [.items[2]]'
mutate_case partial-pods pods '.items |= .[:-1]'
mutate_case old-pod pods '.items[5].metadata.ownerReferences[0].name="old-rs" | .items[5].metadata.ownerReferences[0].uid="old-rs-uid"'
mutate_case foreign-pod pods '.items[5].metadata.ownerReferences[0].controller=false'
mutate_case terminating-pod pods '.items[5].metadata.deletionTimestamp="2026-10-03T00:00:00Z"'
mutate_case wrong-runtime pods '.items[5].status.containerStatuses[0].imageID="containerd://sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"'
mutate_case foreign-runtime-repo pods '.items[5].status.containerStatuses[0].imageID |= sub("devantler-tech";"foreign")'
mutate_case not-running pods '.items[5].status.containerStatuses[0].state={waiting:{reason:"Starting"}}'
mutate_case generic-list-wrong-kind pods '.kind="List" | .items[0].kind="Secret"'
mutate_case route-stale route-publisher-review '.status.parents[0].conditions[0].observedGeneration=2'
mutate_case route-duplicate-parent route-publisher-review '.status.parents += [.status.parents[0]]'
mutate_case route-duplicate-condition route-harbour '.status.parents[0].conditions += [.status.parents[0].conditions[0]]'
mutate_case registry-bypass route-registry '.spec.rules[0].backendRefs=[{name:"data-product-controller",port:8082}]'
mutate_case wrong-gateway-listener gateway '.spec.listeners[0].protocol="HTTP"'
mutate_case gateway-not-programmed gateway '.status.conditions[1].status="False"'
for mode in forbidden empty multi http-failure redirect wrong-body unsafe-storage; do run_case "$mode" fail "$mode"; done
for key in apps chart helm deployment-publisher-review route-publisher-review gateway pods root product service-publisher-review; do
  name="changed-$key"
  mkdir -p "${scratch}/$name/after"
  cp "${scratch}/healthy/"*.json "${scratch}/$name/"
  jq 'if .kind == "PodList" then .items[5].metadata.uid="recreated-uid" else .metadata.uid="recreated-uid" end' \
    "${scratch}/healthy/$key.json" >"${scratch}/$name/after/$key.json"
  expected_urls=33
  run_case "$name" fail flap 60
  expected_urls=11
done
# A change that happens once and then holds is an ordinary reconcile, not a
# moving rollout: the pair is retaken, public checks included, and accepted.
mkdir -p "${scratch}/settled-label/after"
cp "${scratch}/healthy/"*.json "${scratch}/settled-label/"
jq '.metadata.labels.settled="yes"' "${scratch}/healthy/apps.json" >"${scratch}/settled-label/after/apps.json"
expected_urls=22
run_case settled-label pass
expected_urls=11
# The accepted report says a pair was retaken and names what had moved, so the
# cause of a settled difference can be read from a deploy that passed (#4545).
jq -e '.retaken == 1 and .settled == ["apps.metadata.labels"]' "${scratch}/settled-label/stdout" >/dev/null ||
  fail 'settled-label: the accepted report did not name the change that settled'
# Two different objects moving in turn are both named, in a report that still
# passes because the third pair agreed.
mkdir -p "${scratch}/settled-twice/after" "${scratch}/settled-twice/after2"
cp "${scratch}/healthy/"*.json "${scratch}/settled-twice/"
jq '.metadata.labels.settled="yes"' "${scratch}/healthy/apps.json" >"${scratch}/settled-twice/after/apps.json"
jq '.metadata.labels.settled="yes"' "${scratch}/healthy/product.json" >"${scratch}/settled-twice/after2/product.json"
expected_urls=33
run_case settled-twice pass '' 60
expected_urls=11
jq -e '.retaken == 2 and .settled == ["apps.metadata.labels","product.metadata.labels"]' "${scratch}/settled-twice/stdout" >/dev/null ||
  fail 'settled-twice: the accepted report did not name every change that settled'
# A rollout that never moved reports neither field: their presence is the signal.
run_case first-pair-agreed pass
jq -e '(has("retaken") or has("settled")) == false' "${scratch}/first-pair-agreed/stdout" >/dev/null ||
  fail 'first-pair-agreed: a rollout that never moved was reported as retaken'
# The same change is still refused when the state it settles into is wrong.
mkdir -p "${scratch}/settled-wrong/after"
cp "${scratch}/healthy/"*.json "${scratch}/settled-wrong/"
jq '.status.conditions[0].status="False"' "${scratch}/healthy/apps.json" >"${scratch}/settled-wrong/after/apps.json"
# Reach both snapshots before testing the retained change name. A three-second
# budget can expire during the first collection on a loaded runner.
run_case settled-wrong fail '' 15
jq -e '.failure == "deadline_exceeded" and .changed == ["apps.status.conditions"]' "${scratch}/settled-wrong/stdout" >/dev/null ||
  fail 'settled-wrong: a rollout that settled into a wrong state was not refused with the change named'
# Public checks that fail once while the rollout moves are retaken with it.
mkdir -p "${scratch}/moving-public/after"
cp "${scratch}/healthy/"*.json "${scratch}/moving-public/"
jq '.metadata.labels.settled="yes"' "${scratch}/healthy/apps.json" >"${scratch}/moving-public/after/apps.json"
expected_urls=12
run_case moving-public pass flaky-public
expected_urls=11
jq -e '.retaken == 1 and .settled == ["apps.metadata.labels"]' "${scratch}/moving-public/stdout" >/dev/null ||
  fail 'moving-public: the accepted report did not name the change that settled'
# Against a rollout that holds still, one failed public check refuses the deploy.
# This reason requires a failed public request and two complete snapshots.
run_case still-public fail flaky-public 15
jq -e '.failure == "public_contract_incomplete" and has("changed") == false' "${scratch}/still-public/stdout" >/dev/null ||
  fail 'still-public: a failed public check against an unchanged rollout was not refused as such'
# A public failure seen against a pair that then moved belongs to that abandoned
# attempt. When the retaken rollout never becomes acceptable, the deadline refusal
# names the change and must not carry the discarded attempt's public role.
mkdir -p "${scratch}/moved-then-wrong-public/after"
cp "${scratch}/healthy/"*.json "${scratch}/moved-then-wrong-public/"
jq '.status.conditions[0].status="False"' "${scratch}/healthy/apps.json" >"${scratch}/moved-then-wrong-public/after/apps.json"
run_case moved-then-wrong-public fail flaky-public 15
jq -e '.failure == "deadline_exceeded" and .changed == ["apps.status.conditions"] and .phase=="rollout_stability" and .role=="apps" and .reason=="predicate_not_ready"' "${scratch}/moved-then-wrong-public/stdout" >/dev/null ||
  fail 'moved-then-wrong-public: a deadline after a moved rollout carried the abandoned public failure'
mutate_case foreign-source-ref apps '.spec.sourceRef.name="foreign"' 3
mutate_case foreign-source-namespace apps '.spec.sourceRef.namespace="foreign"' 3
mutate_case root-advanced root '.status.artifact.revision="latest@sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"' 3
mutate_case root-unverified root '.status.conditions[1].status="False"' 3
mutate_case root-stale root '.status.observedGeneration=2' 3
mutate_case root-foreign-policy root '.spec.verify.matchOIDCIdentity[0].subject=".*"' 3
mutate_case foreign-chart-key chart '.spec.verify.secretRef={name:"foreign"}' 3
mutate_case foreign-chart-issuer chart '.spec.verify.matchOIDCIdentity[0].issuer=".*"' 3
mutate_case foreign-chart-subject chart '.spec.verify.matchOIDCIdentity[0].subject=".*"' 3
mutate_case product-stale product '.status.observedGeneration=2' 3
mutate_case product-stale-condition product '.status.conditions[0].observedGeneration=2' 3
mutate_case product-not-ready product '.status.conditions[0].status="False"' 3
mutate_case product-declares-ui product '.spec.ui={url:"https://harbour-data.example.com/ui"}' 3
mutate_case service-selector service-publisher-review '.spec.selector["app.kubernetes.io/component"]="foreign"' 3
mutate_case service-port service-harbour '.spec.ports[0].targetPort=9090' 3
mutate_case service-type service-publisher-review '.spec.type="ExternalName"' 3
mutate_case named-container-port deployment-publisher-review '.spec.template.spec.containers[0].ports[0].containerPort=9090' 3
mutate_case pod-selector pods '.items[5].metadata.labels["app.kubernetes.io/component"]="foreign"' 3
mutate_case pod-container-port pods '.items[5].spec.containers[0].ports[0].containerPort=9090' 3
mutate_case endpoint-partial-page endpoints '.metadata.continue="next"' 3
mutate_case endpoint-duplicate-inventory endpoints '.items += [.items[1]]' 3
mutate_case endpoint-malformed-unrelated endpoints '.items += [(.items[1]|.metadata.uid=null|.metadata.labels["kubernetes.io/service-name"]="unrelated")]' 3
mutate_case endpoint-wrong-service-owner endpoints '.items[1].metadata.ownerReferences[0].uid="foreign"' 3
mutate_case endpoint-wrong-service-label endpoints '.items[1].metadata.labels["kubernetes.io/service-name"]="foreign"' 3
mutate_case endpoint-wrong-port endpoints '.items[1].ports[0].port=9090' 3
mutate_case endpoint-unready endpoints '.items[1].endpoints[0].conditions.ready=false' 3
mutate_case endpoint-terminating endpoints '.items[1].endpoints[0].conditions.terminating=true' 3
mutate_case endpoint-wrong-pod endpoints '.items[1].endpoints[0].targetRef.uid="foreign"' 3
mutate_case endpoint-wrong-name endpoints '.items[1].endpoints[0].targetRef.name="foreign"' 3
mutate_case endpoint-wrong-version endpoints '.items[1].endpoints[0].targetRef.apiVersion="apps/v1"' 3
mutate_case endpoint-wrong-ip endpoints '.items[1].endpoints[0].addresses=["10.0.1.1"]' 3
mutate_case endpoint-wrong-family endpoints '.items[1].addressType="IPv6"' 3
mutate_case endpoint-missing-current-pod endpoints '.items[1].endpoints |= .[:1]' 3
mkdir "${scratch}/non-generational-services"
cp "${scratch}/healthy/"*.json "${scratch}/non-generational-services/"
for key in service-harbour service-publisher-review; do
  jq 'del(.metadata.generation)' "${scratch}/healthy/$key.json" >"${scratch}/non-generational-services/$key.json"
done
jq '.items[] |= del(.metadata.generation)' "${scratch}/healthy/endpoints.json" >"${scratch}/non-generational-services/endpoints.json"
run_case non-generational-services pass
mkdir "${scratch}/dual-stack"
cp "${scratch}/healthy/"*.json "${scratch}/dual-stack/"
jq '.items |= map(.status.podIPs += [{ip:("fd00::"+(.metadata.name|split("-")|last))}])' "${scratch}/healthy/pods.json" >"${scratch}/dual-stack/pods.json"
jq '.items += [.items[] | .metadata.name += "-v6" | .metadata.uid += "-v6" | .addressType="IPv6" | .endpoints |= map(.addresses=["fd00::"+(.targetRef.name|split("-")|last)])]' \
  "${scratch}/healthy/endpoints.json" >"${scratch}/dual-stack/endpoints.json"
run_case dual-stack pass
mkdir "${scratch}/dual-stack-partial-family"
cp "${scratch}/dual-stack/"*.json "${scratch}/dual-stack-partial-family/"
jq '.items[3].endpoints |= .[:1]' "${scratch}/dual-stack/endpoints.json" >"${scratch}/dual-stack-partial-family/endpoints.json"
run_case dual-stack-partial-family fail '' 3
mkdir "${scratch}/multiple-slices"
cp "${scratch}/healthy/"*.json "${scratch}/multiple-slices/"
jq '.items |= [.[] | . as $slice | .endpoints | to_entries[] | . as $entry | $slice | .metadata.name += ("-"+($entry.key|tostring)) | .metadata.uid += ("-"+($entry.key|tostring)) | .endpoints=[$entry.value]]' \
  "${scratch}/healthy/endpoints.json" >"${scratch}/multiple-slices/endpoints.json"
run_case multiple-slices pass
for version in v1 ''; do
  name="endpoint-explicit-${version:-empty}-version"
  mkdir "${scratch}/$name"
  cp "${scratch}/healthy/"*.json "${scratch}/$name/"
  jq --arg version "$version" '.items[].endpoints[].targetRef.apiVersion=$version' \
    "${scratch}/healthy/endpoints.json" >"${scratch}/$name/endpoints.json"
  run_case "$name" pass
done
mkdir "${scratch}/declared-policy-update"
cp "${scratch}/healthy/"*.json "${scratch}/declared-policy-update/"
fixture_pool_drain
cp "${scratch}/apps-verify.json" "${scratch}/saved-policy.json"
jq '.matchOIDCIdentity[0].subject="new-explicit-declared-publisher"' "${scratch}/saved-policy.json" >"${scratch}/apps-verify.json"
jq --slurpfile policy "${scratch}/apps-verify.json" '.spec.verify=$policy[0]' "${scratch}/healthy/root.json" >"${scratch}/declared-policy-update/root.json"
run_case declared-policy-update pass
cp "${scratch}/saved-policy.json" "${scratch}/apps-verify.json"
printf '[]\n' >"${scratch}/apps-verify.json"
run_case invalid-policy fail '' 3
printf '%16385s' ' ' >"${scratch}/apps-verify.json"
run_case oversized-policy fail '' 3
cp "${scratch}/saved-policy.json" "${scratch}/apps-verify.json"
run_case bounded-preflight fail preflight-timeout 1
# Two seconds of deliberate preflight plus a three-second startup/cleanup budget
# reaches the hanging plugin; the child still sleeps far beyond the deadline.
run_case bounded-credential-plugin fail hang 5
fixture_pool_finish
printf 'PASS: rollout readback behavior and bounded, read-only request scopes\n'
