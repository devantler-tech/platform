#!/usr/bin/env bash
# Check the public sample and private registry in the layers Flux applies.
set -euo pipefail
root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
scratch="$(mktemp -d)"
trap 'rm -rf "${scratch}"' EXIT
fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

# The Docker provider intentionally deploys no applications.
readonly provider=hetzner
readonly expected_router="Host(\`data-products.\${domain}\`)|data-products"
  kubectl kustomize "${root_dir}/k8s/providers/${provider}/apps" >"${scratch}/apps.yaml"
  kubectl kustomize "${root_dir}/k8s/providers/${provider}/infrastructure/controllers" >"${scratch}/controllers.yaml"
  yq ea -o=json '[.]' "${scratch}/apps.yaml" >"${scratch}/apps.json"
  yq ea -o=json '[.]' "${scratch}/controllers.yaml" >"${scratch}/controllers.json"

  jq -e '[.[] | select(.kind == "HelmRelease" and .metadata.namespace == "data-product-controller")
    | (.spec.values.registryUI.enabled == true and .spec.values.demoProduct.enabled == true
       and (.spec.values.uiContract.enabled // false) == false
       and (.spec.values.engineProviders.enabled // false) == false)] == [true]' \
    "${scratch}/apps.json" >/dev/null || fail "${provider}: the trial release must be active with only registry and sample enabled"

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
    | .metadata.name] | sort == ["allow-data-product-controller", "allow-data-product-controller-harbour"]' \
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
