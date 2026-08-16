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
readonly dex_release="${root_dir}/k8s/bases/infrastructure/controllers/dex/helm-release.yaml"
readonly crossview_origin="https://crossview.\${domain}"
readonly crossview_callback="https://crossview.\${domain}/api/auth/oidc/callback"
readonly dex_fqdn="dex.\${domain}"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v kubectl >/dev/null 2>&1 || fail 'kubectl is required to render the production apps overlay'
command -v yq >/dev/null 2>&1 || fail 'yq v4 is required to inspect the rendered plugin-removal contract'

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
  .spec.values.persistentVolumeClaim.enabled == false
' "${headlamp_release}" >/dev/null ||
  fail 'Headlamp must disable dynamic plugin installation and its plugin PVC'

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
  [.spec.egress[] | select(has("toFQDNs")) | .toFQDNs[]] ==
    [{"matchName": strenv(DEX_FQDN)}]
' "${headlamp_policy}" >/dev/null ||
  fail 'Headlamp egress must retain Dex only, without dynamic plugin download hosts'

 CROSSVIEW_ORIGIN="${crossview_origin}" \
  CROSSVIEW_CALLBACK="${crossview_callback}" \
  yq e -e '
  .spec.values.app.config.server.cors.origin == strenv(CROSSVIEW_ORIGIN) and
  .spec.values.app.config.sso.oidc.callbackURL ==
    strenv(CROSSVIEW_CALLBACK)
' "${crossview_release}" >/dev/null ||
  fail 'Crossview must use its authenticated public origin after the Headlamp plugin is removed'

CROSSVIEW_CALLBACK="${crossview_callback}" yq e -e '
  [.spec.values.config.staticClients[] |
    select(.id == "public-client") |
    .redirectURIs[] |
    select(. == strenv(CROSSVIEW_CALLBACK))] |
  length == 1
' "${dex_release}" >/dev/null ||
  fail 'Dex must register the exact Crossview public OIDC callback'

rendered="$(kubectl kustomize "${root_dir}/k8s/providers/hetzner/apps")" ||
  fail 'the production apps overlay must render successfully'

printf '%s\n' "${rendered}" |
  CROSSVIEW_ORIGIN="${crossview_origin}" \
  yq ea -e '
    [select(
      .apiVersion == "gateway.networking.k8s.io/v1" and
      .kind == "HTTPRoute" and
      .metadata.namespace == "crossview" and
      .metadata.name == "crossview" and
      .spec.hostnames == [strenv(CROSSVIEW_ORIGIN)] and
      .spec.rules[0].backendRefs == [{"name": "crossview-service", "port": 80}]
    )] |
    length == 1
  ' - >/dev/null ||
  fail 'fresh installs must render exactly one Crossview route to crossview-service'

printf 'PASS: Headlamp plugin removal prunes upgrade state and preserves authenticated Crossview access\n'
