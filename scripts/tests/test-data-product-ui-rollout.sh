#!/usr/bin/env bash
set -euo pipefail
umask 077
root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly script="${root_dir}/scripts/verify-data-product-ui-rollout.sh"
scratch="$(mktemp -d)"
trap 'rm -rf "${scratch}"' EXIT
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
exec "$REAL_JQ" "$@"
SH
chmod +x "${scratch}/bin/jq"

cat >"${scratch}/fixtures.jq" <<'JQ'
def meta($name;$ns): {name:$name,namespace:$ns,uid:($name+"-uid"),generation:3};
def labels($name): {"app.kubernetes.io/name":"data-product-controller","app.kubernetes.io/instance":"data-product-controller","app.kubernetes.io/component":(if $name|endswith("-harbour") then "harbour-product" elif $name|endswith("-ui-kit") then "ui-kit" else "controller" end)};
def ports: [{name:"http",containerPort:8080,protocol:"TCP"}];
def chart_verify: {provider:"cosign",matchOIDCIdentity:[{issuer:"^https://token\\.actions\\.githubusercontent\\.com$",subject:"^https://github\\.com/devantler-tech/data-product-controller/\\.github/workflows/publish-chart\\.yaml@refs/tags/v[0-9]+\\.[0-9]+\\.[0-9]+$"}]};
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
  spec:{replicas:2,selector:{matchLabels:labels($name)},template:{metadata:{labels:labels($name)},spec:{containers:[{name:$container,image:($repo+"@"+$index),ports:ports}]}}},
  status:{observedGeneration:3,replicas:2,updatedReplicas:2,readyReplicas:2,availableReplicas:2}};
def rs($name;$container): {apiVersion:"apps/v1",kind:"ReplicaSet",metadata:(meta($name+"-new";"data-product-controller") + {annotations:{"deployment.kubernetes.io/revision":"7"},ownerReferences:[owner("Deployment";$name)]}),
  spec:{replicas:2,template:{spec:{containers:[{name:$container,image:($repo+"@"+$index)}]}}},status:{observedGeneration:3,replicas:2,readyReplicas:2,availableReplicas:2}};
def pod($name;$container;$ordinal): {apiVersion:"v1",kind:"Pod",metadata:(meta($name+"-pod-"+($ordinal|tostring);"data-product-controller") + {labels:labels($name),ownerReferences:[owner("ReplicaSet";$name+"-new")]}),
  spec:{containers:[{name:$container,image:($repo+"@"+$index),ports:ports}]},status:{phase:"Running",podIP:("10.0.0."+($ordinal|tostring)),podIPs:[{ip:("10.0.0."+($ordinal|tostring))}],conditions:[{type:"Ready",status:"True"}],containerStatuses:[{name:$container,ready:true,imageID:($repo+"@"+$child),state:{running:{startedAt:"2026-10-03T00:00:00Z"}}}]}};
