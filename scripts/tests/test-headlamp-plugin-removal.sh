#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly ci_workflow="${root_dir}/.github/workflows/ci.yaml"
readonly headlamp_dir="${root_dir}/k8s/bases/apps/headlamp"
readonly headlamp_release="${headlamp_dir}/helm-release.yaml"
readonly headlamp_policy="${headlamp_dir}/cilium-network-policy.yaml"
readonly crossview_dir="${root_dir}/k8s/bases/apps/crossview"
readonly crossview_release="${crossview_dir}/helm-release.yaml"
readonly crossview_policy="${crossview_dir}/cilium-network-policy.yaml"
readonly dex_release="${root_dir}/k8s/bases/infrastructure/controllers/dex/helm-release.yaml"
readonly auth_proxy_dir="${root_dir}/k8s/bases/infrastructure/controllers/auth-proxy"
readonly auth_proxy_config="${auth_proxy_dir}/config-map.yaml"
readonly auth_proxy_policy="${auth_proxy_dir}/cilium-network-policy.yaml"
readonly oauth2_proxy_grant="${root_dir}/k8s/bases/infrastructure/controllers/oauth2-proxy/reference-grant.yaml"
readonly local_hosts="${root_dir}/hosts"
readonly crossview_origin="https://crossview.\${domain}"
readonly crossview_redirect="${crossview_origin}/"
readonly crossview_hostname="crossview.\${domain}"
readonly crossview_host_rule="Host(\`${crossview_hostname}\`)"
readonly crossview_callback="https://crossview.\${domain}/api/auth/oidc/callback"
readonly retired_crossview_callback="http://localhost:3001/api/auth/oidc/callback"
readonly dex_fqdn="dex.\${domain}"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v kubectl >/dev/null 2>&1 || fail 'kubectl is required to render the production apps overlay'
command -v yq >/dev/null 2>&1 || fail 'yq v4 is required to inspect the rendered plugin-removal contract'

grep -Fxq '127.0.0.1 crossview.platform.lan' "${local_hosts}" ||
  fail 'the optional local Crossview route must resolve through the managed hosts file'

grep -Fq \
  "'scripts/tests/test-headlamp-plugin-removal.sh'" \
  "${ci_workflow}" ||
  fail 'the Headlamp plugin-removal contract must trigger manifest validation'

grep -Fq \
  'run: bash scripts/tests/test-headlamp-plugin-removal.sh' \
  "${ci_workflow}" ||
  fail 'CI must execute the Headlamp plugin-removal contract'

yq e -e '
  .spec.values.config.watchPlugins == false and
  .spec.values.pluginsManager.enabled == false and
  .spec.values.persistentVolumeClaim.enabled == false and
  ([.spec.postRenderers[].kustomize.patches[].patch |
    select(contains("/spec/template/spec/containers/1/"))] |
  length == 0) and
  ([.spec.postRenderers[].kustomize.patches[].patch |
    select(contains("/spec/template/spec/volumes/-"))] |
  length == 0) and
  ([.spec.postRenderers[].kustomize.patches[].patch |
    select(contains("/spec/template/spec/containers/0/volumeMounts/-"))] |
  length == 0) and
  ([.spec.values.volumes[].name] | sort | join(",")) ==
    "headlamp-config,tmp-dir" and
  ([.spec.values.volumeMounts[].name] | sort | join(",")) ==
    "headlamp-config,tmp-dir"
' "${headlamp_release}" >/dev/null ||
  fail 'Headlamp must disable dynamic plugins and render writable volumes through chart values'

if grep -Fq 'claimName: headlamp' "${headlamp_release}"; then
  fail 'the rendered Headlamp Deployment must not remount the retired plugin PVC on upgrade'
fi

if grep -Fq 'persistent-volume-claim.yaml' "${headlamp_dir}/kustomization.yaml"; then
  fail 'the retired Headlamp plugin PVC must not remain in the GitOps inventory'
fi

if [[ -e "${headlamp_dir}/persistent-volume-claim.yaml" ]]; then
  fail 'the retired Headlamp plugin PVC manifest must be deleted so Flux prunes it'
fi

DEX_FQDN="${dex_fqdn}" yq e -e '
  ([.spec.egress[] | select(has("toFQDNs")) | .toFQDNs[]] | length) == 1 and
  ([.spec.egress[] | select(has("toFQDNs")) | .toFQDNs[]][0].matchName ==
    strenv(DEX_FQDN)) and
  ([.spec.egress[] | select(has("toFQDNs")) | .toFQDNs[]][0] | length) == 1
' "${headlamp_policy}" >/dev/null ||
  fail 'Headlamp egress must retain Dex only, without dynamic plugin download hosts'

CROSSVIEW_ORIGIN="${crossview_origin}" \
  CROSSVIEW_CALLBACK="${crossview_callback}" \
  yq e -e '
  .spec.values.config.server.cors.origin == strenv(CROSSVIEW_ORIGIN) and
  .spec.values.config.sso.oidc.callbackURL ==
    strenv(CROSSVIEW_CALLBACK)
' "${crossview_release}" >/dev/null ||
  fail 'Crossview must use its authenticated public origin after the Headlamp plugin is removed'

