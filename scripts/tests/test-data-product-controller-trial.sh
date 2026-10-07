#!/usr/bin/env bash
# Check the public sample and private registry in the layers Flux applies.
set -euo pipefail
root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
scratch="$(mktemp -d)"
trap 'rm -rf "${scratch}"' EXIT
# fail prints the violated trial invariant and stops the test.
fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

# The Docker provider intentionally deploys no applications.
readonly provider=hetzner
readonly expected_router="Host(\`data-products.\${domain}\`)|data-products"
kubectl kustomize "${root_dir}/k8s/providers/${provider}/apps" >"${scratch}/apps.yaml"
kubectl kustomize "${root_dir}/k8s/providers/${provider}/infrastructure/controllers" >"${scratch}/controllers.yaml"
yq ea -o=json '[.]' "${scratch}/apps.yaml" >"${scratch}/apps.json"
yq ea -o=json '[.]' "${scratch}/controllers.yaml" >"${scratch}/controllers.json"

jq -e '[.[] | select(.kind == "HelmRelease" and .metadata.namespace == "data-product-controller")
  | ((.spec.values | has("registryUI") | not) and .spec.values.demoProduct.enabled == true
     and .spec.values.uiContract.enabled == true
     and .spec.values.uiContract.additionalHostOrigins == ["https://product-ui.${domain}"]
     and .spec.values.uiAppearance.enabled == true
     and .spec.values.demoProduct.publicBaseURL == "https://harbour-data.${domain}"
     and ([.spec.values.engineProviders.enabled, .spec.values.provisionedSources.enabled,
           .spec.values.httpSource.enabled, .spec.values.composition.enabled,
           .spec.values.dcatCatalog.enabled, .spec.values.contractProbe.enabled]
          | all(. == false)) and .spec.values.connectorReadiness.enabled==true
     and .spec.values.contractReadiness.enabled==true)] == [true]' \
  "${scratch}/apps.json" >/dev/null || fail "${provider}: the trial must enable only sample, appearance and independent readiness observation with the exact sample publication URL"
jq -er '[.[] | select(.kind=="HelmRelease" and .metadata.name=="data-product-controller") |
  .spec.postRenderers[].kustomize.patches[] | select(.target.kind=="DataProduct" and .target.name=="harbour-observations")] |
  if length==1 then .[0].patch else error("ambiguous Harbour registration patch") end' \
  "${scratch}/apps.json" >"${scratch}/product-patch.yaml"
yq -o=json '.' "${scratch}/product-patch.yaml" >"${scratch}/product-patch.json"
jq -e '[.[] | select(.path=="/spec/connector")] == [{op:"add",path:"/spec/connector",
  value:{adapter:"deployment/v1",resourceRef:{apiVersion:"apps/v1",kind:"Deployment",name:"data-product-controller-harbour"}}}] and
  ([.[] | select(.path=="/spec/contractChecks")] == [{op:"add",path:"/spec/contractChecks",
    value:[{output:"observations",resourceRef:{apiVersion:"apps/v1",kind:"Deployment",name:"data-product-controller-contract-probe"}}]}])' \
  "${scratch}/product-patch.json" >/dev/null || fail "${provider}: Helm-owned Harbour must observe its exact serving and independent contract Deployments"

jq -e '[.[] | select(.kind == "HTTPRoute" and .spec.hostnames == ["data-products.${domain}"])] as $routes |
  ($routes | length) == 1 and
  ($routes[0].spec.rules | length) == 1 and
  $routes[0].spec.rules[0].matches == [{"path":{"type":"PathPrefix","value":"/"}}] and
  $routes[0].spec.rules[0].backendRefs == [{"name":"oauth2-proxy","namespace":"oauth2-proxy","port":80}] and
  $routes[0].metadata.annotations["gethomepage.dev/enabled"] == "true"' \
  "${scratch}/apps.json" >/dev/null || fail "${provider}: every registry path must pass through SSO and appear on Homepage"

jq -e '[.[] | select(.kind == "HTTPRoute" and .spec.hostnames == ["harbour-data.${domain}"])] as $routes |
  ($routes | length) == 1 and
  ($routes[0].spec.rules | length) == 1 and
  $routes[0].spec.rules[0].matches == [{"path":{"type":"PathPrefix","value":"/"}}] and
  $routes[0].spec.rules[0].backendRefs == [{"name":"data-product-controller-harbour","port":80}] and
  [$routes[0].spec.rules[0].filters[] | select(.type == "RequestHeaderModifier")
    | .requestHeaderModifier.remove | map(ascii_downcase) | sort] == [["authorization","cookie"]] and
  ([$routes[0].spec.rules[].filters[] | select(.type == "URLRewrite")] | length) == 0' \
  "${scratch}/apps.json" >/dev/null || fail "${provider}: public sample must retain root paths and remove browser credentials"

jq -e '[.[] | select(.kind == "ReferenceGrant" and .metadata.namespace == "oauth2-proxy")
  | select(.spec.to == [{"group":"","kind":"Service","name":"oauth2-proxy"}])
  | .spec.from[] | select(.namespace == "data-product-controller" and .kind == "HTTPRoute"
    and .group == "gateway.networking.k8s.io")] | length == 1' \
  "${scratch}/controllers.json" >/dev/null || fail "${provider}: SSO backend reference must be authorized narrowly"