def service($name): {apiVersion:"v1",kind:"Service",metadata:meta($name;"data-product-controller"),spec:{type:"ClusterIP",selector:labels($name),ports:[{name:"http",port:80,targetPort:"http",protocol:"TCP"}]}};
def slice($name): {apiVersion:"discovery.k8s.io/v1",kind:"EndpointSlice",metadata:(meta($name+"-slice";"data-product-controller") + {labels:{"kubernetes.io/service-name":$name},ownerReferences:[{apiVersion:"v1",kind:"Service",name:$name,uid:($name+"-uid"),controller:true}]}),addressType:"IPv4",ports:[{name:"http",port:8080,protocol:"TCP"}],endpoints:[range(1;3) as $ordinal | {addresses:[("10.0.0."+($ordinal|tostring))],conditions:{ready:true,serving:true,terminating:false},targetRef:{kind:"Pod",namespace:"data-product-controller",name:($name+"-pod-"+($ordinal|tostring)),uid:($name+"-pod-"+($ordinal|tostring)+"-uid")}}]};
def route($suffix;$host;$backend): {apiVersion:"gateway.networking.k8s.io/v1",kind:"HTTPRoute",metadata:meta("data-product-controller-"+$suffix;"data-product-controller"),
  spec:{parentRefs:[{name:"platform",namespace:"kube-system",sectionName:"https"}],hostnames:[$host+".example.com"],rules:[{matches:[{path:{type:"PathPrefix",value:"/"}}],backendRefs:[$backend],filters:(if $suffix == "registry" then [] else [{type:"RequestHeaderModifier",requestHeaderModifier:{remove:["Cookie","Authorization"]}}] end)}]},
  status:{parents:[{parentRef:{group:"gateway.networking.k8s.io",kind:"Gateway",name:"platform",namespace:"kube-system",sectionName:"https"},controllerName:"io.cilium/gateway-controller",conditions:[condition("Accepted"),condition("ResolvedRefs")]}]}};
{apps:{apiVersion:"kustomize.toolkit.fluxcd.io/v1",kind:"Kustomization",metadata:meta("apps";"flux-system"),spec:{suspend:false,sourceRef:{kind:"OCIRepository",name:"flux-system"}},status:{observedGeneration:3,conditions:[condition("Ready")],lastAppliedRevision:("latest@"+$apps)}},
 root:{apiVersion:"source.toolkit.fluxcd.io/v1",kind:"OCIRepository",metadata:meta("flux-system";"flux-system"),spec:{suspend:false,verify:root_verify},status:{observedGeneration:3,conditions:[condition("Ready"),condition("SourceVerified")],artifact:{revision:("latest@"+$apps),digest:("sha256:"+([range(64)|"e"]|join("")))}}},
 chart:{apiVersion:"source.toolkit.fluxcd.io/v1",kind:"OCIRepository",metadata:meta("data-product-controller";"data-product-controller"),spec:{suspend:false,url:"oci://ghcr.io/devantler-tech/charts/data-product-controller",ref:{digest:$chart},verify:chart_verify},status:{observedGeneration:3,conditions:[condition("Ready"),condition("SourceVerified")],artifact:{revision:$chart,digest:("sha256:"+([range(64)|"e"]|join("")))}}},
 product:{apiVersion:"data.devantler.tech/v1alpha1",kind:"DataProduct",metadata:meta("harbour-observations";"data-product-controller"),spec:{ui:{url:"https://harbour-data.example.com/ui",contract:{apiVersion:"data-product-ui/v2",hostOrigins:["https://data-products.example.com","https://product-ui.example.com"],capabilities:["status","resize","appearance"]}}},status:{observedGeneration:3,conditions:[condition("Ready")]}},
 helm:{apiVersion:"helm.toolkit.fluxcd.io/v2",kind:"HelmRelease",metadata:meta("data-product-controller";"data-product-controller"),spec:{suspend:false,chartRef:{kind:"OCIRepository",name:"data-product-controller"},values:{image:{repository:$repo,tag:"1.20.0",digest:$index},uiContract:{enabled:true,additionalHostOrigins:["https://product-ui.example.com"]},uiAppearance:{enabled:true},controller:{replicas:2},demoProduct:{enabled:true,replicas:2,publicBaseURL:"https://harbour-data.example.com"},route:{enabled:true,host:"data-products.example.com"},connectorReadiness:{enabled:false},contractReadiness:{enabled:false},contractProbe:{enabled:false}}},status:{observedGeneration:3,conditions:[condition("Ready")],lastAttemptedRevision:("1.20.0+"+$chart[7:19]),lastAttemptedRevisionDigest:$chart,lastAttemptedConfigDigest:("sha256:"+([range(64)|"f"]|join(""))),history:[{name:"data-product-controller",namespace:"data-product-controller",chartName:"data-product-controller",chartVersion:("1.20.0+"+$chart[7:19]),ociDigest:$chart,configDigest:("sha256:"+([range(64)|"f"]|join(""))),status:"deployed",version:10}]}},
 "deployment-controller":(deployment("data-product-controller";"controller") | .spec.template.spec.containers[0].env=[
   {name:"CONNECTOR_READINESS_ENABLED",value:"false"},{name:"CONTRACT_READINESS_ENABLED",value:"false"}]),
 "deployment-harbour":deployment("data-product-controller-harbour";"product"),
 "deployment-ui-kit":deployment("data-product-controller-ui-kit";"ui-kit"),
 "deployment-probe":{apiVersion:"apps/v1",kind:"Deployment",metadata:meta("data-product-controller-contract-probe";"data-product-controller"),
   spec:{replicas:0,selector:{matchLabels:probe_labels},template:{metadata:{labels:probe_labels},
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
     egress:[{toFQDNs:[{matchName:"harbour-data.example.com"}],toPorts:[{ports:[{port:"443",protocol:"TCP"}]}]},
       {toEndpoints:[{matchLabels:{"k8s:io.kubernetes.pod.namespace":"kube-system","k8s-app":"kube-dns"}}],
        toPorts:[{ports:[{port:"53",protocol:"UDP"},{port:"53",protocol:"TCP"}],rules:{dns:[{matchName:"harbour-data.example.com"}]}}]}]}},
 replicasets:{apiVersion:"apps/v1",kind:"ReplicaSetList",items:[rs("data-product-controller";"controller"),rs("data-product-controller-harbour";"product"),rs("data-product-controller-ui-kit";"ui-kit"),
   (rs("data-product-controller-contract-probe";"contract-probe") | .metadata.name="dormant-probe-rs" | .metadata.uid="dormant-probe-rs-uid" | .spec.replicas=0 | .status.replicas=0 | .status.readyReplicas=0 | .status.availableReplicas=0)]},
 pods:{apiVersion:"v1",kind:"PodList",items:[pod("data-product-controller";"controller";1),pod("data-product-controller";"controller";2),pod("data-product-controller-harbour";"product";1),pod("data-product-controller-harbour";"product";2),pod("data-product-controller-ui-kit";"ui-kit";1),pod("data-product-controller-ui-kit";"ui-kit";2)]},
 "service-harbour":service("data-product-controller-harbour"),"service-ui-kit":service("data-product-controller-ui-kit"),
 endpoints:{apiVersion:"discovery.k8s.io/v1",kind:"EndpointSliceList",metadata:{},items:[slice("data-product-controller-harbour"),slice("data-product-controller-ui-kit")]},
 "route-registry":route("registry";"data-products";{name:"oauth2-proxy",namespace:"oauth2-proxy",port:80}),
 "route-harbour":route("harbour";"harbour-data";{name:"data-product-controller-harbour",port:80}),
 "route-ui-kit":route("ui-kit";"product-ui";{name:"data-product-controller-ui-kit",port:80}),
 gateway:{apiVersion:"gateway.networking.k8s.io/v1",kind:"Gateway",metadata:meta("platform";"kube-system"),spec:{gatewayClassName:"cilium",listeners:[{name:"https",protocol:"HTTPS",port:443,tls:{mode:"Terminate"}}]},status:{conditions:[condition("Accepted"),condition("Programmed")],listeners:[{name:"https",conditions:[condition("Accepted"),condition("ResolvedRefs"),condition("Programmed")]}]}}}
JQ
jq -n --arg index "$index" --arg child "$child" --arg chart "$chart" --arg apps "$apps" \
  --arg repo 'ghcr.io/devantler-tech/data-product-controller' -f "${scratch}/fixtures.jq" >"${scratch}/all.json"
while IFS= read -r key; do jq --arg key "$key" '.[$key]' "${scratch}/all.json" >"${scratch}/healthy/${key}.json"; done < <(jq -r 'keys[]' "${scratch}/all.json")
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
  data-product-controller:services:data-product-controller-harbour) key=service-harbour ;;
  data-product-controller:services:data-product-controller-ui-kit) key=service-ui-kit ;;
  data-product-controller:endpointslices.discovery.k8s.io:-o) key=endpoints ;;
  data-product-controller:deployments.apps:data-product-controller) key=deployment-controller ;;
  data-product-controller:deployments.apps:data-product-controller-harbour) key=deployment-harbour ;;
  data-product-controller:deployments.apps:data-product-controller-ui-kit) key=deployment-ui-kit ;;
  data-product-controller:deployments.apps:data-product-controller-contract-probe) key=deployment-probe ;;
  data-product-controller:serviceaccounts:data-product-controller-contract-probe) key=probe-account ;;
  data-product-controller:roles.rbac.authorization.k8s.io:data-product-readiness-observer) key=observer-role ;;
  data-product-controller:rolebindings.rbac.authorization.k8s.io:data-product-readiness-observer) key=observer-binding ;;
  data-product-controller:ciliumnetworkpolicies.cilium.io:allow-data-product-controller-contract-probe) key=probe-policy ;;
  data-product-controller:httproutes.gateway.networking.k8s.io:data-product-controller-registry) key=route-registry ;;
  data-product-controller:httproutes.gateway.networking.k8s.io:data-product-controller-harbour) key=route-harbour ;;
  data-product-controller:httproutes.gateway.networking.k8s.io:data-product-controller-ui-kit) key=route-ui-kit ;;
  kube-system:gateways.gateway.networking.k8s.io:platform) key=gateway ;;
  data-product-controller:replicasets.apps:-o) key=replicasets ;;
  data-product-controller:pods:-o) key=pods ;;
  *) exit 93 ;;