CROSSVIEW_CALLBACK="${crossview_callback}" \
  RETIRED_CROSSVIEW_CALLBACK="${retired_crossview_callback}" \
  yq e -e '
  [.spec.values.config.staticClients[] |
    select(.id == "public-client") |
    .redirectURIs[] |
    select(. == strenv(CROSSVIEW_CALLBACK))] |
  length == 1 and
  ([.spec.values.config.staticClients[] |
    select(.id == "public-client") |
    .redirectURIs[] |
    select(. == strenv(RETIRED_CROSSVIEW_CALLBACK))] |
  length == 0)
' "${dex_release}" >/dev/null ||
  fail 'Dex must register only the public Crossview callback, without the retired localhost redirect'

yq e -e '
  (.spec.ingress | length) == 2 and
  ([.spec.ingress[] |
    select(
      (.fromEndpoints | length) == 1 and
      (.fromEndpoints[0].matchLabels | length) == 2 and
      .fromEndpoints[0].matchLabels.app == "auth-proxy" and
      .fromEndpoints[0].matchLabels."k8s:io.kubernetes.pod.namespace" == "oauth2-proxy" and
      (.toPorts | length) == 1 and
      (.toPorts[0].ports | length) == 1 and
      .toPorts[0].ports[0].port == "3001" and
      .toPorts[0].ports[0].protocol == "TCP"
    )] | length) == 1 and
  ([.spec.ingress[] |
    select(
      (.fromEndpoints | length) == 1 and
      .fromEndpoints[0].matchLabels."k8s:io.kubernetes.pod.namespace" == "crossview" and
      (has("fromEntities") | not) and
      (has("toPorts") | not)
    )] | length) == 1
' "${crossview_policy}" >/dev/null ||
  fail 'Crossview ingress must contain only the maintainer auth-proxy and intra-namespace rules'

yq e -e '
  ([.spec.from[] |
    select(
      .group == "gateway.networking.k8s.io" and
      .kind == "HTTPRoute" and
      .namespace == "crossview"
    )] | length) == 1
' "${oauth2_proxy_grant}" >/dev/null ||
  fail 'oauth2-proxy must accept the Crossview route as a maintainer-gated backend'

CROSSVIEW_HOST_RULE="${crossview_host_rule}" yq e -e '
  .data."dynamic.yaml" | from_yaml |
  [
    .http.routers.crossview.rule == strenv(CROSSVIEW_HOST_RULE),
    (.http.routers.crossview.entryPoints | length) == 1,
    .http.routers.crossview.entryPoints[0] == "web",
    .http.routers.crossview.service == "crossview",
    .http.services.crossview.loadBalancer.servers[0].url ==
      "http://crossview-service.crossview.svc.cluster.local:80",
    (.http.services.crossview.loadBalancer.servers | length) == 1
  ] | all
' "${auth_proxy_config}" >/dev/null ||
  fail 'auth-proxy must route the authenticated Crossview host to crossview-service'

yq e -e '
  ([.spec.egress[] |
    select(
      (.toEndpoints | length) == 1 and
      .toEndpoints[0].matchLabels."k8s:io.kubernetes.pod.namespace" == "crossview" and
      (.toPorts | length) == 1 and
      (.toPorts[0].ports | length) == 1 and
      .toPorts[0].ports[0].port == "3001" and
      .toPorts[0].ports[0].protocol == "TCP"
    )] | length) == 1
' "${auth_proxy_policy}" >/dev/null ||
  fail 'auth-proxy must egress only to the Crossview app port for that upstream'

rendered="$(kubectl kustomize "${root_dir}/k8s/providers/hetzner/apps")" ||
  fail 'the production apps overlay must render successfully'

printf '%s\n' "${rendered}" |
  CROSSVIEW_HOSTNAME="${crossview_hostname}" \
    CROSSVIEW_REDIRECT="${crossview_redirect}" \
    yq ea -e '
    [select(
      .apiVersion == "gateway.networking.k8s.io/v1" and
      .kind == "HTTPRoute" and
      .metadata.namespace == "crossview" and
      .metadata.name == "crossview" and
      (.spec.parentRefs | length) == 1 and
      (.spec.parentRefs[0] | length) == 3 and
      .spec.parentRefs[0].name == "platform" and
      .spec.parentRefs[0].namespace == "kube-system" and
      .spec.parentRefs[0].sectionName == "https" and
      (.spec.hostnames | length) == 1 and
      .spec.hostnames[0] == strenv(CROSSVIEW_HOSTNAME) and
      ([.spec.rules[0].filters[] |
        select(
          .type == "RequestHeaderModifier" and
          (.requestHeaderModifier.set | length) == 1 and
          .requestHeaderModifier.set[0].name == "X-Auth-Request-Redirect" and
          .requestHeaderModifier.set[0].value == strenv(CROSSVIEW_REDIRECT)
        )] | length) == 1 and
      .spec.rules[0].backendRefs[0].name == "oauth2-proxy" and
      .spec.rules[0].backendRefs[0].namespace == "oauth2-proxy" and
      .spec.rules[0].backendRefs[0].port == 80 and
      (.spec.rules[0].backendRefs | length) == 1
    )] |
    length == 1
  ' - >/dev/null ||
  fail 'fresh installs must render one maintainer-gated Crossview route on the approved HTTPS parent'

printf 'PASS: Headlamp plugin removal prunes upgrade state and preserves authenticated Crossview access\n'