yq -r 'select(.kind == "ConfigMap" and .metadata.name == "auth-proxy-config") |
  .data."dynamic.yaml"' "${scratch}/controllers.yaml" >"${scratch}/auth.yaml"
[[ "$(yq -r '.http.routers.data-products | [.rule, .service] | join("|")' "${scratch}/auth.yaml")" == "${expected_router}" ]] ||
  fail "${provider}: authenticated registry host must have its own upstream router"
[[ "$(yq -r '.http.services.data-products.loadBalancer.servers[0].url' "${scratch}/auth.yaml")" == 'http://data-product-controller.data-product-controller.svc.cluster.local:80' ]] ||
  fail "${provider}: registry upstream must reach the controller Service"

jq -e '[.[] | select(.kind == "CiliumNetworkPolicy" and .metadata.name == "allow-auth-proxy")
  | .spec.egress[] | select(.toEndpoints == [{"matchLabels":{
    "k8s:io.kubernetes.pod.namespace":"data-product-controller",
    "k8s:app.kubernetes.io/name":"data-product-controller",
    "k8s:app.kubernetes.io/instance":"data-product-controller",
    "k8s:app.kubernetes.io/component":"controller"}}])
  | .toPorts] == [[{"ports":[{"port":"8082","protocol":"TCP"}]}]]' \
  "${scratch}/controllers.json" >/dev/null || fail "${provider}: auth-proxy egress must reach only the registry component port"

jq -e '[.[] | select(.kind == "CiliumNetworkPolicy" and .metadata.namespace == "data-product-controller")
  | .metadata.name] | sort == ["allow-data-product-controller", "allow-data-product-controller-contract-probe", "allow-data-product-controller-harbour", "allow-data-product-controller-ui-kit"]' \
  "${scratch}/apps.json" >/dev/null || fail "${provider}: no additional namespace policy may bypass the component boundary"
jq -e '[.[] | select(.kind == "CiliumNetworkPolicy" and .metadata.namespace == "data-product-controller")
  | select(.spec.endpointSelector.matchLabels["k8s:app.kubernetes.io/component"] == "controller")
  | .spec.ingress] == [[{"fromEndpoints":[{"matchLabels":{"app":"auth-proxy",
    "k8s:io.kubernetes.pod.namespace":"oauth2-proxy"}}],
    "toPorts":[{"ports":[{"port":"8082","protocol":"TCP"}]}]}]]' \
  "${scratch}/apps.json" >/dev/null || fail "${provider}: controller must admit registry traffic only after SSO"
jq -e '[.[] | select(.kind == "CiliumNetworkPolicy" and .metadata.namespace == "data-product-controller")
  | select(.spec.endpointSelector.matchLabels["k8s:app.kubernetes.io/component"] == "harbour-product")
  | .spec.ingress] == [[{"fromEntities":["ingress"],
    "toPorts":[{"ports":[{"port":"8080","protocol":"TCP"}]}]}]]' \
  "${scratch}/apps.json" >/dev/null || fail "${provider}: public ingress must reach only the sample component port"
jq -e '[.[] | select(.kind == "NetworkPolicy" and .metadata.namespace == "data-product-controller")
  | .spec] == [{"podSelector":{},"policyTypes":["Ingress","Egress"]}]' \
  "${scratch}/apps.json" >/dev/null || fail "${provider}: namespace default-deny must stay active"