esac
if [[ "$key" == pods || "$key" == replicasets || "$key" == endpoints ]]; then
  [[ "$*" == "$resource -o json" ]] || exit 94
else
  [[ "$*" == "$resource $name -o json" ]] || exit 95
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
if [[ "$key" == observer-role && "${MODE:-}" == partial-observer ]]; then
  cat "$FIXTURE/observer-role.json"; exit 1
fi
count=0
[[ ! -f "$FIXTURE/$key.count" ]] || count=$(cat "$FIXTURE/$key.count")
count=$((count + 1)); printf '%s\n' "$count" >"$FIXTURE/$key.count"
# The second and later reads return the changed object; in flap mode only every
# other read does, so no two snapshots in a row ever agree. An object with a
# second change returns it from the fourth read on, so the second pair differs
# from the first for another reason and only the third pair agrees.
if [[ "${MODE:-}" != flap && -f "$FIXTURE/after2/$key.json" ]] && ((count >= 4)); then
  cat "$FIXTURE/after2/$key.json"
elif [[ -f "$FIXTURE/after/$key.json" ]] && { { [[ "${MODE:-}" == flap ]] && ((count % 2 == 0)); } || { [[ "${MODE:-}" != flap ]] && ((count >= 2)); }; }; then
  cat "$FIXTURE/after/$key.json"
