#!/usr/bin/env bash

# Render the committed component with its namespace policies and registry route.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly component_dir="${root_dir}/k8s/bases/apps/data-product-controller"
scratch="$(mktemp -d)"
readonly scratch
trap 'rm -rf "${scratch}"' EXIT

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}
for tool in kubectl jq yq; do
  command -v "${tool}" >/dev/null || fail "${tool} is required"
done

kubectl kustomize "${component_dir}" >"${scratch}/rendered.yaml" || fail 'the UI host and its namespace must render'
yq ea -o=json '[.]' "${scratch}/rendered.yaml" >"${scratch}/rendered.json"

cat >"${scratch}/validate.jq" <<'JQ'
def one($kind; $name): [.[] | select(.kind == $kind and .metadata.name == $name)] |
  if length == 1 then .[0] else error("missing or duplicate resource") end;
def labels: {"app.kubernetes.io/name":"data-product-controller",
  "app.kubernetes.io/instance":"data-product-controller", "app.kubernetes.io/component":"ui-kit"};
def cilium_labels: {"k8s:app.kubernetes.io/name":"data-product-controller",
  "k8s:app.kubernetes.io/instance":"data-product-controller", "k8s:app.kubernetes.io/component":"ui-kit"};
def probe:
  .httpGet == {path:"/healthz", port:"http", scheme:"HTTP"} and
  .timeoutSeconds == 2 and .periodSeconds == 10 and .failureThreshold == 3;
def restricted:
  .runAsNonRoot == true and .runAsUser == 65532 and .runAsGroup == 65532 and
  .seccompProfile == {type:"RuntimeDefault"};