jq -e '[.[] | select(.kind=="Deployment" and .metadata.name=="data-product-controller-contract-probe")] as $probe |
  ($probe|length)==1 and $probe[0].spec.replicas==2 and ($probe[0].metadata.labels|has("platform.devantler.tech/replica-floor")|not) and
  ($probe[0].spec.template.spec | .serviceAccountName=="data-product-controller-contract-probe" and .automountServiceAccountToken==false and
    (.volumes//[])==[] and (.initContainers//[])==[] and .imagePullSecrets==[{name:"ghcr-auth"}] and
    (.containers|length)==1 and (.containers[0] | .name=="contract-probe" and .command==["/contract-probe"] and (.args//[])==[] and
      .env==[{name:"CONTRACT_PROBE_URL",value:"https://harbour-data.${domain}/openapi.json"},{name:"CONTRACT_READINESS_ENABLED",value:"true"}] and
      .readinessProbe.httpGet.path=="/readyz" and .livenessProbe.httpGet.path=="/healthz" and
      .securityContext.runAsNonRoot==true and .securityContext.readOnlyRootFilesystem==true and .securityContext.capabilities.drop==["ALL"])) and
  ([.[]|select(.kind=="ServiceAccount" and .metadata.name=="data-product-controller-contract-probe")|.automountServiceAccountToken]==[false]) and
  ([.[]|select((.kind=="Service" or .kind=="HTTPRoute") and .metadata.name=="data-product-controller-contract-probe")]|length)==0' \
  "${scratch}/apps.json" >/dev/null || fail "${provider}: the independent probe must have two replicas and remain token-free and private"
jq -e '[.[]|select(.kind=="Role" and .metadata.name=="data-product-readiness-observer")|.rules] ==
  [[{apiGroups:["apps"],resources:["deployments"],resourceNames:["data-product-controller-harbour","data-product-controller-contract-probe"],verbs:["get"]}]] and
  ([.[]|select(.kind=="RoleBinding" and .metadata.name=="data-product-readiness-observer") |
    {roleRef,subjects}]==[{roleRef:{apiGroup:"rbac.authorization.k8s.io",kind:"Role",name:"data-product-readiness-observer"},
      subjects:[{kind:"ServiceAccount",name:"data-product-controller",namespace:"data-product-controller"}]}])' \
  "${scratch}/apps.json" >/dev/null || fail "${provider}: observation must grant GET on exactly two Deployments to the controller"
jq -e '[.[]|select(.kind=="CiliumNetworkPolicy" and .metadata.name=="allow-data-product-controller-contract-probe")|(.specs==null or .specs==[])] == [true] and
  [.[]|select(.kind=="CiliumNetworkPolicy" and .metadata.name=="allow-data-product-controller-contract-probe")|.spec] ==
  [{endpointSelector:{matchLabels:{"k8s:app.kubernetes.io/name":"data-product-controller","k8s:app.kubernetes.io/instance":"data-product-controller","k8s:app.kubernetes.io/component":"contract-probe"}},
    egress:[{toFQDNs:[{matchName:"harbour-data.${domain}"}],toPorts:[{ports:[{port:"443",protocol:"TCP"}]}]},
      {toEndpoints:[{matchLabels:{"k8s:io.kubernetes.pod.namespace":"kube-system","k8s-app":"kube-dns"}}],
       toPorts:[{ports:[{port:"53",protocol:"UDP"},{port:"53",protocol:"TCP"}],rules:{dns:[{matchName:"harbour-data.${domain}"}]}}]}]}]' \
  "${scratch}/apps.json" >/dev/null || fail "${provider}: contract probe egress must allow only the sample HTTPS host and its DNS lookup"
# Recovery executes the candidate workflow after checking out main. A main
# predating this trial has neither the active app nor this test; an active app
# must never use a missing test as permission to skip its validation.
yq -r '.jobs."heal-prod-on-failure".steps[] |
  select(.name == "🧪 Validate the data product trial before recovery deploy") | .run' \
  "${root_dir}/.github/workflows/ci.yaml" >"${scratch}/recovery-step.sh"
[[ -s "${scratch}/recovery-step.sh" ]] || fail 'recovery trial validation step is missing'
legacy="${scratch}/legacy-main"
mkdir -p "${legacy}/k8s/bases/apps/wedding-app" "${legacy}/k8s/bases/apps/data-product-controller" "${legacy}/scripts"
cp "${root_dir}/scripts/guard-ghcr-fanout-component-gate.sh" "${legacy}/scripts/"
printf 'resources:\n  - wedding-app/\n' >"${legacy}/k8s/bases/apps/kustomization.yaml"
printf 'kind: ExternalSecret\nmetadata:\n  name: ghcr-auth\n' \
  >"${legacy}/k8s/bases/apps/wedding-app/external-secret.yaml"
cp "${legacy}/k8s/bases/apps/wedding-app/external-secret.yaml" \
  "${legacy}/k8s/bases/apps/data-product-controller/external-secret.yaml"
# The fan-out guard counts only what an app's own kustomization deploys, as a
# real app directory always has one.
for app in wedding-app data-product-controller; do
  printf 'resources:\n  - external-secret.yaml\n' >"${legacy}/k8s/bases/apps/${app}/kustomization.yaml"
done
printf 'readonly -a FANOUT_NAMESPACES=(\n  "wedding-app"\n  "kyverno"\n)\n' \
  >"${legacy}/scripts/refresh-flux-ghcr-auth.sh"
if ! (cd "${legacy}" && bash -euo pipefail "${scratch}/recovery-step.sh") \
  >"${scratch}/legacy-recovery.log" 2>&1; then
  cat "${scratch}/legacy-recovery.log" >&2
  fail 'main without the trial must recover without a candidate-only test file'
fi
yq -i '.resources += ["data-product-controller/"]' "${legacy}/k8s/bases/apps/kustomization.yaml"
if (cd "${legacy}" && bash -euo pipefail "${scratch}/recovery-step.sh") \
  >"${scratch}/active-recovery.log" 2>&1; then
  fail 'active trial must not recover without its required validation file'
fi
grep -qF 'test-data-product-controller-trial.sh' "${scratch}/active-recovery.log" ||
  fail 'active recovery must fail for the missing trial validation file'
printf 'resources: [unterminated\n' >"${legacy}/k8s/bases/apps/kustomization.yaml"
if (cd "${legacy}" && bash -euo pipefail "${scratch}/recovery-step.sh") \
  >"${scratch}/invalid-recovery.log" 2>&1; then
  fail 'an unreadable app gate must not permit recovery validation to be skipped'
fi
printf 'PASS: registry SSO, credential-free sample, component policies and checkout-aware recovery validation\n'