else
  cat "$FIXTURE/$key.json"
fi
SH
cat >"${scratch}/bin/curl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == --disable ]] || exit 91
shift
headers='' body='' url='' cookie=0 authorization=0 proto=0 proxy=0
while (($#)); do
  case "$1" in
    --silent|--show-error) shift ;;
    --proxy) [[ "$2" == '' ]] || exit 92; proxy=1; shift 2 ;;
    --connect-timeout) [[ "$2" =~ ^[1-5]$ ]] || exit 93; shift 2 ;;
    --max-time) [[ "$2" =~ ^[1-9][0-9]*$ ]] || exit 94; shift 2 ;;
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
printf 'HTTP/2 200\r\nContent-Security-Policy: %s\r\nReferrer-Policy: no-referrer\r\nX-Content-Type-Options: nosniff\r\n\r\n' "$csp" >"$headers"
case "$url" in
  https://harbour-data.example.com/healthz) printf '' >"$body" ;;
  https://harbour-data.example.com/api/observations) printf '{"items":[{"station":"Nordhavn","temperatureCelsius":12.4,"salinityPsu":7.8,"observedAt":"2026-01-01T00:00:00Z"},{"station":"Sydhavnen","temperatureCelsius":13.1,"salinityPsu":7.2,"observedAt":"2026-01-01T00:00:00Z"}]}\n' >"$body" ;;
  https://harbour-data.example.com/openapi.json) printf '{"openapi":"3.1.0","info":{"title":"Harbour observations"},"paths":{"/api/observations":{"get":{"responses":{"200":{}}}}}}\n' >"$body" ;;
  https://harbour-data.example.com/ui-contract-config) printf '{"hostOrigins":["https://data-products.example.com","https://product-ui.example.com"],"appearanceEnabled":true}\n' >"$body" ;;
  https://product-ui.example.com/healthz) printf 'ok\n' >"$body" ;;
  https://product-ui.example.com/) printf '<body data-appearance-enabled="true"><form id="manifest-form"></form><script src="ui-contract.js"></script><script src="kit.js"></script><link href="kit.css">\n' >"$body" ;;
  https://product-ui.example.com/ui-contract.js) printf 'window.DataProductUI = {};\n' >"$body" ;;
  https://product-ui.example.com/kit.js) printf 'DataProductUI.mount({});\n' >"$body" ;;
  https://product-ui.example.com/kit.css) printf ':root { color-scheme: light; }\n' >"$body" ;;
  *) exit 102 ;;
esac
if [[ "${MODE:-}" == flaky-public && ! -e "$FIXTURE/public-failed-once" ]]; then
  : >"$FIXTURE/public-failed-once"
  printf '503'
  exit 0
fi
case "${MODE:-}" in
  http-failure) printf '503'; exit 0 ;;
  redirect) printf '302'; exit 0 ;;
  missing-csp) printf 'HTTP/2 200\r\n\r\n' >"$headers" ;;
  weak-csp) printf 'HTTP/2 200\r\nContent-Security-Policy: default-src *\r\n\r\n' >"$headers" ;;
  duplicate-csp) printf 'Content-Security-Policy: %s\r\n' "$csp" >>"$headers" ;;
  wrong-body) printf '{"items":[]}\n' >"$body" ;;
  wrong-asset) [[ "$url" != */ui-contract.js ]] || printf 'not a protocol library\n' >"$body" ;;