one("Deployment"; "data-product-controller-ui-kit") as $deployment |
one("Service"; "data-product-controller-ui-kit") as $service |
one("ServiceAccount"; "data-product-controller-ui-kit") as $account |
one("PodDisruptionBudget"; "data-product-controller-ui-kit") as $budget |
one("CiliumNetworkPolicy"; "allow-data-product-controller-ui-kit") as $network |
one("HTTPRoute"; "data-product-controller-ui-kit") as $route |
one("HTTPRoute"; "data-product-controller-registry") as $registry |
one("Namespace"; "data-product-controller") as $namespace |
$deployment.spec.template.spec as $pod |
$pod.containers[0] as $container |
[$deployment,$service,$account,$budget,$network,$route] as $host |
([
  ($host | all(.metadata.namespace == "data-product-controller")),
  ([.[] | select(.metadata.name | endswith("-ui-kit"))] | length == 6),
  ($namespace.metadata.labels["pod-security.kubernetes.io/enforce"] == "restricted"),
  ($deployment.spec.replicas == 2),
  ($deployment.spec.selector == {matchLabels:labels}),
  ($deployment.spec.template.metadata.labels == labels),
  ($pod.serviceAccountName == $account.metadata.name and $pod.automountServiceAccountToken == false),
  ($account.automountServiceAccountToken == false),
  ($pod.imagePullSecrets == [{name:"ghcr-auth"}] and $account.imagePullSecrets == [{name:"ghcr-auth"}]),
  ($pod.securityContext | restricted),
  ($pod.securityContext.fsGroup == 65532),
  (($pod.hostNetwork // false) == false and ($pod.hostPID // false) == false and ($pod.hostIPC // false) == false),
  (($pod.volumes // []) == [] and ($pod.initContainers // []) == [] and ($pod.containers | length) == 1),
  ($container.name == "ui-kit"),
  ($container.image | test("^ghcr\\.io/devantler-tech/data-product-controller(:[0-9]+\\.[0-9]+\\.[0-9]+)?@sha256:[a-f0-9]{64}$")),
  ($container.command == ["/ui-kit"] and $container.args == ["--http-behind-gateway","--listen-address","0.0.0.0:8080"]),
  ($container.env | sort_by(.name) == [{name:"UI_APPEARANCE_ENABLED",value:"true"},{name:"UI_CONTRACT_ENABLED",value:"true"}]),
  (($container.envFrom // []) == [] and ($container.volumeMounts // []) == []),
  ($container.ports == [{name:"http",containerPort:8080,protocol:"TCP"}]),
  ($container.securityContext | restricted),
  ($container.securityContext.allowPrivilegeEscalation == false and $container.securityContext.readOnlyRootFilesystem == true),
  (($container.securityContext.privileged // false) == false and $container.securityContext.capabilities == {drop:["ALL"]}),
  ($container.resources.requests == {cpu:"5m",memory:"32Mi"} and $container.resources.limits == {cpu:"100m",memory:"64Mi"}),
  ($container.readinessProbe | probe),
  ($container.livenessProbe | probe),
  ($pod.topologySpreadConstraints == [{maxSkew:1,topologyKey:"kubernetes.io/hostname",
    whenUnsatisfiable:"ScheduleAnyway",labelSelector:{matchLabels:labels}}]),
  (($pod.affinity // {}) == {}),
  ($service.spec.type == "ClusterIP" and $service.spec.selector == labels),
  ($service.spec.ports == [{name:"http",port:80,targetPort:"http",protocol:"TCP"}]),
  ($budget.spec == {maxUnavailable:1,selector:{matchLabels:labels}}),
  ($network.spec.endpointSelector == {matchLabels:cilium_labels}),
  ($network.spec.ingress == [{fromEntities:["ingress"],toPorts:[{ports:[{port:"8080",protocol:"TCP"}]}]}]),
  (($network.spec.egress // []) == []),
  (($network.spec | keys) - ["endpointSelector","ingress","egress"] == []),
  # Every other allow remains separated from this host by a component label.
  ([.[] | select(.kind == "CiliumNetworkPolicy" and .metadata.namespace == "data-product-controller") |
    .metadata.name as $name | .spec.endpointSelector |
    .matchLabels["k8s:app.kubernetes.io/component"] as $component |
    (($component | type) == "string" and $component != "" and
      ($component != "ui-kit" or $name == "allow-data-product-controller-ui-kit") and
      .matchLabels["k8s:app.kubernetes.io/name"] == "data-product-controller" and
      .matchLabels["k8s:app.kubernetes.io/instance"] == "data-product-controller" and
      (.matchExpressions // []) == [])] | all),
  ([.[] | select(.kind == "NetworkPolicy" and .metadata.namespace == "data-product-controller") |
    {name:.metadata.name,spec:.spec}] == [{name:"default-deny",spec:{podSelector:{},policyTypes:["Ingress","Egress"]}}]),
  ([.[] | select(.kind == "Role" or .kind == "RoleBinding" or .kind == "ClusterRole" or .kind == "ClusterRoleBinding") |
    select(.metadata.name == "data-product-controller-ui-kit" or
      any(.subjects[]?; .kind == "ServiceAccount" and .name == "data-product-controller-ui-kit"))] | length == 0),
  ($route.spec.parentRefs == [{name:"platform",namespace:"kube-system",sectionName:"https"}]),
  ($route.spec.hostnames == ["product-ui.${domain}"]),
  ($route.spec.rules | length == 1),
  ($route.spec.rules[0].matches == [{path:{type:"PathPrefix",value:"/"}}]),
  ($route.spec.rules[0].backendRefs == [{name:"data-product-controller-ui-kit",port:80}]),
  ($route.spec.rules[0].filters == [
    {type:"RequestHeaderModifier",requestHeaderModifier:{remove:["Cookie","Authorization"]}},
    {type:"ResponseHeaderModifier",responseHeaderModifier:{set:[{name:"Strict-Transport-Security",value:"max-age=63072000; includeSubDomains; preload"}]}}]),
  ($registry.spec.rules | all(.backendRefs == [{name:"oauth2-proxy",namespace:"oauth2-proxy",port:80}]))
] | all)
JQ

validate() { jq -e -f "${scratch}/validate.jq" "$1" >/dev/null 2>&1; }
validate "${scratch}/rendered.json" ||
  fail 'the rendered UI host must be complete, restricted, credential-free and Gateway-only; registry access must retain SSO'

assert_rejected() {
  local name="$1" mutation="$2"
  jq "${mutation}" "${scratch}/rendered.json" >"${scratch}/invalid.json"
  cmp -s "${scratch}/rendered.json" "${scratch}/invalid.json" && fail "${name}: control did not change the render"
  if validate "${scratch}/invalid.json"; then
    fail "${name}: unsafe or incomplete resources were accepted"
  fi
}

assert_rejected 'absent component' 'map(select(.metadata.name | endswith("-ui-kit") | not))'
assert_rejected 'duplicate Deployment' '. + [.[] | select(.kind == "Deployment" and .metadata.name == "data-product-controller-ui-kit")]'
assert_rejected 'missing account' 'map(select(.kind != "ServiceAccount"))'
assert_rejected 'single replica' 'map(if .kind == "Deployment" then .spec.replicas = 1 else . end)'
assert_rejected 'API token mount' 'map(if .kind == "Deployment" then .spec.template.spec.automountServiceAccountToken = true else . end)'
assert_rejected 'missing pod seccomp' 'map(if .kind == "Deployment" then del(.spec.template.spec.securityContext.seccompProfile) else . end)'
assert_rejected 'writable root' 'map(if .kind == "Deployment" then .spec.template.spec.containers[0].securityContext.readOnlyRootFilesystem = false else . end)'
assert_rejected 'added capability' 'map(if .kind == "Deployment" then .spec.template.spec.containers[0].securityContext.capabilities.add = ["NET_ADMIN"] else . end)'
assert_rejected 'credential environment' 'map(if .kind == "Deployment" then .spec.template.spec.containers[0].envFrom = [{secretRef:{name:"synthetic-credential"}}] else . end)'
assert_rejected 'credential volume' 'map(if .kind == "Deployment" then .spec.template.spec.volumes = [{name:"credential",secret:{secretName:"synthetic-credential"}}] else . end)'
assert_rejected 'mutable image' 'map(if .kind == "Deployment" then .spec.template.spec.containers[0].image = "ghcr.io/devantler-tech/data-product-controller:latest" else . end)'
assert_rejected 'TLS opt-in missing' 'map(if .kind == "Deployment" then .spec.template.spec.containers[0].args = ["--listen-address","0.0.0.0:8080"] else . end)'
assert_rejected 'missing health probe' 'map(if .kind == "Deployment" then del(.spec.template.spec.containers[0].readinessProbe) else . end)'
assert_rejected 'controller Service selector' 'map(if .kind == "Service" then .spec.selector["app.kubernetes.io/component"] = "controller" else . end)'
assert_rejected 'external Service' 'map(if .kind == "Service" then .spec.type = "LoadBalancer" else . end)'
assert_rejected 'world ingress' 'map(if .metadata.name == "allow-data-product-controller-ui-kit" then .spec.ingress[0].fromEntities = ["world"] else . end)'
assert_rejected 'additional Cilium host allow' '. + [{apiVersion:"cilium.io/v2",kind:"CiliumNetworkPolicy",metadata:{name:"wide",namespace:"data-product-controller"},spec:{endpointSelector:{matchLabels:{"k8s:app.kubernetes.io/name":"data-product-controller","k8s:app.kubernetes.io/instance":"data-product-controller","k8s:app.kubernetes.io/component":"ui-kit"}},ingress:[{fromEntities:["world"]}]}}]'
assert_rejected 'controller allow applies to host' 'map(if .metadata.name == "allow-data-product-controller" then del(.spec.endpointSelector.matchLabels["k8s:app.kubernetes.io/component"]) else . end)'
assert_rejected 'DNS egress' 'map(if .metadata.name == "allow-data-product-controller-ui-kit" then .spec.egress = [{toEntities:["all"]}] else . end)'
assert_rejected 'standard policy wide allow' '. + [{apiVersion:"networking.k8s.io/v1",kind:"NetworkPolicy",metadata:{name:"wide",namespace:"data-product-controller"},spec:{podSelector:{},ingress:[{}]}}]'
assert_rejected 'wrong route backend' 'map(if .metadata.name == "data-product-controller-ui-kit" and .kind == "HTTPRoute" then .spec.rules[0].backendRefs = [{name:"data-product-controller",port:8082}] else . end)'
# Flux substitutes this literal after the manifests leave Kustomize.
# shellcheck disable=SC2016
assert_rejected 'wrong public origin' 'map(if .metadata.name == "data-product-controller-ui-kit" and .kind == "HTTPRoute" then .spec.hostnames = ["data-products.${domain}"] else . end)'
assert_rejected 'HTTP listener' 'map(if .metadata.name == "data-product-controller-ui-kit" and .kind == "HTTPRoute" then .spec.parentRefs[0].sectionName = "http" else . end)'
assert_rejected 'forwarded credentials' 'map(if .metadata.name == "data-product-controller-ui-kit" and .kind == "HTTPRoute" then .spec.rules[0].filters[0].requestHeaderModifier.remove = ["Cookie"] else . end)'
assert_rejected 'registry SSO bypass' 'map(if .metadata.name == "data-product-controller-registry" then .spec.rules[0].backendRefs = [{name:"data-product-controller",port:8082}] else . end)'
assert_rejected 'host RBAC grant' '. + [{apiVersion:"rbac.authorization.k8s.io/v1",kind:"RoleBinding",metadata:{name:"host-grant",namespace:"data-product-controller"},subjects:[{kind:"ServiceAccount",name:"data-product-controller-ui-kit"}],roleRef:{apiGroup:"rbac.authorization.k8s.io",kind:"ClusterRole",name:"edit"}}]'

printf 'PASS: rendered portable UI host and 26 unsafe/incomplete controls\n'
