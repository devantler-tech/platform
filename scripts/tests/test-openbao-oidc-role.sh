#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly vault_config_dir="${root_dir}/k8s/bases/infrastructure/vault-config"
readonly dex_release="${root_dir}/k8s/bases/infrastructure/controllers/dex/helm-release.yaml"
readonly ci_workflow="${root_dir}/.github/workflows/ci.yaml"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

grep -Fq \
  "'scripts/tests/test-openbao-oidc-role.sh'" \
  "${ci_workflow}" ||
  fail 'the OpenBao OIDC role contract must trigger manifest validation'

grep -Fq \
  'run: bash scripts/tests/test-openbao-oidc-role.sh' \
  "${ci_workflow}" ||
  fail 'CI must execute the OpenBao OIDC role contract'

rendered="$(kubectl kustomize "${vault_config_dir}")" ||
  fail 'the OpenBao vault-config base must render'

config_script="$(
  yq eval -r '
    select(.kind == "Job" and .metadata.name == "vault-config") |
    .spec.template.spec.containers[] |
    select(.name == "vault-config") |
    .command[-1]
  ' - <<<"${rendered}"
)" || fail 'the rendered vault-config shell script must be extractable'

sh -n <<<"${config_script}" ||
  fail 'the rendered vault-config shell script must parse'

role_payload="$(
  awk '
    /bao write auth\/oidc\/role\/admin - <<.*OIDC_ROLE_JSON/ {
      capture = 1
      next
    }
    capture && /^[[:space:]]*OIDC_ROLE_JSON[[:space:]]*$/ {
      complete = 1
      exit
    }
    capture {
      print
    }
    END {
      if (!capture || !complete) {
        exit 1
      }
    }
  ' <<<"${config_script}"
)" || fail 'the OpenBao admin role must be written as one JSON stdin payload'

jq -e '
  .bound_claims == {
    "email": ["${admin_email}"],
    "groups": ["devantler-tech:maintainers"]
  } and
  .bound_audiences == ["public-client"] and
  .allowed_redirect_uris == [
    "https://vault.${domain}/ui/vault/auth/oidc/oidc/callback",
    "http://localhost:8250/oidc/callback"
  ] and
  .user_claim == "email" and
  .groups_claim == "groups" and
  .oidc_scopes == ["openid", "email", "profile", "groups"] and
  .policies == ["vault-admin"] and
  .ttl == "8h"
' <<<"${role_payload}" >/dev/null ||
  fail 'the OpenBao admin role JSON must preserve every authorization field'

yq eval -r '
  .spec.values.config.staticClients[] |
  select(.id == "public-client") |
  .redirectURIs[]
' "${dex_release}" | grep -Fx 'http://localhost:8250/oidc/callback' >/dev/null ||
  fail 'Dex public-client must register the OpenBao CLI localhost callback'

printf 'PASS: OpenBao OIDC admin access is bound to the configured maintainer identity with a typed JSON payload\n'