esac
printf '200'
SH
chmod +x "${scratch}/bin/kubectl" "${scratch}/bin/curl"

expected_urls=9
run_case() {
  local name=$1 expected=$2 mode=${3:-} result=0 timeout_seconds=${4:-10}
  local fixture="${scratch}/${name}"
  [[ -d "$fixture" ]] || {
    mkdir "$fixture"
    cp "${scratch}/healthy/"*.json "$fixture/"
  }
  if [[ $# -lt 4 && "$expected" != pass && "$mode" != http-failure && "$mode" != redirect && "$mode" != *csp && "$mode" != wrong-body && "$mode" != wrong-asset && "$mode" != unsafe-storage ]]; then timeout_seconds=3; fi
  PATH="${scratch}/bin:$PATH" REAL_JQ="$real_jq" FIXTURE="$fixture" MODE="$mode" KUBECONFIG="${scratch}/kubeconfig" \
    bash "$script" --context synthetic-ci --domain example.com --image-digest "$index" \
    --runtime-digest "$child" --chart-digest "$chart" --apps-digest "$apps" --apps-verify-file "${scratch}/apps-verify.json" --timeout "$timeout_seconds" \
    >"$fixture/stdout" 2>"$fixture/stderr" || result=$?
  if [[ "$expected" == pass ]]; then
    if [[ "$result" != 0 ]] || ! jq -e '.complete == true and .deployments == 4 and .pods == 6 and .routes == 3 and .publicChecks == 9 and .readinessState == "dormant"' "$fixture/stdout" >/dev/null; then
      local reason
      reason=$(jq -r '.failure // "invalid_report"' "$fixture/stdout" 2>/dev/null) || reason=invalid_report
      case "$reason" in
      invalid_arguments | missing_dependency | private_storage_unavailable | deadline_exceeded | invalid_verification_policy | read_incomplete | public_contract_incomplete | rollout_changed | interrupted) ;;
      *) reason=invalid_report ;;
      esac
      fail "$name: healthy live-shaped rollout was not accepted (helper exit $result, $reason)"
    fi
    [[ $(wc -l <"$fixture/urls") -eq "$expected_urls" ]] || fail "$name: public checks were incomplete"
  else
    if [[ "$result" == 0 ]] || ! jq -e '.complete == false and (.failure | type == "string")' "$fixture/stdout" >/dev/null; then fail "$name: incomplete or unsafe rollout was accepted"; fi
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
mutate_case() {
  local name=$1 key=$2 mutation=$3
  mkdir "${scratch}/$name"
  cp "${scratch}/healthy/"*.json "${scratch}/$name/"
  jq "$mutation" "${scratch}/healthy/$key.json" >"${scratch}/$name/$key.json"
  run_case "$name" fail '' "${4:-3}"
}

mutate_case probe-supplemental-network probe-policy '.specs=[(.spec | .egress=[{toEntities:["world"]}])]'
mutate_case probe-object-network probe-policy '.specs={}'
mutate_case probe-string-network probe-policy '.specs=""'
mutate_case probe-boolean-network probe-policy '.specs=false'
mutate_case helm-missing-connector-flag helm 'del(.spec.values.connectorReadiness.enabled)'
mutate_case helm-missing-contract-flag helm 'del(.spec.values.contractReadiness.enabled)'
mutate_case helm-missing-probe-flag helm 'del(.spec.values.contractProbe.enabled)'
run_case healthy pass
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
mutate_case dormant-product-reference product '.spec.contractChecks=[{output:"observations",resourceRef:{apiVersion:"apps/v1",kind:"Deployment",name:"data-product-controller-contract-probe"}}]'
run_case partial-observer fail partial-observer
run_case healthy-acceptance-bound pass '' 3
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
mutate_case helm-wrong-profile helm '.spec.values.uiAppearance.enabled=false'
mutate_case missing-approved-host helm '.spec.values.uiContract.additionalHostOrigins=[]'
mutate_case partial-deployment deployment-ui-kit '.status.availableReplicas=1'
mutate_case stale-deployment deployment-controller '.status.observedGeneration=2'
mutate_case zero-replicas deployment-harbour '.spec.replicas=0 | .status.replicas=0 | .status.readyReplicas=0 | .status.updatedReplicas=0 | .status.availableReplicas=0'
mutate_case wrong-container deployment-ui-kit '.spec.template.spec.containers[0].name="other"'
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
mutate_case route-stale route-ui-kit '.status.parents[0].conditions[0].observedGeneration=2'
mutate_case route-duplicate-parent route-ui-kit '.status.parents += [.status.parents[0]]'
mutate_case route-duplicate-condition route-harbour '.status.parents[0].conditions += [.status.parents[0].conditions[0]]'
mutate_case registry-bypass route-registry '.spec.rules[0].backendRefs=[{name:"data-product-controller",port:8082}]'
mutate_case wrong-gateway-listener gateway '.spec.listeners[0].protocol="HTTP"'
mutate_case gateway-not-programmed gateway '.status.conditions[1].status="False"'
for mode in forbidden empty multi http-failure redirect missing-csp weak-csp duplicate-csp wrong-body wrong-asset unsafe-storage; do run_case "$mode" fail "$mode"; done
for key in apps chart helm deployment-ui-kit route-ui-kit gateway pods root product service-ui-kit; do
  name="changed-$key"
  mkdir -p "${scratch}/$name/after"
  cp "${scratch}/healthy/"*.json "${scratch}/$name/"
  jq 'if .kind == "PodList" then .items[5].metadata.uid="recreated-uid" else .metadata.uid="recreated-uid" end' \
    "${scratch}/healthy/$key.json" >"${scratch}/$name/after/$key.json"
  expected_urls=27
  run_case "$name" fail flap 20
  expected_urls=9
done
# A change that happens once and then holds is an ordinary reconcile, not a
# moving rollout: the pair is retaken, public checks included, and accepted.
mkdir -p "${scratch}/settled-label/after"
cp "${scratch}/healthy/"*.json "${scratch}/settled-label/"
jq '.metadata.labels.settled="yes"' "${scratch}/healthy/apps.json" >"${scratch}/settled-label/after/apps.json"
expected_urls=18
run_case settled-label pass
expected_urls=9
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
expected_urls=27
run_case settled-twice pass '' 20
expected_urls=9
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
run_case settled-wrong fail '' 3
jq -e '.failure == "deadline_exceeded" and .changed == ["apps.status.conditions"]' "${scratch}/settled-wrong/stdout" >/dev/null ||
  fail 'settled-wrong: a rollout that settled into a wrong state was not refused with the change named'
# Public checks that fail once while the rollout moves are retaken with it.
mkdir -p "${scratch}/moving-public/after"
cp "${scratch}/healthy/"*.json "${scratch}/moving-public/"
jq '.metadata.labels.settled="yes"' "${scratch}/healthy/apps.json" >"${scratch}/moving-public/after/apps.json"
expected_urls=10
run_case moving-public pass flaky-public
expected_urls=9
jq -e '.retaken == 1 and .settled == ["apps.metadata.labels"]' "${scratch}/moving-public/stdout" >/dev/null ||
  fail 'moving-public: the accepted report did not name the change that settled'
# Against a rollout that holds still, one failed public check refuses the deploy.
run_case still-public fail flaky-public 3
jq -e '.failure == "public_contract_incomplete" and has("changed") == false' "${scratch}/still-public/stdout" >/dev/null ||
  fail 'still-public: a failed public check against an unchanged rollout was not refused as such'
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
mutate_case product-wrong-version product '.spec.ui.contract.apiVersion="data-product-ui/v1"' 3
mutate_case product-wrong-ui product '.spec.ui.url="http://harbour-data.example.com/ui"' 3
mutate_case product-wrong-origins product '.spec.ui.contract.hostOrigins=["https://data-products.example.com"]' 3
mutate_case product-missing-appearance product '.spec.ui.contract.capabilities=["status","resize"]' 3
mutate_case service-selector service-ui-kit '.spec.selector["app.kubernetes.io/component"]="foreign"' 3
mutate_case service-port service-harbour '.spec.ports[0].targetPort=9090' 3
mutate_case service-type service-ui-kit '.spec.type="ExternalName"' 3
mutate_case named-container-port deployment-ui-kit '.spec.template.spec.containers[0].ports[0].containerPort=9090' 3
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
for key in service-harbour service-ui-kit; do
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
printf 'PASS: rollout readback behavior and bounded, read-only request scopes\n'
