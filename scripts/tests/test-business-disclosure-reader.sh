#!/usr/bin/env bash
# Inspect the actual rendered bootstrap payload; never authenticate or read a value.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

rendered="$(kubectl kustomize "${root_dir}/k8s/bases/infrastructure/vault-config")"
config_script="$(yq -r '
  select(.kind == "Job" and .metadata.name == "vault-config") |
  .spec.template.spec.containers[] | select(.name == "vault-config") | .command[-1]
' - <<<"${rendered}")"
[ -n "${config_script}" ] && [ "${config_script}" != null ] || fail 'missing rendered configuration script'
sh -n <<<"${config_script}"

extract_payload() {
  local start="$1" end="$2"
  awk -v start="${start}" -v end="${end}" '
    $0 == start { count++; capture=1; next }
    capture && $0 == end { complete++; capture=0; next }
    capture { print }
    END { if (count != 1 || complete != 1 || capture) exit 1 }
  ' <<<"${config_script}"
}

role="$(extract_payload "bao write auth/kubernetes/role/business-site - <<'BUSINESS_SITE_ROLE'" BUSINESS_SITE_ROLE)" ||
  fail 'missing or duplicate dedicated typed role write'
policy="$(extract_payload "bao policy write app-business-site-disclosure-readonly - <<'BUSINESS_SITE_POLICY'" BUSINESS_SITE_POLICY)" ||
  fail 'missing or duplicate dedicated policy write'

valid_role() {
  jq -e '. == {
    "bound_service_account_names": ["business-site"],
    "bound_service_account_namespaces": ["business-site"],
    "token_policies": ["app-business-site-disclosure-readonly"],
    "token_no_default_policy": true,
    "token_ttl": 3600,
    "token_max_ttl": 3600
  }' >/dev/null <<<"$1"
}

valid_policy() {
  jq -e '. == {"path": {
    "secret/data/apps/business-site/public-disclosure": {"capabilities": ["read"]},
    "auth/token/lookup-self": {"capabilities": ["read"]},
    "auth/token/revoke-self": {"capabilities": ["update"]}
  }}' >/dev/null <<<"$1"
}

valid_role "${role}" || fail 'role must bind one exact identity and one read-only policy with a bounded lifetime'
valid_policy "${policy}" || fail 'policy must read only the selected disclosure entry'

for change in \
  '.bound_service_account_names = ["*"]' \
  '.bound_service_account_namespaces = ["*"]' \
  '.bound_service_account_names += ["another-app"]' \
  '.bound_service_account_namespaces += ["another-app"]' \
  '.bound_service_account_namespace_selector = "{}"' \
  '.token_policies += ["default"]' \
  '.token_no_default_policy = false' \
  '.token_max_ttl = 0' \
  '.token_ttl = 7200' \
  'del(.bound_service_account_namespaces)'; do
  invalid="$(jq "${change}" <<<"${role}")"
  if valid_role "${invalid}"; then fail 'widened role mutation was accepted'; fi
done
for change in \
  '.path = {"secret/data/apps/business-site/*": {"capabilities": ["read"]}}' \
  '.path = {"secret/metadata/apps/business-site/public-disclosure": {"capabilities": ["read", "list"]}}' \
  '.path["secret/data/apps/business-site/public-disclosure"].capabilities += ["update"]' \
  '.path = {"secret/data/apps/another-app/public-disclosure": {"capabilities": ["read"]}}' \
  '.path["secret/data/apps/business-site/other"] = {"capabilities": ["read"]}' \
  '.path["auth/token/*"] = {"capabilities": ["read", "update"]}' \
  '.path["auth/token/lookup-self"].capabilities += ["update"]' \
  'del(.path["auth/token/revoke-self"])'; do
  invalid="$(jq "${change}" <<<"${policy}")"
  if valid_policy "${invalid}"; then fail 'widened policy mutation was accepted'; fi
done

grep -Fq "'scripts/tests/test-business-disclosure-reader.sh'" "${root_dir}/.github/workflows/ci.yaml" ||
  fail 'CI must select changes to this contract'
grep -Fq 'bash scripts/tests/test-business-disclosure-reader.sh' "${root_dir}/.github/workflows/ci.yaml" ||
  fail 'CI must execute this contract'

printf 'PASS: rendered business disclosure identity and 18 invalid-payload controls\n'
